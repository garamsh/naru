// Command naru-storageprobe is the service naru's QEMU tests run to see what a storage disk is:
// it reports the file system at -dir, whether it reflinks and whether a marker survived a reboot,
// then, until it is stopped, reports every 2 s how many disks, snapshots and run directories the
// directory holds.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/storageprobe"
)

func main() {
	dir := flag.String("dir", "", "the directory a storage disk is mounted at")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-storageprobe")
	if err := storageprobe.Run(*dir, log); err != nil {
		log.Error("storageprobe failed", "err", err)
		os.Exit(1)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		storageprobe.Tree(*dir, log)
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
