package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// source reads the assets of a release store by file name.
type source interface {
	open(ctx context.Context, name string) (io.ReadCloser, error)
}

// dirSource is a directory laid out as the assets of a release.
type dirSource string

func (d dirSource) open(_ context.Context, name string) (io.ReadCloser, error) {
	root, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	defer root.Close()
	f, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return f, nil
}

// stallTimeout is how long a request may wait for the store's answer, and a body for its next
// bytes, before it is given up. A transfer that keeps moving has no limit here; the caller's
// context bounds it.
const stallTimeout = 30 * time.Second

// maxRedirects is how many redirects one request follows.
const maxRedirects = 5

// httpSource is an https URL under which the assets of a release are. Only the system's root
// certificates are trusted (SSL_CERT_FILE and SSL_CERT_DIR replace them for the process).
type httpSource struct {
	base   string
	client *http.Client
	stall  time.Duration
}

func (h httpSource) open(ctx context.Context, name string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(h.stall, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/"+url.PathEscape(name), nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		timer.Reset(h.stall)
		return &stalling{ReadCloser: resp.Body, timer: timer, cancel: cancel, stall: h.stall, name: name}, nil
	case http.StatusNotFound:
		timer.Stop()
		cancel()
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	default:
		timer.Stop()
		cancel()
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s answered %s", ErrStore, name, resp.Status)
	}
}

// stalling is a body whose request is cancelled when no bytes arrive for stall.
type stalling struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	stall  time.Duration
	name   string
}

func (s *stalling) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.timer.Reset(s.stall)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%w: reading %s: %v", ErrStore, s.name, err)
	}
	return n, err
}

func (s *stalling) Close() error {
	s.timer.Stop()
	defer s.cancel()
	return s.ReadCloser.Close()
}

// sameHostHTTPS follows a redirect only to https on the host the request started at.
func sameHostHTTPS(req *http.Request, via []*http.Request) error {
	switch {
	case len(via) > maxRedirects:
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	case req.URL.Scheme != "https":
		return fmt.Errorf("redirect to %s, not https", req.URL.Scheme)
	case req.URL.Host != via[0].URL.Host:
		return fmt.Errorf("redirect to another host %s", req.URL.Host)
	}
	return nil
}

// newSource is the source of location: an https URL or a directory. client reads an https
// store; nil means the default one.
func newSource(location string, client *http.Client) (source, error) {
	if strings.HasPrefix(location, "https://") {
		c := http.Client{}
		if client != nil {
			c = *client
		}
		c.CheckRedirect = sameHostHTTPS
		return httpSource{base: strings.TrimRight(location, "/"), client: &c, stall: stallTimeout}, nil
	}
	if strings.Contains(location, "://") {
		return nil, fmt.Errorf("boot set store %q is neither an https URL nor a directory", location)
	}
	info, err := os.Stat(location)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("boot set store %q is neither an https URL nor a directory", location)
	}
	return dirSource(location), nil
}

// readTimeout is how long reading one small asset whole may take.
const readTimeout = 2 * time.Minute

// readAll reads the asset name whole, up to limit bytes.
func readAll(ctx context.Context, s source, name string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	rc, err := s.open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrStore, name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrManifest, name, limit)
	}
	return b, nil
}
