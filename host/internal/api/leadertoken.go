package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// leaderTokens keeps the highest fencing token an orch leader has sent, in a file so that a restart
// of the host does not forget it (ADR 0071).
type leaderTokens struct {
	path string

	mu      sync.Mutex
	loaded  bool
	highest int64
}

// admit takes a token: it reports false for one lower than the highest seen. A higher token is
// written to the file before admit returns true, so no call runs on a token the host could forget.
func (l *leaderTokens) admit(token int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.loaded {
		if err := l.load(); err != nil {
			return false, err
		}
	}
	if token < l.highest {
		return false, nil
	}
	if token > l.highest {
		if l.path != "" {
			if err := atomicfile.Write(l.path, []byte(strconv.FormatInt(token, 10)), 0o600); err != nil {
				return false, fmt.Errorf("keep the leader token: %w", err)
			}
		}
		l.highest = token
	}
	return true, nil
}

func (l *leaderTokens) load() error {
	l.loaded = true
	if l.path == "" {
		return nil
	}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		l.loaded = false
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		l.loaded = false
		return fmt.Errorf("%s does not hold a leader token", l.path)
	}
	l.highest = n
	return nil
}

// leaderToken fences the calls that change the host: one that carries the leader token header is
// refused with 409 when the token is lower than the highest seen. A call without the header is an
// operator's and a read is not fenced.
func (s *Server) leaderToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(wire.LeaderTokenHeader)
		if raw == "" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		token, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || token <= 0 {
			writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the leader token is not a positive integer"})
			return
		}
		ok, err := s.tokens.admit(token)
		if err != nil {
			s.deps.Log.Error("the leader token cannot be kept", "err", err.Error())
			writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "the leader token cannot be kept"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "the leader token is lower than one this host has seen: a later leader holds the term", Code: hostapi.CodeStaleLeader})
			return
		}
		next.ServeHTTP(w, r)
	})
}
