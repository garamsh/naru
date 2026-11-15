package identity

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
)

// ErrNoRevocationList is returned when no signed revocation list could be had yet, so nobody can
// be told apart from a revoked identity.
var ErrNoRevocationList = errors.New("no revocation list is available")

// ErrStaleRevocationList is returned when the cached list is older than MaxAge and the signer
// could not give a fresh one, so a revocation made since may not be known.
var ErrStaleRevocationList = errors.New("the revocation list is too old")

// DefaultMaxAge is how old a cached revocation list may be, when no refresh has succeeded, before
// callers are refused.
const DefaultMaxAge = 10 * time.Minute

// DefaultRetryInterval is how long a failed or running fetch of the list holds off the next one.
const DefaultRetryInterval = 10 * time.Second

// fetchTimeout bounds one fetch of the list.
const fetchTimeout = 5 * time.Second

// CRLSource returns the revocation list as the signer signed it.
type CRLSource interface {
	CRL(ctx context.Context) ([]byte, error)
}

// RevocationList keeps the signer's revocation list for a process that checks certificates
// against it: at most TTL old when the source answers, kept for MaxAge from its last refresh when
// it does not, and after that every check fails with ErrStaleRevocationList. With no list at all
// every check fails with ErrNoRevocationList. A fetched list is verified against Key and is never
// replaced by one with a lower sequence number.
type RevocationList struct {
	Source CRLSource
	// Key verifies the list's signature.
	Key ed25519.PublicKey
	TTL time.Duration
	// MaxAge bounds how long a list is kept while it cannot be refreshed; zero means DefaultMaxAge.
	MaxAge time.Duration
	// File, when set, keeps the last verified list and the time it was fetched, so that a restart
	// within MaxAge needs no fetch first.
	File string
	// RetryInterval is how long after a fetch was started none is started again while the list is
	// older than TTL; zero means DefaultRetryInterval.
	RetryInterval time.Duration
	// Seed, when set, is a signed list delivered with the configuration. It is taken when File holds
	// none or a lower one, and is as old as the time it was signed.
	Seed []byte
	Now  func() time.Time
	Log  *slog.Logger

	mu       sync.Mutex
	loaded   bool
	fetching bool
	retryAt  time.Time
	crl      *CRL
	fetched  time.Time
	lastErr  string
}

// RevocationStatus is what a process reports of its list.
type RevocationStatus struct {
	// Seq is the sequence number of the list held; Held is false when there is none.
	Seq  uint64
	Held bool
	// FetchedAt is when the list held was last refreshed.
	FetchedAt time.Time
	// LastError is why the last fetch failed, and empty when it succeeded.
	LastError string
}

// Status reports the list held and the last fetch error.
func (r *RevocationList) Status() RevocationStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	s := RevocationStatus{FetchedAt: r.fetched, LastError: r.lastErr}
	if r.crl != nil {
		s.Held, s.Seq = true, r.crl.Seq
	}
	return s
}

// Age returns how old the list held is, and false when none is held.
func (r *RevocationList) Age() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureLoaded()
	if r.crl == nil {
		return 0, false
	}
	return r.now().Sub(r.fetched), true
}

// Check returns an error when the list cannot be had or every chain is revoked: a chain that holds
// no certificate on the list is enough, as the TLS library accepts a certificate with any chain.
func (r *RevocationList) Check(ctx context.Context, chains [][]*x509.Certificate) error {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return ErrNoChain
	}
	crl, err := r.Current(ctx)
	if err != nil {
		return err
	}
	var revoked error
	for _, chain := range chains {
		if revoked = crl.CheckChain(chain); revoked == nil {
			return nil
		}
	}
	return revoked
}

// VerifyConnection is Check over the verified chains of a handshake, for tls.Config.VerifyConnection
// of a server that requires and verifies a client certificate.
func (r *RevocationList) VerifyConnection(cs tls.ConnectionState) error {
	return r.Check(context.Background(), cs.VerifiedChains)
}

