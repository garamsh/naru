package release

import (
	"archive/tar"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SourcesName is the name of the corresponding-source asset.
func SourcesName(version string) string { return "naru-sources-" + version + ".tar" }

// SourceArchiveMember is the member of the sources tar that holds the git archive of the commit the
// release was built from.
const SourceArchiveMember = "naru-source.tar"

// Fetch downloads url into dest. The default is an HTTP GET; a test replaces it.
type Fetch func(url, dest string) error

func httpFetch(url, dest string) error {
	r, err := http.Get(url) //nolint:gosec // the URL is a pin of this repository
	if err != nil {
		return err
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, r.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sourceTarball returns the path of the verified tarball of c: the file of that name in one of
// downloads (what make image already fetched and checked), or else a fetch of the pin's URL into
// work. Either way its SHA-256 must be the pin's; the returned path is only read.
func sourceTarball(c Component, downloads []string, work string, fetch Fetch) (string, error) {
	var p string
	for _, d := range downloads {
		if _, err := os.Stat(filepath.Join(d, c.TarballName())); err == nil {
			p = filepath.Join(d, c.TarballName())
			break
		}
	}
	if p == "" {
		p = filepath.Join(work, c.Name+"-"+c.TarballName())
		if err := fetch(c.URL, p); err != nil {
			return "", fmt.Errorf("%s: fetch %s: %w", c.Name, c.URL, err)
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != c.SHA256 {
		return "", fmt.Errorf("%s: %s has SHA-256 %s, and the pin in %s states %s", c.Name, p, got, c.Pin, c.SHA256)
	}
	return p, nil
}

// WriteSources writes the corresponding source of the release as a tar: the exact upstream
// tarball of every component whose licence is GPL, LGPL or AGPL (taken from downloads or fetched,
// and checked against the pin), and the git archive of the built commit in sourceArchive. Its
// members are sorted, owned by no one and dated epoch, so equal inputs give equal bytes. A
// copyleft component that is not a tarball is an error: there is no exact source to carry.
func WriteSources(out string, epoch int64, comps []Component, downloads []string, sourceArchive string, fetch Fetch) error {
	if fetch == nil {
		fetch = httpFetch
	}
	work, err := os.MkdirTemp("", "naru-sources-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	members := map[string]string{SourceArchiveMember: sourceArchive}
	for _, c := range comps {
		if !c.Copyleft() {
			continue
		}
		if c.URL == "" {
			return fmt.Errorf("%s is %s and has no source tarball in %s", c.Name, c.License, c.Pin)
		}
		p, err := sourceTarball(c, downloads, work, fetch)
		if err != nil {
			return err
		}
		members["sources/"+c.TarballName()] = p
	}
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	for _, n := range names {
		if err := addMember(tw, n, members[n], time.Unix(epoch, 0)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func addMember(tw *tar.Writer, name, from string, mtime time.Time) error {
	in, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("the source %s: %w", name, err)
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: st.Size(), ModTime: mtime, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	_, err = io.Copy(tw, in)
	return err
}
