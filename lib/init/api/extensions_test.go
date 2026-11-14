package api_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/api"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/init/service"
)

const orchDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeExtensions is the Node's extension store: it keeps what it was given and answers with err.
type fakeExtensions struct {
	err    error
	list   []extension.Entry
	body   string
	digest string
	calls  int
}

func (f *fakeExtensions) Put(body io.Reader, digest string) (extension.Entry, error) {
	f.calls++
	b, _ := io.ReadAll(body)
	f.body, f.digest = string(b), digest
	if f.err != nil {
		return extension.Entry{}, f.err
	}
	return extension.Entry{Image: extension.Image{SHA256: digest, Size: int64(len(b)), Manifest: extension.Manifest{Name: "naru-orch", Version: "v1.2.3", Services: []string{"naru-orchd"}}}}, nil
}

func (f *fakeExtensions) Delete(digest string) error {
	f.calls++
	f.digest = digest
	return f.err
}

func (f *fakeExtensions) List() ([]extension.Entry, error) { return f.list, f.err }

func (r *rig) withExtensions(t *testing.T, e api.Extensions) {
	t.Helper()
	r.server = api.New(api.Deps{
		Services: &r.services, Logs: r.con, Version: "v1.2.3",
		BootID:     func() (string, error) { return "boot-1", nil },
		Uptime:     func() time.Duration { return 90 * time.Second },
		Reboot:     func() { r.rebooted <- struct{}{} },
		Extensions: e,
		Log:        slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	r.start(t)
}

func TestServer_PutExtensionPassesTheBodyAndTheDigestInThePathOn(t *testing.T) {
	r := newRig(t)
	e := &fakeExtensions{}
	r.withExtensions(t, e)

	status, body := r.do(t, r.admin(t), http.MethodPut, "/v1/extensions/"+orchDigest, "the image")

	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, "the image", e.body)
	assert.Equal(t, orchDigest, e.digest)
	got := decode(t, body)
	assert.Equal(t, orchDigest, got["sha256"])
	assert.Equal(t, "naru-orch", got["name"])
	assert.Equal(t, false, got["mounted"])
	r.noReboot(t)
}

func TestServer_ExtensionThatIsRefusedAnswersTheStatusOfTheReason(t *testing.T) {
	tests := map[string]struct {
		method string
		err    error
		status int
	}{
		"a store that accepts the put":    {http.MethodPut, nil, http.StatusOK},
		"a store that accepts the delete": {http.MethodDelete, nil, http.StatusNoContent},
		"a digest that is wrong":          {http.MethodPut, fmt.Errorf("%w: the body is x", extension.ErrDigest), http.StatusBadRequest},
		"bytes that are not an image":     {http.MethodPut, fmt.Errorf("%w: wrong fs type", extension.ErrImage), http.StatusBadRequest},
		"an image that is too large":      {http.MethodPut, extension.ErrTooLarge, http.StatusRequestEntityTooLarge},
		"a failure of the Node":           {http.MethodPut, errors.New("write /var/lib/naru/extensions: secret detail"), http.StatusInternalServerError},
		"an image that is not stored":     {http.MethodDelete, extension.ErrNotFound, http.StatusNotFound},
		"an image that is mounted":        {http.MethodDelete, extension.ErrMounted, http.StatusConflict},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.withExtensions(t, &fakeExtensions{err: tt.err})

			status, body := r.do(t, r.admin(t), tt.method, "/v1/extensions/"+orchDigest, "x")

			assert.Equal(t, tt.status, status)
			if tt.status >= http.StatusBadRequest {
				assert.NotEmpty(t, decode(t, body)["error"])
			}
			assert.NotContains(t, string(body), "secret detail", "a failure of the Node is left to its log")
		})
	}
}

func TestServer_DeleteExtensionAnswers204(t *testing.T) {
	r := newRig(t)
	e := &fakeExtensions{}
	r.withExtensions(t, e)

	status, _ := r.do(t, r.admin(t), http.MethodDelete, "/v1/extensions/"+orchDigest, "")

	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, orchDigest, e.digest)
}