// Current returns the list in force, refreshing it when it is older than TTL. One caller at a time
// fetches, outside the lock; a caller that finds a fetch under way, or one that failed less than
// RetryInterval ago, does not wait for the source and is answered from the list held while that is
// within MaxAge.
func (r *RevocationList) Current(ctx context.Context) (*CRL, error) {
	r.mu.Lock()
	r.ensureLoaded()
	now := r.now()
	if r.crl != nil && now.Sub(r.fetched) < r.TTL {
		crl := r.crl
		r.mu.Unlock()
		return crl, nil
	}
	if r.fetching || now.Before(r.retryAt) {
		defer r.mu.Unlock()
		return r.heldOrRefuse(now, errors.New(cmp.Or(r.lastErr, "a fetch is under way")))
	}
	r.fetching = true
	r.retryAt = now.Add(r.retry())
	prev := r.crl
	r.mu.Unlock()

	crl, data, err := r.fetch(ctx, prev)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetching = false
	now = r.now()
	if err != nil {
		r.lastErr = err.Error()
		return r.heldOrRefuse(now, err)
	}
	r.lastErr, r.retryAt = "", time.Time{}
	r.crl, r.fetched = crl, now
	r.save(data)
	return crl, nil
}

// heldOrRefuse answers from the list held when it is within MaxAge and refuses otherwise. The caller
// holds r.mu.
func (r *RevocationList) heldOrRefuse(now time.Time, cause error) (*CRL, error) {
	if r.crl == nil {
		return nil, fmt.Errorf("%w: %v", ErrNoRevocationList, cause)
	}
	if age := now.Sub(r.fetched); age > r.maxAge() {
		return nil, fmt.Errorf("%w: %s old, not refreshed: %v", ErrStaleRevocationList, age.Round(time.Second), cause)
	}
	r.log().Warn("revocation list not refreshed, keeping the last one", "err", cause.Error())
	return r.crl, nil
}

func (r *RevocationList) maxAge() time.Duration {
	if r.MaxAge == 0 {
		return DefaultMaxAge
	}
	return r.MaxAge
}

func (r *RevocationList) retry() time.Duration {
	if r.RetryInterval == 0 {
		return DefaultRetryInterval
	}
	return r.RetryInterval
}

func (r *RevocationList) ensureLoaded() {
	if !r.loaded {
		r.load()
		r.loaded = true
	}
}

func (r *RevocationList) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *RevocationList) log() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

func (r *RevocationList) fetch(ctx context.Context, prev *CRL) (*CRL, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	data, err := r.Source.CRL(ctx)
	if err != nil {
		return nil, nil, err
	}
	crl, err := VerifyCRL(r.Key, data)
	if err != nil {
		return nil, nil, err
	}
	if prev != nil {
		if err := crl.CheckSeq(prev.Seq); err != nil {
			return nil, nil, err
		}
	}
	return crl, data, nil
}

// kept is the list File holds.
type kept struct {
	FetchedAt time.Time `json:"fetchedAt"`
	List      []byte    `json:"list"`
}

// load reads File and SeedFile. Neither is trusted beyond the signature: a file that fails to
// verify is ignored and logged.
func (r *RevocationList) load() {
	if r.File != "" {
		if b, err := os.ReadFile(r.File); err == nil {
			crl, verr := r.verifyKept(b)
			if verr != nil {
				r.log().Warn("the kept revocation list is ignored", "file", r.File, "err", verr.Error())
			} else {
				r.crl, r.fetched = crl.CRL, crl.fetched
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			r.log().Warn("the kept revocation list cannot be read", "file", r.File, "err", err.Error())
		}
	}
	if len(r.Seed) == 0 {
		return
	}
	crl, err := VerifyCRL(r.Key, r.Seed)
	if err != nil {
		r.log().Warn("the revocation list delivered with the configuration is ignored", "err", err.Error())
		return
	}
	if r.crl == nil || crl.Seq > r.crl.Seq {
		r.crl, r.fetched = crl, seedFetched(crl.IssuedAt, r.now())
		r.save(r.Seed)
	}
}

type keptList struct {
	*CRL
	fetched time.Time
}

func (r *RevocationList) verifyKept(b []byte) (keptList, error) {
	var k kept
	if err := json.Unmarshal(b, &k); err != nil {
		return keptList{}, err
	}
	crl, err := VerifyCRL(r.Key, k.List)
	return keptList{crl, k.FetchedAt}, err
}

// save keeps data in File. A failure is logged: the list in memory is still in force.
func (r *RevocationList) save(data []byte) {
	if r.File == "" {
		return
	}
	b, err := json.Marshal(kept{FetchedAt: r.fetched, List: data})
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(r.File), 0o700); err == nil {
			err = atomicfile.Write(r.File, b, 0o600)
		}
	}
	if err != nil {
		r.log().Warn("the revocation list cannot be kept", "file", r.File, "err", err.Error())
	}
}

// seedFetched is when a seed counts as refreshed: when it was signed, and not later than now.
func seedFetched(issued, now time.Time) time.Time {
	if issued.After(now) {
		return now
	}
	return issued
}
