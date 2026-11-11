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

// The OVMF firmware of the Debian ovmf package: the code is read-only and shared, the variable
// store is copied per Node.
const (
	OVMFCode = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	OVMFVars = "/usr/share/OVMF/OVMF_VARS_4M.fd"
)

// Spec says what a Node boots.
type Spec struct {
	// Config is the Node's configuration. Its Name names the QEMU process, which
	// is prefixed "naru15-" so a stray one can be found.
	Config Config
	// Disk is the image's naru.img, which the Node boots under OVMF: no -kernel, -initrd or
	// -append, and the kernel's built-in command line is the only one.
	Disk string
	// Dir receives the disks and the console log.
	Dir string
	// Mcast is the multicast address and port, "230.78.0.1:5000", of the network the
	// Nodes of a cluster share. QEMU's socket backend carries it over the loopback, with no privilege.
	Mcast string
	// NaruYAML replaces the user-data the Config renders, when not empty.
	NaruYAML []byte
	// NetworkConfig replaces the network-config the Config renders, when not empty.
	NetworkConfig []byte
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
	networkConfig := spec.NetworkConfig
	if len(networkConfig) == 0 {
		networkConfig = spec.Config.NetworkConfig()
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	cidata := filepath.Join(spec.Dir, "cidata.img")
	if err := BuildCidata(cidata, "naru15-"+spec.Config.Name, naruYAML, networkConfig); err != nil {
		return nil, err
	}
	dataDisk, err := BuildDataDisk(spec.Dir, spec.Config.Inputs.Dir)
	if err != nil {
		return nil, err
	}
	n := &Node{Name: spec.Config.Name, console: filepath.Join(spec.Dir, "console.log"), done: make(chan struct{})}
	name := "naru15-" + spec.Config.Name
	args, err := qemuArgs(spec, name, cidata, dataDisk, n.console)
	if err != nil {
		return nil, err
	}
	n.cmd = exec.Command("qemu-system-x86_64", args...) //nolint:gosec // arguments are built here
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

// qemuArgs is the QEMU command line of a Node, laid out as Proxmox lays out a guest: a q35
// machine under OVMF, the boot and data disks on a virtio-scsi controller, the NoCloud drive as a
// CD-ROM on the AHCI bus, one virtio NIC, and the serial console written to console.
func qemuArgs(spec Spec, name, cidata, dataDisk, console string) ([]string, error) {
	if _, err := os.Stat(spec.Disk); err != nil {
		return nil, fmt.Errorf("the boot disk is missing; make disk builds it: %w", err)
	}
	vars := filepath.Join(spec.Dir, "OVMF_VARS.fd")
	if err := copyInto(OVMFVars, vars); err != nil {
		return nil, fmt.Errorf("copy the OVMF variable store: %w", err)
	}
	if _, err := os.Stat(OVMFCode); err != nil {
		return nil, err
	}
	return []string{
		"-name", name + ",process=" + name,
		"-enable-kvm", "-cpu", "host", "-smp", "2", "-m", "3072",
		"-machine", "q35",
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file=" + OVMFCode,
		"-drive", "if=pflash,format=raw,unit=1,file=" + vars,
		"-device", "virtio-scsi-pci,id=scsihw0",
		"-drive", "if=none,id=boot,format=raw,readonly=on,file=" + spec.Disk,
		"-device", "scsi-hd,bus=scsihw0.0,drive=boot,bootindex=0",
		"-drive", "if=none,id=data,format=raw,file=" + dataDisk,
		"-device", "scsi-hd,bus=scsihw0.0,drive=data",
		"-drive", "if=none,id=cidata,format=raw,media=cdrom,readonly=on,file=" + cidata,
		"-device", "ide-cd,bus=ide.1,drive=cidata",
		"-netdev", "socket,id=net0,mcast=" + spec.Mcast + ",localaddr=127.0.0.1",
		"-device", "virtio-net-pci,netdev=net0,mac=" + spec.Config.MAC(),
		"-display", "none", "-monitor", "none", "-serial", "file:" + console,
		"-no-reboot",
	}, nil
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
