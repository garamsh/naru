package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
)

// fleet is two hosts as orch admits them: one authority issues every host's pair, which serves the
// host's API and is the client certificate it fetches an export with, and every host trusts it for
// its clients; orch's clients come from another authority each host trusts too.
type fleet struct {
	hosts  authority
	orchCA authority
	clock  *clock
	byID   map[string]*env
	orch   tls.Certificate
	// idle is each host's Deps.StreamIdle.
	idle time.Duration
}

func newFleet(t *testing.T) *fleet {
	f := &fleet{hosts: newAuthority(t, "naru-host-ca"), orchCA: newAuthority(t, "naru-orch-ca"), clock: &clock{t: time.Now()}, byID: map[string]*env{}}
	f.orch, _, _ = f.orchCA.issueSubject(t, pkix.Name{CommonName: "orch", OrganizationalUnit: []string{string(identity.RoleOrch)}}, false)
	return f
}

// hostPair is the pair the signer issues to the host by ID: role host, the ID its common name and
// DNS name, for serving and for clients.
func (f *fleet) hostPair(t *testing.T, id string) tls.Certificate {
	t.Helper()
	pair, _, _ := f.hosts.issueName(t, pkix.Name{CommonName: id, OrganizationalUnit: []string{string(identity.RoleHost)}}, id)
	return pair
}

// start serves the host by ID, which imports from the fleet's other hosts by their addresses.
func (f *fleet) start(t *testing.T, id string) *env {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	pair := f.hostPair(t, id)
	clients := x509.NewCertPool()
	clients.AddCert(f.hosts.cert)
	clients.AddCert(f.orchCA.cert)
	conf := ServerTLS(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil }, clients, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e := &env{rt: newFake(), addr: ln.Addr().String(), ca: f.orchCA, pool: x509.NewCertPool()}
	e.pool.AddCert(f.hosts.cert)
	roots := x509.NewCertPool()
	roots.AddCert(f.hosts.cert)
	srv := New(Deps{
		Runtime: e.rt, HostFile: write("host.json", []byte(`{}`)), InitFile: filepath.Join(dir, "init.json"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: prometheus.NewRegistry(), LeaderTokenFile: filepath.Join(dir, "leader-token"),
		Now: f.clock.now, StreamIdle: f.idle,
		Importer: &Importer{
			Pair:  func() (*tls.Certificate, error) { return &pair, nil },
			Roots: roots,
			Address: func(host string) (string, error) {
				if h, ok := f.byID[host]; ok {
					return h.addr, nil
				}
				return "", io.ErrUnexpectedEOF
			},
		},
	})
	ctx, stop := context.WithCancel(context.Background())
	e.stop, e.served = stop, make(chan error, 1)
	go func() { e.served <- srv.Serve(ctx, ln, conf) }()
	t.Cleanup(stop)
	f.byID[id] = e
	return e
}

// clientOf is a client of e's API presenting pair, which checks e's serving certificate against
// the host authority by address.
func (e *env) clientOf(pair tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true,
	}}}
}

// stoppedDisk gives e a stopped microVM holding the disk name with content at its start.
func stoppedDisk(e *env, vm, disk string, size uint64, content string) string {
	id := vm + "." + disk
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	e.rt.disks[id] = mvmd.Disk{ID: id, SizeBytes: size, Holder: vm}
	e.rt.data[id] = []byte(content)
	e.rt.machines[vm] = mvmd.Machine{ID: vm, State: mvmd.StateStopped, Disks: []mvmd.Attachment{{ID: id}}}
	return id
}

