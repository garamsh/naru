package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// logCalls logs each call with its status and how long it took, and turns a panic in a handler into
// an Internal status, so a bug in one call does not end the driver the kubelet is talking to.
func logCalls(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic", "method", info.FullMethod, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "panic: %v", p)
			}
			level := slog.LevelInfo
			if err != nil {
				level = slog.LevelWarn
			}
			log.Log(ctx, level, "call", "method", info.FullMethod, "code", status.Code(err).String(), "took", time.Since(start), "error", err)
		}()
		return next(ctx, req)
	}
}
