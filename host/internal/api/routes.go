package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/hostapi"
)

// maxRoutesBody bounds a table: every route of the fleet, each alias's pair among them.
const maxRoutesBody = 64 << 20

// maxRevisionFile bounds the proxy's revision file, a decimal int64 and a newline.
const maxRevisionFile = 32

// Routes is where the host hands orch's routing table to its naru-proxy (ADR 0112 item 5): the
// routes file hostd writes, in the proxy's group, and the revision file the proxy writes back.
type Routes struct {
	// File is the routes file, written mode 0640; GID is its group, and a negative GID leaves the
	// file in hostd's own.
	File string
	GID  int
	// RevisionFile is where the proxy writes the revision it has loaded.
	RevisionFile string
	// MaxAge is the proxy's --route-max-age, which GET /v1/host reports.
	MaxAge time.Duration

	mu sync.Mutex
	// taken is the revision of the last table written, read from File at the first push.
	taken  int64
	loaded bool
}

// lastTaken is the revision of the table in File, 0 when there is none that parses: a restarted
// hostd refuses a lower one than it took before.
func (rt *Routes) lastTaken() int64 {
	if !rt.loaded {
		rt.loaded = true
		var f hostapi.RoutesFile
		if b, err := os.ReadFile(rt.File); err == nil && json.Unmarshal(b, &f) == nil {
			rt.taken = f.Revision
		}
	}
	return rt.taken
}

// proxyLoaded is the revision in the proxy's revision file, 0 while it has written none. The file
// is in the proxy's directory, so it is opened without following a link and read to a bound.
func (rt *Routes) proxyLoaded() (int64, error) {
	f, err := os.OpenFile(rt.RevisionFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRevisionFile+1))
	if err != nil {
		return 0, err
	}
	if len(b) > maxRevisionFile {
		return 0, errors.New("the revision file is longer than a revision")
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// putRoutes takes the table orch's leader pushes and writes it for the proxy, with the time the
// host took it and its mesh address, and answers the revision the proxy has loaded. A host that
// runs no proxy is 501. A route the proxy would drop is 422, and so is a name given twice. A table
// of a lower revision than one taken is 409, and so is one sent more than the allowance before it
// arrived by this host's clock, whose answer reports the difference, so a clock off by more than
// the skew allowed is seen in orch's log.
func (s *Server) putRoutes(w http.ResponseWriter, r *http.Request) {
	rt := s.deps.Routes
	if rt == nil {
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host runs no proxy"})
		return
	}
	var req hostapi.RoutesRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRoutesBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body is not one JSON object of the fields the API names"})
		return
	}
	if fields := checkTable(req); len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	now := s.now()
	if late := now.Sub(req.SentAt); late > hostapi.RouteAllowance {
		s.deps.Log.Warn("refused a routing table sent too long before it arrived; orch's clock and this host's may differ",
			"revision", req.Revision, "sent_at", req.SentAt, "late", late.String(), "allowed", hostapi.RouteAllowance.String())
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Code: hostapi.CodeLateTable,
			Error: fmt.Sprintf("the table was sent %s before it arrived by this host's clock, more than the %s allowed (a %s push and %s of skew)",
				late.Round(time.Millisecond), hostapi.RouteAllowance, hostapi.MaxRouteDelay, hostapi.MaxRouteSkew)})
		return
	}
	var mesh netip.Addr
	if s.deps.Networks != nil {
		mesh = s.deps.Networks.Mesh()
	}
	rt.mu.Lock()
	if taken := rt.lastTaken(); req.Revision < taken {
		rt.mu.Unlock()
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Code: hostapi.CodeStaleTable,
			Error: fmt.Sprintf("the table's revision %d is below %d, which this host took", req.Revision, taken)})
		return
	}
	if req.Routes == nil {
		req.Routes = []hostapi.Route{}
	}
	b, err := json.Marshal(hostapi.RoutesFile{RoutesRequest: req, ReceivedAt: now.UTC(), MeshAddress: mesh})
	if err == nil {
		var opts []atomicfile.Option
		if rt.GID >= 0 {
			opts = append(opts, atomicfile.Owner(os.Getuid(), rt.GID))
		}
		err = atomicfile.Write(rt.File, b, 0o640, opts...)
	}
	if err == nil {
		rt.taken = req.Revision
	}
	rt.mu.Unlock()
	if err != nil {
		s.deps.Log.Error("cannot write the routes file", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "the routes file cannot be written"})
		return
	}
	loaded, err := rt.proxyLoaded()
	if err != nil {
		s.deps.Log.Warn("cannot read the proxy's revision file", "err", err.Error())
	}
	writeJSON(w, http.StatusOK, hostapi.RoutesAnswer{Loaded: loaded})
}

// checkTable names each route a proxy would drop, by Route.Check, and each name given twice.
func checkTable(req hostapi.RoutesRequest) []string {
	var fields []string
	seen := make(map[string]bool, len(req.Routes))
	for i, rte := range req.Routes {
		if _, err := rte.Check(); err != nil {
			fields = append(fields, fmt.Sprintf("routes[%d]: %s", i, err))
		}
		if seen[rte.Hostname] {
			fields = append(fields, fmt.Sprintf("routes[%d].hostname: %s is named twice", i, rte.Hostname))
		}
		seen[rte.Hostname] = true
	}
	return fields
}
