// Package apply changes a running Node's configuration: it merges a patch into
// the stored configuration, validates the result, stores it as the next
// revision and carries the change out on the running Node
// (docs/architecture/adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md).
package apply

import (
	"sync"

	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/store"
)

// Mode is how a change reaches the Node.
type Mode string

const (
	// NoReboot applies the change live, and refuses what needs a reboot.
	NoReboot Mode = "no-reboot"
	// Staged stores the change for the next boot and takes no live action.
	Staged Mode = "staged"
	// Reboot stores the change, as Staged does, and has the caller reboot the Node.
	Reboot Mode = "reboot"
)

// Request is one call to Apply.
type Request struct {
	// Revision is the revision the request was made against.
	Revision int
	// Patch is a merge patch with keyed lists, decoded from JSON. Exactly one
	// of Patch and Config is non-nil.
	Patch map[string]any
	// Config replaces the configuration whole, decoded from JSON.
	Config map[string]any
	Mode   Mode
	// DryRun lists the actions and does none of them, the store write included.
	DryRun bool
}

// Action is one thing an apply does: store, start, stop, restart,
// write-file, issue-certificate, forget-certificate, reload-api, sync-time,
// stop-time-sync or reboot.
type Action struct {
	Action string
	// Target is the service, the path, the certificate name, spec.api, or
	// config for the store, node for the reboot.
	Target string
}

// Result is what a successful Apply did.
type Result struct {
	// Revision is the revision stored after the call, which is the one before it for a dry run.
	Revision int
	Mode     Mode
	DryRun   bool
	Actions  []Action
	// Reboot is true when the caller is to reboot the Node now: the mode is Reboot, the change is
	// stored and the request is not a dry run.
	Reboot bool
}

// Store writes the next revision.
type Store interface {
	Save(store.Stored) error
}

// Services starts, stops and restarts the Node's services.
type Services interface {
	Start(config.Service) error
	Stop(name string)
	Restart(name string)
}

// Files replaces a file of spec.files that is already written.
type Files interface {
	ReplaceFile(config.File) error
}

// Time points the clock at the time servers.
type Time interface {
	// ConfigureTime has the clock follow servers, each a host:port, and
	// synchronise at once in the background, or follow none when servers is empty.
	ConfigureTime(servers []string)
}

// Routes adds and removes the static routes of spec.network.routes in the kernel's table.
type Routes interface {
	AddRoute(config.Route) error
	DeleteRoute(config.Route) error
}

// Certificates issues the named certificates of spec again and keeps renewing
// the certificates of spec.
type Certificates interface {
	Reissue(spec config.PKI, names []string) error
}

// API is the Node API's listener.
type API interface {
	// Reload makes the listener use the pair and the client CA of a.
	Reload(a config.API) error
}

// Deps is what the engine acts through.
type Deps struct {
	Store        Store
	Services     Services
	Files        Files
	Time         Time
	Routes       Routes
	Certificates Certificates
	API          API
}

// Engine applies changes to one Node. Its methods are safe to call from
// several goroutines: applies run one at a time.
type Engine struct {
	deps Deps

	mu      sync.Mutex
	stored  store.Stored
	running config.NodeConfig
}

// New returns an Engine for a Node that booted stored, which is therefore also
// what runs.
func New(deps Deps, stored store.Stored) *Engine {
	return &Engine{deps: deps, stored: stored, running: stored.Config}
}

// Config returns the stored configuration and its revision.
func (e *Engine) Config() (revision int, cfg config.NodeConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stored.Revision, e.stored.Config
}

// Apply merges the request into the stored configuration, validates the
// result and, unless the request is a dry run, stores it as the next revision
// and carries it out. It returns *StaleError, *InvalidError, *StoreError or,
// when an action fails after the store, *FailedError. Every error but the last
// leaves the Node unchanged.
func (e *Engine) Apply(req Request) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if req.Mode != NoReboot && req.Mode != Staged && req.Mode != Reboot {
		return Result{}, &InvalidError{Fields: []string{"mode"}, Message: `must be "no-reboot", "staged" or "reboot"`}
	}
	if (req.Patch == nil) == (req.Config == nil) {
		return Result{}, &InvalidError{Fields: []string{"patch", "config"}, Message: "exactly one must be given"}
	}
	if req.Revision != e.stored.Revision {
		return Result{}, &StaleError{Stored: e.stored.Revision, Requested: req.Revision}
	}
	next, err := e.result(req)
	if err != nil {
		return Result{}, err
	}
	if err := e.refuse(next, req.Mode); err != nil {
		return Result{}, err
	}

	steps := []step{{action: Action{"store", "config"}}}
	if req.Mode == NoReboot {
		steps = append(steps, e.plan(next)...)
	}
	res := Result{Revision: e.stored.Revision, Mode: req.Mode, DryRun: req.DryRun}
	for _, s := range steps {
		res.Actions = append(res.Actions, s.action)
	}
	if req.Mode == Reboot {
		res.Actions = append(res.Actions, Action{"reboot", "node"})
	}
	if req.DryRun {
		return res, nil
	}

	st := store.Stored{Revision: e.stored.Revision + 1, BaseSha256: e.stored.BaseSha256, Config: next}
	if err := e.deps.Store.Save(st); err != nil {
		return Result{}, &StoreError{Err: err}
	}
	e.stored = st
	res.Revision = st.Revision
	for i, s := range steps[1:] {
		if err := s.do(); err != nil {
			return Result{}, &FailedError{Revision: st.Revision, Done: res.Actions[:i+1], Failed: s.action, Err: err}
		}
	}
	if req.Mode == NoReboot {
		e.running = next
	}
	res.Reboot = req.Mode == Reboot
	return res, nil
}

// result returns the configuration the request asks for, validated.
func (e *Engine) result(req Request) (config.NodeConfig, error) {
	doc := any(req.Config)
	if req.Patch != nil {
		current, err := document(e.stored.Config)
		if err != nil {
			return config.NodeConfig{}, err
		}
		if doc, err = merge(current, req.Patch, ""); err != nil {
			return config.NodeConfig{}, err
		}
	}
	data, err := yaml.Dump(doc)
	if err != nil {
		return config.NodeConfig{}, &InvalidError{Fields: []string{"config"}, Message: err.Error()}
	}
	next, err := config.Parse(data)
	if err != nil {
		fields := config.Fields(err)
		if len(fields) == 0 {
			fields = []string{"config"}
		}
		return config.NodeConfig{}, &InvalidError{Fields: fields, Message: err.Error()}
	}
	return next, nil
}

// document returns cfg as a decoded YAML document, which a patch merges into.
func document(cfg config.NodeConfig) (map[string]any, error) {
	data, err := yaml.Dump(cfg)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Load(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}
