package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// crlTTL is how long hostd uses a revocation list before it asks for a fresh one.
const crlTTL = 30 * time.Second

// revocationConfig is what the flags say of the revocation list (ADR 0062).
type revocationConfig struct {
	keyFile      string
	maxAge       time.Duration
	signerSocket string
	orchURL      string
	orchCA       string
	file         string
	seedFile     string
}

// newRevocations builds the list hostd checks every client and every orch call against. It is nil,
// and logged, when no revocation key is given: such a host checks no revocation. A key with no
// source to fetch the list from is an error, because the host would refuse every client.
func newRevocations(c revocationConfig, log *slog.Logger) (*identity.RevocationList, error) {
	if c.keyFile == "" {
		log.Warn("no revocation public key: client certificates are checked against the client CA only, not against the revocation list")
		return nil, nil
	}
	data, err := os.ReadFile(c.keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the revocation public key: %w", err)
	}
	key, err := identity.ParseRevocationPublic(data)
	if err != nil {
		return nil, err
	}
	var seed []byte
	if c.seedFile != "" {
		if seed, err = os.ReadFile(c.seedFile); err != nil {
			return nil, fmt.Errorf("read the revocation list delivered with the configuration: %w", err)
		}
	}
	var src identity.CRLSource
	switch {
	case c.signerSocket != "":
		src = identity.SocketSource{Path: c.signerSocket}
	case c.orchURL != "" && c.orchCA != "":
		if src, err = orchapi.RevocationSource(c.orchURL, c.orchCA); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("--revocation-public-key needs the signer's socket or --orch-url and --orch-ca-sha256 to fetch the list from")
	}
	return &identity.RevocationList{Source: src, Key: key, TTL: crlTTL, MaxAge: c.maxAge, File: c.file, Seed: seed, Now: time.Now, Log: log}, nil
}

// prefetch takes the list once at start, so a host that cannot get one says so in its log before
// the first client is refused.
func prefetch(ctx context.Context, list *identity.RevocationList, log *slog.Logger) {
	if list == nil {
		return
	}
	if _, err := list.Current(ctx); err != nil {
		log.Warn("no usable revocation list yet: every client is refused until one is had", "err", err.Error())
		return
	}
	st := list.Status()
	log.Info("revocation list", "seq", st.Seq, "fetched", st.FetchedAt.Format(time.RFC3339))
}

// ownCertificateInterval is how often a running hostd checks its own certificates again.
const ownCertificateInterval = time.Minute

// checkOwnCertificates returns identity.ErrRevoked, naming the file and the serial, when a
// certificate in any of files is on list: a host whose seed or stored configuration carries a key
// the signer revoked must not serve with it. A file that is not there is skipped, a nil list
// revokes nothing, and a list that cannot be had is not a verdict (the error is returned plain, for
// the caller to retry).
func checkOwnCertificates(ctx context.Context, list *identity.RevocationList, files ...string) error {
	if list == nil {
		return nil
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		var chain []*x509.Certificate
		for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return fmt.Errorf("parse %s: %w", file, err)
			}
			chain = append(chain, cert)
		}
		if len(chain) == 0 {
			continue
		}
		if err := list.Check(ctx, [][]*x509.Certificate{chain}); err != nil {
			if errors.Is(err, identity.ErrRevoked) {
				return fmt.Errorf("the certificate in %s (subject %s) is on the revocation list, so its key is not used: %w", file, chain[0].Subject.CommonName, err)
			}
			return err
		}
	}
	return nil
}

// refuseRevokedKey checks the host's own certificates every interval, and calls fail with the reason
// when one is revoked. A list that cannot be had is logged, and the check retried. It returns when
// one is revoked and when ctx ends.
func refuseRevokedKey(ctx context.Context, list *identity.RevocationList, interval time.Duration, log *slog.Logger, fail func(error), files ...string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := checkOwnCertificates(ctx, list, files...)
		switch {
		case err == nil:
		case errors.Is(err, identity.ErrRevoked):
			fail(err)
			return
		case ctx.Err() == nil:
			log.Warn("cannot check the host's own certificates against the revocation list yet", "err", err.Error())
		}
	}
}
