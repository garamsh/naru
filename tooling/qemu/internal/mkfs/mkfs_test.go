package mkfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArgv_RunsMke2fsThroughTheShippedLoaderAndLibraries(t *testing.T) {
	got := Argv("/var/lib/mvmd/inputs/bin", []string{"-q", "-F", "-U", "u", "/disk"})

	assert.Equal(t, []string{
		"/var/lib/mvmd/inputs/bin/ld.so", "--library-path", "/var/lib/mvmd/inputs/bin/lib",
		"/var/lib/mvmd/inputs/bin/mke2fs", "-t", "ext4", "-q", "-F", "-U", "u", "/disk",
	}, got)
}

func TestLibraries_ReadsLddOutput(t *testing.T) {
	ldd := "\tlinux-vdso.so.1 (0x00007ffd)\n" +
		"\tlibext2fs.so.2 => /lib/x86_64-linux-gnu/libext2fs.so.2 (0x00007f1)\n" +
		"\tlibc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f2)\n" +
		"\t/lib64/ld-linux-x86-64.so.2 (0x00007f3)\n"

	loader, libs, err := Libraries(ldd)

	require.NoError(t, err)
	assert.Equal(t, "/lib64/ld-linux-x86-64.so.2", loader)
	assert.Equal(t, []string{"/lib/x86_64-linux-gnu/libext2fs.so.2", "/lib/x86_64-linux-gnu/libc.so.6"}, libs)
}

func TestLibraries_FailsOnAStaticExecutable(t *testing.T) {
	_, _, err := Libraries("\tnot a dynamic executable\n")

	assert.Error(t, err)
}
