package linux

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Reaper holds wait4(-1), as it does in PID 1, while many programs run to completion at once.
// A program that is waited on by its own pid instead would sometimes find it reaped already.
func TestReaper_NoExitStatusIsLostWhileProgramsRunConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reaper := NewReaper()
	go reaper.Run(ctx)
	h := &Host{run: reaper.Output}

	const programs = 300
	var wg sync.WaitGroup
	errs := make([]error, programs)
	outs := make([]string, programs)
	for i := range programs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.run("/bin/sh", "-c", fmt.Sprintf("echo out%d; exit %d", i, i%2*3))
			errs[i], outs[i] = err, string(out)
		}()
	}
	wg.Wait()

	for i := range programs {
		assert.Equal(t, fmt.Sprintf("out%d\n", i), outs[i], "program %d", i)
		if i%2 == 0 {
			assert.NoError(t, errs[i], "program %d exited with status 0", i)
		} else {
			require.Error(t, errs[i], "program %d", i)
			assert.Contains(t, errs[i].Error(), "exit status 3", "program %d", i)
		}
	}
}

func TestReaper_ExecKillsAProgramWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reaper := NewReaper()
	go reaper.Run(ctx)
	run, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := reaper.Exec(run, exec.Command("sleep", "60"), io.Discard, nil)
		done <- err
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("the program outlived its context")
	}
}
