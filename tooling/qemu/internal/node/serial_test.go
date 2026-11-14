package node

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A unix socket path may have no more than 107 bytes. The serial socket of a Node whose directory
// carries a long test name is still inside that, and two Nodes do not share one.
func TestSerialSocketPathStaysInsideTheUnixSocketLimitForALongNodeDirectory(t *testing.T) {
	long := "/home/sauce/.cache/t-w14/TestControlPlane_" + strings.Repeat("ThreeNodesWhoseSerialSocket", 4) + "123456/001/"
	one, two := SerialSocketPath(long+"node-1"), SerialSocketPath(long+"node-2")

	assert.LessOrEqual(t, len(one), 107, one)
	assert.NotEqual(t, one, two)
	assert.Equal(t, one, SerialSocketPath(long+"node-1"), "the path is the same each time it is asked")
}
