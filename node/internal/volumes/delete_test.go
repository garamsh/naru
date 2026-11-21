package volumes_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteVolume_RemovesTheDirectoryLiftsTheQuotaAndAnswersARetry(t *testing.T) {
	r := newRig(t, true)
	id := mustCreate(t, r, "pvc-a", 1<<20, mountCap())
	require.NoError(t, remove(r, id))
	assert.NoDirExists(t, r.dir(id))
	assert.Zero(t, r.q.limit[r.dir(id)])
	assert.NoError(t, remove(r, id), "a volume already gone is deleted")
}
