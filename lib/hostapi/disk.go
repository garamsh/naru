package hostapi

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// GrantSeconds is how long an export's grant may be fetched: once, within it.
const GrantSeconds = 600

// The filesystems of a disk as an export and an import name them.
const (
	FilesystemExt4 = "ext4"
	FilesystemNone = "none"
)

// The codes of a disk copy's refusals.
const (
	// CodeGrantRefused: 409, an import whose source refused the fetch: the grant is unknown,
	// expired, already fetched, or names another host.
	CodeGrantRefused = "grant-refused"
	// CodeDigestMismatch: 409, an import whose bytes, or the source's digest of them, are not of
	// the SHA-256 the import named. The target holds no disk of it.
	CodeDigestMismatch = "digest-mismatch"
	// CodeSourceUnavailable: 503, an import whose source the target could not reach.
	CodeSourceUnavailable = "source-unavailable"
)

// ExportPathPrefix is where a source serves an export's stream to the host its grant names:
// GET ExportPathPrefix + grant. It is the one route a host's own certificate reaches.
const ExportPathPrefix = "/v1/disks/exports/"

// ExportRequest is the body of POST /v1/disks/{name}/export: the ID of the host that is to fetch
// the disk.
type ExportRequest struct {
	Host string `json:"host"`
}

// Export is what POST /v1/disks/{name}/export answers: the grant the host it names fetches the disk
// with, once, until ExpiresAt, and the disk's size, filesystem and SHA-256 in hex as the source read
// it, which the import names.
type Export struct {
	Grant      string    `json:"grant"`
	SizeBytes  uint64    `json:"sizeBytes"`
	Filesystem string    `json:"filesystem"`
	SHA256     string    `json:"sha256"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// ImportRequest is the body of POST /v1/disks/import: the disk to make, its identifier
// <microVM>.<disk> as a create's imported disk names it, from the export of the source host by ID
// with the grant, of the size, filesystem and SHA-256 the export answered.
type ImportRequest struct {
	Name       string `json:"name"`
	SourceHost string `json:"sourceHost"`
	Grant      string `json:"grant"`
	SizeBytes  uint64 `json:"sizeBytes"`
	Filesystem string `json:"filesystem"`
	SHA256     string `json:"sha256"`
}

// Imported is what POST /v1/disks/import answers: the disk made, whose bytes hash to SHA256.
type Imported struct {
	Name       string `json:"name"`
	SizeBytes  uint64 `json:"sizeBytes"`
	Filesystem string `json:"filesystem"`
	SHA256     string `json:"sha256"`
}

// ExportDisk asks the host for a grant to the disk by its identifier, for the host by ID.
func (c *Client) ExportDisk(ctx context.Context, name, host string) (Export, error) {
	var e Export
	err := c.do(ctx, http.MethodPost, "/v1/disks/"+url.PathEscape(name)+"/export", ExportRequest{Host: host}, &e)
	return e, err
}

// ImportDisk has the host pull the disk from its source and make it.
func (c *Client) ImportDisk(ctx context.Context, r ImportRequest) (Imported, error) {
	var d Imported
	err := c.do(ctx, http.MethodPost, "/v1/disks/import", r, &d)
	return d, err
}
