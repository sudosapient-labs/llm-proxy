package logging

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
)

const sentryRequestContextKey = "__sentry_request_context__"

// SetSentryRequestContext lets a WebSocket turn share its trace with existing
// Gin panic recovery without replacing the socket's request/cancellation context.
func SetSentryRequestContext(c *gin.Context, ctx context.Context) {
	c.Set(sentryRequestContextKey, ctx)
}

func sentryRequestContext(c *gin.Context) context.Context {
	if value, ok := c.Get(sentryRequestContextKey); ok {
		if ctx, okCtx := value.(context.Context); okCtx && ctx != nil {
			return ctx
		}
	}
	return c.Request.Context()
}

// SentryRequests runs inside request-ID logging and outside existing recovery.
// Models/listing, management, health probes and WebSocket handshakes are excluded.
// Responses WebSocket turns create their own transactions in the handler.
func SentryRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if !isAIAPIPath(route) || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") ||
			(c.Request.Method != http.MethodPost && !strings.Contains(route, ":action")) {
			c.Next()
			return
		}
		ctx, trace := observability.Begin(c.Request.Context(), route, c.Request.Method, GetRequestID(c.Request.Context()), c.GetHeader("sentry-trace"))
		c.Request = c.Request.WithContext(ctx)
		if trace != nil {
			c.Request.Header = c.Request.Header.Clone()
			for _, key := range []string{"sentry-trace", "baggage", "traceparent", "tracestate"} {
				c.Request.Header.Del(key)
			}
		}
		defer func() {
			recovered := recover()
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				observability.Outcome(ctx, context.Canceled, 0)
			}
			trace.Finish(c.Writer.Status(), errors.Is(c.Request.Context().Err(), context.Canceled))
			if recovered != nil {
				panic(recovered)
			}
		}()
		c.Next()
	}
}
