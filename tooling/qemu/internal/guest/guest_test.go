package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var d = strings.Repeat("a", 64)

func valid() string {
	return `{"kernels":{"naru":{"path":"/usr/lib/naru/guest/kernels/naru/vmlinux","sha256":"` + d + `"}},
"default_kernel":"naru",
"guest_initrd":{"path":"/usr/lib/naru/guest/guest-initrd.cpio","sha256":"` + d + `"},
"converter_initrd":{"path":"/usr/lib/naru/guest/converter-initrd.cpio","sha256":"` + d + `"}}`
}

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.json")
	require.NoError(t, os.WriteFile(p, []byte(s), 0o644))
	return p
}

func TestRead_ReturnsTheDefaultKernelAndBothInitrds(t *testing.T) {
	m, err := Read(write(t, valid()))

	require.NoError(t, err)
	assert.Equal(t, "/usr/lib/naru/guest/kernels/naru/vmlinux", m.Default().Path)
	assert.Equal(t, d, m.Default().SHA256)
	assert.Equal(t, "/usr/lib/naru/guest/guest-initrd.cpio", m.GuestInitrd.Path)
	assert.Equal(t, "/usr/lib/naru/guest/converter-initrd.cpio", m.ConverterInitrd.Path)
}

func TestRead_RejectsWhatMvmdsKernelsMapWouldNot(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"a name with an upper-case letter": {`"naru":{`, `"Naru":{`, "is not 1 to 63 characters"},
		"a name of 64 characters":          {`"naru":{`, `"` + strings.Repeat("a", 64) + `":{`, "is not 1 to 63 characters"},
		"a default that is not declared":   {`"default_kernel":"naru"`, `"default_kernel":"other"`, "is not one of the declared kernels"},
		"a short digest":                   {`"sha256":"` + d + `"}},`, `"sha256":"abc"}},`, "is not 64 lowercase hex digits"},
		"a relative path":                  {`"/usr/lib/naru/guest/guest-initrd.cpio"`, `"guest-initrd.cpio"`, "is not absolute"},
		"a field it does not know":         {`"default_kernel"`, `"extra":1,"default_kernel"`, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(valid(), tc.from, tc.to, 1)
			require.NotEqual(t, valid(), s)

			_, err := Read(write(t, s))

			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRead_RejectsAManifestWithoutKernels(t *testing.T) {
	_, err := Read(write(t, `{"kernels":{},"default_kernel":"naru"}`))

	assert.ErrorContains(t, err, "no kernel is declared")
}
