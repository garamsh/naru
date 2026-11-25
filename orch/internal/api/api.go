// Package api is naru-orchd's HTTPS API. It authenticates the caller's certificate, checks the
// role against the endpoint's action, calls the services and translates every failure to a status
// once, in writeError.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/groupsvc"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/machines"
	"github.com/garamsh/naru/orch/internal/restore"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// maxBody bounds a request body.
const maxBody = 1 << 20

// Snapshots takes a sealed etcd snapshot now and returns the newest one written.
type Snapshots interface {
	Take(ctx context.Context) (orchapi.SnapshotResult, error)
	Latest() ([]byte, error)
}

// Restorer restores the plain etcd snapshot read from r, whose SHA-256 the caller states in hex,
// into a member that holds no data, and returns once the member runs. It returns
// restore.ErrRejected for a snapshot it will not restore and ErrRestored once a restore has begun.
type Restorer interface {
	Restore(ctx context.Context, r io.Reader, sha256Hex string) error
}

// ErrRestored is returned by a Restorer that has already restored, or is restoring.
var ErrRestored = errors.New("this member is not waiting for a restore")

// maxRestore bounds a restore's body; etcd's default size limit is 2 GiB, and a member's keys are small.
const maxRestore = 1 << 30

// Networks is the networks service the handlers call.
type Networks interface {
	Apply(ctx context.Context, by, name string, spec orchapi.NetworkSpec) (orchapi.Network, error)
	Get(ctx context.Context, name string) (orchapi.Network, error)
	List(ctx context.Context) ([]orchapi.Network, error)
	Delete(ctx context.Context, by, name string) error
}

// Exposures is the exposures service the handlers call. The calls on one exposure name the network
// the path names; AllExposures is the readers' view of every network.
type Exposures interface {
	ApplyPool(ctx context.Context, by, name string, spec orchapi.AddressPoolSpec) (orchapi.AddressPool, error)
	Pools(ctx context.Context) ([]orchapi.AddressPool, error)
	DeletePool(ctx context.Context, by, name string) error
	ApplyExposure(ctx context.Context, by, network, id string, spec orchapi.ExposureSpec) (orchapi.Exposure, error)
	Exposure(ctx context.Context, network, id string) (orchapi.Exposure, error)
	AllExposures(ctx context.Context) ([]orchapi.Exposure, error)
	DeleteExposure(ctx context.Context, by, network, id string) error
	ApplyProxyExposure(ctx context.Context, by, id string, spec orchapi.ProxyExposureSpec) (orchapi.ProxyExposure, error)
	ProxyExposure(ctx context.Context, id string) (orchapi.ProxyExposure, error)
	ProxyExposures(ctx context.Context) ([]orchapi.ProxyExposure, error)
	DeleteProxyExposure(ctx context.Context, by, id string) error
}

// Machines is what the operator API reads of the Machine API's records.
type Machines interface {
	All(ctx context.Context) ([]orchapi.Machine, error)
	OperatorMachine(ctx context.Context, caller, name string) (orchapi.Machine, error)
	// Move records a move of the caller's Machine by the subject by, and CancelMove abandons it
	// before its commit.
	Move(ctx context.Context, by, caller, name string, req orchapi.MoveRequest) (orchapi.Machine, error)
	CancelMove(ctx context.Context, caller, name string) (orchapi.Machine, error)
	AbandonedVolumes(ctx context.Context) ([]orchapi.AbandonedVolumes, error)
	DeleteAbandonedVolumes(ctx context.Context, caller, name, host string, epoch uint64) error
	PutCaller(ctx context.Context, c machines.Caller) (machines.Caller, error)
	Callers(ctx context.Context) ([]machines.Caller, error)
}

// Deps is what the handlers call.
type Deps struct {
	Inventory inventory.Service
	Machines  Machines
	Groups    groupsvc.Service
	Networks  Networks
	// Exposures is nil while the member has no way to reach the hosts; its endpoints answer 501.
	Exposures Exposures
	Signer    inventory.Signer
	Auth      auth.Authenticator
	Snapshots Snapshots
	// Metrics counts the requests; nil counts none.
	Metrics *Metrics
	Log     *slog.Logger
}

