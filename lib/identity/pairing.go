package identity

import "crypto/sha256"

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PairingCode is what a person compares to be sure two ends hold the same
// key: the first 40 bits of the SHA-256 of the public key's DER, in Crockford
// base32, formatted XXXX-XXXX.
func PairingCode(pubDER []byte) string {
	sum := sha256.Sum256(pubDER)
	var n uint64
	for _, b := range sum[:5] {
		n = n<<8 | uint64(b)
	}
	out := make([]byte, 0, 9)
	for i := 7; i >= 0; i-- {
		out = append(out, crockford[(n>>(5*uint(i)))&31])
		if i == 4 {
			out = append(out, '-')
		}
	}
	return string(out)
}
