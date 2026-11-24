//go:build qemu_real

package tests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// TestLAN_KernelLifetimeProofOnQEMUKernel proves on the production QEMU kernel
// (6.18.63) that RTM_NEWADDR|NLM_F_REPLACE (AddrReplace) resets valid_lft on
// every renewal.
//
// Background: the original fleet failure (before d2fddcb) was not a valid_lft
// reset failure. The management-prefix check in checkExposure refused every
// exposure address when the management prefix equals the LAN subnet (single-NIC
// hosts, aurora: 192.168.0.0/24). d2fddcb removed that check and fixed the
// fleet test. AddrReplace was verified to reset valid_lft correctly on kernel
// 6.18.63 during this investigation (the before binary at e965b41 passed in the
// first QEMU run).
//
// A sequential wrapper (naru-lantest-seq) runs the test binary inside a fresh
// network namespace (via CLONE_NEWNET + exec). The wrapper polls etcd until
// active (up to 2 minutes) before starting — necessary because etcd startup on
// a 2-vCPU guest saturates the CPU. The test address uses a 60-second lifetime
// so CPU stalls during the QEMU run cannot expire it accidentally (observed
// stalls up to ~15 s; with a 60-second lifetime polled to half-way, stall
// tolerance is ~28 s).
//
// Pre-built static linux/amd64 binaries must be in ~/.cache/naru-pm/:
//
//	naru-lantest      (HEAD, AddrReplace)
//	naru-lantest-seq  (wrapper, built from tooling sources)
func TestLAN_KernelLifetimeProofOnQEMUKernel(t *testing.T) {
	const (
		lanBin = "naru-lantest"
		seqBin = "naru-lantest-seq"
	)

	cacheDir := filepath.Join(os.Getenv("HOME"), ".cache/naru-pm")
	for _, name := range []string{lanBin, seqBin} {
		src := filepath.Join(cacheDir, name)
		dst := filepath.Join(inputs.Dir, "bin", name)
		data, err := os.ReadFile(src)
		require.NoError(t, err, "pre-built binary %s not found in ~/.cache/naru-pm/", name)
		require.NoError(t, os.WriteFile(dst, data, 0o755))
	}

	c := start(t, node.ClusterSpec{
		N:       1,
		GenNode: true,
		ExtraServices: []node.ExtraService{
			{
				Name:   "naru94-lantest-seq",
				Binary: fixture.GuestRoot + "/bin/" + seqBin,
				Env:    map[string]string{"PATH": "/bin"},
			},
		},
	})

	nd := c.Nodes[0]

	// The wrapper outputs one marker when it finishes:
	//   naru-lantest-seq: lft-renewal exit=N (AddrReplace — must be 0)
	//
	// Allow 2*bootTimeout + 5 min for guest boot, etcd poll (up to 2 min),
	// and the ~65-second test run (60-second lifetime + polling overhead).
	require.Eventually(t, func() bool {
		return strings.Contains(nd.Console(), "naru-lantest-seq: lft-renewal exit=0")
	}, 2*bootTimeout+5*time.Minute, time.Second,
		"AddrReplace must reset valid_lft on QEMU kernel 6.18.63 (TestLAN_AddResetsTheKernelLifetimeOnRenewal)")
}
