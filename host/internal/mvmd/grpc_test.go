package mvmd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFail_MapsMvmdsOutcomes(t *testing.T) {
	g := New("/nonexistent")
	cases := map[codes.Code]error{
		codes.NotFound:           ErrNotFound,
		codes.AlreadyExists:      ErrConflict,
		codes.InvalidArgument:    ErrInvalid,
		codes.FailedPrecondition: ErrPrecondition,
		codes.Unavailable:        ErrUnavailable,
	}
	for code, want := range cases {
		err := g.fail(status.Error(code, "reason"))
		assert.ErrorIs(t, err, want, code.String())
	}
	assert.NoError(t, g.fail(nil))
	other := g.fail(status.Error(codes.Internal, "boom"))
	for _, e := range cases {
		assert.False(t, errors.Is(other, e), "an internal failure is none of the named outcomes")
	}
}
