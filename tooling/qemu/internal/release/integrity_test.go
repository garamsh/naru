package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

const epoch = 1794355200

func sumOf(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// fakeComponents returns three components, one GPL, one LGPL and one MIT, and a downloads directory
// that holds the GPL one's tarball.
func fakeComponents(t *testing.T) ([]release.Component, string) {
	t.Helper()
	dl := t.TempDir()
	gpl, lgpl := []byte("gpl tarball"), []byte("lgpl tarball")
	require.NoError(t, os.WriteFile(filepath.Join(dl, "gplpkg-1.tar.xz"), gpl, 0o600))
	return []release.Component{
		{Name: "gplpkg", Version: "1", License: "GPL-2.0-only WITH Linux-syscall-note", URL: "https://example.test/gplpkg-1.tar.xz", SHA256: sumOf(gpl), Pin: "image/a/pin.env"},
		{Name: "lgplpkg", Version: "2", License: "MIT AND LGPL-2.1-or-later", URL: "https://example.test/lgplpkg-2.tar.gz", SHA256: sumOf(lgpl), Pin: "image/b/pin.env"},
		{Name: "mitpkg", Version: "3", License: "MIT", URL: "https://example.test/mitpkg-3.tar.gz", SHA256: sumOf([]byte("mit tarball")), Pin: "image/c/pin.env"},
	}, dl
}

func fetcher(calls *[]string, files map[string][]byte) release.Fetch {
	return func(url, dest string) error {
		*calls = append(*calls, url)
		b, ok := files[url]
		if !ok {
			return fmt.Errorf("404 %s", url)
		}
		return os.WriteFile(dest, b, 0o600)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(c), 0o600))
	}
	return root
}

func TestReadComponents_RefusesAPinWithoutALicenseAndNamesIt(t *testing.T) {
	root := writeTree(t, map[string]string{"image/pkgs/foo/pin.env": "FOO_VERSION=1\nFOO_URL=https://example.test/foo-1.tar.xz\nFOO_SHA256=" + sumOf(nil) + "\n"})

	_, err := release.ReadComponents(root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FOO_LICENSE")
	assert.Contains(t, err.Error(), "image/pkgs/foo/pin.env")
}

func TestReadComponents_ReadsTarballsGitPinsAndQuotedLicences(t *testing.T) {
	root := writeTree(t, map[string]string{
		"image/pkgs/foo/pin.env":   "# c\nFOO_VERSION=1\nFOO_URL=https://example.test/foo-1.tar.xz\nFOO_SHA256=ab\nFOO_LICENSE='GPL-2.0-only AND MIT'\n",
		"image/host/bar/pin.env":   "BAR_REPO=https://example.test/bar\nBAR_COMMIT=cafe\nBAR_LICENSE=NOASSERTION\nBAR_NOTHING=x\n",
		"image/host/guest/pin.env": "GUEST_INITRD_SHA256=ff\n",
	})

	got, err := release.ReadComponents(root)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "bar", got[0].Name)
	assert.Equal(t, "cafe", got[0].Version, "a commit pin without a version states its commit")
	assert.Equal(t, "foo", got[1].Name)
	assert.Equal(t, "GPL-2.0-only AND MIT", got[1].License)
	assert.True(t, got[1].Copyleft())
	assert.False(t, got[0].Copyleft())
}

func TestReadComponents_EveryPinOfTheRepositoryStatesALicense(t *testing.T) {
	got, err := release.ReadComponents(repoRoot(t))

	require.NoError(t, err)
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
		assert.NotEmpty(t, c.License, c.Name)
	}
	for _, want := range []string{"kernel", "util-linux", "e2fsprogs", "gaon", "mvmd", "containerd", "cacert", "musl"} {
		assert.True(t, names[want], "%s is a component", want)
	}
}

