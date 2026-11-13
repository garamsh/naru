// Package registry writes the files mvmd and the credential helpers read for the registries of a
// Node (docs/architecture/adr/0026-a-node-takes-registry-credentials-and-trust-as-configuration-an-ecr-credential-comes-from-iam-roles-anywhere-and-an-agent-pair-without-the-required-organization-is-set-aside.md):
// the credentials file, the CA bundle and the home directory of mvmd and the credential helpers
// it runs.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/init/internal/config"
)

// Dir is the directory the files are written under, init's own.
const Dir = "/run/naru"

// The files and the directory, relative to the Writer's directory. mvmd's configuration names the
// first two and its environment's HOME the third; docs/architecture/config.md states the paths.
const (
	credentialsFile = "registry/credentials.json"
	caFile          = "registry/ca.pem"
	homeDir         = "registry/home"
)

// Writer writes the files of spec.registries under Dir.
type Writer struct{ Dir string }

// Write makes the files match spec, and writes each whole by a rename, so a reader sees the old
// file or the new one. The credentials file is always written, with no entry when no registry
// has a username and password, because mvmd refuses a configuration that names a file that
// is not there. The CA bundle exists only while some registry has a ca. The home directory is
// always there, with mode 0700, because a credential helper writes its log under it. It never
// logs a value.
func (w Writer) Write(spec config.Spec) error {
	creds := map[string]map[string]string{}
	var bundle strings.Builder
	for _, r := range spec.Registries {
		if c := r.Credential; c != nil && c.Helper == "" {
			creds[r.Host] = map[string]string{"username": c.Username, "password": c.Password}
		}
		if r.CA != "" {
			bundle.WriteString(strings.TrimRight(r.CA, "\n"))
			bundle.WriteString("\n")
		}
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, w.put(credentialsFile, data, 0o600))
	if bundle.Len() > 0 {
		errs = append(errs, w.put(caFile, []byte(bundle.String()), 0o644))
	} else {
		errs = append(errs, w.remove(caFile))
	}
	errs = append(errs, w.home())
	return errors.Join(errs...)
}

// home makes the home directory with mode 0700, whatever mode an earlier boot left it with.
func (w Writer) home() error {
	path := filepath.Join(w.Dir, homeDir)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", path, err)
	}
	return nil
}

func (w Writer) put(rel string, data []byte, mode os.FileMode) error {
	path := filepath.Join(w.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (w Writer) remove(rel string) error {
	if err := os.Remove(filepath.Join(w.Dir, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
