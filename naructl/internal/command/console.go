package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/naructl/internal/console"
)

func consoleCmd(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("console", stderr)
	listen := o.fs.String("listen", "127.0.0.1:8484", "the address the console serves on; a loopback IP address")
	ttl := o.fs.Duration("admission-ttl", time.Hour, "how long a signed admission stays valid")
	if _, err := o.parse(args, 0); err != nil {
		return err
	}
	if *ttl <= 0 {
		return errors.New("--admission-ttl must be positive")
	}
	person, orch, err := o.connect()
	if err != nil {
		return err
	}
	ln, err := console.Listen(*listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	srv, err := console.New(console.Config{
		Orch: orch,
		Sign: func(a identity.Admission) ([]byte, error) {
			return identity.SignAdmission(a, person.Key, person.Cert)
		},
		AdmissionTTL: *ttl,
		AllowedHosts: loopbackHosts(ln.Addr()),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, _ = fmt.Fprintf(stdout, "naructl: console on http://%s, acting as %s; interrupt to stop\n", ln.Addr(), person.Cert.Subject.CommonName)
	return srv.Serve(ctx, ln)
}

// loopbackHosts are the Host headers a browser sends to the loopback listener.
func loopbackHosts(addr net.Addr) []string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return []string{addr.String(), net.JoinHostPort("localhost", port)}
}
