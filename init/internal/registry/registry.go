// Package registry writes the files mvmd and the credential helpers read for the registries of a
// Node (docs/architecture/adr/0026-registry-credentials-and-trust-are-configuration-and-an-ecr-credential-is-renewed-through-iam-roles-anywhere.md):
// the credentials file, the CA bundle and the AWS profile.
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

// The files, relative to the Writer's directory. mvmd's configuration names the first two and
// the services' environment the third; docs/architecture/config.md states the paths.
const (
	credentialsFile = "registry/credentials.json"
	caFile          = "registry/ca.pem"
	awsConfigFile   = "aws/config"
)

// Writer writes the files of spec.registries and spec.aws under Dir.
type Writer struct{ Dir string }

// Write makes the files match spec, and writes each whole by a rename, so a reader sees the old
// file or the new one. The credentials file is always written, with no entry when no registry
// has a username and password, because mvmd refuses a configuration that names a file that
// is not there. The CA bundle exists only while some registry has a ca, and the AWS profile only
// while spec.aws is set. It never logs a value.
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
	if a := spec.AWS; a != nil {
		errs = append(errs, w.put(awsConfigFile, []byte(awsProfile(*a)), 0o644))
	} else {
		errs = append(errs, w.remove(awsConfigFile))
	}
	return errors.Join(errs...)
}

// awsProfile is the AWS config file whose default profile gets its credentials from
// aws_signing_helper, which trades the Node's certificate for the role's session.
func awsProfile(a config.AWS) string {
	return fmt.Sprintf("[default]\nregion = %s\ncredential_process = aws_signing_helper credential-process"+
		" --certificate %s --private-key %s --trust-anchor-arn %s --profile-arn %s --role-arn %s --region %s\n",
		a.Region, a.Certificate, a.Key, a.TrustAnchorArn, a.ProfileArn, a.RoleArn, a.Region)
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
