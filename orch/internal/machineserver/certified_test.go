package machineserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
)

// noAuth fails the test if a handler asks it to authenticate: the refusal comes first.
type noAuth struct{ t *testing.T }

func (a noAuth) Authenticate(context.Context, [][]*x509.Certificate) (identity.Identity, error) {
	a.t.Error("a request without a certificate reached a handler")
	return identity.Identity{}, identity.ErrNoChain
}

// denyAll is a limiter over its registrations for every source.
type denyAll struct{}

func (denyAll) Allow(string) bool { return false }

func TestNew_EveryRouteButRegisterRefusesARequestWithoutACertificate(t *testing.T) {
	h := &handler{Deps{Auth: noAuth{t}, Sources: denyAll{}, Log: slog.New(slog.DiscardHandler)}}
	api := New(h.Deps)
	routes := h.routes()
	if len(routes) < 20 {
		t.Fatalf("walked %d routes; the API has more", len(routes))
	}
	for _, rt := range routes {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			method, path = http.MethodGet, "/v2/anything"
		}
		path = strings.NewReplacer("{name}", "m1", "{hostname}", "a.example").Replace(path)
		for _, state := range []*tls.ConnectionState{nil, {}} {
			r := httptest.NewRequest(method, path, strings.NewReader("{}"))
			r.TLS = state
			w := httptest.NewRecorder()
			api.ServeHTTP(w, r)
			res := w.Result()
			body, _ := io.ReadAll(res.Body)
			var e machineapi.ErrorBody
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("%s: %v: %s", rt.pattern, err, body)
			}
			want, code := http.StatusForbidden, machineapi.CodeForbidden
			if rt.pattern == anonymous {
				// It passes the refusal and reaches register's own first check.
				want, code = http.StatusTooManyRequests, machineapi.CodeRateLimited
			}
			if res.StatusCode != want || e.Code != code {
				t.Errorf("%s %s (tls %v): %d %s, want %d %s", method, path, state != nil, res.StatusCode, e.Code, want, code)
			}
		}
	}
}
