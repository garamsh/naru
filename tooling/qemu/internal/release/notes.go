package release

import (
	"fmt"
	"strings"
	"text/template"
)

// Notes is what the release notes template (docs/operate/release-notes.md.tmpl) may name.
type Notes struct {
	Tag, Commit string
	// ESPBytes and UpgradeKernelBytes are the figures make release wrote beside the assets.
	ESPBytes, UpgradeKernelBytes int64
	// Recreate adds the paragraph that tells Nodes of an earlier image to be recreated.
	Recreate bool
	// ListedCount is the number of assets SHA256SUMS lists.
	ListedCount int
	// ByDesign names the listed assets that match ByDesignPattern, each in code quotes.
	ByDesign string
	// CACertDate is the date of the root bundle the image carries (CACERT_DATE of image/pkgs/cacert/pin.env).
	CACertDate string
	// Sums is every line of SHA256SUMS, in file order.
	Sums []Sum
}

// Sum is one line of SHA256SUMS.
type Sum struct{ Name, Digest string }

// ReadSums fills ListedCount, ByDesign and Sums from the SHA256SUMS of the build. It fails when the
// file lists no asset that differs by design, since the notes would then name none.
func (n *Notes) ReadSums(sums []byte) error {
	m, err := parseSums("SHA256SUMS", sums)
	if err != nil {
		return err
	}
	var by []string
	n.Sums = nil
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		digest, name, _ := strings.Cut(line, "  ")
		if line == "" {
			continue
		}
		n.Sums = append(n.Sums, Sum{Name: name, Digest: digest})
		if byDesign.MatchString(name) {
			by = append(by, "`"+name+"`")
		}
	}
	if len(by) == 0 {
		return fmt.Errorf("SHA256SUMS lists no asset matching %s, which the notes name as differing by design", ByDesignPattern)
	}
	n.ListedCount, n.ByDesign = len(m), strings.Join(by, ", ")
	return nil
}

// ReadCACertDate fills CACertDate from the text of image/pkgs/cacert/pin.env, read by parsePin like
// every other pin: the date is the Version of the cacert component. It fails when the pin has no
// cacert component or no date, since the notes would then state no date for the bundle.
func (n *Notes) ReadCACertDate(pin string) error {
	cs, err := parsePin("image/pkgs/cacert/pin.env", pin)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Name == "cacert" && c.Version != "" {
			n.CACertDate = c.Version
			return nil
		}
	}
	return fmt.Errorf("the cacert pin has no CACERT_DATE, which the notes state")
}

// RenderNotes executes the template text with n. A name the template uses that Notes lacks is an error.
func RenderNotes(text string, n Notes) ([]byte, error) {
	t, err := template.New("release notes").Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := t.Execute(&b, n); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