// RestoreDeps is what a member that waits for a restore calls.
type RestoreDeps struct {
	Restorer Restorer
	Auth     auth.Authenticator
	Metrics  *Metrics
	Log      *slog.Logger
}

type handler struct {
	Deps
}

type ctxKey struct{}

// New returns the API's handler.
func New(d Deps) http.Handler {
	h := &handler{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hosts", h.guard(auth.Read, h.hosts))
	mux.HandleFunc("GET /v1/pending", h.guard(auth.Read, h.pending))
	mux.HandleFunc("GET /v1/events", h.guard(auth.Read, h.events))
	mux.HandleFunc("GET /v1/microvms", h.guard(auth.Read, h.microVMs))
	mux.HandleFunc("GET /v1/machines", h.guard(auth.Read, h.machines))
	mux.HandleFunc("GET /v1/machines/{caller}/{name}", h.guard(auth.Read, h.machine))
	mux.HandleFunc("GET /v1/machines/abandoned-volumes", h.guard(auth.Read, h.abandonedVolumes))
	mux.HandleFunc("DELETE /v1/machines/{caller}/{name}/abandoned-volumes/{host}/{epoch}", h.guard(auth.Attest, h.deleteAbandonedVolumes))
	mux.HandleFunc("POST /v1/machines/{caller}/{name}/move", h.guard(auth.Operate, h.moveMachine))
	mux.HandleFunc("DELETE /v1/machines/{caller}/{name}/move", h.guard(auth.Operate, h.cancelMove))
	mux.HandleFunc("GET /v1/callers", h.guard(auth.Read, h.callerList))
	mux.HandleFunc("PUT /v1/callers/{name}", h.guard(auth.Operate, h.callerApply))
	mux.HandleFunc("GET /v1/groups", h.guard(auth.Read, h.groupList))
	mux.HandleFunc("GET /v1/groups/{name}", h.guard(auth.Read, h.groupGet))
	mux.HandleFunc("PUT /v1/groups/{name}", h.guard(auth.Operate, h.groupApply))
	mux.HandleFunc("DELETE /v1/groups/{name}", h.guard(auth.Operate, h.groupDelete))
	mux.HandleFunc("GET /v1/networks", h.guard(auth.Read, h.networkList))
	mux.HandleFunc("GET /v1/networks/{name}", h.guard(auth.Read, h.networkGet))
	mux.HandleFunc("PUT /v1/networks/{name}", h.guard(auth.Operate, h.networkApply))
	mux.HandleFunc("DELETE /v1/networks/{name}", h.guard(auth.Operate, h.networkDelete))
	mux.HandleFunc("GET /v1/address-pools", h.guard(auth.Read, h.poolList))
	mux.HandleFunc("PUT /v1/address-pools/{name}", h.guard(auth.Operate, h.poolApply))
	mux.HandleFunc("DELETE /v1/address-pools/{name}", h.guard(auth.Operate, h.poolDelete))
	mux.HandleFunc("GET /v1/exposures", h.guard(auth.Read, h.exposureList))
	mux.HandleFunc("PUT /v1/networks/{name}/exposures/{id}", h.guard(auth.Operate, h.operatorExposureApply))
	mux.HandleFunc("GET /v1/networks/{name}/exposures/{id}", h.guard(auth.Read, h.operatorExposureGet))
	mux.HandleFunc("DELETE /v1/networks/{name}/exposures/{id}", h.guard(auth.Operate, h.operatorExposureDelete))
	mux.HandleFunc("GET /v1/proxy-exposures", h.guard(auth.Read, h.proxyExposureList))
	mux.HandleFunc("PUT /v1/proxy-exposures/{id}", h.guard(auth.Operate, h.proxyExposureApply))
	mux.HandleFunc("GET /v1/proxy-exposures/{id}", h.guard(auth.Read, h.proxyExposureGet))
	mux.HandleFunc("DELETE /v1/proxy-exposures/{id}", h.guard(auth.Operate, h.proxyExposureDelete))
	mux.HandleFunc("GET /v1/crl", h.crl)
	mux.HandleFunc("POST /v1/admit", h.guard(auth.Operate, h.admit))
	mux.HandleFunc("POST /v1/enrollment-keys", h.guard(auth.Operate, h.createKey))
	mux.HandleFunc("DELETE /v1/enrollment-keys/{id}", h.guard(auth.Operate, h.revokeKey))
	mux.HandleFunc("PUT /v1/hosts/{id}/labels", h.guard(auth.Operate, h.labels))
	mux.HandleFunc("POST /v1/hosts/{id}/drain", h.guard(auth.Operate, h.drain))
	mux.HandleFunc("POST /v1/hosts/{id}/undrain", h.guard(auth.Operate, h.undrain))
	mux.HandleFunc("POST /v1/hosts/{id}/abandon", h.guard(auth.Attest, h.abandon))
	mux.HandleFunc("DELETE /v1/hosts/{id}", h.guard(auth.Operate, h.removeHost))
	mux.HandleFunc("GET /v1/profile", h.guard(auth.Read, h.profile))
	mux.HandleFunc("PUT /v1/profile", h.guard(auth.Operate, h.setProfile))
	mux.HandleFunc("POST /v1/snapshot", h.guard(auth.Administer, h.snapshot))
	mux.HandleFunc("GET /v1/snapshots/latest", h.guard(auth.Administer, h.latestSnapshot))
	mux.HandleFunc("PUT /v1/restore", h.guard(auth.Administer, h.alreadyRestored))
	mux.HandleFunc("POST /v1/heartbeat", h.guard(auth.Report, h.heartbeat))
	// Enrollment is the one path without a client certificate: a key, or a proof of one, authenticates.
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	mux.HandleFunc("GET /v1/enroll/{hostID}", h.poll)
	return h.middleware(h.Metrics.count(mux))
}

// NewRestoring returns the handler of a member that holds no data and waits for a restore: PUT
// /v1/restore, with the same certificate check and root role as the full API, and 503 for the rest.
func NewRestoring(d RestoreDeps) http.Handler {
	h := &handler{Deps{Auth: d.Auth, Metrics: d.Metrics, Log: d.Log}}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/restore", h.guard(auth.Administer, func(w http.ResponseWriter, r *http.Request, who identity.Identity) {
		h.restore(d.Restorer, w, r, who)
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, orchapi.Error{Message: "this member waits for a restore"})
	})
	return h.middleware(h.Metrics.count(mux))
}

