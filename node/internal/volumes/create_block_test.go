package volumes_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestCreateVolume_BlockClaimsDisksInOrderAndSkipsOneTooSmall(t *testing.T) {
	r := newRig(t, false, "d2", "d1", "gone")
	// d2 is too small for 2 GiB and is skipped; d1 is claimed.
	a := mustCreate(t, r, "pvc-a", 2<<30, blockCap())
	assert.Equal(t, "block-pvc-a", a)
	assert.Equal(t, a, mustCreate(t, r, "pvc-a", 2<<30, blockCap()), "a retry gets the same volume")
	_, err := create(r, "pvc-b", 2<<30, blockCap())
	assert.Equal(t, codes.ResourceExhausted, code(err))
	mustCreate(t, r, "pvc-c", 1<<30, blockCap())
}

func TestCreateVolume_BlockRefusesADiskTheNodeUsesAndGivesAFreeOne(t *testing.T) {
	r := newRig(t, false, "d1", "d2")
	r.disks.busy["/dev/vdb"] = errors.New("/dev/vdb1 is mounted at /var/lib/naru")
	// d1 is the first listed and large enough, and it holds the Node's own file system.
	id := mustCreate(t, r, "pvc-a", 1<<30, blockCap())
	v, err := r.loadSerial(id)
	require.NoError(t, err)
	assert.Equal(t, "d2", v, "the free disk is the one handed out")
	_, err = create(r, "pvc-b", 1<<30, blockCap())
	assert.Equal(t, codes.ResourceExhausted, code(err))
	assert.Contains(t, err.Error(), "/var/lib/naru", "the refusal names what uses the disk")
}

func TestDeleteVolume_BlockForgetsTheClaimAndLeavesTheDisk(t *testing.T) {
	r := newRig(t, false, "d1")
	id := mustCreate(t, r, "pvc-a", 2<<30, blockCap())
	require.NoError(t, remove(r, id))
	assert.Empty(t, r.q.limit, "deleting a block volume touches no quota")
	assert.Equal(t, id, mustCreate(t, r, "pvc-a", 2<<30, blockCap()), "the freed disk is handed out again")
}
