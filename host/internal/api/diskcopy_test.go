package api

import (
	"bytes"
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
	"sync"
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
	// idle, rate and transfers are each host's Deps.StreamIdle, MinStreamRate and Transfers, and
	// addrs the addresses of hosts the fleet does not serve; logs is what every host logs.
	idle      time.Duration
	rate      uint64
	rates     map[string]uint64
	transfers int
	addrs     map[string]string
	logs      *syncBuffer
}

// rateOf is the host's MinStreamRate: its own, else the fleet's.
func (f *fleet) rateOf(id string) uint64 {
	if r, ok := f.rates[id]; ok {
		return r
	}
	return f.rate
}

// syncBuffer is a log the hosts write from their own goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newFleet(t *testing.T) *fleet {
	f := &fleet{rates: map[string]uint64{}, hosts: newAuthority(t, "naru-host-ca"), orchCA: newAuthority(t, "naru-orch-ca"), clock: &clock{t: time.Now()}, byID: map[string]*env{}, addrs: map[string]string{}, logs: &syncBuffer{}}
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
		Log: slog.New(slog.NewTextHandler(f.logs, nil)), Metrics: prometheus.NewRegistry(), LeaderTokenFile: filepath.Join(dir, "leader-token"),
		Now: f.clock.now, StreamIdle: f.idle, MinStreamRate: f.rateOf(id), Transfers: f.transfers,
		Importer: &Importer{
			Pair:  func() (*tls.Certificate, error) { return &pair, nil },
			Roots: func() *x509.CertPool { return roots },
			Address: func(host string) (string, error) {
				if h, ok := f.byID[host]; ok {
					return h.addr, nil
				}
				if a, ok := f.addrs[host]; ok {
					return a, nil
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

func TestDiskCopy_BytesThatDoNotHashToTheDigestAreAStreamRefusedAndLeaveNoDisk(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	// The source's own digest agrees with the export; the bytes on the wire do not.
	src.rt.mu.Lock()
	src.rt.corrupt = map[string]bool{disk: true}
	src.rt.mu.Unlock()

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeStreamRefused, eb.Code, eb.Error)
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
	// A rate of a byte a second puts the deadline as a whole weeks away: only the idle bound ends it.
	f.idle, f.rate = 200*time.Millisecond, 1
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

func TestDiskCopy_NeitherALogNorAnImportsErrorHoldsAGrant(t *testing.T) {
	f, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	code, _ := importOf(t, tgt, orch, x)
	require.Equal(t, http.StatusOK, code)

	y := exportOf(t, src, orch, disk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.addrs["h-closed"] = ln.Addr().String()
	require.NoError(t, ln.Close())
	code, body := tgt.do(t, orch, "POST", "/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-3.data", SourceHost: "h-closed", Grant: y.Grant, SizeBytes: y.SizeBytes, Filesystem: y.Filesystem, SHA256: y.SHA256,
	})
	require.Equal(t, http.StatusServiceUnavailable, code, string(body))

	assert.NotContains(t, string(body), y.Grant, "the import's error names no grant")
	assert.Contains(t, string(body), grantTag(y.Grant), "it names the grant by its tag")
	logs := f.logs.String()
	assert.Contains(t, logs, hostapi.ExportPathPrefix+grantTag(x.Grant), "the fetch is logged by the grant's tag")
	for _, g := range []string{x.Grant, y.Grant} {
		assert.NotContains(t, logs, g, "no log holds a grant")
	}
}

func TestDiskCopy_AStreamThatOpensWithAnExtentPutsNoBytesInTheError(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	src.rt.mu.Lock()
	src.rt.extentFirst = map[string]bool{disk: true}
	src.rt.mu.Unlock()

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, hostapi.CodeDigestMismatch, eb.Code)
	assert.Contains(t, eb.Error, "an extent of 23 bytes at 0")
	assert.NotContains(t, eb.Error, "written before the move", "the disk's bytes are not in the error")
	assert.False(t, tgt.holds("m-abc-2.data"))
}

func TestDiskCopy_ACertificateThatNamesTheHostOnlyInItsDNSNamesDoesNotFetch(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	pair, _, _ := f.hosts.issueName(t, pkix.Name{CommonName: "h-other", OrganizationalUnit: []string{string(identity.RoleHost)}}, "h-tgt")

	code, _ := src.do(t, src.clientOf(pair), "GET", hostapi.ExportPathPrefix+x.Grant, nil)
	assert.Equal(t, http.StatusForbidden, code, "the grant's host is the certificate's common name, which the signer sets")
}

func TestDiskCopy_TwoFetchesOfAGrantAtOnceServeOne(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	target := src.clientOf(f.hostPair(t, "h-tgt"))
	codes := make(chan int, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			code, _ := src.do(t, target, "GET", hostapi.ExportPathPrefix+x.Grant, nil)
			codes <- code
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusGone}, got)
}

// hold runs n calls of do, each of which takes one of the host's transfers until the test ends, and
// returns once they all hold it.
func hold(t *testing.T, n int, started func() int, do func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	for range n {
		wg.Go(func() { do(ctx) })
	}
	require.Eventually(t, func() bool { return started() >= n }, 5*time.Second, 10*time.Millisecond)
}

// bounded sends the request and requires an answer within 5 s: a call the bound does not refuse
// would wait on the stalled ones.
func bounded(t *testing.T, c *http.Client, method, url string, body any) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err, "the call beyond the bound was not answered")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// within sends the request under ctx and ignores its outcome.
func within(ctx context.Context, c *http.Client, method, url string, body any) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return
	}
	if resp, err := c.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// reads counts the ReadDisk calls of the disk the fake made.
