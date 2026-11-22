package hostclient_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

func bootSet(kernel, initramfs io.Reader) groups.BootSet {
	return groups.BootSet{
		Name: "naru-node-v1", KernelSHA256: "k", InitramfsSHA256: "i",
		Open: func(context.Context) (io.ReadCloser, io.ReadCloser, error) {
			return io.NopCloser(kernel), io.NopCloser(initramfs), nil
		},
	}
}

func TestAddBootSetStreamsTheNameAndBothFilesToTheHost(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h1", Address: h.addr()}})

	require.NoError(t, c.AddBootSet(t.Context(), "h1", bootSet(strings.NewReader("KERNEL"), strings.NewReader("INITRAMFS"))))

	body := string(h.body("POST /v1/boot-sets"))
	for _, want := range []string{`"name":"naru-node-v1"`, `"kernelSha256":"k"`, `"initramfsSha256":"i"`, "KERNEL", "INITRAMFS"} {
		require.Contains(t, body, want)
	}
	require.Less(t, strings.Index(body, "KERNEL"), strings.Index(body, "INITRAMFS"), "the kernel goes first")
}

func TestAddBootSetEndsWhenAFileFailsItsCheck(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h1", Address: h.addr()}})
	bad := errors.New("checksum check failed")

	err := c.AddBootSet(t.Context(), "h1", bootSet(io.MultiReader(strings.NewReader("KER"), errReader{bad}), strings.NewReader("INITRAMFS")))

	require.ErrorIs(t, err, bad)
	require.NotContains(t, string(h.body("POST /v1/boot-sets")), "INITRAMFS", "the initramfs is not sent after the kernel failed")
}

func TestAddBootSetRefusesAHostTheListDidNotRead(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h1", Address: h.addr()}})
	require.Error(t, c.AddBootSet(t.Context(), "elsewhere", bootSet(strings.NewReader("K"), strings.NewReader("I"))))
	require.False(t, h.saw("POST /v1/boot-sets"), "no request should reach the host")
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

func TestAddBootSetClosesTheFileItWasGivenWhenTheOtherIsMissing(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h1", Address: h.addr()}})
	kernel := &closeCounter{Reader: strings.NewReader("K")}
	set := groups.BootSet{Name: "naru-node-v1", Open: func(context.Context) (io.ReadCloser, io.ReadCloser, error) {
		return kernel, nil, nil
	}}

	require.Error(t, c.AddBootSet(t.Context(), "h1", set), "a set that opens one file is refused, not a nil dereference")
	require.Equal(t, 1, kernel.closed)

	both := groups.BootSet{Name: "naru-node-v1", KernelSHA256: "k", InitramfsSHA256: "i", Open: func(context.Context) (io.ReadCloser, io.ReadCloser, error) {
		return kernel, io.NopCloser(strings.NewReader("I")), nil
	}}
	kernel.closed = 0
	require.NoError(t, c.AddBootSet(t.Context(), "h1", both))
	require.Equal(t, 1, kernel.closed, "control: the files of a whole set are closed once")
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
