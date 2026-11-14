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
	doc, err := document(redact(cfg))
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

// redact returns cfg with private material replaced by REDACTED: spec.api.key, each
// spec.pki.authorities[].key, each spec.registries[].credential.password, and the content of each spec.files entry that gives group and
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
	cfg.Spec.Files = slices.Clone(cfg.Spec.Files)
	for i := range cfg.Spec.Files {
		if f := &cfg.Spec.Files[i]; f.Perm()&0o044 == 0 && f.Content != "" {
			f.Content = redacted
		}
	}
	return cfg
}

// document returns cfg as the generic document its YAML form decodes to, which JSON encodes
// with the field names a patch uses.
func document(cfg config.NodeConfig) (any, error) {
	data, err := yaml.Dump(cfg)
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
