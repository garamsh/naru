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

// httpSource is an https URL under which the assets of a release are.
type httpSource struct {
	base   string
	client *http.Client
}

func (h httpSource) open(ctx context.Context, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s answered %s", ErrStore, name, resp.Status)
	}
}

// newSource is the source of location: an https URL or a directory.
func newSource(location string, client *http.Client) (source, error) {
	if strings.HasPrefix(location, "https://") {
		if client == nil {
			client = http.DefaultClient
		}
		return httpSource{base: strings.TrimRight(location, "/"), client: client}, nil
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

// readAll reads the asset name whole, up to limit bytes.
func readAll(ctx context.Context, s source, name string, limit int64) ([]byte, error) {
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
