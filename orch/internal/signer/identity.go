package signer

import (
	"context"
	"fmt"
	"os"
)

// Files are where orch keeps its own identity: its key, its certificate with the authority's after
// it, and the authority.
type Files struct{ Cert, Key, CA string }

// EnsureIdentity checks that orch's identity is at f. The signer certifies an orch only for a
// request root signs, which orch cannot make, so orch never asks for its own certificate: root
// has the signer issue it (POST /v1/orch) and places it, with its key and the authority, at f.
// name and hosts are what the certificate was issued for; they are not used to ask.
func (c *Client) EnsureIdentity(_ context.Context, f Files, name string, hosts []string) error {
	for _, path := range []string{f.Cert, f.Key, f.CA} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("orch's identity is missing (%s): root has the signer issue one for %q and %v, and places it there: %w", path, name, hosts, err)
		}
	}
	return nil
}