func TestSBOM_IsSPDXWithEveryComponentAndTheModulesOfEveryGoBinaryAndNoTimeButTheEpoch(t *testing.T) {
	comps, _ := fakeComponents(t)
	bins, err := release.ReadGoBinaries(nil, []string{tool(t).Bin}, nil)
	require.NoError(t, err)
	require.Len(t, bins, 1, "naructl is a Go binary")

	a, err := release.SBOM("v9", epoch, comps, bins, nil)
	require.NoError(t, err)
	b, err := release.SBOM("v9", epoch, comps, bins, nil)
	require.NoError(t, err)

	assert.Equal(t, a, b, "equal inputs, equal bytes")
	var doc struct {
		SPDXVersion  string
		CreationInfo struct{ Created string }
		Packages     []struct {
			Name, VersionInfo, LicenseDeclared, DownloadLocation string
			Checksums                                            []struct{ ChecksumValue string }
		}
		Relationships []struct{ RelationshipType string }
	}
	require.NoError(t, json.Unmarshal(a, &doc))
	assert.Equal(t, "SPDX-2.3", doc.SPDXVersion)
	assert.Equal(t, "2026-11-11T00:00:00Z", doc.CreationInfo.Created, "SOURCE_DATE_EPOCH")
	var gpl, naructlPkg bool
	modules := 0
	for _, p := range doc.Packages {
		switch {
		case p.Name == "gplpkg":
			gpl = true
			assert.Equal(t, "GPL-2.0-only WITH Linux-syscall-note", p.LicenseDeclared)
			assert.Equal(t, "https://example.test/gplpkg-1.tar.xz", p.DownloadLocation)
			require.Len(t, p.Checksums, 1)
			assert.Equal(t, comps[0].SHA256, p.Checksums[0].ChecksumValue)
		case p.Name == filepath.Base(tool(t).Bin):
			naructlPkg = true
		case strings.Contains(p.Name, "."):
			modules++
		}
	}
	assert.True(t, gpl, "a component is a package")
	assert.True(t, naructlPkg, "a Go binary is a package")
	assert.Positive(t, modules, "its modules are packages")
	assert.Contains(t, string(a), "DEPENDS_ON")
}

func TestSBOM_RefusesToRunWithoutAnEpoch(t *testing.T) {
	_, err := release.SBOM("v9", 0, nil, nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "SOURCE_DATE_EPOCH")
}

func readTar(t *testing.T, p string) (names []string, content map[string]string, hdrs map[string]*tar.Header) {
	t.Helper()
	f, err := os.Open(p)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	content, hdrs = map[string]string{}, map[string]*tar.Header{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, content, hdrs
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		names = append(names, h.Name)
		content[h.Name], hdrs[h.Name] = string(b), h
	}
}

func TestWriteSources_HoldsTheGPLFamilyTarballsAndTheSourceArchiveAndNothingElseSortedAndOwnedByNoOne(t *testing.T) {
	comps, dl := fakeComponents(t)
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("git archive"), 0o600))
	var calls []string
	out := filepath.Join(t.TempDir(), "sources.tar")

	err := release.WriteSources(out, epoch, comps, []string{dl}, archive, fetcher(&calls, map[string][]byte{"https://example.test/lgplpkg-2.tar.gz": []byte("lgpl tarball")}))

	require.NoError(t, err)
	names, content, hdrs := readTar(t, out)
	assert.Equal(t, []string{"naru-source.tar", "sources/gplpkg-1.tar.xz", "sources/lgplpkg-2.tar.gz"}, names, "sorted, and the MIT package is not in it")
	assert.Equal(t, "gpl tarball", content["sources/gplpkg-1.tar.xz"])
	assert.Equal(t, "git archive", content["naru-source.tar"])
	assert.Equal(t, []string{"https://example.test/lgplpkg-2.tar.gz"}, calls, "a tarball in a downloads directory is not fetched")
	for n, h := range hdrs {
		assert.Zero(t, h.Uid+h.Gid, n)
		assert.Empty(t, h.Uname+h.Gname, n)
		assert.Equal(t, int64(epoch), h.ModTime.Unix(), n)
	}
}

func TestWriteSources_TwoRunsWriteTheSameBytes(t *testing.T) {
	comps, dl := fakeComponents(t)
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("git archive"), 0o600))
	var calls []string
	f := fetcher(&calls, map[string][]byte{"https://example.test/lgplpkg-2.tar.gz": []byte("lgpl tarball")})
	a, b := filepath.Join(t.TempDir(), "a.tar"), filepath.Join(t.TempDir(), "b.tar")

	require.NoError(t, release.WriteSources(a, epoch, comps, []string{dl}, archive, f))
	require.NoError(t, release.WriteSources(b, epoch, comps, []string{dl}, archive, f))

	x, err := os.ReadFile(a)
	require.NoError(t, err)
	y, err := os.ReadFile(b)
	require.NoError(t, err)
	assert.Equal(t, x, y)
}

