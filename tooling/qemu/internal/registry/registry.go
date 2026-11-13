// Package registry serves the OCI image a test publishes, over TLS, from a
// directory. It answers only what mvmd asks of a registry: the version check
// and manifests and blobs by digest (as mvmd's own end-to-end fixture does).
package registry

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Index names the file of the directory that lists the media type of every
// blob, as a JSON object from digest to media type. The blob of a digest
// "sha256:HEX" is the file named HEX.
const Index = "index.json"

// Handler serves the blobs of dir.
func Handler(dir string) (http.Handler, error) {
	raw, err := os.ReadFile(filepath.Join(dir, Index))
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	if err := json.Unmarshal(raw, &types); err != nil {
		return nil, err
	}
	return &handler{dir: dir, types: types}, nil
}

type handler struct {
	dir   string
	types map[string]string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "read-only registry", http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	var digest string
	for _, route := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(rest, route); i >= 0 {
			digest = rest[i+len(route):]
			break
		}
	}
	mediaType, found := h.types[digest]
	hex, isSHA := strings.CutPrefix(digest, "sha256:")
	if !found || !isSHA || strings.ContainsAny(hex, "/.") {
		http.NotFound(w, req)
		return
	}
	body, err := os.ReadFile(filepath.Join(h.dir, hex))
	if err != nil {
		http.NotFound(w, req)
		return
	}
	slog.Info("served", "method", req.Method, "path", req.URL.Path)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// Basic, when its User is not empty, makes the registry answer 401 to a request that does not
// carry that user and password (HTTP basic authentication), as a registry that wants a credential
// does. The refusal is logged, so a test sees that mvmd asked and was turned away.
type Basic struct{ User, Password string }

func (b Basic) wrap(next http.Handler) http.Handler {
	if b.User == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		if !ok || user != b.User || pass != b.Password {
			slog.Info("refused", "method", req.Method, "path", req.URL.Path, "credential", ok)
			w.Header().Set("Www-Authenticate", `Basic realm="naru test registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// Serve serves dir on addr over TLS with the certificate and key files, until it fails.
func Serve(addr, dir, certFile, keyFile string, auth Basic) error {
	h, err := Handler(dir)
	if err != nil {
		return err
	}
	h = auth.wrap(h)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	return srv.ListenAndServeTLS("", "")
}
