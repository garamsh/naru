package pki

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// TokensDir is the directory, inside a CA's directory, that holds one bootstrap token per Node, in a
// file named for the Node.
const TokensDir = "tokens"

const (
	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	idLen         = 6
	secretLen     = 16
)

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	tokenPattern = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
)

// ValidToken reports whether s is a bootstrap token as gaon reads one: <id>.<secret>, matching
// [a-z0-9]{6}\.[a-z0-9]{16}.
func ValidToken(s string) bool { return tokenPattern.MatchString(s) }

// NewToken returns a token drawn from crypto/rand.
func NewToken() (string, error) {
	draw := func(n int) (string, error) {
		b := make([]byte, n)
		for i := range b {
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(tokenAlphabet))))
			if err != nil {
				return "", err
			}
			b[i] = tokenAlphabet[k.Int64()]
		}
		return string(b), nil
	}
	id, err := draw(idLen)
	if err != nil {
		return "", err
	}
	secret, err := draw(secretLen)
	if err != nil {
		return "", err
	}
	return id + "." + secret, nil
}

func tokenPath(caDir, name string) string { return filepath.Join(caDir, TokensDir, name) }

// EnsureToken returns the token of the Node of that name in the CA's directory, writing a new one
// (0600) when there is none. The name is a DNS label, so it cannot leave the directory.
func EnsureToken(caDir, name string) (string, error) {
	if !dnsLabel.MatchString(name) {
		return "", fmt.Errorf("the name %q is not a DNS label", name)
	}
	t, err := ReadToken(caDir, name)
	switch {
	case err == nil:
		return t, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if t, err = NewToken(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(caDir, TokensDir), 0o700); err != nil {
		return "", err
	}
	if err := WriteFile(tokenPath(caDir, name), []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// ReadToken reads the token of the Node of that name, and refuses a file that holds anything else.
func ReadToken(caDir, name string) (string, error) {
	b, err := os.ReadFile(tokenPath(caDir, name))
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if !ValidToken(t) {
		return "", fmt.Errorf("%s does not hold a bootstrap token", tokenPath(caDir, name))
	}
	return t, nil
}

// ReadTokens reads the token of every Node the CA's directory has one for, in the order of the names.
func ReadTokens(caDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(caDir, TokensDir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var out []string
	for _, n := range names {
		t, err := ReadToken(caDir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
