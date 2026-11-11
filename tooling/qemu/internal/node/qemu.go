package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Spec says what a Node boots.
type Spec struct {
	// Config is the Node's configuration. Its Name names the QEMU process, which
	// is prefixed "naru5-" so a stray one can be found.
	Config Config
	// Kernel and Initrd are the image's bzImage and initramfs.cpio.gz.
	Kernel, Initrd string
	// Dir receives the disks and the console log.
	Dir string
	// Mcast is the multicast address and port, "230.78.0.1:5000", of the network the
	// Nodes of a cluster share. QEMU's socket backend carries it over the loopback, with no privilege.
	Mcast string
	// NaruYAML replaces the configuration the Config renders, when not empty.
	NaruYAML []byte
}

// Node is one running QEMU machine.
type Node struct {
	// Name is the Node's name, such as "node-1".
	Name    string
	console string
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	once    sync.Once
}

// Start builds the Node's disks and boots it with its serial console written
// to Dir/console.log. The caller must Stop it.
func Start(spec Spec) (*Node, error) {
	naruYAML := spec.NaruYAML
	if len(naruYAML) == 0 {
		var err error
		if naruYAML, err = spec.Config.NaruYAML(); err != nil {
			return nil, err
		}
	}
	mvmdJSON, err := spec.Config.MvmdJSON()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	configDisk, dataDisk, err := BuildDisks(spec.Dir, naruYAML, mvmdJSON, spec.Config.Inputs.Dir)
	if err != nil {
		return nil, err
	}
	n := &Node{Name: spec.Config.Name, console: filepath.Join(spec.Dir, "console.log"), done: make(chan struct{})}
	name := "naru5-" + spec.Config.Name
	// ip= is client:server:gateway:netmask:hostname:device:autoconf (the kernel's nfsroot.rst).
	ip := fmt.Sprintf("ip=%s:::255.255.255.0:%s:eth0:off", spec.Config.IP(), spec.Config.Name)
	n.cmd = exec.Command("qemu-system-x86_64", //nolint:gosec // arguments are built here
		"-name", name+",process="+name,
		"-enable-kvm", "-cpu", "host", "-smp", "2", "-m", "3072",
		"-kernel", spec.Kernel, "-initrd", spec.Initrd,
		"-append", "console=ttyS0 panic=-1 "+ip,
		"-drive", "file="+configDisk+",format=raw,if=virtio,readonly=on",
		"-drive", "file="+dataDisk+",format=raw,if=virtio",
		"-netdev", "socket,id=net0,mcast="+spec.Mcast+",localaddr=127.0.0.1",
		"-device", "virtio-net-pci,netdev=net0,mac="+spec.Config.MAC(),
		"-display", "none", "-monitor", "none", "-serial", "file:"+n.console,
		"-no-reboot",
	)
	if out, err := os.Create(filepath.Join(spec.Dir, "qemu.log")); err == nil {
		n.cmd.Stdout, n.cmd.Stderr = out, out
	}
	if err := n.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	go func() {
		n.waitErr = n.cmd.Wait()
		close(n.done)
	}()
	return n, nil
}

// Console is what the Node has written to its serial console so far.
func (n *Node) Console() string {
	b, _ := os.ReadFile(n.console)
	return string(b)
}

// WaitFor returns once the console contains every line-fragment of want in
// order on one line, or an error when the timeout passes or QEMU exits first.
func (n *Node) WaitFor(ctx context.Context, timeout time.Duration, want ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if hasLine(n.Console(), want) {
			return nil
		}
		select {
		case <-n.done:
			if hasLine(n.Console(), want) {
				return nil
			}
			return fmt.Errorf("qemu exited (%v) before the console had %q", n.waitErr, want)
		case <-ctx.Done():
			return errors.New("timed out waiting for the console to have " + fmt.Sprintf("%q", want))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop ends the QEMU process and waits for it.
func (n *Node) Stop() {
	n.once.Do(func() {
		_ = n.cmd.Process.Kill()
		<-n.done
	})
}

// hasLine reports whether one line of console contains every fragment in order.
func hasLine(console string, fragments []string) bool {
	for _, line := range strings.Split(console, "\n") {
		rest := line
		ok := true
		for _, f := range fragments {
			i := strings.Index(rest, f)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(f):]
		}
		if ok {
			return true
		}
	}
	return false
}
