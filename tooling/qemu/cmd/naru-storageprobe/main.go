// Command naru-storageprobe is the service naru's QEMU tests run to see what a storage disk is:
// it reports the file system at -dir, whether it reflinks and whether a marker survived a reboot,
// then waits to be stopped.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
	<-stop
}
