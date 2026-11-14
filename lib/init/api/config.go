package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
)

// redacted stands in GET /v1/config for private material.
const redacted = "REDACTED"

// maxApplyBody bounds a request body: a configuration carries files and certificates.
const maxApplyBody = 8 << 20

// errNotApplying is what POST /v1/config answers while no engine is attached: the boot has
// not finished, or it was refused.
var errNotApplying = errors.New("the Node is not applying changes: its boot has not finished or was refused")

// config returns the revision and configuration the Node holds.
func (s *Server) config() (int, config.NodeConfig) {
	s.mu.Lock()
	engine, chosen := s.engine, s.chosen
	s.mu.Unlock()
	if engine != nil {
		return engine.Config()
	}
	return chosen.Revision, chosen.Config
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	revision, cfg := s.config()
	info, err := s.versionInfo()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the boot ID cannot be read"})
		return
	}
	doc, err := configDocument(redact(cfg), s.status(revision, info))
	if err != nil {
		s.deps.Log.Error("encode the configuration", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the configuration cannot be encoded"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Revision int `json:"revision"`
		Config   any `json:"config"`
	}{revision, doc})
}

// status is what the Node observes of itself: its version, boot, slot, last upgrade, the
// revision of the configuration and each service's state and restarts.
func (s *Server) status(revision int, info versionBody) config.Status {
	st := config.Status{Version: info.Version, BootID: info.BootID, Slot: info.Slot, Revision: revision, Services: []config.ServiceStatus{}}
	if l := info.LastUpgrade; l != nil {
		st.LastUpgrade = &config.UpgradeStatus{From: l.From, To: l.To, Result: l.Result, Reason: l.Reason, Time: l.Time}
	}
	for _, b := range s.serviceList() {
		st.Services = append(st.Services, config.ServiceStatus{Name: b.Name, State: b.State, Restarts: b.Restarts})
	}
	return st
}

// configDocument is the one document GET /v1/config returns: the configuration and, beside its
// spec, the status.
func configDocument(cfg config.NodeConfig, st config.Status) (any, error) {
	doc, err := document(cfg)
	if err != nil {
		return nil, err
	}
	stDoc, err := document(st)
	if err != nil {
		return nil, err
	}
	doc.(map[string]any)[config.StatusKey] = stDoc
	return doc, nil
}

// redact returns cfg with private material replaced by REDACTED: spec.api.key, each
// spec.pki.authorities[].key, each spec.registries[].credential.password, spec.host.orch.enrollKey, and the content of each spec.files entry that gives group and
// other no read. A field that is empty holds no material and is left empty.
func redact(cfg config.NodeConfig) config.NodeConfig {
	if a := cfg.Spec.API; a != nil {
		c := *a
		if c.Key != "" {
			c.Key = redacted
		}
		cfg.Spec.API = &c
	}
	cfg.Spec.PKI.Authorities = slices.Clone(cfg.Spec.PKI.Authorities)
	for i := range cfg.Spec.PKI.Authorities {
		if a := &cfg.Spec.PKI.Authorities[i]; a.Key != "" {
			a.Key = redacted
		}
	}
	cfg.Spec.Registries = slices.Clone(cfg.Spec.Registries)
	for i := range cfg.Spec.Registries {
		if c := cfg.Spec.Registries[i].Credential; c != nil && c.Password != "" {
			c := *c
			c.Password = redacted
			cfg.Spec.Registries[i].Credential = &c
		}
	}
	if h := cfg.Spec.Host; h != nil && h.Orch != nil && h.Orch.EnrollKey != "" {
		h, o := *h, *h.Orch
		o.EnrollKey = redacted
		h.Orch = &o
		cfg.Spec.Host = &h
	}
	cfg.Spec.Files = slices.Clone(cfg.Spec.Files)
	for i := range cfg.Spec.Files {
		if f := &cfg.Spec.Files[i]; f.Perm()&0o044 == 0 && f.Content != "" {
			f.Content = redacted
		}
	}
	return cfg
}

// document returns v as the generic document its YAML form decodes to, which JSON encodes
// with the field names a patch uses.
func document(v any) (any, error) {
	data, err := yaml.Dump(v)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Load(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// applyRequest is the body of POST /v1/config.
type applyRequest struct {
	Revision *int           `json:"revision"`
	Patch    map[string]any `json:"patch"`
	Config   map[string]any `json:"config"`
	Mode     string         `json:"mode"`
	DryRun   bool           `json:"dryRun"`
}

type actionBody struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

func actionBodies(actions []apply.Action) []actionBody {
	out := make([]actionBody, 0, len(actions))
	for _, a := range actions {
		out = append(out, actionBody{a.Action, a.Target})
	}
	return out
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	var body applyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApplyBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil && dec.More() {
		err = errors.New("the body holds more than one JSON value")
	}
	if err == nil && body.Revision == nil {
		err = errors.New("revision is missing")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "the body is not an apply request: " + err.Error()})
		return
	}

	for _, doc := range []map[string]any{body.Patch, body.Config} {
		if _, named := doc[config.StatusKey]; named {
			s.writeApplyError(w, &apply.InvalidError{Fields: []string{config.StatusKey}, Message: "status is observed, not declared: no apply may name it"})
			return
		}
	}

	s.mu.Lock()
	engine := s.engine
	s.mu.Unlock()
	if engine == nil {
		s.writeApplyError(w, errNotApplying)
		return
	}
	res, err := engine.Apply(apply.Request{
		Revision: *body.Revision, Patch: body.Patch, Config: body.Config,
		Mode: apply.Mode(body.Mode), DryRun: body.DryRun,
	})
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	s.reply(w, http.StatusOK, struct {
		Revision int          `json:"revision"`
		Mode     apply.Mode   `json:"mode"`
		DryRun   bool         `json:"dryRun"`
		Actions  []actionBody `json:"actions"`
	}{res.Revision, res.Mode, res.DryRun, actionBodies(res.Actions)}, res.Reboot)
}

// writeApplyError is the one place an apply's error becomes a status: 409 for a stale
// revision, 422 for an invalid request, and 500 for a failure.
func (s *Server) writeApplyError(w http.ResponseWriter, err error) {
	var stale *apply.StaleError
	var invalid *apply.InvalidError
	var failed *apply.FailedError
	switch {
	case errors.As(err, &stale):
		writeJSON(w, http.StatusConflict, struct {
			errorBody
			Revision int `json:"revision"`
		}{errorBody{Error: stale.Error()}, stale.Stored})
	case errors.As(err, &invalid):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: invalid.Error(), Fields: invalid.Fields})
	case errors.As(err, &failed):
		s.deps.Log.Error("apply failed after the revision was stored", "revision", failed.Revision, "action", failed.Failed.Action, "target", failed.Failed.Target, "err", failed.Err)
		writeJSON(w, http.StatusInternalServerError, struct {
			errorBody
			Revision int          `json:"revision"`
			Actions  []actionBody `json:"actions"`
			Failed   actionBody   `json:"failed"`
		}{errorBody{Error: failed.Error()}, failed.Revision, actionBodies(failed.Done), actionBody{failed.Failed.Action, failed.Failed.Target}})
	default:
		s.deps.Log.Error("apply failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}
