package machineserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// names records the names a limiter was asked for and allows those in allow.
type names struct {
	asked []string
	allow bool
}

func (n *names) Allow(name string) bool {
	n.asked = append(n.asked, name)
	return n.allow
}

// refusing is a signer that refuses every token.
type refusing struct{ calls int }

func (r *refusing) RegisterCaller(context.Context, string, []byte) (identity.Issued, error) {
	r.calls++
	return identity.Issued{}, fmt.Errorf("%w: spent", inventory.ErrSignerRefused)
}

func (r *refusing) RenewCaller(context.Context, []byte) (identity.Issued, error) {
	return identity.Issued{}, nil
}

func registration(t *testing.T, remote, token string) *http.Request {
	t.Helper()
	b, err := json.Marshal(machineapi.RegisterRequest{Token: token, CSR: []byte("csr")})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, anonymous[len("POST "):], bytes.NewReader(b))
	r.RemoteAddr = remote
	return r
}

func token(nonce string) string {
	return identity.CallerTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"name":"ci","nonce":"`+nonce+`"}`)) + ".sig"
}

func TestRegister_LimitsTheSourceByItsSlash64AndEveryMemberSourceTogetherAndLogsEachRefusal(t *testing.T) {
	var logs bytes.Buffer
	sources, all, signer := &names{allow: true}, &names{allow: false}, &refusing{}
	api := New(Deps{Credentials: signer, Sources: sources, Registrations: all, Log: slog.New(slog.NewTextHandler(&logs, nil))})

	w := httptest.NewRecorder()
	api.ServeHTTP(w, registration(t, "[2001:db8:1:2:aaaa::1]:5555", token("n1")))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the member's registrations: %d, want 429", w.Code)
	}
	if signer.calls != 0 {
		t.Fatal("a limited registration reached the signer")
	}
	if strings.Join(sources.asked, ",") != "2001:db8:1:2::/64" || strings.Join(all.asked, ",") != "" || len(all.asked) != 1 {
		t.Fatalf("asked the source limiter %q and the member's %q", sources.asked, all.asked)
	}
	if !strings.Contains(logs.String(), "over its registrations") || !strings.Contains(logs.String(), "source=2001:db8:1:2::/64") {
		t.Fatalf("the limited registration is not logged with its source: %s", logs.String())
	}

	logs.Reset()
	all.allow = true
	w = httptest.NewRecorder()
	api.ServeHTTP(w, registration(t, "192.0.2.7:5555", token("n2")))
	if w.Code != http.StatusForbidden || signer.calls != 1 {
		t.Fatalf("a refused token: %d after %d calls, want 403 after 1", w.Code, signer.calls)
	}
	line := logs.String()
	for _, want := range []string{"the signer refused the token", "source=192.0.2.7", "token_nonce=n2"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused registration's log lacks %q: %s", want, line)
		}
	}

	logs.Reset()
	sources.allow = false
	w = httptest.NewRecorder()
	api.ServeHTTP(w, registration(t, "192.0.2.7:5555", token("n3")))
	if w.Code != http.StatusTooManyRequests || signer.calls != 1 {
		t.Fatalf("over the source's registrations: %d after %d calls, want 429 after 1", w.Code, signer.calls)
	}
	if !strings.Contains(logs.String(), "the source is over its registrations") {
		t.Fatalf("the source's limit is not logged: %s", logs.String())
	}
}
