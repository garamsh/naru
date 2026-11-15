package console_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/naructl/internal/console"
)

const (
	seedOrchURL = "https://orch.example:8443"
	seedCA      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// seedCSRF reads the token of the install-seed form.
func (e env) seedCSRF(t *testing.T) string {
	t.Helper()
	status, body := e.get(t, "/install-seed")
	require.Equal(t, http.StatusOK, status)
	m := csrfField.FindStringSubmatch(body)
	require.NotNil(t, m, "the install-seed form carries a CSRF token: %s", body)
	return m[1]
}

// postSeed posts the install-seed form and returns the response with its body read.
func (e env) postSeed(t *testing.T, form url.Values) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/install-seed", form)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

func TestInstallSeed_TheFormShowsNoKeyAndMakesNone(t *testing.T) {
	orch := &fakeOrch{}
	e := start(t, orch, nil)
	status, body := e.get(t, "/install-seed")
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `action="/install-seed"`)
	assert.Contains(t, body, seedOrchURL)
	assert.NotContains(t, body, seedSecret)
	assert.Empty(t, orch.keys, "looking at the form makes no key")
}

func TestInstallSeed_DownloadsAnISOThatCarriesTheKeyAndOnlyThere(t *testing.T) {
	orch := &fakeOrch{}
	e := start(t, orch, nil)
	resp, body := e.postSeed(t, url.Values{"csrf": {e.seedCSRF(t)}, "ttl": {"6h"}, "name": {"rack-3"}})

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "naru-install-seed.iso")
	assert.Equal(t, []time.Time{now.Add(6 * time.Hour)}, orch.keys, "one key, valid for the lifetime asked")
	// The volume is a cidata drive whose user-data is the join form with the key in it.
	assert.Contains(t, string(body), "cidata")
	assert.Contains(t, string(body), "enrollKey: "+seedSecret)
	assert.Contains(t, string(body), "url: "+seedOrchURL)
	assert.Contains(t, string(body), "caSHA256: "+seedCA)
	assert.Contains(t, string(body), "name: rack-3")
	assert.NotContains(t, string(body), "network-config", "no network configuration: the installer asks DHCP")
	// No page afterwards shows the key.
	_, after := e.get(t, "/install-seed")
	assert.NotContains(t, after, seedSecret)
	_, events := e.get(t, "/events")
	assert.NotContains(t, events, seedSecret)
}

func TestInstallSeed_RefusesWhatItCouldNotUseBeforeMakingAKey(t *testing.T) {
	for name, tc := range map[string]struct {
		form   func(csrf string) url.Values
		status int
	}{
		"a wrong CSRF token": {func(string) url.Values { return url.Values{"csrf": {"nope"}, "ttl": {"1h"}} }, http.StatusForbidden},
		"no lifetime":        {func(c string) url.Values { return url.Values{"csrf": {c}} }, http.StatusUnprocessableEntity},
		"a negative one":     {func(c string) url.Values { return url.Values{"csrf": {c}, "ttl": {"-1h"}} }, http.StatusUnprocessableEntity},
		"a bad name":         {func(c string) url.Values { return url.Values{"csrf": {c}, "ttl": {"1h"}, "name": {"Rack_3"}} }, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			orch := &fakeOrch{}
			e := start(t, orch, nil)
			resp, body := e.postSeed(t, tc.form(e.seedCSRF(t)))
			assert.Equal(t, tc.status, resp.StatusCode)
			assert.NotContains(t, resp.Header.Get("Content-Disposition"), "attachment")
			assert.NotContains(t, string(body), seedSecret)
			assert.Empty(t, orch.keys, "a refused form costs no key")
		})
	}
}

func TestInstallSeed_AnswersWithoutAnISOWhenOrchRefusesTheKey(t *testing.T) {
	orch := &fakeOrch{keyErr: &orchapi.Error{Status: http.StatusForbidden, Message: "not allowed"}}
	e := start(t, orch, nil)
	resp, body := e.postSeed(t, url.Values{"csrf": {e.seedCSRF(t)}, "ttl": {"1h"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.NotContains(t, resp.Header.Get("Content-Disposition"), "attachment")
	assert.Contains(t, string(body), "not allowed")

	orch.keyErr = errors.New("connection refused")
	resp, _ = e.postSeed(t, url.Values{"csrf": {e.seedCSRF(t)}, "ttl": {"1h"}})
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestInstallSeed_SaysSoWhenTheConsoleDoesNotKnowOrch(t *testing.T) {
	s, err := console.New(console.Config{
		Orch:         &fakeOrch{},
		Sign:         func(identity.Admission) ([]byte, error) { return nil, nil },
		AdmissionTTL: time.Hour,
		AllowedHosts: []string{"example.com"},
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/install-seed", nil))
	assert.Contains(t, rec.Body.String(), "cannot make a seed")
}

func TestPending_ShowsTheHardwareBesideThePairingCode(t *testing.T) {
	key := hostKeyDER(t)
	orch := &fakeOrch{pending: []orchapi.Pending{
		{HostID: "h-aaaa", PublicKey: key, FirstSeen: now, Hardware: &orchapi.Hardware{
			CPUs: 8, MemoryBytes: 16 << 30,
			Disks: []orchapi.Disk{{Name: "sda", Size: 500 << 30, Model: "SSD", Partitions: 2}, {Name: "sdb", Size: 100 << 30, Model: "USB"}},
			Links: []orchapi.Link{{Name: "eth0", MAC: "52:54:00:12:34:56", Carrier: true}},
		}},
		{HostID: "h-bbbb", PublicKey: key, FirstSeen: now},
	}}
	e := start(t, orch, nil)
	_, body := e.get(t, "/pending")
	assert.Contains(t, body, identity.PairingCode(key))
	assert.Contains(t, body, "8 CPU, 16384 MiB memory")
	assert.Contains(t, body, "sda 500 GiB SSD (2 partition(s))")
	assert.Contains(t, body, "sdb 100 GiB USB (0 partition(s))")
	assert.Contains(t, body, "eth0 52:54:00:12:34:56 (carrier)")
	assert.Equal(t, 1, strings.Count(body, "Reported hardware"), "a host that reported none shows none")
}
