package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// template is the checked-in release notes template, the one the workflow renders.
const template = "../../../../docs/operate/release-notes.md.tmpl"

const sums = `1111  naru-data-v0.1.0-dev.13.qcow2
2222  naru-host-v0.1.0-dev.13.qcow2
3333  naru-sbom-v0.1.0-dev.13.spdx.json
`

func renderNotes(t *testing.T, sumsText string, extra ...string) (string, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "SHA256SUMS")
	require.NoError(t, os.WriteFile(f, []byte(sumsText), 0o644))
	args := append([]string{"-template", template, "-sums", f, "-tag", "v0.1.0-dev.13", "-commit", "abc123",
		"-esp-bytes", "536870912", "-upgrade-kernel-bytes", "211239936"}, extra...)
	var out bytes.Buffer
	err := notes(args, &out)
	return out.String(), err
}

func TestNotes_StatesTheBuildsFiguresAndTheAssetsThatDifferByDesign(t *testing.T) {
	got, err := renderNotes(t, sums)
	require.NoError(t, err)
	assert.Contains(t, got, "naru v0.1.0-dev.13, built from commit abc123")
	assert.Contains(t, got, "ESP is 536870912 bytes")
	assert.Contains(t, got, "is 211239936 bytes")
	assert.Contains(t, got, "`SHA256SUMS` of the other 3,")
	assert.Contains(t, got, "except those of the by-design assets (`naru-data-v0.1.0-dev.13.qcow2`)")
	assert.Contains(t, got, "- `naru-host-v0.1.0-dev.13.qcow2` `2222`\n")
	assert.NotContains(t, got, "three that differ", "the stale count of by-design assets is gone")
	assert.NotContains(t, got, "{{")
	assert.NotContains(t, got, "must be recreated")
}

func TestNotes_RecreationParagraphOnlyWhenAsked(t *testing.T) {
	with, err := renderNotes(t, sums, "-requires-recreation")
	require.NoError(t, err)
	assert.Contains(t, with, "must be recreated")
	assert.Contains(t, with, "Recreate the Node from `naru-host-v0.1.0-dev.13.qcow2`")
	without, err := renderNotes(t, sums)
	require.NoError(t, err)
	assert.NotContains(t, without, "must be recreated")
}

func TestNotes_RefusesSumsWithNoAssetThatDiffersByDesign(t *testing.T) {
	_, err := renderNotes(t, "2222  naru-host-v0.1.0-dev.13.qcow2\n")
	assert.ErrorContains(t, err, "no asset matching")
}

func TestNotes_RefusesAMissingFigure(t *testing.T) {
	var out bytes.Buffer
	err := notes([]string{"-template", template, "-sums", "x", "-tag", "v1.0.0", "-commit", "c"}, &out)
	assert.ErrorContains(t, err, "are required")
}

func TestPrevious_PrintsThePredecessorOrNothing(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, previous([]string{"v0.1.0-dev.10"}, strings.NewReader("v0.1.0\nv0.1.0-dev.9\nv0.1.0-dev.10\n\n"), &out))
	assert.Equal(t, "v0.1.0-dev.9\n", out.String())
	out.Reset()
	require.NoError(t, previous([]string{"v0.1.0-dev.9"}, strings.NewReader("v0.1.0-dev.9\n"), &out))
	assert.Empty(t, out.String())
	assert.Error(t, previous(nil, strings.NewReader(""), &out))
}
