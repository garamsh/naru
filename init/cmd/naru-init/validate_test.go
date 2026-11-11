package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

const validYAML = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: mvmd
      binary: /usr/bin/mvmd
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/mvmd
`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "naru.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidate_AFileInitAcceptsExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer

	code := validate(write(t, validYAML), &out, &errOut)

	assert.Equal(t, 0, code, errOut.String())
	assert.Contains(t, out.String(), "valid; node=node-1 services=1")
}

func TestValidate_AFileInitRefusesExitsNonZeroAndSaysWhy(t *testing.T) {
	var out, errOut bytes.Buffer

	code := validate(write(t, "kind: Wrong\n"), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "invalid configuration")
}

func TestValidate_AMissingFileExitsNonZero(t *testing.T) {
	var out, errOut bytes.Buffer

	assert.Equal(t, 1, validate(filepath.Join(t.TempDir(), "none.yaml"), &out, &errOut))
}
