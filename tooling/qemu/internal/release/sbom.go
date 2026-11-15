package release

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// ReadGoBinaries returns the Go binaries among the regular files of dirs and of files, in the order
// given (a directory's files sorted by name). A file that is not a Go binary is skipped: the
// initramfs directories also hold the C programs of the image, which the pins already name.
func ReadGoBinaries(dirs, files []string) ([]GoBinary, error) {
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
	for _, p := range paths {
		bi, err := buildinfo.ReadFile(p)
		if err != nil {
			continue
		}
		a, err := digest(p)
		if err != nil {
			return nil, err
		}
		b := GoBinary{Name: filepath.Base(p), SHA256: a.SHA256, GoVersion: bi.GoVersion, MainPath: bi.Main.Path}
		for _, d := range bi.Deps {
			if d.Replace != nil {
				d = d.Replace
			}
			b.Modules = append(b.Modules, Module{Path: d.Path, Version: d.Version})
		}
		sort.Slice(b.Modules, func(i, j int) bool {
			return b.Modules[i].Path+b.Modules[i].Version < b.Modules[j].Path+b.Modules[j].Version
		})
		out = append(out, b)
	}
	return out, nil
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
// each Go binary with a DEPENDS_ON relationship to each module it was built from, and the Go
// standard library of each toolchain version. The only time in it is epoch, so equal inputs give
// equal bytes.
func SBOM(version string, epoch int64, comps []Component, bins []GoBinary) ([]byte, error) {
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
