package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
)

// A disk is copied host to host (ADR 0109 items 5 and 8): orch asks the source for an export, which
// reads the disk for its digest and answers a grant bound to the target's host ID; orch then asks
// the target to import with that grant, and the target fetches the stream from the source over the
// mesh with its own certificate, writing it into mvmd, which publishes the disk only when its bytes
// hash to the digest the export answered.

const (
	// grantLife is how long a grant may be fetched.
	grantLife = hostapi.GrantSeconds * time.Second
	// maxGrants bounds the grants a host holds at once; an export beyond it is 503 until some
	// expire or are fetched.
	maxGrants = 256
	// diskStreamType is the content type of an export's stream: mvmd's ReadDisk messages, each
	// length-delimited.
	diskStreamType = "application/vnd.naru.disk-stream"
	// defaultStreamIdle ends a disk's stream between hosts on which nothing moved for this long: a
	// source whose write to the target, or a target whose read from the source, does not progress.
	defaultStreamIdle = 2 * time.Minute
)

var (
	grantForm  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostIDForm = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// Importer is what a host needs to fetch a disk from another host's export. Nil Deps.Importer
// makes POST /v1/disks/import 501.
type Importer struct {
	// Pair is the certificate the host reports to orch with: its host ID, role host, which the
	// source's export checks the grant against.
	Pair func() (*tls.Certificate, error)
	// Roots are the authorities a source's serving certificate chains to; it must also name the
	// source's host ID.
	Roots *x509.CertPool
	// Address is host:port of the API of the host by ID over the mesh: its mesh address and the
	// port the host API listens on.
	Address func(hostID string) (string, error)
}

// grant is one export: the disk, the host that may fetch it, what the source read, and whether it
// was fetched.
type grant struct {
	disk    string
	host    string
	export  hostapi.Export
	fetched bool
}

// grants are the exports a host holds, by grant.
type grants struct {
	mu sync.Mutex
	by map[string]*grant
}

// add records g under a new grant, dropping the expired ones first; false when maxGrants are held.
func (gs *grants) add(g *grant, now time.Time) (string, bool, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	id := hex.EncodeToString(b)
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.by == nil {
		gs.by = map[string]*grant{}
	}
	for k, v := range gs.by {
		if !now.Before(v.export.ExpiresAt) {
			delete(gs.by, k)
		}
	}
	if len(gs.by) >= maxGrants {
		return "", false, nil
	}
	gs.by[id] = g
	return id, true, nil
}

// take returns the grant for a fetch by the certificate cert at now, marking it fetched; else the
// status and message of the refusal. A grant for another host is refused and left as it was.
func (gs *grants) take(id string, cert *x509.Certificate, now time.Time) (*grant, int, string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	g, ok := gs.by[id]
	switch {
	case !ok:
		return nil, http.StatusNotFound, "this host holds no such grant"
	case cert.VerifyHostname(g.host) != nil:
		return nil, http.StatusForbidden, "the grant is for another host"
	case !now.Before(g.export.ExpiresAt):
		delete(gs.by, id)
		return nil, http.StatusGone, "the grant has expired"
	case g.fetched:
		return nil, http.StatusGone, "the grant was fetched already"
	}
	g.fetched = true
	return g, 0, ""
}

func (s *Server) streamIdle() time.Duration {
	if s.deps.StreamIdle > 0 {
		return s.deps.StreamIdle
	}
	return defaultStreamIdle
}

func (s *Server) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
}

// diskRef reports whether name is a microVM's disk identifier, <microVM>.<disk>.
func diskRef(name string) bool {
	vm, disk, ok := strings.Cut(name, ".")
	return ok && dnsLabel.MatchString(vm) && dnsLabel.MatchString(disk)
}

