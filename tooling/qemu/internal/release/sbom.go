package release

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SBOMName is the name of the SPDX software bill of materials asset.
func SBOMName(version string) string { return "naru-sbom-" + version + ".spdx.json" }

// GoBinary is a Go binary a release ships, with the modules it was built from.
type GoBinary struct {
	Name, SHA256, GoVersion, MainPath string
	Modules                           []Module
}

// Module is one module a Go binary's build information names.
type Module struct{ Path, Version string }

// ReadGoBinaries returns the Go binaries among the regular files of dirs, the files, and the
// members of the gzip-compressed newc cpio archives, in the order given (a directory's files
// sorted by name; the same name and digest twice is returned once). A file that is not a Go
// executable is skipped: the directories also hold the C programs of the image, which the pins
// already name. Any other failure (a missing, unreadable or truncated file) is returned.
func ReadGoBinaries(dirs, files, archives []string) ([]GoBinary, error) {
	var paths []string
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.Type().IsRegular() {
				paths = append(paths, filepath.Join(d, e.Name()))
			}
		}
	}
	paths = append(paths, files...)
	var out []GoBinary
	seen := map[string]bool{}
	add := func(name, sum string, bi *buildinfo.BuildInfo) {
		if k := name + " " + sum; !seen[k] {
			seen[k] = true
			out = append(out, goBinary(name, sum, bi))
		}
	}
	for _, p := range paths {
		bi, err := buildinfo.ReadFile(p)
		if isNotGo(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		a, err := digest(p)
		if err != nil {
			return nil, err
		}
		add(filepath.Base(p), a.SHA256, bi)
	}
	for _, p := range archives {
		err := walkCpio(p, func(name string, data []byte) error {
			bi, err := buildinfo.Read(bytes.NewReader(data))
			if isNotGo(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %s: %w", p, name, err)
			}
			base := filepath.Base(name)
			if base == "init" {
				// The initramfs installs naru-init as /init; the SBOM names the program.
				base = filepath.Base(bi.Path)
			}
			add(base, fmt.Sprintf("%x", sha256.Sum256(data)), bi)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// isNotGo reports whether err says the file is not a Go executable, which is not a failure.
func isNotGo(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "not a Go executable") || strings.Contains(err.Error(), "unrecognized file format"))
}

func goBinary(name, sum string, bi *buildinfo.BuildInfo) GoBinary {
	b := GoBinary{Name: name, SHA256: sum, GoVersion: bi.GoVersion, MainPath: bi.Main.Path}
	for _, d := range bi.Deps {
		if d.Replace != nil {
			d = d.Replace
		}
		b.Modules = append(b.Modules, Module{Path: d.Path, Version: d.Version})
	}
	sort.Slice(b.Modules, func(i, j int) bool {
		return b.Modules[i].Path+b.Modules[i].Version < b.Modules[j].Path+b.Modules[j].Version
	})
	return b
}

// walkCpio calls fn with the name and content of every regular file of the gzip-compressed newc
// cpio archive at path. The kernel's initramfs format is the only one it reads.
func walkCpio(path string, fn func(name string, data []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	r := bufio.NewReader(zr)
	pad := func(n int64) error {
		_, err := io.CopyN(io.Discard, r, (4-n%4)%4)
		return err
	}
	for {
		var hdr [110]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return fmt.Errorf("%s: cpio header: %w", path, err)
		}
		if string(hdr[:6]) != "070701" {
			return fmt.Errorf("%s: not a newc cpio archive", path)
		}
		field := func(i int) (int64, error) { return strconv.ParseInt(string(hdr[6+8*i:14+8*i]), 16, 64) }
		mode, err1 := field(1)
		size, err2 := field(6)
		nsize, err3 := field(11)
		if err := errors.Join(err1, err2, err3); err != nil {
			return fmt.Errorf("%s: cpio header: %w", path, err)
		}
		nb := make([]byte, nsize)
		if _, err := io.ReadFull(r, nb); err != nil {
			return fmt.Errorf("%s: cpio name: %w", path, err)
		}
		if err := pad(110 + nsize); err != nil {
			return fmt.Errorf("%s: cpio name: %w", path, err)
		}
		name := strings.TrimRight(string(nb), "\x00")
		if name == "TRAILER!!!" {
			return nil
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("%s: cpio member %s: %w", path, name, err)
		}
		if err := pad(size); err != nil {
			return fmt.Errorf("%s: cpio member %s: %w", path, name, err)
		}
		if mode&0o170000 == 0o100000 {
			if err := fn(name, data); err != nil {
				return err
			}
		}
	}
}

type spdxDoc struct {
	SPDXVersion   string         `json:"spdxVersion"`
	DataLicense   string         `json:"dataLicense"`
	SPDXID        string         `json:"SPDXID"`
	Name          string         `json:"name"`
	Namespace     string         `json:"documentNamespace"`
	CreationInfo  spdxCreation   `json:"creationInfo"`
	Packages      []spdxPackage  `json:"packages"`
	Relationships []spdxRelation `json:"relationships"`
}

type spdxCreation struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string       `json:"name"`
	SPDXID           string       `json:"SPDXID"`
	VersionInfo      string       `json:"versionInfo,omitempty"`
	DownloadLocation string       `json:"downloadLocation"`
	FilesAnalyzed    bool         `json:"filesAnalyzed"`
	Checksums        []spdxSum    `json:"checksums,omitempty"`
	LicenseConcluded string       `json:"licenseConcluded"`
	LicenseDeclared  string       `json:"licenseDeclared"`
	CopyrightText    string       `json:"copyrightText"`
	SourceInfo       string       `json:"sourceInfo,omitempty"`
	ExternalRefs     []spdxExtRef `json:"externalRefs,omitempty"`
}

type spdxSum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"checksumValue"`
}

type spdxExtRef struct {
	Category string `json:"referenceCategory"`
	Type     string `json:"referenceType"`
	Locator  string `json:"referenceLocator"`
}

type spdxRelation struct {
	From string `json:"spdxElementId"`
	Type string `json:"relationshipType"`
	To   string `json:"relatedSpdxElement"`
}

var spdxUnsafe = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

func spdxID(kind, name, version string) string {
	return "SPDXRef-" + kind + "-" + spdxUnsafe.ReplaceAllString(name+"-"+version, "-")
}

// SBOM returns the SPDX 2.3 JSON document of a release: one package for each component, one for
// each add-on image (named by its manifest digest), one for each Go binary with a DEPENDS_ON relationship to each module it was built from, and the Go
// standard library of each toolchain version. The only time in it is epoch, so equal inputs give
// equal bytes.
func SBOM(version string, epoch int64, comps []Component, bins []GoBinary, images []AddonImage) ([]byte, error) {
	if epoch <= 0 {
		return nil, fmt.Errorf("the SBOM needs SOURCE_DATE_EPOCH, and has %d", epoch)
	}
	doc := spdxDoc{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT", Name: "naru-" + version,
		Namespace:    "https://github.com/garamsh/naru/releases/download/" + version + "/" + SBOMName(version),
		CreationInfo: spdxCreation{Created: time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z"), Creators: []string{"Tool: naru-release"}},
	}
	add := func(p spdxPackage) {
		doc.Packages = append(doc.Packages, p)
	}
	rel := func(from, typ, to string) {
		doc.Relationships = append(doc.Relationships, spdxRelation{from, typ, to})
	}
	for _, c := range comps {
		id := spdxID("Pkg", c.Name, c.Version)
		p := spdxPackage{
			Name: c.Name, SPDXID: id, VersionInfo: c.Version, LicenseConcluded: "NOASSERTION", LicenseDeclared: c.License,
			CopyrightText: "NOASSERTION", SourceInfo: "pinned in " + c.Pin,
		}
		if c.Repo != "" {
			p.DownloadLocation = "git+" + c.Repo + "@" + c.Commit
		} else {
			p.DownloadLocation = c.URL
			p.Checksums = []spdxSum{{"SHA256", c.SHA256}}
		}
		add(p)
		rel("SPDXRef-DOCUMENT", "DESCRIBES", id)
	}
	for _, im := range images {
		id := spdxID("Addon", im.Name, im.Digest)
		add(spdxPackage{
			Name: "addon-image-" + im.Name, SPDXID: id, DownloadLocation: "NOASSERTION", Checksums: []spdxSum{{"SHA256", strings.TrimPrefix(im.Digest, "sha256:")}},
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
			SourceInfo: "the OCI image of the " + im.Name + " add-on in the initramfs, named by its manifest digest",
		})
		rel("SPDXRef-DOCUMENT", "DESCRIBES", id)
	}
	seen := map[string]bool{}
	ensure := func(p spdxPackage) string {
		if !seen[p.SPDXID] {
			seen[p.SPDXID] = true
			add(p)
		}
		return p.SPDXID
	}
	for i, b := range bins {
		id := fmt.Sprintf("SPDXRef-Bin-%d-%s", i+1, spdxUnsafe.ReplaceAllString(b.Name, "-"))
		add(spdxPackage{
			Name: b.Name, SPDXID: id, DownloadLocation: "NOASSERTION", Checksums: []spdxSum{{"SHA256", b.SHA256}},
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
			SourceInfo: "a Go binary of module " + b.MainPath + ", built with " + b.GoVersion,
		})
		rel("SPDXRef-DOCUMENT", "DESCRIBES", id)
		std := ensure(spdxPackage{
			Name: "go-stdlib", SPDXID: spdxID("Go", "stdlib", b.GoVersion), VersionInfo: b.GoVersion, DownloadLocation: "NOASSERTION",
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "BSD-3-Clause", CopyrightText: "NOASSERTION",
		})
		rel(id, "DEPENDS_ON", std)
		for _, m := range b.Modules {
			mid := ensure(spdxPackage{
				Name: m.Path, SPDXID: spdxID("Go", m.Path, m.Version), VersionInfo: m.Version, DownloadLocation: "NOASSERTION",
				LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
				ExternalRefs: []spdxExtRef{{"PACKAGE-MANAGER", "purl", "pkg:golang/" + m.Path + "@" + m.Version}},
			})
			rel(id, "DEPENDS_ON", mid)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
