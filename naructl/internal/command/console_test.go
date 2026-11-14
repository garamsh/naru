package command_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/naructl/internal/command"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestConsole_RefusesBeforeServing(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	wrong := filepath.Join(t.TempDir(), "wrong")
	require.NoError(t, os.WriteFile(wrong, []byte("battery staple"), 0o600))
	full := op.args(f, "console")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no flags", []string{"console"}, "--orch-url is required"},
		{"no identity", []string{"console", "--orch-url", f.srv.URL, "--orch-ca", op.orchAuth}, "--identity is required"},
		{"wrong passphrase", []string{"console", "--orch-url", f.srv.URL, "--orch-ca", op.orchAuth, "--identity", op.person, "--passphrase-file", wrong}, "passphrase"},
		{"missing passphrase file", []string{"console", "--orch-url", f.srv.URL, "--orch-ca", op.orchAuth, "--identity", op.person, "--passphrase-file", wrong + ".none"}, "passphrase file"},
		{"remote address", append(append([]string{}, full...), "--listen", "0.0.0.0:0"), "loopback"},
		{"no remote flag", append(append([]string{}, full...), "--allow-remote"), "allow-remote"},
		{"zero ttl", append(append([]string{}, full...), "--admission-ttl", "0s"), "positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := command.Run(tc.args, &out, &errOut)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, out.String(), "console on", "nothing was served")
		})
	}
}

// TestConsole_AdmitsWithTheOperatorsIdentityAndAKeyThatStaysHere runs the command against an orch that
// demands the operator's certificate and checks what it is sent.
func TestConsole_AdmitsWithTheOperatorsIdentityAndAKeyThatStaysHere(t *testing.T) {
	hostDER := hostKey(t)
	var mu sync.Mutex
	var requests []orchapi.AdmitRequest
	orch := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "alice" {
			http.Error(w, "operator certificate required", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/pending":
			_ = json.NewEncoder(w).Encode([]orchapi.Pending{pendingHost("host-new", hostDER)})
		case "/v1/admit":
			var req orchapi.AdmitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			requests = append(requests, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(orchapi.Host{ID: req.HostID})
		default:
			http.NotFound(w, r)
		}
	}))
	orch.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	orch.StartTLS()
	t.Cleanup(orch.Close)
	op := newOperator(t, &fakeOrch{srv: orch}, "operator")

	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- command.Run(append(op.args(&fakeOrch{srv: orch}, "console"), "--listen", "127.0.0.1:0"), &out, io.Discard)
	}()
	var base string
	require.Eventually(t, func() bool {
		m := regexp.MustCompile(`console on (http://[0-9.]+:[0-9]+)`).FindStringSubmatch(out.String())
		if m != nil {
			base = m[1]
		}
		return m != nil
	}, 10*time.Second, 20*time.Millisecond)

	resp, err := http.Get(base + "/pending")
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(page))
	code := identity.PairingCode(hostDER)
	assert.Contains(t, string(page), code)
	m := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(string(page))
	require.NotNil(t, m)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.PostForm(base+"/pending/admit", url.Values{"csrf": {m[1]}, "host": {"host-new"}, "code": {code}, "region": {"kr-1"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// A request carrying another Host header is refused: the page of another origin cannot use the console.
	req, err := http.NewRequest(http.MethodGet, base+"/pending", nil)
	require.NoError(t, err)
	req.Host = "console.attacker.example"
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGINT))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the console did not stop on an interrupt")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 1)
	assert.Equal(t, code, requests[0].Code)
	_, adm, err := identity.VerifyAdmission(requests[0].Statement, op.pool, nil, time.Now())
	require.NoError(t, err, "orch gets a statement the operator's certificate verifies")
	assert.Equal(t, "host-new", adm.HostID)
	assert.Equal(t, "kr-1", adm.Region)
	assert.NotContains(t, strings.ToUpper(string(requests[0].Statement)), "PRIVATE KEY")
}
