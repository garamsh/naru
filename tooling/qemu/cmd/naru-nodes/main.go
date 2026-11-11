// Command naru-nodes boots N naru Nodes under QEMU on one network and keeps
// them up until it is interrupted. It reports each Node's console log, so
// that the Nodes can be watched; `make nodes` runs it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "naru-nodes:", err)
		os.Exit(1)
	}
}

func run() error {
	n := flag.Int("n", 2, "number of Nodes")
	root := flag.String("root", ".", "repository root")
	image := flag.String("image", "", "directory make image writes")
	guest := flag.String("guest", "", "directory make guest-inputs writes")
	dir := flag.String("dir", "", "directory for the Nodes' disks and logs")
	probe := flag.Bool("probe", false, "run the mvmd-restart probe on node-1")
	timeout := flag.Duration("timeout", 3*time.Minute, "how long the Nodes have to reach each other")
	flag.Parse()
	if *image == "" || *guest == "" || *dir == "" {
		return fmt.Errorf("-image, -guest and -dir are required")
	}
	if *n < 2 {
		return fmt.Errorf("-n is %d; the nodes target boots at least two", *n)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	staged, err := fixture.Stage(fixture.Sources{Root: *root, ImageDir: *image, GuestDir: *guest}, filepath.Join(*dir, "inputs"))
	if err != nil {
		return fmt.Errorf("stage the inputs: %w", err)
	}
	probeOn := 0
	if *probe {
		probeOn = 1
	}
	c, err := node.StartCluster(node.ClusterSpec{
		N: *n, Kernel: filepath.Join(*image, "bzImage"), Initrd: filepath.Join(*image, "initramfs.cpio.gz"),
		Dir: *dir, Inputs: staged, ProbeOn: probeOn,
	})
	if err != nil {
		return err
	}
	defer c.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, nd := range c.Nodes {
		if err := nd.WaitFor(ctx, *timeout, "peer accepts connections"); err != nil {
			return fmt.Errorf("%s: %w\n%s", nd.Name, err, nd.Console())
		}
		fmt.Printf("%s: up; its stand-in dialled its peer across the network (console: %s/%s/console.log)\n", nd.Name, *dir, nd.Name)
	}
	fmt.Printf("%d nodes are up; interrupt to stop them\n", *n)
	<-ctx.Done()
	return nil
}