// TLSConfig is the server's configuration: the client's certificate is verified against clientCA
// when it sends one, and the handlers refuse a request that needs one and has none.
func TLSConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    clientCA,
	}
}

// middleware recovers a panic as a 500 and logs each request with a trace id.
func (h *handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id [8]byte
		_, _ = rand.Read(id[:])
		trace := hex.EncodeToString(id[:])
		log := h.Log.With("trace_id", trace, "method", r.Method, "path", r.URL.Path)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, log))
		w.Header().Set("X-Trace-Id", trace)
		defer func() {
			if v := recover(); v != nil {
				log.Error("handler panicked", "panic", v)
				writeJSON(w, http.StatusInternalServerError, orchapi.Error{Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func logger(r *http.Request) *slog.Logger { return r.Context().Value(ctxKey{}).(*slog.Logger) }

type authed func(w http.ResponseWriter, r *http.Request, who identity.Identity)

// guard authenticates the certificate and checks the endpoint's action.
func (h *handler) guard(a auth.Action, fn authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			writeJSON(w, http.StatusUnauthorized, orchapi.Error{Message: "a client certificate is required"})
			return
		}
		who, err := h.Auth.Authenticate(r.Context(), r.TLS.VerifiedChains)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !auth.Allowed(who, a) {
			logger(r).Warn("refused: role may not do this", "subject", who.Subject, "role", string(who.Role))
			writeJSON(w, http.StatusForbidden, orchapi.Error{Message: "the role may not do this"})
			return
		}
		fn(w, r, who)
	}
}

// tooLarge reports whether err is the body limit being hit, which is not a malformed body.
func tooLarge(err error) bool {
	var big *http.MaxBytesError
	return errors.As(err, &big)
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if tooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, orchapi.Error{Message: "the request body is too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, orchapi.Error{Message: "the request body is not valid"})
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if tooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, orchapi.Error{Message: "the request body is too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, orchapi.Error{Message: "the request body holds more than one value"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// storeFull reports whether err is etcd's answer to a write while the database is over its quota or its
// no-space alarm is armed.
func storeFull(err error) bool {
	return errors.Is(err, rpctypes.ErrNoSpace) || rpctypes.Error(err) == rpctypes.ErrNoSpace
}

// writeError is the one place an error becomes a status. What is not a known failure is logged
// and answered as a 500 without its detail.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, identity.ErrRevoked), errors.Is(err, identity.ErrRole), errors.Is(err, identity.ErrNoChain):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, identity.ErrNoRevocationList), errors.Is(err, identity.ErrStaleRevocationList):
		status, msg = http.StatusServiceUnavailable, "the revocation list is not available"
		logger(r).Error("no revocation list", "err", err.Error())
	case errors.Is(err, inventory.ErrAbandoned):
		status, msg = http.StatusGone, err.Error()
	case errors.Is(err, inventory.ErrRateLimited):
		status, msg = http.StatusTooManyRequests, err.Error()
	case errors.Is(err, inventory.ErrKeyRefused):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, inventory.ErrCodeMismatch), errors.Is(err, inventory.ErrSignerRefused):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, exposures.ErrForbidden):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, errNoExposures):
		status, msg = http.StatusNotImplemented, err.Error()
	case errors.Is(err, exposures.ErrUnprocessable):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, inventory.ErrNotFound), errors.Is(err, machines.ErrNotFound), errors.Is(err, machines.ErrNoMove):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, machines.ErrNotPlaced), errors.Is(err, machines.ErrMoving), errors.Is(err, machines.ErrDeleting),
		errors.Is(err, machines.ErrLost), errors.Is(err, machines.ErrNotLost), errors.Is(err, errAbandonedBound):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, machineapi.ErrInvalid):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, restore.ErrRejected):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, snapshot.ErrNoSnapshot):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, ErrRestored):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, inventory.ErrConflict), errors.Is(err, inventory.ErrTooManyPending),
		errors.Is(err, inventory.ErrPoolFull), errors.Is(err, inventory.ErrMeshPoolFull), errors.Is(err, inventory.ErrNoProfile), errors.Is(err, inventory.ErrPrefixUnrecorded):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, inventory.ErrRedacted):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	case tooLarge(err):
		status, msg = http.StatusRequestEntityTooLarge, "the request body is too large"
	case errors.Is(err, inventory.ErrInvalid), errors.Is(err, orchapi.ErrBadProof), errors.Is(err, machineapi.ErrInvalid):
		status, msg = http.StatusBadRequest, err.Error()
	case storeFull(err):
		status, msg = http.StatusInsufficientStorage, "orch's database is full (etcdserver: mvcc: database space exceeded): it refuses writes until the housekeeper has compacted and defragmented it and cleared the no-space alarm"
		logger(r).Error("the database is full", "err", err.Error())
	case errors.Is(err, inventory.ErrSignerFailed):
		status, msg = http.StatusBadGateway, "the signer failed"
		logger(r).Error("signer failed", "err", err.Error())
	default:
		logger(r).Error("request failed", "err", err.Error())
	}
	writeJSON(w, status, orchapi.Error{Message: msg})
}

