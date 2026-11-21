package volumes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseID_AcceptsAKindAndANameAndNothingThatNamesAPath(t *testing.T) {
	for _, ok := range []string{"local-a", "block-pvc-1.x"} {
		id, err := parseID(ok)
		assert.NoError(t, err, ok)
		assert.Equal(t, volumeID(ok), id)
	}
	for _, bad := range []string{"", "local", "local-", "local-/x", "local-..", "local-a/../b", "block-A", "pv-a"} {
		_, err := parseID(bad)
		assert.Error(t, err, bad)
	}
}