func TestWriteSources_FailsOnATarballItCannotGetAndOnOneThatIsNotThePinnedBytes(t *testing.T) {
	comps, dl := fakeComponents(t)
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("git archive"), 0o600))
	var calls []string

	missing := release.WriteSources(filepath.Join(t.TempDir(), "s.tar"), epoch, comps, []string{dl}, archive, fetcher(&calls, nil))
	wrong := release.WriteSources(filepath.Join(t.TempDir(), "s.tar"), epoch, comps, []string{dl}, archive, fetcher(&calls, map[string][]byte{"https://example.test/lgplpkg-2.tar.gz": []byte("another tarball")}))
	require.NoError(t, os.WriteFile(filepath.Join(dl, "gplpkg-1.tar.xz"), []byte("changed in the downloads directory"), 0o600))
	tampered := release.WriteSources(filepath.Join(t.TempDir(), "s.tar"), epoch, comps, []string{dl}, archive, fetcher(&calls, nil))

	require.Error(t, missing)
	assert.Contains(t, missing.Error(), "lgplpkg")
	require.Error(t, wrong)
	assert.Contains(t, wrong.Error(), "lgplpkg")
	assert.Contains(t, wrong.Error(), "SHA-256")
	require.Error(t, tampered)
	assert.Contains(t, tampered.Error(), "gplpkg")
}

func TestWriteSources_RefusesACopyleftComponentThatIsNotATarball(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("x"), 0o600))
	comps := []release.Component{{Name: "gitgpl", License: "GPL-3.0-or-later", Repo: "https://example.test/r", Commit: "c", Pin: "image/x/pin.env"}}

	err := release.WriteSources(filepath.Join(t.TempDir(), "s.tar"), epoch, comps, nil, archive, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitgpl is GPL-3.0-or-later and has no source tarball")
}

func TestCompare_AcceptsEqualSumsAndToleratesTheAssetsThatDifferByDesign(t *testing.T) {
	a := "aa  naru-host-v9.qcow2\n11  naru-data-v9.qcow2\n21  naru-node-1.test.yaml\n"
	b := "aa  naru-host-v9.qcow2\n22  naru-data-v9.qcow2\n21  naru-node-1.test.yaml\n"
	c := "aa  naru-host-v9.qcow2\n22  naru-data-v9.qcow2\n23  naru-node-1.test.yaml\n"

	assert.NoError(t, release.Compare([]byte(a), []byte(b)))
	err := release.Compare([]byte(a), []byte(c))
	require.Error(t, err, "the test user-data is reproducible, so a difference in it is one")
	assert.Contains(t, err.Error(), "naru-node-1.test.yaml")
}

func TestCompare_NamesTheAssetThatDiffersAndTheOnesOnlyOneSideHas(t *testing.T) {
	a := "aa  same\nbb  changed\ncc  only-published\n"
	b := "aa  same\nb2  changed\ndd  only-new\n"

	err := release.Compare([]byte(a), []byte(b))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed: published bb, new build b2")
	assert.Contains(t, err.Error(), "only-published: only the published release has it")
	assert.Contains(t, err.Error(), "only-new: only the new build has it")
	assert.NotContains(t, err.Error(), "same")
}

func TestCompare_RefusesAFileThatListsNothing(t *testing.T) {
	assert.Error(t, release.Compare(nil, []byte("aa  x\n")))
	assert.Error(t, release.Compare([]byte("not a sums line\n"), []byte("aa  x\n")))
}

func TestByDesignPattern_IsTheSkipPatternOfImageRepro(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "image", "repro.sh"))
	require.NoError(t, err)

	assert.Contains(t, string(b), "skip='"+release.ByDesignPattern+"'", "image/repro.sh and internal/release name the same assets")
}

func TestBuild_ListsTheSBOMAndTheSourcesInSHA256SUMSAndTheSBOMStatesTheComponents(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	_, err := release.Build(o)

	require.NoError(t, err)
	sums, err := os.ReadFile(filepath.Join(o.Out, "SHA256SUMS"))
	require.NoError(t, err)
	for _, n := range []string{release.SBOMName("v9"), release.SourcesName("v9")} {
		assert.Contains(t, string(sums), "  "+n+"\n", "SHA256SUMS lists it")
		_, err := os.Stat(filepath.Join(o.Out, n))
		assert.NoError(t, err)
	}
	assert.Equal(t, "naru-sbom-v9.spdx.json", release.SBOMName("v9"))
	assert.Equal(t, "naru-sources-v9.tar", release.SourcesName("v9"))
}

func TestBuild_RefusesAReleaseWhosePinHasNoLicense(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))
	o.Components = nil
	o.Root = writeTree(t, map[string]string{
		"image/pkgs/foo/pin.env":              "FOO_URL=https://example.test/foo.tar.xz\nFOO_SHA256=ab\n",
		"image/host/initramfs/initramfs.list": "file /bin/mvmd x\nfile /bin/etcd x\nfile /bin/gaon-apiserver x\nfile /bin/gaon-scheduler x\nfile /bin/gaon-controllers x\nfile /bin/gaon-node-agent x\n",
	})

	_, err := release.Build(o)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FOO_LICENSE")
}