func (h *handler) hosts(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Hosts(r.Context())
	reply(w, r, out, err)
}

// machines lists every caller's Machines, with no secret of any.
func (h *handler) machines(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Machines.All(r.Context())
	reply(w, r, out, err)
}

func (h *handler) machine(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Machines.OperatorMachine(r.Context(), r.PathValue("caller"), r.PathValue("name"))
	reply(w, r, out, err)
}

// moveMachine records an operator's move; an empty body moves the Machine within its own shape.
func (h *handler) moveMachine(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.MoveRequest
	// A body of no bytes, of a known length or chunked, is a move within the Machine's own shape.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !decode(w, r, &req) {
			return
		}
	}
	out, err := h.Machines.Move(r.Context(), who.Subject, r.PathValue("caller"), r.PathValue("name"), req)
	reply(w, r, out, err)
}

func (h *handler) abandonedVolumes(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Machines.AbandonedVolumes(r.Context())
	reply(w, r, out, err)
}

func (h *handler) deleteAbandonedVolumes(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	epoch, err := strconv.ParseUint(r.PathValue("epoch"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, orchapi.Error{Message: "the epoch is not a number"})
		return
	}
	if err := h.Machines.DeleteAbandonedVolumes(r.Context(), r.PathValue("caller"), r.PathValue("name"), r.PathValue("host"), epoch); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) cancelMove(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Machines.CancelMove(r.Context(), r.PathValue("caller"), r.PathValue("name"))
	reply(w, r, out, err)
}

