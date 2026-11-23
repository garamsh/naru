package config

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func guestOf(initrd string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return json.Marshal(map[string]any{
			"kernels":        map[string]any{"naru": map[string]string{"path": "/k", "sha256": "kk"}},
			"default_kernel": "naru",
			"guest_initrd":   map[string]string{"path": "/g", "sha256": initrd},
		})
	}
}

const mvmdLike = `{"initrd_sha256": {{json .guest_initrd.sha256}}, "kernels": {{json .kernels}}, "kernel": {{json (index .kernels .default_kernel).path}}}`

func TestRender_TakesTheDigestsFromTheGuestManifestOfTheBootedImage(t *testing.T) {
	f := File{Path: "/run/x/c.json", Content: mvmdLike, Template: TemplateGuest}
	old, err := f.Render(guestOf("old"))
	require.NoError(t, err)
	now, err := f.Render(guestOf("new"))
	require.NoError(t, err)
	require.JSONEq(t, `{"initrd_sha256":"old","kernels":{"naru":{"path":"/k","sha256":"kk"}},"kernel":"/k"}`, string(old))
	require.JSONEq(t, `{"initrd_sha256":"new","kernels":{"naru":{"path":"/k","sha256":"kk"}},"kernel":"/k"}`, string(now))
}

func TestRender_ControlAFileWithNoTemplateIsWrittenAsItStandsAndNeverReadsTheManifest(t *testing.T) {
	f := File{Path: "/run/x/c.json", Content: mvmdLike}
	got, err := f.Render(func() ([]byte, error) { return nil, errors.New("read") })
	require.NoError(t, err)
	require.Equal(t, mvmdLike, string(got))
}

func TestRender_RefusesWhatItCannotRenderAndWritesNothing(t *testing.T) {
	missing := File{Path: "/p", Content: `{{json .nope}}`, Template: TemplateGuest}
	_, err := missing.Render(guestOf("a"))
	require.ErrorContains(t, err, "nope")
	_, err = File{Path: "/p", Content: "x", Template: TemplateGuest}.Render(func() ([]byte, error) { return nil, errors.New("no file") })
	require.ErrorContains(t, err, "no file")
	_, err = File{Path: "/p", Content: "x", Template: TemplateGuest}.Render(func() ([]byte, error) { return []byte("{"), nil })
	require.ErrorContains(t, err, "not JSON")
}

func TestCheckTemplate_RefusesAnotherNameAndContentThatDoesNotParse(t *testing.T) {
	require.NoError(t, File{Content: "{{", Template: ""}.checkTemplate())
	require.NoError(t, File{Content: mvmdLike, Template: TemplateGuest}.checkTemplate())
	require.ErrorContains(t, File{Content: "x", Template: "host"}.checkTemplate(), `must be "guest"`)
	require.ErrorContains(t, File{Content: "{{", Template: TemplateGuest}.checkTemplate(), "not a template")
}
