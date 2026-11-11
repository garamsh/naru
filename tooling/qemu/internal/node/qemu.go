package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
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
	Name string
	// argv builds the QEMU command line for a console file; Reboot starts the same machine again
	// from it.
	argv    func(console string) []string
	dir     string
	boots   int
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
	name := "naru15-" + spec.Config.Name
	n := &Node{Name: spec.Config.Name, dir: spec.Dir}
	if _, err := qemuArgs(spec, name, cidata, dataDisk, ""); err != nil {
		return nil, err
	}
	n.argv = func(console string) []string {
		args, _ := qemuArgs(spec, name, cidata, dataDisk, console)
		return args
	}
	if err := n.launch(); err != nil {
		return nil, err
	}
	return n, nil
}

// launch starts the QEMU process of the Node's next boot, with the serial console written to a file
// of its own: console.log for the first boot, console-2.log for the second.
func (n *Node) launch() error {
	n.boots++
	n.console = filepath.Join(n.dir, "console.log")
	if n.boots > 1 {
		n.console = filepath.Join(n.dir, fmt.Sprintf("console-%d.log", n.boots))
	}
	n.done = make(chan struct{})
	n.once = sync.Once{}
	n.waitErr = nil
	cmd := exec.Command("qemu-system-x86_64", n.argv(n.console)...) //nolint:gosec // arguments are built here
	if out, err := os.Create(filepath.Join(n.dir, "qemu.log")); err == nil {
		cmd.Stdout, cmd.Stderr = out, out
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu: %w", err)
	}
	n.cmd = cmd
	done := n.done
	go func() {
		n.waitErr = cmd.Wait()
		close(done)
	}()
	return nil
}

// Reboot kills the Node's QEMU process, as a power cut does, and starts it again on the same boot
// disk, data disk, NoCloud drive and firmware variables. Its Console is then the new boot's. It must
// not be called while a WaitFor is in progress.
func (n *Node) Reboot() error {
	n.Stop()
	return n.launch()
}

// qemuArgs is the QEMU command line of a Node, laid out as Proxmox lays out a guest: a q35
// machine under OVMF, the boot and data disks on a virtio-scsi controller, the NoCloud drive as a
// CD-ROM on the AHCI bus, one virtio NIC, and the serial console written to console.
func qemuArgs(spec Spec, name, cidata, dataDisk, console string) ([]string, error) {
	if _, err := os.Stat(spec.Disk); err != nil {
		return nil, fmt.Errorf("the boot disk is missing; make disk builds it: %w", err)
	}
	vars := filepath.Join(spec.Dir, "OVMF_VARS.fd")
	// The variable store is copied once: a Node that boots again keeps its own.
	if _, err := os.Stat(vars); err != nil {
		if err := copyInto(OVMFVars, vars); err != nil {
			return nil, fmt.Errorf("copy the OVMF variable store: %w", err)
		}
	}
	if _, err := os.Stat(OVMFCode); err != nil {
		return nil, err
	}
	args := []string{
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
	}
	// The control-plane Node of a gaon cluster has a second NIC on QEMU's user-mode network,
	// whose forward from 127.0.0.1 is how the test host reaches the API server without
	// privilege (ADR 0012). The network has no route off the host and the Nodes' network is
	// untouched.
	if spec.Config.hostNIC() {
		args = append(args,
			"-netdev", fmt.Sprintf("user,id=net1,restrict=on,hostfwd=tcp:127.0.0.1:%d-%s:%s", spec.Config.Gaon.HostPort, HostNICAddress, APIPort),
			"-device", "virtio-net-pci,netdev=net1,mac="+spec.Config.hostMAC(),
		)
	}
	return append(args, "-display", "none", "-monitor", "none", "-serial", "file:"+console, "-no-reboot"), nil
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
func hasLine(console string, fragments []string) bool { return judge.HasLine(console, fragments) }
