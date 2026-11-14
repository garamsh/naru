package signer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// peeredFile marks a directory whose signer has taken a list from orch's sync. It holds nothing.
	peeredFile = "peered"
	// maxSyncAge is how long after the last list it took a peered signer still accepts a reissue
	// of root: three of orch's sync intervals. Past it the signer may be missing a nonce another
	// signer used.
	maxSyncAge = 30 * time.Second
)

// ErrNotSynced is returned by a reissue of root on a signer whose peers have not reached it lately.
var ErrNotSynced = errors.New("signer has not taken the shared revocation list lately")

// notePush records that a list was pushed at now and, the first time, marks the directory peered.
// The caller holds s.mu.
func (s *Signer) notePush(now time.Time) error {
	if !s.peered {
		if err := writeFile(filepath.Join(s.dir, peeredFile), nil); err != nil {
			return err
		}
		s.peered = true
	}
	s.lastPush = now
	return nil
}

// checkSynced refuses a signer that is peered and took no list within maxSyncAge of now. A signer
// that never took one is alone and is guarded by its own list. After a restart lastPush is zero,
// so a peered signer waits for the next push. The caller holds s.mu.
func (s *Signer) checkSynced(now time.Time) error {
	if s.peered && now.Sub(s.lastPush) > maxSyncAge {
		return fmt.Errorf("%w: wait for orch's next sync", ErrNotSynced)
	}
	return nil
}

func peeredIn(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, peeredFile))
	return err == nil
}
