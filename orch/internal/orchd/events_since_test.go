package orchd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/orchtest"
)

func TestEventsSinceACursorAreOnlyTheEventsAfterIt(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
	first, err := o.reader.EventsSince(t.Context(), "")
	require.NoError(t, err)
	require.NotEmpty(t, first)
	for _, e := range first {
		require.NotEmpty(t, e.Cursor, "each event says where it is in the log")
	}
	cursor := first[len(first)-1].Cursor

	none, err := o.reader.EventsSince(t.Context(), cursor)
	require.NoError(t, err)
	assert.Empty(t, none, "nothing happened since")

	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.201.0.0/16")))
	next, err := o.reader.EventsSince(t.Context(), cursor)
	require.NoError(t, err)
	require.Len(t, next, 1, "only the event that came after")
	assert.Greater(t, next[0].Cursor, cursor)

	all, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	assert.Len(t, all, len(first)+1, "Events still lists the whole log")
}

func TestEventsSinceAnInvalidCursorIsRefused(t *testing.T) {
	o := startOne(t, orchtest.Options{})

	_, err := o.reader.EventsSince(t.Context(), "not-a-revision")

	assert.Equal(t, http.StatusBadRequest, status(err))
}
