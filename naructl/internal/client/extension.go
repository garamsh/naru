package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Extension is one entry of GET /v1/extensions: a system extension image the Node stores.
type Extension struct {
	SHA256   string   `json:"sha256"`
	Size     int64    `json:"size"`
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Services []string `json:"services"`
	Mounted  bool     `json:"mounted"`
	// MountPoint is where the Node mounted it, and empty when it did not.
	MountPoint string `json:"mountPoint"`
}

// Extensions returns every extension image the Node stores.
func (c *Client) Extensions(ctx context.Context) ([]Extension, error) {
	var body struct {
		Extensions []Extension `json:"extensions"`
	}
	err := c.getJSON(ctx, "/v1/extensions", &body)
	return body.Extensions, err
}

// PutExtension streams the image read from image, size bytes long, to the Node, which stores it if
// its SHA-256 is digest and it holds a valid manifest. It returns the entry the Node made.
func (c *Client) PutExtension(ctx context.Context, digest string, image io.Reader, size int64) (Extension, error) {
	path := "/v1/extensions/" + digest
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, image)
	if err != nil {
		return Extension{}, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return Extension{}, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return Extension{}, statusError(resp)
	}
	var e Extension
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&e); err != nil {
		return Extension{}, fmt.Errorf("PUT %s: the answer is not what the Node API sends: %w", path, err)
	}
	return e, nil
}

// DeleteExtension removes the stored image with the SHA-256 digest, which the Node refuses while it
// is mounted.
func (c *Client) DeleteExtension(ctx context.Context, digest string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/extensions/"+digest, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
