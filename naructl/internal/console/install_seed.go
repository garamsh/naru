package console

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/naructl/internal/seed"
)

// defaultSeedTTL is what the install-seed form offers as the enrollment key's lifetime.
const defaultSeedTTL = "24h"

// seedFilename is the name the browser saves the ISO under.
const seedFilename = "naru-install-seed.iso"

// installSeed shows the form that makes an install seed.
func (s *Server) installSeed(w http.ResponseWriter, r *http.Request) {
	p := s.seedPage()
	p.TTL = defaultSeedTTL
	s.render(w, http.StatusOK, "install-seed", p)
}

func (s *Server) seedPage() page {
	return page{
		Title: "Install seed", Active: "install-seed", CSRF: s.csrf,
		NoSeed:   s.cfg.OrchURL == "" || s.cfg.OrchCASHA256 == "",
		SeedOrch: s.cfg.OrchURL,
	}
}

// makeSeed creates an enrollment key in orch as the operator and answers with the ISO that carries
// it. The key is in the ISO and nowhere on a page: orch keeps only its hash, and what the browser
// saves is the only copy. Creating the key is the operator's act, and orch logs it as one.
func (s *Server) makeSeed(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !s.csrfOK(r.PostForm.Get("csrf")) {
		http.Error(w, "bad CSRF token", http.StatusForbidden)
		return
	}
	p := s.seedPage()
	p.TTL = strings.TrimSpace(r.PostForm.Get("ttl"))
	p.SeedName = strings.TrimSpace(r.PostForm.Get("name"))
	refuse := func(status int, err error) {
		p.Error = err.Error()
		s.render(w, status, "install-seed", p)
	}
	if p.NoSeed {
		refuse(http.StatusNotImplemented, errors.New("the console was started without orch's URL and the SHA-256 of its authority, so it cannot make a seed"))
		return
	}
	ttl, err := time.ParseDuration(p.TTL)
	if err != nil || ttl <= 0 {
		refuse(http.StatusUnprocessableEntity, errors.New("the lifetime is not a positive duration such as 24h"))
		return
	}
	join := seed.Join{OrchURL: s.cfg.OrchURL, CASHA256: s.cfg.OrchCASHA256, Name: p.SeedName}
	// The seed is checked before a key is made, so a refused name costs no key.
	if err := join.CheckTarget(); err != nil {
		refuse(http.StatusUnprocessableEntity, err)
		return
	}
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	key, err := s.cfg.Orch.CreateEnrollmentKey(ctx, s.cfg.Now().Add(ttl))
	if err != nil {
		status := http.StatusBadGateway
		var oe *orchapi.Error
		if errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500 {
			status = oe.Status
		}
		s.cfg.Log.Warn("create enrollment key", "err", err)
		refuse(status, err)
		return
	}
	join.EnrollKey = key.Secret
	img, err := join.ISO()
	if err != nil {
		s.cfg.Log.Error("build install seed", "key", key.ID, "err", err)
		refuse(http.StatusInternalServerError, errors.New("building the seed failed; revoke enrollment key "+key.ID))
		return
	}
	s.cfg.Log.Info("install seed made", "key", key.ID, "expires", key.ExpiresAt)
	h := w.Header()
	h.Set("Content-Type", "application/x-iso9660-image")
	h.Set("Content-Disposition", `attachment; filename="`+seedFilename+`"`)
	_, _ = w.Write(img)
}