// exportDisk reads the disk once for its header and digest, and answers a grant for the host the
// request names to fetch it with. A disk the host does not hold is 404; one a running microVM
// holds, or that another call is reading or changing, is 409.
func (s *Server) exportDisk(w http.ResponseWriter, r *http.Request) {
	var req hostapi.ExportRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	switch {
	case !diskRef(name):
		invalid(w, "name")
		return
	case !hostIDForm.MatchString(req.Host):
		invalid(w, "host")
		return
	}
	var e hostapi.Export
	err := s.deps.Runtime.ReadDisk(r.Context(), name, func(p mvmd.DiskPart) error {
		switch p := p.(type) {
		case mvmd.DiskHeader:
			e.SizeBytes, e.Filesystem = p.SizeBytes, p.Filesystem
		case mvmd.DiskDigest:
			e.SHA256 = string(p)
		}
		return nil
	})
	if err == nil && (e.SHA256 == "" || e.SizeBytes == 0) {
		err = errors.New("mvmd's stream of the disk ended without its header or digest")
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	e.ExpiresAt = s.now().Add(grantLife).UTC()
	id, ok, err := s.grants.add(&grant{disk: name, host: req.Host, export: e}, s.now())
	switch {
	case err != nil:
		s.fail(w, err)
		return
	case !ok:
		writeJSON(w, http.StatusServiceUnavailable, hostapi.ErrorBody{Error: fmt.Sprintf("the host holds %d grants, which is its bound", maxGrants)})
		return
	}
	e.Grant = id
	writeJSON(w, http.StatusOK, e)
}

// fetchExport streams the disk of a grant to the host it names, once, within its life. Only a
// certificate of role host reaches it, and the grant's host is checked against the certificate's
// names. A stream mvmd cuts, or whose digest is not the export's, ends without its digest, so the
// target sees it end short.
func (s *Server) fetchExport(w http.ResponseWriter, r *http.Request) {
	cert := clientCertificate(r)
	if role, err := identity.RoleOf(cert); err != nil || role != identity.RoleHost {
		writeJSON(w, http.StatusForbidden, hostapi.ErrorBody{Error: "an export is fetched only with a host's certificate"})
		return
	}
	id := r.PathValue("grant")
	if !grantForm.MatchString(id) {
		writeJSON(w, http.StatusNotFound, hostapi.ErrorBody{Error: "this host holds no such grant"})
		return
	}
	g, status, msg := s.grants.take(id, cert, s.now())
	if g == nil {
		writeJSON(w, status, hostapi.ErrorBody{Error: msg})
		return
	}
	out := bufio.NewWriterSize(w, 1<<20)
	rc := http.NewResponseController(w)
	started := false
	err := s.deps.Runtime.ReadDisk(r.Context(), g.disk, func(p mvmd.DiskPart) error {
		if err := rc.SetWriteDeadline(time.Now().Add(s.streamIdle())); err != nil {
			return err
		}
		if d, ok := p.(mvmd.DiskDigest); ok && string(d) != g.export.SHA256 {
			return fmt.Errorf("the disk's digest is %s now, and %s at its export", d, g.export.SHA256)
		}
		if !started {
			w.Header().Set("Content-Type", diskStreamType)
			w.WriteHeader(http.StatusOK)
			started = true
		}
		return mvmd.WriteDiskPart(out, p)
	})
	if err == nil {
		err = out.Flush()
	}
	switch {
	case err == nil:
	case !started:
		s.fail(w, err)
	default:
		// The status is sent, and the stream ends without its digest, which the target refuses.
		s.deps.Log.Error("an export's stream ended short", "disk", g.disk, "host", g.host, "err", err.Error())
	}
}

// importDisk fetches the disk the request names from its source and has mvmd make it. The source's
// header must be of the request's size and filesystem and its digest the request's SHA-256, which
// is the digest mvmd checks the bytes against.
func (s *Server) importDisk(w http.ResponseWriter, r *http.Request) {
	var req hostapi.ImportRequest
	if !decode(w, r, &req) {
		return
	}
	var fields []string
	if !diskRef(req.Name) {
		fields = append(fields, "name")
	}
	if !hostIDForm.MatchString(req.SourceHost) {
		fields = append(fields, "sourceHost")
	}
	if !grantForm.MatchString(req.Grant) {
		fields = append(fields, "grant")
	}
	if req.SizeBytes == 0 {
		fields = append(fields, "sizeBytes")
	}
	if req.Filesystem != hostapi.FilesystemExt4 && req.Filesystem != hostapi.FilesystemNone {
		fields = append(fields, "filesystem")
	}
	if !sha256Hex.MatchString(req.SHA256) {
		fields = append(fields, "sha256")
	}
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	if s.deps.Importer == nil {
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started with no pair or mesh to fetch another host's disk with"})
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body, err := s.fetchFrom(ctx, req.SourceHost, req.Grant)
	if err != nil {
		s.failImport(w, err)
		return
	}
	defer func() { _ = body.Close() }()
	in := bufio.NewReaderSize(&idleReader{r: body, idle: s.streamIdle(), cancel: cancel}, 1<<20)
	first, err := mvmd.ReadDiskPart(in)
	if err != nil {
		s.failImport(w, sourceError{fmt.Errorf("the source's stream: %w", err)})
		return
	}
	h := mvmd.DiskHeader{SizeBytes: req.SizeBytes, Filesystem: req.Filesystem}
	if got, ok := first.(mvmd.DiskHeader); !ok || got != h {
		s.failImport(w, digestError{fmt.Errorf("the source's stream opens with %+v and not the header %+v", first, h)})
		return
	}
	err = s.deps.Runtime.CreateDiskFromStream(ctx, req.Name, h, func(send func(mvmd.DiskPart) error) error {
		for {
			p, err := mvmd.ReadDiskPart(in)
			if err != nil {
				return sourceError{fmt.Errorf("the source's stream: %w", err)}
			}
			switch p := p.(type) {
			case mvmd.DiskExtent:
				if err := send(p); err != nil {
					return err
				}
			case mvmd.DiskDigest:
				if string(p) != req.SHA256 {
					return digestError{fmt.Errorf("the source's digest is %q and not %s", p, req.SHA256)}
				}
				if err := send(p); err != nil {
					return err
				}
				if _, err := mvmd.ReadDiskPart(in); !errors.Is(err, io.EOF) {
					return sourceError{errors.New("the source's stream goes on after its digest")}
				}
				return nil
			default:
				return sourceError{errors.New("the source's stream has a second header")}
			}
		}
	})
	if err != nil {
		s.failImport(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hostapi.Imported{Name: req.Name, SizeBytes: req.SizeBytes, Filesystem: req.Filesystem, SHA256: req.SHA256})
}

// fetchFrom opens the export's stream on the source host: over the mesh, presenting the host's own
// pair, and accepting only a serving certificate that chains to the roots and names the source.
func (s *Server) fetchFrom(ctx context.Context, source, grant string) (io.ReadCloser, error) {
	imp := s.deps.Importer
	addr, err := imp.Address(source)
	if err != nil {
		return nil, sourceError{err}
	}
	check := identity.PeerCheck{Roots: imp.Roots, Usage: x509.ExtKeyUsageServerAuth, Names: []string{source}, Revocations: s.deps.Revocations, Now: s.deps.Now}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: check.VerifyConnection,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return imp.Pair() },
		},
		ResponseHeaderTimeout: s.streamIdle(),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+hostapi.ExportPathPrefix+grant, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, sourceError{err}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
		return nil, grantError{fmt.Errorf("the source answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))}
	}
	return closeBoth{resp.Body, tr}, nil
}

// idleReader cancels the import when a read of r has not returned for idle: a source that stalls
// would otherwise hold the import and mvmd's stream open.
type idleReader struct {
	r      io.Reader
	idle   time.Duration
	cancel context.CancelFunc
}

func (i *idleReader) Read(p []byte) (int, error) {
	t := time.AfterFunc(i.idle, i.cancel)
	defer t.Stop()
	return i.r.Read(p)
}

// closeBoth closes the body and the transport's connections with it.
type closeBoth struct {
	io.ReadCloser
	tr *http.Transport
}

func (c closeBoth) Close() error {
	err := c.ReadCloser.Close()
	c.tr.CloseIdleConnections()
	return err
}

// The ways an import fails on its source's side rather than mvmd's.
type (
	sourceError struct{ error }
	grantError  struct{ error }
	digestError struct{ error }
)

// failImport answers an import's failure: the source unreachable or its stream cut is 503, a fetch
// the source refused 409 grant-refused, a stream of other bytes than the request named, by the
// source's digest or by mvmd's, 409 digest-mismatch; the rest as any mvmd call's.
func (s *Server) failImport(w http.ResponseWriter, err error) {
	var (
		se sourceError
		ge grantError
		de digestError
	)
	switch {
	case errors.As(err, &se):
		writeJSON(w, http.StatusServiceUnavailable, hostapi.ErrorBody{Error: err.Error(), Code: hostapi.CodeSourceUnavailable})
	case errors.As(err, &ge):
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: err.Error(), Code: hostapi.CodeGrantRefused})
	case errors.As(err, &de), errors.Is(err, mvmd.ErrInvalid):
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: err.Error(), Code: hostapi.CodeDigestMismatch})
	default:
		s.fail(w, err)
	}
}
