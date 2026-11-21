package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	issuer   = "https://token.actions.githubusercontent.com"
	identity = "https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/v1.2.3"
	version  = "v1.2.3"
)

// testRequirements are the verifier's, less the signed certificate timestamp, which the test
// Sigstore's certificates carry none of.
var testRequirements = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}

// noProof is an entity whose bundle carries no inclusion proof.
type noProof struct{ verify.SignedEntity }

func (noProof) HasInclusionProof() bool { return false }

// withProof is an entity that says its bundle carries an inclusion proof: the test Sigstore signs
// a message with a signed entry timestamp and no proof, which a real cosign bundle also carries.
type withProof struct{ verify.SignedEntity }

func (withProof) HasInclusionProof() bool { return true }

func hexSum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type release struct {
	dir            string
	vs             *ca.VirtualSigstore
	kernel, initrd []byte
	store          *Store
}

// newRelease writes a release of version into a directory, SHA256SUMS signed by signer under issuer.
func newRelease(t *testing.T, signer string, edit func(r *release)) *release {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	r := &release{dir: t.TempDir(), vs: vs, kernel: []byte("kernel bytes"), initrd: []byte("initramfs bytes")}
	m, _ := json.Marshal(manifest{Name: "naru-node-v1-2-3", Version: version,
		Kernel:    file{File: "vmlinuz", SHA256: hexSum(r.kernel), Bytes: int64(len(r.kernel))},
		Initramfs: file{File: "initramfs.img", SHA256: hexSum(r.initrd), Bytes: int64(len(r.initrd))}})
	if edit != nil {
		edit(r)
	}
	r.write(t, "vmlinuz", r.kernel)
	r.write(t, "initramfs.img", r.initrd)
	r.write(t, "naru-node-"+version+".json", m)
	sums := []byte(hexSum(m) + "  naru-node-" + version + ".json\n" +
		hexSum(r.kernel) + "  vmlinuz\n" + hexSum(r.initrd) + "  initramfs.img\n")
	r.write(t, "SHA256SUMS", sums)
	r.write(t, "SHA256SUMS.sigstore.json", []byte("bundle"))
	e, err := vs.Sign(signer, issuer, sums)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := newSignatureWith(vs, issuer, identity, testRequirements...)
	if err != nil {
		t.Fatal(err)
	}
	sig.parse = func([]byte) (verify.SignedEntity, error) { return withProof{e}, nil }
	r.store = &Store{src: dirSource(r.dir), sig: sig}
	return r
}

func (r *release) write(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFetchReturnsTheVerifiedBootSet(t *testing.T) {
	r := newRelease(t, identity, nil)
	set, err := r.store.Fetch(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	if set.Name != "naru-node-v1-2-3" || set.KernelSHA256 != hexSum(r.kernel) || set.InitramfsSHA256 != hexSum(r.initrd) {
		t.Fatalf("boot set = %+v", set)
	}
	k, i, err := set.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	defer i.Close()
	if got, err := io.ReadAll(k); err != nil || string(got) != string(r.kernel) {
		t.Fatalf("kernel = %q, %v", got, err)
	}
	if got, err := io.ReadAll(i); err != nil || string(got) != string(r.initrd) {
		t.Fatalf("initramfs = %q, %v", got, err)
	}
}

func TestFetchRefusesARelease(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T) *release
		want error
	}{
		{"bad signature: the bundle signs other bytes", func(t *testing.T) *release {
			r := newRelease(t, identity, nil)
			r.write(t, "SHA256SUMS", append([]byte("# changed\n"), mustRead(t, r.dir, "SHA256SUMS")...))
			return r
		}, ErrSignature},
		{"wrong identity", func(t *testing.T) *release {
			return newRelease(t, "https://github.com/evil/repo/.github/workflows/x.yml@refs/heads/main", nil)
		}, ErrIdentity},
		{"no inclusion proof", func(t *testing.T) *release {
			r := newRelease(t, identity, nil)
			inner := r.store.sig.parse
			r.store.sig.parse = func(b []byte) (verify.SignedEntity, error) { e, err := inner(b); return noProof{e}, err }
			return r
		}, ErrSignature},
		{"manifest changed after signing", func(t *testing.T) *release {
			r := newRelease(t, identity, nil)
			r.write(t, "naru-node-"+version+".json", []byte(`{"name":"naru-node-v1-2-3","version":"v1.2.3"}`))
			return r
		}, ErrChecksum},
		{"manifest digest differs from SHA256SUMS", func(t *testing.T) *release {
			return newRelease(t, identity, func(r *release) { r.kernel = []byte("other") })
		}, ErrChecksum},
		{"release not in the store", func(t *testing.T) *release {
			r := newRelease(t, identity, nil)
			os.Remove(filepath.Join(r.dir, "SHA256SUMS.sigstore.json"))
			return r
		}, ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.make(t)
			set, err := r.store.Fetch(context.Background(), version)
			if !errors.Is(err, c.want) {
				t.Fatalf("Fetch = %v, want %v", err, c.want)
			}
			if set.Open != nil {
				t.Fatal("a boot set came back with a release that does not verify")
			}
		})
	}
}

