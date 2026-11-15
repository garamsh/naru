package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/naructl/internal/console"
)

func consoleCmd(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	listen := o.fs.String("listen", "127.0.0.1:8484", "the address the console serves on; a loopback IP address")
	ttl := o.fs.Duration("admission-ttl", time.Hour, "how long a signed admission stays valid")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if *ttl <= 0 {
			return errors.New("--admission-ttl must be positive")
		}
		person, orch, err := o.connect()
		if err != nil {
			return err
		}
		caDigest, err := authorityDigest(*o.ca)
		if err != nil {
			return err
		}
		ln, err := console.Listen(*listen)
		if err != nil {
			return err
		}
		defer func() { _ = ln.Close() }()
		hosts, err := loopbackHosts(ln.Addr())
		if err != nil {
			return err
		}
		srv, err := console.New(console.Config{
			Orch:  orch,
			Fleet: console.NewFleet(orch),
			Sign: func(a identity.Admission) ([]byte, error) {
				return identity.SignAdmission(a, person.Key, person.Cert)
			},
			AdmissionTTL: *ttl,
			AllowedHosts: hosts,
			OrchURL:      *o.url,
			OrchCASHA256: caDigest,
		})
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		_, _ = fmt.Fprintf(stdout, "naructl: console on http://%s, acting as %s; interrupt to stop\n", ln.Addr(), person.Cert.Subject.CommonName)
		return srv.Serve(ctx, ln)
	}
}

// authorityDigest is the hex SHA-256 of the first certificate of the --orch-ca file: what an install
// seed gives a host to pin orch's authority by.
func authorityDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the authority: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("the authority file holds no certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// loopbackHosts are the Host headers a browser sends to the loopback listener.
func loopbackHosts(addr net.Addr) ([]string, error) {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, fmt.Errorf("the console's address %q has no port: %w", addr, err)
	}
	return []string{addr.String(), net.JoinHostPort("localhost", port)}, nil
}
