package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// asRole authenticates every request as one fixed identity.
type asRole struct{ who identity.Identity }

func (a asRole) Authenticate(context.Context, [][]*x509.Certificate) (identity.Identity, error) {
	return a.who, nil
}

// guardedRoutes reads the "METHOD /path" of every route api.go registers behind a role check, so that a
// route added later fails the test until it is classified below.
func guardedRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	var out []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ [^"]+)", h\.guard\(`).FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "the pattern found no route: the test reads nothing")
	return out
}

// exposerMay is every guarded route an exposer may call (ADR 0085 items 4, 7 and 9). The member serves
// no exposures in this test, so what it may call answers 501 and the rest 403 (docs/architecture/orch.md).
var exposerMay = map[string]bool{
	"GET /v1/exposures":               true,
	"GET /v1/exposures/{id}":          true,
	"PUT /v1/exposures/{id}":          true,
	"DELETE /v1/exposures/{id}":       true,
	"GET /v1/networks/{name}/members": true,
}

func TestAnExposerReachesExactlyItsExposureAndMemberRoutes(t *testing.T) {
	h := New(Deps{Auth: asRole{identity.Identity{Subject: identity.ExposerPrefix + "lab", Role: identity.RoleExposer}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	placeholder := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	routes := guardedRoutes(t)
	for may := range exposerMay {
		require.Contains(t, routes, may, "the table names a route api.go does not register")
	}
	for _, route := range routes {
		method, path, _ := strings.Cut(route, " ")
		r := httptest.NewRequest(method, placeholder.ReplaceAllString(strings.ReplaceAll(path, "{name}", "lab"), "x"), strings.NewReader("{}"))
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if exposerMay[route] {
			// 501: the guard let it through to a handler whose service this test leaves out, so the
			// harness reaches handlers and the 403 of the others is the guard's.
			require.Equal(t, http.StatusNotImplemented, w.Code, route)
			continue
		}
		require.Equal(t, http.StatusForbidden, w.Code, route)
	}
}

func TestOnlyAnOperatorDeclaresAnExposureOfANetwork(t *testing.T) {
	call := func(role identity.Role, method string) int {
		h := New(Deps{Auth: asRole{identity.Identity{Subject: "x", Role: role}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		r := httptest.NewRequest(method, "/v1/networks/lab/exposures/api", strings.NewReader("{}"))
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, role := range []identity.Role{identity.RoleReader, identity.RoleExposer, identity.RoleHost} {
		require.Equal(t, http.StatusForbidden, call(role, http.MethodPut), "%s may not declare one", role)
	}
	require.Equal(t, http.StatusNotImplemented, call(identity.RoleOperator, http.MethodPut), "control: the operator reaches the handler, whose service this test leaves out")
	require.Equal(t, http.StatusNotImplemented, call(identity.RoleReader, http.MethodGet), "control: a reader may read it")
	require.Equal(t, http.StatusForbidden, call(identity.RoleExposer, http.MethodGet), "an exposer reads its exposures at its own route")
}
