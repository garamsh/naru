package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	m              manifest
	// manifestPad and sumsPad are bytes appended to the manifest and to SHA256SUMS before signing.
	manifestPad, sumsPad int
	// signer and issuer sign SHA256SUMS; configured is the identity the store is set to.
	signer, issuer, configured string
	store                      *Store
}

// newRelease writes a release of version into a directory, SHA256SUMS signed by the signer. edit
// changes the release before it is written and signed.
func newRelease(t *testing.T, edit func(r *release)) *release {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	r := &release{dir: t.TempDir(), vs: vs, kernel: []byte("kernel bytes"), initrd: []byte("initramfs bytes"),
		signer: identity, issuer: issuer, configured: identity}
	r.m = manifest{Name: "naru-node-v1-2-3", Version: version,
		Kernel:    file{File: "vmlinuz", SHA256: hexSum(r.kernel), Bytes: int64(len(r.kernel))},
		Initramfs: file{File: "initramfs.img", SHA256: hexSum(r.initrd), Bytes: int64(len(r.initrd))}}
	if edit != nil {
		edit(r)
	}
	m, _ := json.Marshal(r.m)
	m = append(m, bytes.Repeat([]byte(" "), r.manifestPad)...)
	r.write(t, "vmlinuz", r.kernel)
	r.write(t, "initramfs.img", r.initrd)
	r.write(t, "naru-node-"+version+".json", m)
	sums := []byte(hexSum(m) + "  naru-node-" + version + ".json\n" +
		hexSum(r.kernel) + "  vmlinuz\n" + hexSum(r.initrd) + "  initramfs.img\n")
	sums = append(sums, bytes.Repeat([]byte("\n"), r.sumsPad)...)
	r.write(t, "SHA256SUMS", sums)
	r.write(t, "SHA256SUMS.sigstore.json", []byte("bundle"))
	e, err := vs.Sign(r.signer, r.issuer, sums)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := newSignatureWith(vs, issuer, r.configured, testRequirements...)
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
	r := newRelease(t, nil)
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

// A refusal is asserted beside the same release unchanged but for the one thing under test, which
// is accepted: a refusal that holds for any other reason fails the control.
func TestFetchRefusesARelease(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(r *release)
		tamper func(t *testing.T, r *release)
		want   error
	}{
		{name: "the bundle signs other bytes", want: ErrSignature, tamper: func(t *testing.T, r *release) {
			r.write(t, "SHA256SUMS", append([]byte("# changed\n"), mustRead(t, r.dir, "SHA256SUMS")...))
		}},
		{name: "another signer", want: ErrIdentity, edit: func(r *release) {
			r.signer = "https://github.com/evil/repo/.github/workflows/x.yml@refs/heads/main"
		}},
		{name: "another issuer", want: ErrIdentity, edit: func(r *release) { r.issuer = "https://accounts.google.com" }},
		{name: "an exact identity's near miss", want: ErrIdentity, edit: func(r *release) { r.signer = identity + "0" }},
		{name: "no inclusion proof", want: ErrSignature, tamper: func(t *testing.T, r *release) {
			inner := r.store.sig.parse
			r.store.sig.parse = func(b []byte) (verify.SignedEntity, error) { e, err := inner(b); return noProof{e}, err }
		}},
		{name: "manifest changed after signing", want: ErrChecksum, tamper: func(t *testing.T, r *release) {
			r.write(t, "naru-node-"+version+".json", []byte(`{"name":"naru-node-v1-2-3","version":"v1.2.3"}`))
		}},
		{name: "manifest digest differs from SHA256SUMS", want: ErrChecksum, edit: func(r *release) {
			r.m.Kernel.SHA256 = hexSum([]byte("other"))
		}},
		{name: "manifest names another boot set", want: ErrManifest, edit: func(r *release) { r.m.Name = "naru-node-v9-9-9" }},
		{name: "manifest names another version", want: ErrManifest, edit: func(r *release) { r.m.Version = "v9.9.9" }},
		{name: "manifest file with a directory", want: ErrManifest, edit: func(r *release) { r.m.Kernel.File = "sub/vmlinuz" }},
		{name: "manifest file out of the directory", want: ErrManifest, edit: func(r *release) { r.m.Initramfs.File = "../initramfs.img" }},
		{name: "manifest larger than its limit", want: ErrManifest, edit: func(r *release) { r.manifestPad = maxMani }},
		{name: "SHA256SUMS larger than its limit", want: ErrManifest, edit: func(r *release) { r.sumsPad = maxSmall }},
		{name: "release not in the store", want: ErrNotFound, tamper: func(t *testing.T, r *release) {
			os.Remove(filepath.Join(r.dir, "SHA256SUMS.sigstore.json"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			control := newRelease(t, nil)
			if _, err := control.store.Fetch(context.Background(), version); err != nil {
				t.Fatalf("control: the unchanged release = %v", err)
			}
			r := newRelease(t, c.edit)
			if c.tamper != nil {
				c.tamper(t, r)
			}
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

// A file whose length is not the manifest's ends its read in an error, whether it is shorter or
// longer, beside the file of the length the manifest gives.
func TestFetchRefusesAFileOfAnotherLength(t *testing.T) {
	for name, length := range map[string]func(n int) int64{
		"longer than the manifest says":  func(n int) int64 { return int64(n - 1) },
		"shorter than the manifest says": func(n int) int64 { return int64(n + 1) },
		"as the manifest says":           func(n int) int64 { return int64(n) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, func(r *release) { r.m.Kernel.Bytes = length(len(r.kernel)) })
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
			_, err = io.ReadAll(k)
			if (name == "as the manifest says") != (err == nil) || (err != nil && !errors.Is(err, ErrChecksum)) {
				t.Fatalf("kernel read = %v", err)
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
	r := newRelease(t, nil)
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

func TestFetchRefusesAVersionThatNamesNoRelease(t *testing.T) {
	r := newRelease(t, nil)
	if _, err := r.store.Fetch(context.Background(), "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fetch of a version the store lacks = %v, want ErrNotFound", err)
	}
	for _, v := range []string{"", "../x", "a/b", "v1..2", "naru-node-v1-2-3"} {
		if _, err := r.store.Fetch(context.Background(), v); !errors.Is(err, ErrManifest) {
			t.Fatalf("Fetch(%q) = %v, want ErrManifest", v, err)
		}
	}
}

// signedBy is a signature check over r's SHA256SUMS signed by signer, for identity.
func signedBy(t *testing.T, r *release, signer, configured string) error {
	t.Helper()
	sums := mustRead(t, r.dir, "SHA256SUMS")
	e, err := r.vs.Sign(signer, issuer, sums)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := newSignatureWith(r.vs, issuer, configured, testRequirements...)
	if err != nil {
		t.Fatal(err)
	}
	sig.parse = func([]byte) (verify.SignedEntity, error) { return withProof{e}, nil }
	return sig.check(nil, sums, version)
}

func TestIdentityPrefixNamesOneRepository(t *testing.T) {
	r := newRelease(t, nil)
	const repo = "https://github.com/garamsh/naru/"
	workflow := repo + ".github/workflows/release.yml@refs/tags/v1.2.3"
	if err := signedBy(t, r, workflow, repo+"*"); err != nil {
		t.Fatalf("control: the repository's own workflow under its prefix = %v", err)
	}
	for _, other := range []string{
		"https://github.com/garamsh/naru-evil/.github/workflows/release.yml@refs/tags/v1.2.3",
		"https://github.com/garamsh/narux/.github/workflows/release.yml@refs/tags/v1.2.3",
		"https://github.com/other/naru/.github/workflows/release.yml@refs/tags/v1.2.3",
	} {
		if err := signedBy(t, r, other, repo+"*"); !errors.Is(err, ErrIdentity) {
			t.Errorf("%s under the prefix = %v, want ErrIdentity", other, err)
		}
	}
	for _, prefix := range []string{
		"*", "https://*", "https://github.com/*", "https://github.com/garamsh/*", "https://github.com/garamsh/naru*",
		"https://github.com/garamsh//*", "http://github.com/garamsh/naru/*", "https://github.com/garamsh/na*ru/*",
	} {
		if _, err := newSignatureWith(r.vs, issuer, prefix, testRequirements...); err == nil {
			t.Errorf("prefix %q was accepted", prefix)
		}
	}
	if _, err := newSignatureWith(r.vs, issuer, repo+".github/*", testRequirements...); err != nil {
		t.Errorf("control: a prefix inside the repository = %v", err)
	}
}

func TestDefaultIdentityIsTheReleaseWorkflowAtTheVersionsTag(t *testing.T) {
	tag := func(v string) string {
		return "https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/" + v
	}
	r := newRelease(t, nil)
	if err := signedBy(t, r, tag(version), DefaultIdentity); err != nil {
		t.Fatalf("control: the workflow at the version's tag = %v", err)
	}
	for _, signer := range []string{
		tag("v1.2.4"),
		"https://github.com/garamsh/naru/.github/workflows/other.yml@refs/tags/" + version,
		"https://github.com/garamsh/naru/.github/workflows/release.yml@refs/heads/main",
	} {
		if err := signedBy(t, r, signer, DefaultIdentity); !errors.Is(err, ErrIdentity) {
			t.Errorf("signer %s = %v, want ErrIdentity", signer, err)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]Config{
		"an empty identity":          {Location: dir, Issuer: issuer},
		"no issuer":                  {Location: dir, Identity: identity},
		"plain http":                 {Location: "http://example.com/r", Issuer: issuer, Identity: identity},
		"no directory":               {Location: filepath.Join(dir, "missing"), Issuer: issuer, Identity: identity},
		"a prefix over every repo":   {Location: dir, Issuer: issuer, Identity: "https://github.com/*"},
		"a prefix without its slash": {Location: dir, Issuer: issuer, Identity: "https://github.com/garamsh/naru*"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	for _, identity := range []string{identity, DefaultIdentity, "https://github.com/garamsh/naru/*"} {
		if _, err := New(Config{Location: dir, Issuer: issuer, Identity: identity}); err != nil {
			t.Fatalf("control, the embedded trust root and a directory with identity %q: %v", identity, err)
		}
	}
}