// exportOf has orch export the disk of src for h-tgt.
func exportOf(t *testing.T, src *env, orch *http.Client, disk string) hostapi.Export {
	t.Helper()
	code, body := src.do(t, orch, "POST", "/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	require.Equal(t, http.StatusOK, code, string(body))
	var x hostapi.Export
	require.NoError(t, json.Unmarshal(body, &x))
	return x
}

// copyFixture is a fleet of h-src, holding a stopped microVM's disk, and h-tgt.
func copyFixture(t *testing.T) (f *fleet, src, tgt *env, orch *http.Client, disk string) {
	t.Helper()
	f = newFleet(t)
	src, tgt = f.start(t, "h-src"), f.start(t, "h-tgt")
	orch = src.clientOf(f.orch)
	disk = stoppedDisk(src, "m-abc-1", "data", 1<<20, "written before the move")
	return f, src, tgt, orch, disk
}

func TestDiskCopy_TheTargetImportsTheExportedDiskByteForByte(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)

	x := exportOf(t, src, orch, disk)
	assert.Equal(t, uint64(1<<20), x.SizeBytes)
	assert.Equal(t, hostapi.FilesystemExt4, x.Filesystem)
	assert.Equal(t, digestOf(src.rt.diskBytes(disk)), x.SHA256, "the export answers the digest of the disk's whole size")
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), x.ExpiresAt, time.Minute)

	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-2.data", SourceHost: "h-src", Grant: x.Grant, SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
	})
	require.Equal(t, http.StatusOK, code, string(body))
	tgt.rt.mu.Lock()
	defer tgt.rt.mu.Unlock()
	assert.Equal(t, uint64(1<<20), tgt.rt.disks["m-abc-2.data"].SizeBytes)
	assert.Equal(t, "written before the move", string(tgt.rt.data["m-abc-2.data"]))
	assert.Equal(t, x.SHA256, tgt.rt.digests["m-abc-2.data"], "mvmd was given the export's digest")
}

func TestDiskCopy_AFetchWithAnotherHostsCertificateIsRefusedAndTheGrantStaysTheTargets(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)

	other := src.clientOf(f.hostPair(t, "h-other"))
	code, body := src.do(t, other, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusForbidden, code, string(body))

	target := src.clientOf(f.hostPair(t, "h-tgt"))
	code, _ = src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusOK, code, "the refusal did not spend the grant")
}

func TestDiskCopy_ASecondFetchOfAGrantIsRefused(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	target := src.clientOf(f.hostPair(t, "h-tgt"))

	code, _ := src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	require.Equal(t, http.StatusOK, code)
	code, body := src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusGone, code, string(body))
}

func TestDiskCopy_AnExpiredGrantIsRefused(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	target := src.clientOf(f.hostPair(t, "h-tgt"))

	f.clock.advance(10*time.Minute - time.Second)
	f.clock.advance(time.Second)
	code, body := src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusGone, code, string(body))
}

func TestDiskCopy_AGrantIsFetchedJustBeforeItExpires(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	target := src.clientOf(f.hostPair(t, "h-tgt"))

	f.clock.advance(10*time.Minute - time.Second)
	code, body := src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusOK, code, string(body))
}

func TestDiskCopy_OnlyAHostCertificateFetchesAndAHostCertificateReachesNothingElse(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)

	code, _ := src.do(t, orch, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusForbidden, code, "orch's certificate does not fetch")
	operator, _, _ := f.hosts.issueName(t, pkix.Name{CommonName: "h-tgt", OrganizationalUnit: []string{string(identity.RoleOperator)}}, "h-tgt")
	code, _ = src.do(t, src.clientOf(operator), "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusForbidden, code, "a certificate naming the grant's host with another role does not fetch")

	host := src.clientOf(f.hostPair(t, "h-tgt"))
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/host"}, {"GET", "/v1/microvms"}, {"POST", "/v1/disks/" + disk + "/export"}, {"POST", "/v1/disks/import"},
	} {
		code, _ := src.do(t, host, c.method, c.path, map[string]any{})
		assert.Equal(t, http.StatusForbidden, code, "%s %s", c.method, c.path)
	}
}

