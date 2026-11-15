package hostclient_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/leader"
)

func TestMutatingCallsCarryTheTermsTokenAndReadsAndOperatorCallsDoNot(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	ctx, end := leader.NewTerm(t.Context(), 42, "/k")
	defer end(nil)

	require.NoError(t, c.Stop(ctx, "h", "m"))
	require.NoError(t, c.Start(ctx, "h", "m"))
	require.NoError(t, c.Delete(ctx, "h", "m"))
	require.NoError(t, c.Create(ctx, "h", groups.MachineSpec{ID: "m", Template: groups.Template{Form: groups.FormOS, CPUs: 1, MemoryMiB: 128, NodeVersion: "node-2", Disks: []groups.Disk{{Name: "data", SizeBytes: 1 << 30}}}}))
	_, err := c.List(ctx)
	require.NoError(t, err)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range []string{"POST /v1/microvms/m/stop", "POST /v1/microvms/m/start", "DELETE /v1/microvms/m", "POST /v1/microvms"} {
		assert.Equal(t, "42", h.tokens[key], key)
	}
	assert.Empty(t, h.tokens["GET /v1/microvms"], "a read carried the token")

	// A call outside any term is an operator's.
	h.mu.Unlock()
	require.NoError(t, c.Stop(t.Context(), "h", "m"))
	h.mu.Lock()
	assert.Empty(t, h.tokens["POST /v1/microvms/m/stop"])
}

func TestAHostThatHasSeenALaterTokenEndsTheTermOfTheCaller(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	h.mu.Lock()
	h.status["POST /v1/microvms/m/start"] = 409
	h.codes["POST /v1/microvms/m/start"] = hostapi.CodeStaleLeader
	h.status["POST /v1/microvms/m/stop"] = 409
	h.mu.Unlock()

	// A conflict of another kind leaves the term alone.
	ctx, end := leader.NewTerm(t.Context(), 7, "/k")
	defer end(nil)
	require.Error(t, c.Stop(ctx, "h", "m"))
	assert.NoError(t, ctx.Err())

	err := c.Start(ctx, "h", "m")
	var he *hostapi.Error
	require.ErrorAs(t, err, &he)
	assert.Equal(t, hostapi.CodeStaleLeader, he.Code)
	assert.ErrorIs(t, context.Cause(ctx), leader.ErrTermLost)
}
