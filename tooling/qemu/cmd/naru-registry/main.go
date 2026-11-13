// Command naru-registry is the service the QEMU tests configure to serve a
// test OCI image over TLS on a Node's loopback, so that mvmd pulls it with no
// host in the Node's network. See internal/registry.
package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/garamsh/naru/tooling/qemu/internal/registry"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:5000", "address to serve on")
	dir := flag.String("dir", "", "directory holding index.json, the blobs, cert.pem and key.pem")
	cert := flag.String("cert", "", "certificate file; dir/cert.pem when empty")
	key := flag.String("key", "", "key file; dir/key.pem when empty")
	user := flag.String("user", "", "require this user (HTTP basic authentication)")
	password := flag.String("password", "", "the password of -user")
	flag.Parse()
	if *cert == "" {
		*cert = filepath.Join(*dir, "cert.pem")
	}
	if *key == "" {
		*key = filepath.Join(*dir, "key.pem")
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-registry")
	slog.SetDefault(log)
	log.Info("serving", "addr", *addr, "dir", *dir)
	if err := registry.Serve(*addr, *dir, *cert, *key, registry.Basic{User: *user, Password: *password}); err != nil {
		log.Error("registry failed", "err", err)
		os.Exit(1)
	}
}
