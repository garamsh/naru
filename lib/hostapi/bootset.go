package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

// BootSetRequest is the first part, named "boot-set", of the body of POST /v1/boot-sets: the set's
// name and the SHA-256, in hex, of its kernel and its initramfs. The body is multipart/form-data
// whose parts are, in this order, "boot-set" (this as JSON), "kernel" and "initramfs".
type BootSetRequest struct {
	Name            string `json:"name"`
	KernelSHA256    string `json:"kernelSha256"`
	InitramfsSHA256 string `json:"initramfsSha256"`
}

// AddBootSet adds the boot set r names to the host, with no reboot, streaming the kernel and then
// the initramfs; the host has mvmd check each against the SHA-256 in r. The same set again is
// accepted, and the same name with other contents is a 409.
func (c *Client) AddBootSet(ctx context.Context, r BootSetRequest, kernel, initramfs io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { _ = pw.CloseWithError(writeBootSet(mw, r, kernel, initramfs)) }()
	_, err := c.exchange(ctx, http.MethodPost, "/v1/boot-sets", pr, mw.FormDataContentType(), nil)
	if err != nil {
		_ = pr.CloseWithError(err)
	}
	return err
}

// writeBootSet writes the three parts the host API takes, in order.
func writeBootSet(mw *multipart.Writer, r BootSetRequest, kernel, initramfs io.Reader) error {
	request, err := json.Marshal(r)
	if err != nil {
		return err
	}
	for _, p := range []struct {
		name string
		r    io.Reader
	}{{"boot-set", bytes.NewReader(request)}, {"kernel", kernel}, {"initramfs", initramfs}} {
		w, err := mw.CreateFormFile(p.name, p.name)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, p.r); err != nil {
			return err
		}
	}
	return mw.Close()
}

// RemoveBootSet removes the boot set name from the host; one that is absent is removed as well. A
// set a microVM names is a 409.
func (c *Client) RemoveBootSet(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/boot-sets/"+url.PathEscape(name), nil, nil)
}