// callerList shows the operator's grants to the Machine API's callers.
func (h *handler) callerList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	cs, err := h.Machines.Callers(r.Context())
	out := make([]orchapi.Caller, 0, len(cs))
	for _, c := range cs {
		out = append(out, orchapi.Caller{Name: c.Name, AliasDomains: c.AliasDomains})
	}
	reply(w, r, out, err)
}

// callerApply replaces a caller's grant of alias domains (ADR 0112 item 3).
func (h *handler) callerApply(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	var spec orchapi.CallerSpec
	if decode(w, r, &spec) {
		c, err := h.Machines.PutCaller(r.Context(), machines.Caller{Name: r.PathValue("name"), AliasDomains: spec.AliasDomains})
		reply(w, r, orchapi.Caller{Name: c.Name, AliasDomains: c.AliasDomains}, err)
	}
}

func (h *handler) pending(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Pending(r.Context())
	reply(w, r, out, err)
}

func (h *handler) events(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Events(r.Context(), r.URL.Query().Get("since"))
	reply(w, r, out, err)
}

// crl answers without a client certificate: the list is signed by the revocation key and holds
// only serial numbers, so it is public, and a host that has none of the certificates orch accepts
// needs it to check the ones it is offered (ADR 0062).
func (h *handler) crl(w http.ResponseWriter, r *http.Request) {
	out, err := h.Signer.CRL(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (h *handler) admit(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.AdmitRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.Admit(r.Context(), who.Subject, req)
	reply(w, r, out, err)
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.CreateKeyRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.CreateKey(r.Context(), who.Subject, req.ExpiresAt)
	reply(w, r, out, err)
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Inventory.RevokeKey(r.Context(), who.Subject, r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) labels(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.SetLabelsRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.SetLabels(r.Context(), who.Subject, r.PathValue("id"), req)
	reply(w, r, out, err)
}

func (h *handler) drain(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	out, err := h.Inventory.Drain(r.Context(), who.Subject, r.PathValue("id"))
	reply(w, r, out, err)
}

// abandon records an operator's abandonment of a fenced host. A revocation the signer refused or
// failed is answered as the error, with the abandonment recorded.
func (h *handler) abandon(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.AbandonRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.Abandon(r.Context(), who.Subject, r.PathValue("id"), req)
	reply(w, r, out, err)
}

// abandonedBound refuses the removal of an abandoned host while any Machine is bound to it. No
// Machine is bound to an abandoned host after the check: placement and a move's target leave it
// out, guarded on its abandonment.
func (h *handler) abandonedBound(ctx context.Context, id string) error {
	hosts, err := h.Inventory.Hosts(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(hosts, func(x orchapi.Host) bool { return x.ID == id })
	if i < 0 || hosts[i].Abandonment == nil || h.Machines == nil {
		return nil
	}
	ms, err := h.Machines.All(ctx)
	if err != nil {
		return err
	}
	var bound []string
	for _, m := range ms {
		if m.Binding != nil && m.Binding.Host == id {
			bound = append(bound, m.Caller+"/"+m.Name)
		}
	}
	if len(bound) > 0 {
		return fmt.Errorf("%w: %s; move each with --abandon-volumes or delete it first", errAbandonedBound, strings.Join(bound, ", "))
	}
	return nil
}

func (h *handler) undrain(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	out, err := h.Inventory.Undrain(r.Context(), who.Subject, r.PathValue("id"))
	reply(w, r, out, err)
}

// errAbandonedBound is a removal of an abandoned host a Machine is still bound to: the record of
// its abandonment is what marks that Machine lost until an operator moves or deletes it.
var errAbandonedBound = errors.New("the host is abandoned and a Machine is still bound to it")

func (h *handler) removeHost(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.abandonedBound(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.Inventory.Remove(r.Context(), who.Subject, r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) profile(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	b, err := h.Inventory.Profile(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orchapi.HostProfile{Profile: b})
}

func (h *handler) setProfile(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.HostProfile
	if !decode(w, r, &req) {
		return
	}
	if err := h.Inventory.SetProfile(r.Context(), who.Subject, req.Profile); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) microVMs(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.MicroVMs(r.Context())
	reply(w, r, out, err)
}

func (h *handler) groupList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.List(r.Context())
	if err == nil {
		err = h.withHold(r.Context(), out)
	}
	reply(w, r, out, err)
}

func (h *handler) groupGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.Get(r.Context(), r.PathValue("name"))
	if err == nil {
		one := []orchapi.Group{out}
		if err = h.withHold(r.Context(), one); err == nil {
			out = one[0]
		}
	}
	reply(w, r, out, err)
}

func (h *handler) groupApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var spec orchapi.GroupSpec
	if !decode(w, r, &spec) {
		return
	}
	name := r.PathValue("name")
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.Name != name {
		writeError(w, r, fmt.Errorf("%w: the body names group %q and the path %q", inventory.ErrInvalid, spec.Name, name))
		return
	}
	out, err := h.Groups.Apply(r.Context(), who.Subject, spec)
	reply(w, r, out, err)
}

func (h *handler) groupDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Groups.Delete(r.Context(), who.Subject, r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) networkList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Networks.List(r.Context())
	reply(w, r, out, err)
}

func (h *handler) networkGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Networks.Get(r.Context(), r.PathValue("name"))
	reply(w, r, out, err)
}

func (h *handler) networkApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var spec orchapi.NetworkSpec
	if !decode(w, r, &spec) {
		return
	}
	out, err := h.Networks.Apply(r.Context(), who.Subject, r.PathValue("name"), spec)
	reply(w, r, out, err)
}