func TestDiskCopy_ExportAndImportNeedTheProvisioner(t *testing.T) {
	f, src, tgt, _, disk := copyFixture(t)
	reader, _, _ := f.orchCA.issueSubject(t, pkix.Name{CommonName: "r", OrganizationalUnit: []string{string(identity.RoleReader)}}, false)
	c := src.clientOf(reader)

	code, _ := src.do(t, c, "POST", "/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusForbidden, code)
	code, _ = tgt.do(t, c, "POST", "/v1/disks/import", hostapi.ImportRequest{})
	assert.Equal(t, http.StatusForbidden, code)
}

func TestDiskCopy_ADiskARunningMicroVMHoldsIsNotExported(t *testing.T) {
	_, src, _, orch, disk := copyFixture(t)
	src.rt.mu.Lock()
	m := src.rt.machines["m-abc-1"]
	m.State = mvmd.StateRunning
	src.rt.machines["m-abc-1"] = m
	src.rt.mu.Unlock()

	code, body := src.do(t, orch, "POST", "/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusConflict, code, string(body))
	code, _ = src.do(t, orch, "POST", "/v1/disks/m-abc-1.none/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusNotFound, code)
}

// importOf imports the export x of the disk from h-src to the target as m-abc-2.data.
func importOf(t *testing.T, tgt *env, orch *http.Client, x hostapi.Export) (int, hostapi.ErrorBody) {
	t.Helper()
	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-2.data", SourceHost: "h-src", Grant: x.Grant, SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
	})
	var eb hostapi.ErrorBody
	_ = json.Unmarshal(body, &eb)
	return code, eb
}

func (e *env) holds(id string) bool {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	_, ok := e.rt.disks[id]
	return ok
}

func TestDiskCopy_ADiskThatChangedSinceItsExportIsNotImported(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	src.rt.mu.Lock()
	src.rt.data[disk] = []byte("written after the export")
	src.rt.mu.Unlock()

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusServiceUnavailable, code, "the source cut its stream short: %+v", eb)
	assert.False(t, tgt.holds("m-abc-2.data"), "the target holds nothing of it")
}

func TestDiskCopy_AnImportOfAnotherDigestThanTheSourcesIsADigestMismatch(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	x.SHA256 = strings.Repeat("0", 64)

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeDigestMismatch, eb.Code, eb.Error)
	assert.False(t, tgt.holds("m-abc-2.data"))
}

func TestDiskCopy_BytesThatDoNotHashToTheDigestLeaveNoDisk(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	// The source's own digest agrees with the export; the bytes on the wire do not.
	src.rt.mu.Lock()
	src.rt.corrupt = map[string]bool{disk: true}
	src.rt.mu.Unlock()

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeDigestMismatch, eb.Code, eb.Error)
	assert.False(t, tgt.holds("m-abc-2.data"))
}

func TestDiskCopy_AnImportWhoseFetchTheSourceRefusesIsGrantRefused(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	code, _ := importOf(t, tgt, orch, x)
	require.Equal(t, http.StatusOK, code)

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeGrantRefused, eb.Code, eb.Error)
}

func TestDiskCopy_AnImportFromAHostThatIsNotAPeerIsSourceUnavailable(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-2.data", SourceHost: "h-gone", Grant: x.Grant, SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
	})
	assert.Equal(t, http.StatusServiceUnavailable, code, string(body))
}

func TestDiskCopy_TheTargetRefusesASourceWhoseCertificateNamesAnotherHost(t *testing.T) {
	f, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	// h-src's address is given for h-impostor: its certificate names h-src.
	f.byID["h-impostor"] = src

	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-2.data", SourceHost: "h-impostor", Grant: x.Grant, SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
	})
	assert.Equal(t, http.StatusServiceUnavailable, code, string(body))
	assert.False(t, tgt.holds("m-abc-2.data"))
}

