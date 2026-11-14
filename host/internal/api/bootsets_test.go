package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// bootSetBody is a multipart body of the parts in order: each is a form name and its bytes.
func bootSetBody(t *testing.T, parts ...[2]string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		w, err := mw.CreateFormFile(p[0], p[0])
		require.NoError(t, err)
		_, err = io.WriteString(w, p[1])
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return mw.FormDataContentType(), buf.Bytes()
}

func requestPart(t *testing.T, name, kernel, initramfs string) [2]string {
	t.Helper()
	b, err := json.Marshal(BootSetRequest{Name: name, KernelSHA256: sha256Of([]byte(kernel)), InitramfsSHA256: sha256Of([]byte(initramfs))})
	require.NoError(t, err)
	return [2]string{"boot-set", string(b)}
}

func (e *env) post(t *testing.T, c *http.Client, path, contentType string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", "https://"+e.addr+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// addable is the body of a valid add of the set name, whose files are k and i.
func addable(t *testing.T, name, k, i string) (string, []byte) {
	t.Helper()
	return bootSetBody(t, requestPart(t, name, k, i), [2]string{"kernel", k}, [2]string{"initramfs", i})
}

func TestBootSets_AddStreamsBothFilesToMvmdWithTheDigestsTheRequestNames(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	ct, body := addable(t, "naru-node-v1", "kernel bytes", "initramfs bytes")

	code, b := e.post(t, c, "/v1/boot-sets", ct, body)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, mvmd.BootSet{Name: "naru-node-v1", KernelDigest: sha256Of([]byte("kernel bytes")), InitrdDigest: sha256Of([]byte("initramfs bytes"))}, e.rt.bootSets["naru-node-v1"])
	assert.Equal(t, [2][]byte{[]byte("kernel bytes"), []byte("initramfs bytes")}, e.rt.bootFiles["naru-node-v1"], "the kernel and the initrd, each in its place")

	// The same request again is the same answer.
	code, b = e.post(t, c, "/v1/boot-sets", ct, body)
	assert.Equal(t, 200, code, string(b))
}

func TestBootSets_AFileThatIsNotTheNamedHashIsRefusedAndAddsNothing(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	ct, body := bootSetBody(t, requestPart(t, "naru-node-v1", "kernel bytes", "initramfs bytes"), [2]string{"kernel", "another kernel"}, [2]string{"initramfs", "initramfs bytes"})

	code, b := e.post(t, c, "/v1/boot-sets", ct, body)
	assert.Equal(t, 422, code, string(b))
	assert.Contains(t, string(b), "kernel")
	assert.Empty(t, e.rt.bootSets)
}

func TestBootSets_TheSameNameWithOtherHashesIs409(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	ct, body := addable(t, "naru-node-v1", "k", "i")
	code, b := e.post(t, c, "/v1/boot-sets", ct, body)
	require.Equal(t, 200, code, string(b))

	ct, body = addable(t, "naru-node-v1", "k2", "i")
	code, b = e.post(t, c, "/v1/boot-sets", ct, body)
	assert.Equal(t, 409, code, string(b))
}

func TestBootSets_ABodyOutOfShapeIs400AndAnInvalidRequestIs422AndNoneKeepsASet(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	req := requestPart(t, "naru-node-v1", "k", "i")
	for name, parts := range map[string][][2]string{
		"no initramfs":  {req, {"kernel", "k"}},
		"parts swapped": {req, {"initramfs", "i"}, {"kernel", "k"}},
		"no kernel":     {req},
		"no request":    {{"kernel", "k"}, {"initramfs", "i"}},
		"unknown field": {{"boot-set", `{"name":"x","kernelSha256":"","initramfsSha256":"","more":1}`}, {"kernel", "k"}, {"initramfs", "i"}},
		"not JSON":      {{"boot-set", `nope`}, {"kernel", "k"}, {"initramfs", "i"}},
	} {
		ct, body := bootSetBody(t, parts...)
		code, b := e.post(t, c, "/v1/boot-sets", ct, body)
		assert.Equal(t, 400, code, "%s: %s", name, b)
	}
	assert.Empty(t, e.rt.bootSets)

	code, b := e.post(t, c, "/v1/boot-sets", "application/json", []byte(`{}`))
	assert.Equal(t, 400, code, string(b))

	empty := sha256Of(nil)
	for name, r := range map[string]BootSetRequest{
		"a name with a dot": {Name: "naru-node-v0.1", KernelSHA256: empty, InitramfsSHA256: empty},
		"an uppercase name": {Name: "Naru", KernelSHA256: empty, InitramfsSHA256: empty},
		"a short hash":      {Name: "ok", KernelSHA256: "abc", InitramfsSHA256: empty},
		"a hash in upper":   {Name: "ok", KernelSHA256: empty, InitramfsSHA256: "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"},
	} {
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		ct, body := bootSetBody(t, [2]string{"boot-set", string(raw)}, [2]string{"kernel", ""}, [2]string{"initramfs", ""})
		code, b := e.post(t, c, "/v1/boot-sets", ct, body)
		assert.Equal(t, 422, code, "%s: %s", name, b)
	}
	assert.Empty(t, e.rt.calls, "none of them reached mvmd")
}

func TestBootSets_RemoveIs204AndARepeatIsToo_AndASetAMicroVMNamesIs409(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	ct, body := addable(t, "naru-node-v1", "k", "i")
	code, b := e.post(t, c, "/v1/boot-sets", ct, body)
	require.Equal(t, 200, code, string(b))
	e.rt.machines["web"] = mvmd.Machine{ID: "web", BootSet: "naru-node-v1"}

	code, b = e.do(t, c, "DELETE", "/v1/boot-sets/naru-node-v1", nil)
	assert.Equal(t, 409, code, string(b))
	assert.Contains(t, e.rt.bootSets, "naru-node-v1")

	delete(e.rt.machines, "web")
	for range 2 {
		code, b = e.do(t, c, "DELETE", "/v1/boot-sets/naru-node-v1", nil)
		assert.Equal(t, 204, code, string(b))
	}
	assert.Empty(t, e.rt.bootSets)

	code, _ = e.do(t, c, "DELETE", "/v1/boot-sets/Bad.Name", nil)
	assert.Equal(t, 422, code)
}

func TestBootSets_AReaderMayNeitherAddNorRemove(t *testing.T) {
	e := start(t)
	reader := e.as(t, ReaderOrganization)
	ct, body := addable(t, "naru-node-v1", "k", "i")

	code, _ := e.post(t, reader, "/v1/boot-sets", ct, body)
	assert.Equal(t, 403, code)
	code, _ = e.do(t, reader, "DELETE", "/v1/boot-sets/naru-node-v1", nil)
	assert.Equal(t, 403, code)
	assert.Empty(t, e.rt.calls, "a refused request reached mvmd")

	// The control: a provisioner's add is accepted.
	code, b := e.post(t, e.as(t, ProvisionerOrganization), "/v1/boot-sets", ct, body)
	assert.Equal(t, 200, code, string(b))
}

func TestBootSets_MvmdNotAnsweringIs503(t *testing.T) {
	e := start(t)
	e.rt.failAt["addBootSet naru-node-v1"] = mvmd.ErrUnavailable
	ct, body := addable(t, "naru-node-v1", "k", "i")
	code, b := e.post(t, e.as(t, ProvisionerOrganization), "/v1/boot-sets", ct, body)
	assert.Equal(t, 503, code, string(b))
}
