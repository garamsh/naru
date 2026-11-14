package inventory

import "context"

// Signer is the signer on orch's own host. It holds every authority key, which orch never reads.
type Signer interface {
	// IssueHost passes an operator-signed admission statement and the host's certificate request
	// and returns the host's certificate, PEM. It returns ErrSignerRefused when the signer
	// refuses the statement.
	IssueHost(ctx context.Context, statement, csr []byte) ([]byte, error)
	// CRL returns the revocation list as the signer signed it.
	CRL(ctx context.Context) ([]byte, error)
}