func (h *handler) networkDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Networks.Delete(r.Context(), who.Subject, r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errNoExposures is the answer of a member that has no way to reach the hosts.
var errNoExposures = errors.New("this member serves no exposures")

func (h *handler) exposures() (Exposures, error) {
	if h.Exposures == nil {
		return nil, errNoExposures
	}
	return h.Exposures, nil
}

func (h *handler) poolList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := svc.Pools(r.Context())
	reply(w, r, out, err)
}

func (h *handler) poolApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	var spec orchapi.AddressPoolSpec
	if decode(w, r, &spec) {
		out, err := svc.ApplyPool(r.Context(), who.Subject, r.PathValue("name"), spec)
		reply(w, r, out, err)
	}
}

func (h *handler) poolDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := svc.DeletePool(r.Context(), who.Subject, r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exposureList shows a reader every network's exposures.
func (h *handler) exposureList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := svc.AllExposures(r.Context())
	reply(w, r, out, err)
}

// operatorExposureApply makes or changes the operator's exposure of the network the path names.
func (h *handler) operatorExposureApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	var spec orchapi.ExposureSpec
	if decode(w, r, &spec) {
		out, err := svc.ApplyExposure(r.Context(), who.Subject, r.PathValue("name"), r.PathValue("id"), spec)
		reply(w, r, out, err)
	}
}