func mustRead(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchRefusesFilesThatDiffer(t *testing.T) {
	r := newRelease(t, identity, nil)
	r.write(t, "vmlinuz", []byte("kernel BYTES"))
	set, err := r.store.Fetch(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	k, i, err := set.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	defer i.Close()
	if _, err := io.ReadAll(k); !errors.Is(err, ErrChecksum) {
		t.Fatalf("kernel read = %v, want ErrChecksum", err)
	}
	if _, err := io.ReadAll(i); err != nil {
		t.Fatalf("the intact initramfs read = %v", err)
	}
}

func TestFetchRefusesAManifestForAnotherVersion(t *testing.T) {
	r := newRelease(t, identity, nil)
	if _, err := r.store.Fetch(context.Background(), "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fetch of a version the store lacks = %v, want ErrNotFound", err)
	}
	for _, v := range []string{"", "../x", "a/b", "v1..2"} {
		if _, err := r.store.Fetch(context.Background(), v); !errors.Is(err, ErrManifest) {
			t.Fatalf("Fetch(%q) = %v, want ErrManifest", v, err)
		}
	}
}

func TestIdentityPrefix(t *testing.T) {
	r := newRelease(t, identity, nil)
	for prefix, ok := range map[string]bool{
		"https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/*": true,
		"https://github.com/other/*": false,
	} {
		sig, err := newSignatureWith(r.vs, issuer, prefix, testRequirements...)
		if err != nil {
			t.Fatal(err)
		}
		sig.parse = r.store.sig.parse
		err = sig.check(nil, mustRead(t, r.dir, "SHA256SUMS"))
		if ok != (err == nil) {
			t.Fatalf("prefix %q: check = %v", prefix, err)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]Config{
		"no identity":  {Location: dir, Issuer: issuer},
		"no issuer":    {Location: dir, Identity: identity},
		"plain http":   {Location: "http://example.com/r", Issuer: issuer, Identity: identity},
		"no directory": {Location: filepath.Join(dir, "missing"), Issuer: issuer, Identity: identity},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	if _, err := New(Config{Location: dir, Issuer: issuer, Identity: identity}); err != nil {
		t.Fatalf("the embedded trust root and a directory: %v", err)
	}
}

func TestHTTPSStore(t *testing.T) {
	r := newRelease(t, identity, nil)
	srv := httptest.NewTLSServer(http.FileServer(http.Dir(r.dir)))
	defer srv.Close()
	src, err := newSource(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	r.store.src = src
	if _, err := r.store.Fetch(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Fetch(context.Background(), "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 404 = %v, want ErrNotFound", err)
	}
}
