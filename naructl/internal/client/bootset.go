package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

// BootSetManifest is naru-node-<version>.json, the boot set a release carries: the name mvmd
// offers it under, the release's version, and the kernel and the initramfs, each as a file in the
// same directory with its SHA-256 (hex).
type BootSetManifest struct {
	Name      string      `json:"name"`
	Version   string      `json:"version"`
	Kernel    BootSetFile `json:"kernel"`
	Initramfs BootSetFile `json:"initramfs"`
}

// BootSetFile is a file of a boot set.
type BootSetFile struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// AddBootSet adds the boot set m names to the host, with no reboot, streaming the kernel and then
// the initramfs; the host has mvmd check each against the SHA-256 in m. The same set again is
// accepted, and the same name with other contents is a 409.
func (h *HostClient) AddBootSet(ctx context.Context, m BootSetManifest, kernel, initramfs io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { pw.CloseWithError(writeBootSet(mw, m, kernel, initramfs)) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.c.base+"/v1/boot-sets", pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.c.http.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("POST /v1/boot-sets: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// writeBootSet writes the three parts the host API takes, in order.
func writeBootSet(mw *multipart.Writer, m BootSetManifest, kernel, initramfs io.Reader) error {
	request, err := json.Marshal(struct {
		Name            string `json:"name"`
		KernelSHA256    string `json:"kernelSha256"`
		InitramfsSHA256 string `json:"initramfsSha256"`
	}{m.Name, m.Kernel.SHA256, m.Initramfs.SHA256})
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
func (h *HostClient) RemoveBootSet(ctx context.Context, name string) error {
	resp, err := h.c.do(ctx, http.MethodDelete, "/v1/boot-sets/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}