func TestWriteSources_FetchesEveryTarballWhenADownloadsDirectoryDoesNotExist(t *testing.T) {
	comps, _ := fakeComponents(t)
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("git archive"), 0o600))
	var calls []string
	out := filepath.Join(t.TempDir(), "sources.tar")
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	err := release.WriteSources(out, epoch, comps, []string{missing}, archive, fetcher(&calls, map[string][]byte{
		"https://example.test/gplpkg-1.tar.xz":  []byte("gpl tarball"),
		"https://example.test/lgplpkg-2.tar.gz": []byte("lgpl tarball"),
	}))

	require.NoError(t, err)
	assert.Equal(t, []string{"https://example.test/gplpkg-1.tar.xz", "https://example.test/lgplpkg-2.tar.gz"}, calls)
}

// newc writes a gzip-compressed newc cpio archive of the given regular files, in the order given.
func newc(t *testing.T, members [][2]string) string {
	t.Helper()
	var b bytes.Buffer
	entry := func(name string, mode int, data []byte) {
		fmt.Fprintf(&b, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x", 1, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		b.WriteString(name + "\x00")
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
		b.Write(data)
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	for _, m := range members {
		data, err := os.ReadFile(m[1])
		require.NoError(t, err)
		entry(m[0], 0o100755, data)
	}
	entry("TRAILER!!!", 0, nil)
	p := filepath.Join(t.TempDir(), "initramfs.cpio.gz")
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, err := zw.Write(b.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(p, z.Bytes(), 0o600))
	return p
}

func TestReadGoBinaries_ReadsTheGoBinariesOfAnInitramfsAndNamesInitByItsProgram(t *testing.T) {
	text := filepath.Join(t.TempDir(), "notgo")
	require.NoError(t, os.WriteFile(text, []byte("#!/bin/sh\n"), 0o600))
	archive := newc(t, [][2]string{{"init", tool(t).Bin}, {"bin/script", text}, {"bin/naructl2", tool(t).Bin}})

	bins, err := release.ReadGoBinaries(nil, nil, []string{archive})

	require.NoError(t, err)
	require.Len(t, bins, 2, "the script is not a Go binary")
	assert.Equal(t, "naructl", bins[0].Name, "/init is named by the program it was built from")
	assert.Equal(t, "naructl2", bins[1].Name)
	assert.NotEmpty(t, bins[0].Modules)
}

func TestReadGoBinaries_FailsOnAFileItCannotReadInsteadOfSkippingIt(t *testing.T) {
	_, err := release.ReadGoBinaries(nil, []string{"/nonexistent-naructl"}, nil)
	require.Error(t, err, "a missing file")
	_, err = release.ReadGoBinaries([]string{"/nonexistent-dir"}, nil, nil)
	require.Error(t, err, "a missing directory")
	_, err = release.ReadGoBinaries(nil, nil, []string{"/nonexistent.cpio.gz"})
	require.Error(t, err, "a missing archive")
}

func TestBuild_TheSBOMNamesTheGoBinariesOfTheInitramfsArchives(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))
	o.Archives = []string{newc(t, [][2]string{{"bin/helper", tool(t).Bin}})}

	_, err := release.Build(o)

	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(o.Out, release.SBOMName(o.Version)))
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(b), `"SPDXRef-Bin-2-helper"`), "naructl as an asset is the first binary, the one in the archive the second")
}

func TestWriteSources_RefusesTwoPinsWhoseTarballsHaveTheSameName(t *testing.T) {
	comps, dl := fakeComponents(t)
	comps[1].URL = "https://other.test/gplpkg-1.tar.xz"
	comps[1].SHA256 = sumOf([]byte("gpl tarball"))
	archive := filepath.Join(t.TempDir(), "source.tar")
	require.NoError(t, os.WriteFile(archive, []byte("git archive"), 0o600))

	err := release.WriteSources(filepath.Join(t.TempDir(), "s.tar"), epoch, comps, []string{dl}, archive, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "gplpkg and lgplpkg both have the source tarball gplpkg-1.tar.xz")
}

func TestNewHTTPFetch_RetriesAfterAFailureAndGivesUpOnAStalledServer(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("tarball"))
	}))
	dest := filepath.Join(t.TempDir(), "t")

	require.NoError(t, release.NewHTTPFetch(time.Second, 3, time.Millisecond)(srv.URL, dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "tarball", string(got))
	assert.Equal(t, 2, hits)
	srv.Close()

	stall := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stall }))
	defer slow.Close()
	defer close(stall)
	start := time.Now()
	err = release.NewHTTPFetch(50*time.Millisecond, 2, time.Millisecond)(slow.URL, filepath.Join(t.TempDir(), "x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "after 2 tries")
	assert.Less(t, time.Since(start), 5*time.Second)
}
