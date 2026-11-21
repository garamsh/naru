package release

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// httpStore serves r's directory over TLS and points r.store at it.
func httpStore(t *testing.T, r *release, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	src, err := newSource(srv.URL+"/store", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	r.store.src = src
	return srv
}

func TestHTTPSStore(t *testing.T) {
	r := newRelease(t, nil)
	httpStore(t, r, http.StripPrefix("/store", http.FileServer(http.Dir(r.dir))))
	if _, err := r.store.Fetch(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Fetch(context.Background(), "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 404 = %v, want ErrNotFound", err)
	}
}

func TestHTTPSStoreFollowsRedirectsOnlyToHTTPSOnItsHost(t *testing.T) {
	r := newRelease(t, nil)
	files := http.FileServer(http.Dir(r.dir))
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		elsewhere.Add(1)
		files.ServeHTTP(w, req)
	}))
	defer other.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		elsewhere.Add(1)
		files.ServeHTTP(w, req)
	}))
	defer plain.Close()
	mux := http.NewServeMux()
	mux.Handle("/store/", http.StripPrefix("/store", files))
	mux.HandleFunc("/same/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/store/"+strings.TrimPrefix(req.URL.Path, "/same/"), http.StatusFound)
	})
	mux.HandleFunc("/other/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/"+strings.TrimPrefix(req.URL.Path, "/other/"), http.StatusFound)
	})
	mux.HandleFunc("/downgrade/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, plain.URL+"/"+strings.TrimPrefix(req.URL.Path, "/downgrade/"), http.StatusFound)
	})
	mux.HandleFunc("/loop/", func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, req.URL.Path, http.StatusFound) })
	srv := httpStore(t, r, mux)

	for _, c := range []struct{ path, refused string }{
		{"/same", ""},
		{"/other", "another host"},
		{"/downgrade", "not https"},
		{"/loop", "redirects"},
	} {
		src, err := newSource(srv.URL+c.path, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		rc, err := src.open(context.Background(), "SHA256SUMS")
		if c.refused == "" {
			if err != nil {
				t.Fatalf("%s: control, a redirect within the host = %v", c.path, err)
			}
			rc.Close()
			continue
		}
		if !errors.Is(err, ErrStore) || !strings.Contains(err.Error(), c.refused) {
			t.Errorf("%s: open = %v, want ErrStore naming %q", c.path, err, c.refused)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("%d requests reached a host the store did not name", n)
	}
}

func TestHTTPSStoreGivesUpOnAStalledStore(t *testing.T) {
	r := newRelease(t, nil)
	files := http.FileServer(http.Dir(r.dir))
	mux := http.NewServeMux()
	mux.Handle("/store/", http.StripPrefix("/store", files))
	mux.HandleFunc("/silent/", func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
	mux.HandleFunc("/halfway/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("begin"))
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	mux.HandleFunc("/trickle/", func(w http.ResponseWriter, req *http.Request) {
		for range 8 {
			w.Write([]byte("0123456789"))
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
	})
	srv := httpStore(t, r, mux)
	const stall = 200 * time.Millisecond

	// /trickle takes 480ms in all, longer than the stall, but never goes quiet for the stall.
	for _, c := range []struct {
		path    string
		stalled bool
	}{{"/store", false}, {"/trickle", false}, {"/silent", true}, {"/halfway", true}} {
		src, err := newSource(srv.URL+c.path, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		src = withStall(t, src, stall)
		start := time.Now()
		rc, err := src.open(context.Background(), "SHA256SUMS")
		if err == nil {
			_, err = io.ReadAll(rc)
			rc.Close()
		}
		if !c.stalled {
			if err != nil {
				t.Fatalf("%s: a store that keeps sending: %v", c.path, err)
			}
			continue
		}
		if !errors.Is(err, ErrStore) {
			t.Errorf("%s: read = %v, want ErrStore", c.path, err)
		}
		if took := time.Since(start); took > 10*stall {
			t.Errorf("%s: gave up after %s, want about %s", c.path, took, stall)
		}
	}
}

func withStall(t *testing.T, s source, d time.Duration) source {
	t.Helper()
	h, ok := s.(httpSource)
	if !ok {
		t.Fatalf("source is %T, not an httpSource", s)
	}
	h.stall = d
	return h
}

// A kernel that stops early is an error however the store stops: a connection dropped under a
// declared length is a read error, and a body that ends cleanly short is a checksum failure. Only
// the whole file reads without one.
func TestHTTPSStoreMidStreamEndIsAnError(t *testing.T) {
	r := newRelease(t, nil)
	files := http.FileServer(http.Dir(r.dir))
	partial := func(abort bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !strings.HasSuffix(req.URL.Path, "/vmlinuz") {
				files.ServeHTTP(w, req)
				return
			}
			if abort {
				w.Header().Set("Content-Length", strconv.Itoa(len(r.kernel)))
			}
			w.Write(r.kernel[:5])
			if abort {
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}
		})
	}
	mux := http.NewServeMux()
	mux.Handle("/whole/", http.StripPrefix("/whole", files))
	mux.Handle("/abort/", http.StripPrefix("/abort", partial(true)))
	mux.Handle("/clean/", http.StripPrefix("/clean", partial(false)))
	srv := httpStore(t, r, mux)

	for _, c := range []struct {
		path string
		want error
	}{{"/whole", nil}, {"/abort", ErrStore}, {"/clean", ErrChecksum}} {
		src, err := newSource(srv.URL+c.path, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		r.store.src = src
		set, err := r.store.Fetch(context.Background(), version)
		if err != nil {
			t.Fatal(err)
		}
		k, i, err := set.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(k)
		k.Close()
		i.Close()
		if c.want == nil && (err != nil || string(got) != string(r.kernel)) || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: kernel read = %q, %v, want %v", c.path, got, err, c.want)
		}
	}
}
