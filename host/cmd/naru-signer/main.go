// Command naru-signer is the signer of naru's identities (docs/architecture/signer.md): the one
// process that holds the authority keys. Its subcommands are init, serve, join-key and join.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

const defaultDir = "/var/lib/naru/signer"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: naru-signer init|serve|join-key|join [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = runInit(log, os.Args[2:])
	case "serve":
		err = runServe(log, os.Args[2:])
	case "join-key":
		err = runJoinKey(log, os.Args[2:])
	case "join":
		err = runJoin(log, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "naru-signer: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		log.Error("naru-signer failed", "command", os.Args[1], "err", err.Error())
		os.Exit(1)
	}
}

func open(log *slog.Logger, dir string) (*signer.Signer, error) {
	return signer.Open(signer.Options{Dir: dir, Log: log})
}

func runInit(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "the signer's directory, mode 0700")
	recovery := fs.String("recovery-public", "", "file holding the recovery key's public half")
	csrFile := fs.String("root-csr", "", "file holding the DER certificate request of the first root identity")
	out := fs.String("root-cert", "", "file the first root certificate is written to")
	revPub := fs.String("revocation-public-key", "", "file the revocation key's public half is written to, PKIX PEM, which orch reads")
	_ = fs.Parse(args)
	if *recovery == "" || *csrFile == "" || *out == "" || *revPub == "" {
		return errors.New("--recovery-public, --root-csr, --root-cert and --revocation-public-key are required")
	}
	pubData, err := os.ReadFile(*recovery)
	if err != nil {
		return err
	}
	pub, err := identity.ParseRecoveryPublic(pubData)
	if err != nil {
		return err
	}
	csr, err := os.ReadFile(*csrFile)
	if err != nil {
		return err
	}
	s, err := open(log, *dir)
	if err != nil {
		return err
	}
	root, err := s.Init(signer.InitOptions{RecoveryPublic: pub, RootCSR: csr})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, root.Certificate, 0o644); err != nil { //nolint:gosec // a certificate holds nothing secret
		return err
	}
	auth, err := s.Authorities()
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(auth.RevocationKey))
	if err != nil {
		return err
	}
	return os.WriteFile(*revPub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644) //nolint:gosec // a public key
}

func runServe(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "the signer's directory, mode 0700")
	socket := fs.String("socket", "/run/naru-signer/signer.sock", "unix socket of the API")
	mode := fs.String("socket-mode", "0660", "octal mode of the socket")
	peer := fs.String("peer-listen", "", "address of the peer listener other signers replicate from; empty serves none")
	names := fs.String("peer-name", "", "comma-separated DNS names or IPs of the peer listener's serving certificate")
	_ = fs.Parse(args)
	m, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return fmt.Errorf("--socket-mode: %w", err)
	}
	s, err := open(log, *dir)
	if err != nil {
		return err
	}
	if !s.Initialised() {
		return signer.ErrNotInitialised
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ln, err := signer.ListenSocket(*socket, os.FileMode(m), log)
	if err != nil {
		return err
	}
	servers := []*http.Server{{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}}
	listeners := []net.Listener{ln}
	if *peer != "" {
		conf, err := s.PeerTLSConfig(strings.Split(*names, ","))
		if err != nil {
			return err
		}
		pl, err := net.Listen("tcp", *peer)
		if err != nil {
			return err
		}
		servers = append(servers, &http.Server{Handler: s.PeerHandler(), TLSConfig: conf, ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, pl)
	}
	errc := make(chan error, len(servers))
	for i := range servers {
		go func() {
			if servers[i].TLSConfig != nil {
				errc <- servers[i].ServeTLS(listeners[i], "", "")
				return
			}
			errc <- servers[i].Serve(listeners[i])
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}
	return nil
}

func runJoinKey(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("join-key", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "the signer's directory, mode 0700")
	out := fs.String("out", "", "file the join key's public half is written to")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("--out is required")
	}
	s, err := open(log, *dir)
	if err != nil {
		return err
	}
	pub, err := s.NewJoinKey()
	if err != nil {
		return err
	}
	return os.WriteFile(*out, pub, 0o644) //nolint:gosec // a public key
}

func runJoin(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "the signer's directory, mode 0700")
	hostID := fs.String("host-id", "", "the host ID root authorised")
	peer := fs.String("peer", "", "https URL of the existing signer's peer listener")
	peerCA := fs.String("peer-ca", "", "file holding the PEM of the orch authority the peer's certificate chains to")
	authz := fs.String("authorisation", "", "file holding root's signed replicate request")
	_ = fs.Parse(args)
	if *hostID == "" || *peer == "" || *peerCA == "" || *authz == "" {
		return errors.New("--host-id, --peer, --peer-ca and --authorisation are required")
	}
	ca, err := os.ReadFile(*peerCA)
	if err != nil {
		return err
	}
	req, err := os.ReadFile(*authz)
	if err != nil {
		return err
	}
	s, err := open(log, *dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return s.Join(ctx, signer.JoinOptions{HostID: *hostID, PeerURL: *peer, PeerCA: ca, Authorisation: req})
}