func (f *fakeRuntime) reads(disk string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "readDisk "+disk {
			n++
		}
	}
	return n
}

func TestDiskCopy_ExportsReadingBeyondTheBoundAreBusy(t *testing.T) {
	_, src, _, orch, disk := copyFixture(t)
	src.rt.mu.Lock()
	src.rt.stallAt = map[string]int{disk: 0}
	src.rt.mu.Unlock()
	hold(t, 2, func() int { return src.rt.reads(disk) }, func(ctx context.Context) {
		within(ctx, orch, "POST", "https://"+src.addr+"/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	})

	code, body := bounded(t, orch, "POST", "https://"+src.addr+"/v1/disks/"+disk+"/export", hostapi.ExportRequest{Host: "h-tgt"})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, hostapi.CodeBusy, codeOf(t, body).Code)
}

func TestDiskCopy_FetchesBeyondTheBoundAreBusyAndLeaveTheGrantUnspent(t *testing.T) {
	f, src, _, orch, disk := copyFixture(t)
	var grants []string
	for range 3 {
		grants = append(grants, exportOf(t, src, orch, disk).Grant)
	}
	before := src.rt.reads(disk)
	src.rt.mu.Lock()
	src.rt.stallAt = map[string]int{disk: 1}
	src.rt.mu.Unlock()
	target := src.clientOf(f.hostPair(t, "h-tgt"))
	i := 0
	hold(t, 2, func() int { return src.rt.reads(disk) - before }, func(ctx context.Context) {
		src.rt.mu.Lock()
		g := grants[i]
		i++
		src.rt.mu.Unlock()
		within(ctx, target, "GET", "https://"+src.addr+hostapi.ExportPathPrefix+g, nil)
	})

	code, body := bounded(t, target, "GET", "https://"+src.addr+hostapi.ExportPathPrefix+grants[2], nil)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, hostapi.CodeBusy, codeOf(t, body).Code)
	src.rt.mu.Lock()
	delete(src.rt.stallAt, disk)
	src.rt.mu.Unlock()
}

func TestDiskCopy_ImportsBeyondTheBoundAreBusy(t *testing.T) {
	f := newFleet(t)
	f.transfers = 1
	src, tgt := f.start(t, "h-src"), f.start(t, "h-tgt")
	orch := src.clientOf(f.orch)
	disk := stoppedDisk(src, "m-abc-1", "data", 1<<20, "written before the move")
	x, y := exportOf(t, src, orch, disk), exportOf(t, src, orch, disk)
	before := src.rt.reads(disk)
	src.rt.mu.Lock()
	src.rt.stallAt = map[string]int{disk: 1}
	src.rt.mu.Unlock()
	hold(t, 1, func() int { return src.rt.reads(disk) - before }, func(ctx context.Context) {
		within(ctx, orch, "POST", "https://"+tgt.addr+"/v1/disks/import", hostapi.ImportRequest{
			Name: "m-abc-2.data", SourceHost: "h-src", Grant: x.Grant, SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
		})
	})

	code, body := bounded(t, orch, "POST", "https://"+tgt.addr+"/v1/disks/import", hostapi.ImportRequest{
		Name: "m-abc-3.data", SourceHost: "h-src", Grant: y.Grant, SizeBytes: y.SizeBytes, Filesystem: y.Filesystem, SHA256: y.SHA256,
	})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, hostapi.CodeBusy, codeOf(t, body).Code)
}

// trickling is a fleet whose source sends 3 MiB in parts of 64 KiB, one each 100 ms: the source's
// buffer of 1 MiB goes out every 1.6 s, within the idle bound of 2 s, and the whole takes 4.8 s,
// past a deadline of the idle bound alone; rates are the hosts' MinStreamRate.
func trickling(t *testing.T, src, tgt uint64) {
	t.Helper()
	f := newFleet(t)
	f.idle = 2 * time.Second
	f.rates["h-src"], f.rates["h-tgt"] = src, tgt
	s, tg := f.start(t, "h-src"), f.start(t, "h-tgt")
	orch := s.clientOf(f.orch)
	disk := stoppedDisk(s, "m-abc-1", "data", 4<<20, strings.Repeat("x", 3<<20))
	x := exportOf(t, s, orch, disk)
	s.rt.mu.Lock()
	s.rt.trickle = map[string]time.Duration{disk: 100 * time.Millisecond}
	s.rt.mu.Unlock()
	endsUnavailable(t, tg, orch, x)
}

func TestDiskCopy_AnImportFromASourceThatTricklesEndsAtItsDeadline(t *testing.T) {
	// The target's deadline is the idle bound; the source's weeks away.
	trickling(t, 1, 1<<30)
}

func TestDiskCopy_AFetchThatTricklesEndsAtTheSourcesDeadline(t *testing.T) {
	trickling(t, 1<<30, 1)
}

func TestDiskCopy_AnImportOfADiskWhoseMicroVMStartedSinceItsExportIsSourceUnavailable(t *testing.T) {
	_, src, tgt, orch, disk := copyFixture(t)
	x := exportOf(t, src, orch, disk)
	src.rt.mu.Lock()
	m := src.rt.machines["m-abc-1"]
	m.State = mvmd.StateRunning
	src.rt.machines["m-abc-1"] = m
	src.rt.mu.Unlock()

	code, eb := importOf(t, tgt, orch, x)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, hostapi.CodeSourceUnavailable, eb.Code, "the grant was good; the source could not read the disk: %s", eb.Error)
}
