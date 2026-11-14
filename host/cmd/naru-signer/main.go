// Command naru-signer is the signer of naru's identities (docs/architecture/signer.md): the one
// process that holds the authority keys. Its subcommands are init, serve, join-key and join.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/metrics"
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
	csrFile := fs.String("root-csr", "", "file holding the certificate request of the first root identity, PEM or DER")
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
	csr, err := readCSR(*csrFile)
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
	revPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := os.WriteFile(*revPub, revPEM, 0o644); err != nil { //nolint:gosec // a public key
		return err
	}
	// Init runs the first boot as a service whose output is on the console, which is where the
	// operator reads what spec.orch needs: neither holds a secret.
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Certificate})
	fmt.Printf("root certificate:\n%s\nrevocation public key:\n%s", certPEM, revPEM)
	return nil
}

// readCSR reads a certificate request that is PEM or DER and returns its DER.
func readCSR(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "CERTIFICATE REQUEST" {
			return nil, fmt.Errorf("%s holds a PEM %q, not a CERTIFICATE REQUEST", path, block.Type)
		}
		return block.Bytes, nil
	}
	return data, nil
}

func runServe(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "the signer's directory, mode 0700")
	socket := fs.String("socket", "/run/naru-signer/signer.sock", "unix socket of the API")
	mode := fs.String("socket-mode", "0660", "octal mode of the socket")
	peer := fs.String("peer-listen", "", "address of the peer listener other signers replicate from; empty serves none")
	names := fs.String("peer-name", "", "comma-separated DNS names or IPs of the peer listener's serving certificate")
	metricsAddr := fs.String("metrics-addr", "", "address that serves /metrics over plain HTTP; empty serves none")
	_ = fs.Parse(args)
	m, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return fmt.Errorf("--socket-mode: %w", err)
	}
	s, err := open(log, *dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// A blank signer serves its socket alone, so that restore (ADR 0040) can reach it, and ends once
	// the directory holds the authorities, so that the next start serves everything, the peer listener
	// included, which signs its serving certificate with an authority it did not have.
	blank := !s.Initialised()
	if blank {
		log.Warn("the signer holds no authorities: only the socket is served, for a restore")
		peer = new(string)
	}

	ln, err := signer.ListenSocket(*socket, os.FileMode(m), log)
	if err != nil {
		return err
	}
	reg := metrics.New()
	requests := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "naru_signer_http_requests_total", Help: "Requests the signer served, by listener, method and status code.",
	}, []string{"listener", "method", "code"})
	count := func(listener string, h http.Handler) http.Handler {
		return promhttp.InstrumentHandlerCounter(requests.MustCurryWith(prometheus.Labels{"listener": listener}), h)
	}
	if *metricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, *metricsAddr, reg); err != nil {
				log.Error("the metrics listener stopped", "addr", *metricsAddr, "err", err.Error())
			}
		}()
	}
	servers := []*http.Server{{Handler: count("socket", s.Handler()), ReadHeaderTimeout: 10 * time.Second}}
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
		servers = append(servers, &http.Server{Handler: count("peer", s.PeerHandler()), TLSConfig: conf, ReadHeaderTimeout: 10 * time.Second})
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
	filled := make(chan struct{})
	if blank {
		go func() {
			for !s.Initialised() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
			close(filled)
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-filled:
		log.Info("the signer holds the authorities now: it ends to be started with all its listeners")
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
	if err := os.WriteFile(*out, pub, 0o644); err != nil { //nolint:gosec // a public key
		return err
	}
	// Init runs this as a setup whose output is on the console, which is where the operator reads
	// the public key to have root authorise it; naructl signer authorise-replica reads the raw bytes.
	fmt.Printf("join key (base64, the raw 32 bytes of the public key):\n%s\n", base64.StdEncoding.EncodeToString(pub))
	return nil
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
