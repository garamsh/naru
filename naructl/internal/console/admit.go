package console

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

const maxFormBytes = 16 << 10

// admit signs an admission for a pending host and posts it to orch. The key it signs the admission
// of is the one orch lists for the host, never one the form carries, and the operator must have typed
// that key's pairing code.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOK(r.PostForm.Get("csrf")) {
		http.Error(w, "bad CSRF token", http.StatusForbidden)
		return
	}
	p := page{Title: "Pending hosts", Active: "pending", Region: strings.TrimSpace(r.PostForm.Get("region")), Labels: r.PostForm.Get("labels")}
	refuse := func(status int, err error) {
		p.Error = err.Error()
		s.renderPending(w, r, status, p)
	}

	hostID := r.PostForm.Get("host")
	labels, err := parseLabels(p.Labels)
	if err != nil {
		refuse(http.StatusUnprocessableEntity, err)
		return
	}
	if p.Region == "" {
		refuse(http.StatusUnprocessableEntity, errors.New("a region is required"))
		return
	}
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	list, err := s.cfg.Orch.Pending(ctx)
	if err != nil {
		s.fail(w, "pending", p, err)
		return
	}
	var host *orchapi.Pending
	for i := range list {
		if list[i].HostID == hostID {
			host = &list[i]
		}
	}
	if host == nil {
		refuse(http.StatusNotFound, fmt.Errorf("%w: %q", ErrNoPending, hostID))
		return
	}
	code := normalizeCode(r.PostForm.Get("code"))
	if code != identity.PairingCode(host.PublicKey) {
		refuse(http.StatusUnprocessableEntity, ErrCodeMismatch)
		return
	}
	sum := sha256.Sum256(host.PublicKey)
	stmt, err := s.cfg.Sign(identity.Admission{
		HostID:        host.HostID,
		HostKeySHA256: hex.EncodeToString(sum[:]),
		Region:        p.Region,
		Labels:        labels,
		ExpiresAt:     s.cfg.Now().Add(s.cfg.AdmissionTTL),
	})
	if err != nil {
		s.cfg.Log.Error("sign admission", "host", host.HostID, "err", err)
		refuse(http.StatusInternalServerError, errors.New("signing the admission failed"))
		return
	}
	_, err = s.cfg.Orch.Admit(ctx, orchapi.AdmitRequest{HostID: host.HostID, Code: code, Statement: stmt, Region: p.Region, Labels: labels})
	if err != nil {
		status := http.StatusBadGateway
		var oe *orchapi.Error
		if errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500 {
			status = oe.Status
		}
		s.cfg.Log.Warn("admit", "host", host.HostID, "err", err)
		refuse(status, err)
		return
	}
	s.cfg.Log.Info("admitted", "host", host.HostID, "region", p.Region)
	http.Redirect(w, r, "/pending?admitted="+url.QueryEscape(host.HostID), http.StatusSeeOther)
}

// normalizeCode puts what an operator typed in the form PairingCode writes: upper case, with the
// dash where it goes.
func normalizeCode(typed string) string {
	c := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(typed))
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

// parseLabels reads key=value pairs, one per line or separated by commas.
func parseLabels(text string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ',' }) {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("label %q is not key=value", strings.TrimSpace(f))
		}
		out[k] = v
	}
	return out, nil
}
