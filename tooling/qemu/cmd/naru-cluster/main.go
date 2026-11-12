// Command naru-cluster boots QEMU Nodes from the assets of a naru release, with the gaon binaries a
// harness supplies, writes cluster.json once every Node is up, and keeps the Nodes running until
// it is signalled. It needs nothing of a checkout: `GOBIN=<dir> go install
// github.com/garamsh/naru/tooling/qemu/cmd/naru-cluster@<commit>` builds it, and `<dir>/naru-cluster`
// runs it. docs/architecture/harness.md is its contract: the flags, cluster.json, the exit statuses
// and what a harness may rely on.
package main

import (
	"context"
	"errors"
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
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// gaonServices are the binaries -gaon-bin may supply, named as the services are.
var gaonServices = []string{"gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}

// disableNames are the services -disable may name; naructl refuses any other.
var disableNames = []string{"registry", "etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}

// exitSignalled is the status of a run that was signalled before cluster.json was written.
const exitSignalled = 3

// consoleTail is how many lines of a Node's console an error carries.
const consoleTail = 40

// errSignalled says a signal arrived before every Node was up.
var errSignalled = errors.New("signalled before every Node was up; every Node started was stopped")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "naru-cluster:", err)
		if errors.Is(err, errSignalled) {
			os.Exit(exitSignalled)
		}
		os.Exit(1)
	}
}

func run() error {
	release := flag.String("release", "", "directory holding the assets of a naru release")
	n := flag.Int("n", 2, "number of Nodes")
	dir := flag.String("dir", "", "directory for the Nodes' disks and logs, and cluster.json")
	gaonBin := flag.String("gaon-bin", "", "directory of gaon binaries (gaon-apiserver, gaon-scheduler, gaon-controllers, gaon-node-agent) the services run in place of the image's")
	extra := flag.String("inputs", "", "directory copied to /var/lib/naru/inputs/extra/ on every Node")
	disable := flag.String("disable", "", "comma-separated services left out of every Node's configuration: "+strings.Join(disableNames, ", "))
	bootPKI := flag.Bool("boot-pki", true, "node-1 issues its certificates at boot from spec.pki and signs the agents' requests, each agent presents a bootstrap token (false: every Node carries its pairs as files)")
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
		if !slices.Contains(disableNames, d) {
			return fmt.Errorf("-disable: %q is not a service; the names are %s", d, strings.Join(disableNames, ", "))
		}
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	// A directory of an earlier run would carry its inputs and Node directories into this one.
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		return fmt.Errorf("-dir %s is not empty; give it a directory that does not exist or is empty", root)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	// The handler comes before the first Node, so that a signal at any point stops the Nodes started.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(root, 0o755); err != nil {
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
	if ctx.Err() != nil {
		return errSignalled
	}

	c, err := node.StartCluster(node.ClusterSpec{
		N: *n, Disk: rel.BootDisk, Dir: root, Inputs: rel.Inputs, Gaon: true, BootPKI: *bootPKI, Naructl: naructl.Tool{Bin: rel.Naructl},
		Disable: disabled, GaonBinaries: overrides,
	})
	if err != nil {
		return err
	}
	defer c.Stop()
	if ctx.Err() != nil {
		return errSignalled
	}
	deadline := time.Now().Add(*timeout)
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

	if err := waitUp(ctx, c, disabled, deadline, log); err != nil {
		if ctx.Err() != nil {
			return errSignalled
		}
		return err
	}
	if ctx.Err() != nil {
		return errSignalled
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
// executable and 0644 otherwise, since the services read them as other users. It refuses a
// symbolic link or any file that is not regular, so what a Node carries is what the directory holds.
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
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file or a directory (a symbolic link is refused)", p)
		}
		info, err := d.Info()
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

// upNode is what waitServices needs of a Node, so that a test can stand in for one.
type upNode interface {
	WaitFor(ctx context.Context, timeout time.Duration, want ...string) error
	Console() string
}

// serviceWait is a Node and the services whose start waitServices waits for.
type serviceWait struct {
	name     string
	node     upNode
	services []string
}

// waitServices waits until every Node's init has started its services. The deadline is one for all
// of them together.
func waitServices(ctx context.Context, deadline time.Time, waits []serviceWait, log *slog.Logger) error {
	for _, w := range waits {
		for _, s := range w.services {
			if err := w.node.WaitFor(ctx, time.Until(deadline), "started", "service="+s); err != nil {
				return fmt.Errorf("%s: %w\n%s", w.name, err, tail(w.node.Console(), consoleTail))
			}
		}
		log.Info("node's services started", "node", w.name)
	}
	return nil
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// waitUp waits until every Node's init has started the services its configuration names, and, where
// the API server and the Node Agent both run, until every Node is registered and Ready, all before
// the one deadline.
func waitUp(ctx context.Context, c *node.Cluster, disabled []string, deadline time.Time, log *slog.Logger) error {
	on := func(s string) bool { return !slices.Contains(disabled, s) }
	var waits []serviceWait
	for i, nd := range c.Nodes {
		services := []string{"registry", "gaon-node-agent"}
		if i == 0 {
			services = append(services, "etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers")
		}
		services = slices.DeleteFunc(services, func(s string) bool { return !on(s) })
		waits = append(waits, serviceWait{name: nd.Name, node: nd, services: services})
	}
	if err := waitServices(ctx, deadline, waits, log); err != nil {
		return err
	}
	if !on("gaon-apiserver") || !on("gaon-node-agent") {
		return nil
	}
	api, err := hostapi.New(c.HostAddr, c.Client)
	if err != nil {
		return err
	}
	for _, nd := range c.Nodes {
		for {
			if got, err := api.Node(ctx, nd.Name); err == nil && got.Ready() {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not register and become Ready before the -timeout deadline", nd.Name)
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
	for to, from := range map[string]string{f.PKI.CA: c.Client.CA, f.PKI.ClientCert: c.Client.Cert, f.PKI.ClientKey: c.Client.Key} {
		content, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, content, 0o600); err != nil {
			return err
		}
	}
	for i, nd := range c.Nodes {
		f.Nodes = append(f.Nodes, clusterfile.Node{Name: nd.Name, PID: nd.Pid(), SerialLog: nd.ConsolePath(), ControlPlane: i == 0})
	}
	return clusterfile.Write(dir, f)
}
