package auth

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func telemetryOptions(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if opts.Headers != nil {
		opts.Headers = opts.Headers.Clone()
		for _, key := range []string{"sentry-trace", "baggage", "traceparent", "tracestate"} {
			opts.Headers.Del(key)
		}
	}
	return opts
}

func executeObserved(ctx context.Context, exec ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, count bool) (cliproxyexecutor.Response, error) {
	ctx, attempt := observability.BeginAttempt(ctx, auth.Provider, req.Model)
	var resp cliproxyexecutor.Response
	var err error
	if count {
		resp, err = exec.CountTokens(ctx, auth, req, telemetryOptions(opts))
	} else {
		resp, err = exec.Execute(ctx, auth, req, telemetryOptions(opts))
	}
	attempt.Finish(err, 0)
	return resp, err
}

func streamObserved(ctx context.Context, exec ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	execCtx, attempt := observability.BeginAttempt(ctx, auth.Provider, req.Model)
	result, err := exec.ExecuteStream(execCtx, auth, req, telemetryOptions(opts))
	if attempt == nil {
		return result, err
	}
	if err != nil {
		attempt.Finish(err, 0)
		return result, err
	}
	if result == nil || result.Chunks == nil {
		attempt.Finish(errors.New("invalid stream"), 0)
		return result, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamErr error
		defer func() { attempt.Finish(streamErr, 0) }()
		for {
			select {
			case <-ctx.Done():
				streamErr = ctx.Err()
				discardStreamChunks(result.Chunks)
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				if chunk.Err != nil {
					streamErr = chunk.Err
					attempt.Finish(streamErr, 0)
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					streamErr = ctx.Err()
					discardStreamChunks(result.Chunks)
					return
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}, nil
}

func refreshObserved(ctx context.Context, exec ProviderExecutor, auth *Auth) (*Auth, error) {
	start := time.Now()
	updated, err := exec.Refresh(ctx, auth)
	observability.Refresh(ctx, auth.Provider, time.Since(start), err, err != nil && (isUnauthorizedError(err) || isInvalidGrantError(err)))
	return updated, err
}
