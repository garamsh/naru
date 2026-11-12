// Command naru-cluster boots QEMU Nodes from the assets of a naru release, with the gaon binaries a
// harness supplies, writes cluster.json once every Node is up, and keeps the Nodes running until
// it is signalled. It needs nothing of a checkout: `go run
// github.com/garamsh/naru/tooling/qemu/cmd/naru-cluster@<commit>` runs it. docs/architecture/harness.md
// is its contract: the flags, cluster.json and what a harness may rely on.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/clusterfile"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// gaonServices are the binaries -gaon-bin may supply, named as the services are.
var gaonServices = []string{"gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "naru-cluster:", err)
		os.Exit(1)
	}
}

func run() error {
	release := flag.String("release", "", "directory holding the assets of a naru release")
	n := flag.Int("n", 2, "number of Nodes")
	dir := flag.String("dir", "", "directory for the Nodes' disks and logs, and cluster.json")
	gaonBin := flag.String("gaon-bin", "", "directory of gaon binaries (gaon-apiserver, gaon-scheduler, gaon-controllers, gaon-node-agent) the services run in place of the image's")
	extra := flag.String("inputs", "", "directory copied to /var/lib/naru/inputs/extra/ on every Node")
	disable := flag.String("disable", "", "comma-separated services left out of every Node's configuration: "+strings.Join(node.ServiceNames, ", "))
	bootPKI := flag.Bool("boot-pki", true, "node-1 issues its certificates at boot from spec.pki (false: every Node carries its pairs as files)")
	timeout := flag.Duration("timeout", 5*time.Minute, "how long the Nodes have to come up")
	flag.Parse()
	if *release == "" || *dir == "" {
		return fmt.Errorf("-release and -dir are required")
	}
	if *n < 1 {
		return fmt.Errorf("-n is %d; a cluster has at least one Node", *n)
	}
	var disabled []string
	if *disable != "" {
		disabled = strings.Split(*disable, ",")
	}
	for _, d := range disabled {
		if !slices.Contains(node.ServiceNames, d) {
			return fmt.Errorf("-disable: %q is not a service; the names are %s", d, strings.Join(node.ServiceNames, ", "))
		}
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// A cluster.json left by an earlier run would be read as this one's.
	if err := os.Remove(filepath.Join(root, clusterfile.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	rel, err := fixture.ReadRelease(*release, filepath.Join(root, "release"))
	if err != nil {
		return fmt.Errorf("read the release: %w", err)
	}
	overrides, err := stageGaonBinaries(*gaonBin, rel.Inputs.Dir)
	if err != nil {
		return err
	}
	if *extra != "" {
		if err := copyTree(*extra, filepath.Join(rel.Inputs.Dir, "extra")); err != nil {
			return fmt.Errorf("-inputs: %w", err)
		}
	}

	c, err := node.StartCluster(node.ClusterSpec{
		N: *n, Disk: rel.BootDisk, Dir: root, Inputs: rel.Inputs, Gaon: true, BootPKI: *bootPKI,
		Disable: disabled, GaonBinaries: overrides,
	})
	if err != nil {
		return err
	}
	defer c.Stop()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, nd := range c.Nodes {
		log.Info("node started", "node", nd.Name, "pid", nd.Pid(), "serial", nd.ConsolePath())
		// A Node whose process ends before the cluster does is a power-off, such as a harness's
		// kill: it is logged, not restarted, and the other Nodes keep running.
		go func() {
			select {
			case <-nd.Done():
				if ctx.Err() == nil {
					log.Warn("node lost: its QEMU process ended; treated as a power-off, not restarted", "node", nd.Name, "pid", nd.Pid())
				}
			case <-ctx.Done():
			}
		}()
	}

	if err := waitUp(ctx, c, disabled, *timeout, log); err != nil {
		return err
	}
	if err := writeClusterFile(root, c, rel.Inputs.Image); err != nil {
		return err
	}
	log.Info("every Node is up", "file", filepath.Join(root, clusterfile.Name))
	<-ctx.Done()
	log.Info("signalled; stopping every Node")
	return nil
}

// stageGaonBinaries copies the gaon binaries found in from into the inputs tree under gaon/, and
// returns the services they are for. A binary the directory lacks is not an error: the Nodes run
// the image's.
func stageGaonBinaries(from, inputs string) ([]string, error) {
	if from == "" {
		return nil, nil
	}
	var found []string
	for _, name := range gaonServices {
		src := filepath.Join(from, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("-gaon-bin: %w", err)
		}
		if err := copyFile(src, filepath.Join(inputs, "gaon", name), 0o755); err != nil {
			return nil, fmt.Errorf("-gaon-bin: %w", err)
		}
		found = append(found, name)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("-gaon-bin: %s holds none of %s", from, strings.Join(gaonServices, ", "))
	}
	return found, nil
}

// copyTree copies the files of from into to, directories 0755, files 0755 where the source is
// executable and 0644 otherwise, since the services read them as other users.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return copyFile(p, filepath.Join(to, rel), mode)
	})
}

func copyFile(from, to string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(to, mode)
}

// waitUp waits until every Node's init has started the services its configuration names, and, where
// the API server and the Node Agent both run, until every Node is registered and Ready.
func waitUp(ctx context.Context, c *node.Cluster, disabled []string, timeout time.Duration, log *slog.Logger) error {
	on := func(s string) bool { return !slices.Contains(disabled, s) }
	for _, nd := range c.Nodes {
		services := []string{"registry", "gaon-node-agent"}
		if nd == c.Nodes[0] {
			services = append(services, "etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers")
		}
		for _, s := range services {
			if !on(s) {
				continue
			}
			if err := nd.WaitFor(ctx, timeout, "started", "service="+s); err != nil {
				return fmt.Errorf("%s: %w\n%s", nd.Name, err, nd.Console())
			}
		}
		log.Info("node's services started", "node", nd.Name)
	}
	if !on("gaon-apiserver") || !on("gaon-node-agent") {
		return nil
	}
	api, err := hostapi.New(c.HostAddr, c.Certs)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for _, nd := range c.Nodes {
		for {
			if got, err := api.Node(ctx, nd.Name); err == nil && got.Ready() {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not register and become Ready within %s", nd.Name, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		log.Info("node is registered and Ready", "node", nd.Name)
	}
	return nil
}

// writeClusterFile writes the client pair and the CA under dir/pki and then cluster.json, which
// comes last: its presence says every Node is up.
func writeClusterFile(dir string, c *node.Cluster, img fixture.Image) error {
	pkiDir := filepath.Join(dir, "pki")
	if err := os.MkdirAll(pkiDir, 0o755); err != nil {
		return err
	}
	f := clusterfile.File{
		Version:   clusterfile.Version,
		APIServer: clusterfile.APIServer{Address: c.HostAddr},
		PKI: clusterfile.PKI{
			CA: filepath.Join(pkiDir, "ca.pem"), ClientCert: filepath.Join(pkiDir, "client.pem"), ClientKey: filepath.Join(pkiDir, "client.key"),
		},
		Images: []clusterfile.Image{{Reference: img.Repository + "@" + img.Digest}},
	}
	for path, content := range map[string][]byte{f.PKI.CA: c.Certs.CA, f.PKI.ClientCert: c.Certs.Client.CertPEM, f.PKI.ClientKey: c.Certs.Client.KeyPEM} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
	}
	for i, nd := range c.Nodes {
		f.Nodes = append(f.Nodes, clusterfile.Node{Name: nd.Name, PID: nd.Pid(), SerialLog: nd.ConsolePath(), ControlPlane: i == 0})
	}
	return clusterfile.Write(dir, f)
}