func TestDiskCopy_ExportAndImportRefuseWhatIsNotTheirs(t *testing.T) {
	_, src, tgt, orch, _ := copyFixture(t)
	code, _ := src.do(t, orch, "POST", "/v1/disks/no-dot/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	code, _ = src.do(t, orch, "POST", "/v1/disks/m-abc-1.data/export", hostapi.ExportRequest{Host: ""})
	assert.Equal(t, http.StatusUnprocessableEntity, code)

	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{Filesystem: "xfs"})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	assert.ElementsMatch(t, []string{"name", "sourceHost", "grant", "sizeBytes", "filesystem", "sha256"}, codeOf(t, body).Fields)
}

func TestDiskCopy_ExportsBeyondTheBoundAre503(t *testing.T) {
	_, src, _, orch, disk := copyFixture(t)
	for range maxGrants {
		exportOf(t, src, orch, disk)
	}
	code, _ := src.do(t, orch, "POST", "/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestDiskCopy_ExpiredGrantsDoNotCountAgainstTheBound(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	for range maxGrants {
		exportOf(t, src, orch, disk)
	}
	f.clock.advance(10 * time.Minute)
	exportOf(t, src, orch, disk)
}

func TestDiskCopy_AnImporterlessHostAnswers501(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-2.data", SourceHost: "h-src", Grant: strings.Repeat("a", 64), SizeBytes: 1, Filesystem: "ext4", SHA256: strings.Repeat("b", 64),
	})
	assert.Equal(t, http.StatusNotImplemented, code)
}

// stalled is a fleet whose hosts end a stream idle for 200 ms, h-src holding a disk of 2 MiB of
// content whose stream stops moving before its part at the index.
func stalled(t *testing.T, at int) (tgt *env, orch *http.Client, x hostapi.Export) {
	t.Helper()
	f := newFleet(t)
	f.idle = 200 * time.Millisecond
	src, tgt := f.start(t, "h-src"), f.start(t, "h-tgt")
	orch = src.clientOf(f.orch)
	disk := stoppedDisk(src, "m-abc-1", "data", 4<<20, strings.Repeat("x", 2<<20))
	x = exportOf(t, src, orch, disk)
	src.rt.mu.Lock()
	src.rt.stallAt = map[string]int{disk: at}
	src.rt.mu.Unlock()
	return tgt, orch, x
}

// endsUnavailable imports x and requires the import to end within 10 s with 503 and no disk.
func endsUnavailable(t *testing.T, tgt *env, orch *http.Client, x hostapi.Export) {
	t.Helper()
	done := make(chan int, 1)
	go func() { code, _ := importOf(t, tgt, orch, x); done <- code }()
	select {
	case code := <-done:
		assert.Equal(t, http.StatusServiceUnavailable, code)
	case <-time.After(10 * time.Second):
		t.Fatal("the import did not end on a stalled source")
	}
	assert.False(t, tgt.holds("m-abc-2.data"))
}

func TestDiskCopy_AnImportFromASourceThatStallsMidStreamEnds(t *testing.T) {
	// The extent, beyond the source's buffer, reaches the target; the digest never comes.
	tgt, orch, x := stalled(t, 2)
	endsUnavailable(t, tgt, orch, x)
}

func TestDiskCopy_AnImportFromASourceThatSendsNoAnswerEnds(t *testing.T) {
	tgt, orch, x := stalled(t, 0)
	endsUnavailable(t, tgt, orch, x)
}

func TestDiskCopy_AnExportOfAStreamWithoutItsDigestFails(t *testing.T) {
	_, src, _, orch, disk := copyFixture(t)
	src.rt.mu.Lock()
	src.rt.truncate = map[string]bool{disk: true}
	src.rt.mu.Unlock()
	code, body := src.do(t, orch, "POST", "/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusBadGateway, code, string(body))
}

// The bytes and the digest are the source's, so mvmd would make the disk: the header is the check.
func TestDiskCopy_AnImportOfAnotherFilesystemThanTheSourcesIsRefused(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	x.Filesystem = hostapi.FilesystemNone

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeDigestMismatch, eb.Code, eb.Error)
	assert.False(t, tgt.holds("m-abc-2.data"))
}
