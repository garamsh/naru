package release

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrevious_OrdersPrereleasesBySemVerPrecedence(t *testing.T) {
	// Listed out of order on purpose: neither list order nor publish time may decide.
	tags := []string{"v0.1.0", "v0.1.0-dev.10", "v0.1.0-rc.1", "v0.1.0-dev.9", "v0.0.9"}
	for _, c := range []struct{ tag, want string }{
		{"v0.1.0-dev.10", "v0.1.0-dev.9"},
		{"v0.1.0-rc.1", "v0.1.0-dev.10"},
		{"v0.1.0", "v0.1.0-rc.1"},
		{"v0.1.0-dev.9", "v0.0.9"},
	} {
		got, err := Previous(c.tag, tags)
		require.NoError(t, err, c.tag)
		assert.Equal(t, c.want, got, "previous of %s", c.tag)
	}
}

func TestPrevious_NoPredecessor(t *testing.T) {
	got, err := Previous("v0.0.9", []string{"v0.1.0", "v0.1.0-dev.9"})
	require.NoError(t, err)
	assert.Empty(t, got, "every other release is above the tag")
	got, err = Previous("v0.1.0-dev.1", nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPrevious_LeavesTheCurrentTagOutAndIgnoresNamesThatAreNotVersions(t *testing.T) {
	got, err := Previous("v0.1.0-dev.10", []string{"v0.1.0-dev.10", "nightly", "v0.1.0-dev.9"})
	require.NoError(t, err)
	assert.Equal(t, "v0.1.0-dev.9", got)
	// A tag is never its own predecessor, even listed twice and with nothing below it.
	got, err = Previous("v0.1.0-dev.9", []string{"v0.1.0-dev.9", "v0.1.0-dev.9"})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPrevious_RefusesATagThatIsNotAVersion(t *testing.T) {
	_, err := Previous("main", []string{"v0.1.0"})
	assert.ErrorContains(t, err, "not a SemVer version")
}