// operatorExposureGet shows one exposure of the network the path names, of whichever owner.
func (h *handler) operatorExposureGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := svc.Exposure(r.Context(), r.PathValue("name"), r.PathValue("id"))
	reply(w, r, out, err)
}

// operatorExposureDelete releases an exposure of the network the path names, of whichever owner: an
// operator's own, and one an exposer stored before ADR 0114.
func (h *handler) operatorExposureDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := svc.DeleteExposure(r.Context(), who.Subject, r.PathValue("name"), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proxyExposureList shows a reader the proxy exposures (ADR 0112 item 8).
func (h *handler) proxyExposureList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := svc.ProxyExposures(r.Context())
	reply(w, r, out, err)
}

// proxyExposureApply makes a proxy exposure or changes its ports.
func (h *handler) proxyExposureApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	var spec orchapi.ProxyExposureSpec
	if decode(w, r, &spec) {
		out, err := svc.ApplyProxyExposure(r.Context(), who.Subject, r.PathValue("id"), spec)
		reply(w, r, out, err)
	}
}

// proxyExposureGet shows one proxy exposure.
func (h *handler) proxyExposureGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := svc.ProxyExposure(r.Context(), r.PathValue("id"))
	reply(w, r, out, err)
}

// proxyExposureDelete releases a proxy exposure.
func (h *handler) proxyExposureDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	svc, err := h.exposures()
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := svc.DeleteProxyExposure(r.Context(), who.Subject, r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) snapshot(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Snapshots.Take(r.Context())
	reply(w, r, out, err)
}

func (h *handler) latestSnapshot(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	sealed, err := h.Snapshots.Latest()
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(sealed)
}

// restore streams the body to the restorer. The server's read timeout is lifted for it: a snapshot
// is larger than any other body.
func (h *handler) restore(rs Restorer, w http.ResponseWriter, r *http.Request, who identity.Identity) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(10 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))
	logger(r).Info("restoring a snapshot", "subject", who.Subject)
	if err := rs.Restore(r.Context(), http.MaxBytesReader(w, r.Body, maxRestore), r.Header.Get(orchapi.HeaderSnapshotSHA256)); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// alreadyRestored answers a restore sent to a member that runs: restoring replaces nothing.
func (h *handler) alreadyRestored(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	writeError(w, r, ErrRestored)
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Inventory.Heartbeat(r.Context(), who.Subject); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) enroll(w http.ResponseWriter, r *http.Request) {
	var req orchapi.EnrollRequest
	if !decode(w, r, &req) {
		return
	}
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		src = r.RemoteAddr
	}
	out, err := h.Inventory.Enroll(r.Context(), src, req)
	reply(w, r, out, err)
}

func (h *handler) poll(w http.ResponseWriter, r *http.Request) {
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		src = r.RemoteAddr
	}
	out, err := h.Inventory.Poll(r.Context(), src, r.PathValue("hostID"), r.Header.Get(orchapi.HeaderTime), r.Header.Get(orchapi.HeaderSignature))
	reply(w, r, out, err)
}

func reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// The longest a request's body may take to arrive, and its answer to be written, on orch's API.
const (
	ReadTimeout  = 30 * time.Second
	WriteTimeout = 60 * time.Second
)

// Serve runs h on l, a listener that already speaks TLS, until ctx ends: a request's body must
// arrive within read and its answer be written within write.
func Serve(ctx context.Context, l net.Listener, h http.Handler, read, write time.Duration) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: read, WriteTimeout: write, IdleTimeout: 2 * time.Minute}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(done)
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
