// Package consolepem reads the PEM blocks a service printed on a Node's serial console: the signer
// logs the first root certificate and the revocation public key there, which is where the operator
// takes them from (docs/architecture/signer.md). Each console line may begin with a log prefix, which
// the reader drops.
package consolepem

import (
	"encoding/pem"
	"strings"
)

// Extract returns the PEM blocks of the type typ ("CERTIFICATE", "PUBLIC KEY") in console, in order,
// each as PEM text. A block cut off before its END line is not returned.
func Extract(console, typ string) []string {
	begin, end := "-----BEGIN "+typ+"-----", "-----END "+typ+"-----"
	var out []string
	var cur []string
	in := false
	for _, line := range strings.Split(console, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.Contains(line, begin):
			in, cur = true, []string{begin}
			if rest := strings.TrimSpace(line[strings.Index(line, begin)+len(begin):]); rest != "" && !strings.Contains(rest, end) {
				cur = append(cur, rest)
			}
		case in && strings.Contains(line, end):
			cur = append(cur, end)
			text := strings.Join(cur, "\n") + "\n"
			if b, _ := pem.Decode([]byte(text)); b != nil {
				out = append(out, text)
			}
			in = false
		case in:
			cur = append(cur, lastField(line))
		}
	}
	return out
}

// lastField is the base64 of a block's line after any prefix a logger put before it: base64 has no
// spaces, so it is the last field.
func lastField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}
