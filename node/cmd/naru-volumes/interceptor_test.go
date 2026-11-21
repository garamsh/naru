package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLogCalls_LogsTheStatusAndTurnsAPanicIntoInternal(t *testing.T) {
	var out bytes.Buffer
	call := logCalls(slog.New(slog.NewTextHandler(&out, nil)))
	info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Node/NodePublishVolume"}

	resp, err := call(context.Background(), nil, info, func(context.Context, any) (any, error) { return "ok", nil })
	require.NoError(t, err)
	assert.Equal(t, "ok", resp, "the control: a call that returns is passed through")

	_, err = call(context.Background(), nil, info, func(context.Context, any) (any, error) { return nil, status.Error(codes.NotFound, "x") })
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Contains(t, out.String(), "code=NotFound")

	_, err = call(context.Background(), nil, info, func(context.Context, any) (any, error) { panic(errors.New("boom")) })
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, out.String(), "boom")
}
