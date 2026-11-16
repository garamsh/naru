package main

import (
	"context"
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