func TestServer_GetExtensionsListsWhatIsStoredAndMounted(t *testing.T) {
	r := newRig(t)
	r.withExtensions(t, &fakeExtensions{list: []extension.Entry{
		{Image: extension.Image{SHA256: orchDigest, Size: 7, Manifest: extension.Manifest{Name: "naru-orch", Version: "v1.2.3", Services: []string{"naru-orchd", "naru-signer"}}}, Mounted: true, MountPoint: "/usr/lib/naru/extensions/naru-orch"},
		{Image: extension.Image{SHA256: "ff", Size: 9, Manifest: extension.Manifest{Name: "naru-orch", Version: "v1.3.0", Services: []string{"naru-orchd"}}}},
	}})

	status, body := r.do(t, r.client(t, r.clientCA, "naru:reader"), http.MethodGet, "/v1/extensions", "")

	require.Equal(t, http.StatusOK, status, string(body))
	list := decode(t, body)["extensions"].([]any)
	require.Len(t, list, 2)
	first := list[0].(map[string]any)
	assert.Equal(t, orchDigest, first["sha256"])
	assert.Equal(t, "v1.2.3", first["version"])
	assert.Equal(t, true, first["mounted"])
	assert.Equal(t, "/usr/lib/naru/extensions/naru-orch", first["mountPoint"])
	assert.Equal(t, []any{"naru-orchd", "naru-signer"}, first["services"])
	second := list[1].(map[string]any)
	assert.Equal(t, false, second["mounted"])
	assert.NotContains(t, second, "mountPoint")
}

func TestServer_GetExtensionsOfAnEmptyStoreIsAnEmptyList(t *testing.T) {
	r := newRig(t)
	r.withExtensions(t, &fakeExtensions{})

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/extensions", "")

	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, map[string]any{"extensions": []any{}}, decode(t, body))
}

func TestServer_ChangingExtensionsIsForAnAdmin(t *testing.T) {
	tests := map[string]struct {
		organizations []string
		status        int
	}{
		"admin":   {[]string{"naru:admin"}, http.StatusOK},
		"reader":  {[]string{"naru:reader"}, http.StatusForbidden},
		"no role": {nil, http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			e := &fakeExtensions{}
			r.withExtensions(t, e)

			status, _ := r.do(t, r.client(t, r.clientCA, tt.organizations...), http.MethodPut, "/v1/extensions/"+orchDigest, "x")

			assert.Equal(t, tt.status, status)
			if tt.status == http.StatusOK {
				assert.Equal(t, 1, e.calls)
			} else {
				assert.Zero(t, e.calls, "a refused client's body is not read")
			}
		})
	}
}

func TestServer_ExtensionsAre501OnANodeWithoutAStore(t *testing.T) {
	r := newRig(t)
	r.start(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		path := "/v1/extensions"
		if method != http.MethodGet {
			path += "/" + orchDigest
		}
		status, _ := r.do(t, r.admin(t), method, path, "x")

		assert.Equal(t, http.StatusNotImplemented, status, method)
	}
}

func TestServer_ServicesNameTheExtensionTheirBinaryComesFrom(t *testing.T) {
	r := newRig(t)
	r.services.status = append(r.services.status, service.Status{Name: "naru-orchd", State: service.StateRunning, PID: 7})
	r.chosen.Config.Spec.Services = append(r.chosen.Config.Spec.Services,
		config.Service{Name: "naru-orchd", Binary: "/usr/lib/naru/extensions/naru-orch/bin/naru-orchd"})
	r.engine.cfg = r.chosen.Config
	r.start(t)

	status, body := r.do(t, r.client(t, r.clientCA, "naru:reader"), http.MethodGet, "/v1/services", "")

	require.Equal(t, http.StatusOK, status, string(body))
	byName := map[string]map[string]any{}
	for _, s := range decode(t, body)["services"].([]any) {
		m := s.(map[string]any)
		byName[m["name"].(string)] = m
	}
	assert.Equal(t, "naru-orch", byName["naru-orchd"]["extension"])
	assert.NotContains(t, byName["mvmd"], "extension", "a service of the image names none")
}
