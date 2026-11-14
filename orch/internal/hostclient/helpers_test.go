package hostclient_test

import (
	"crypto/tls"
	"time"
)

// insecure trusts the httptest server's self-signed certificate; the test is of the requests.
func insecure() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } //nolint:gosec

func timeZero() time.Time { return time.Time{} }
