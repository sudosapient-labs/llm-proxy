package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// The forwarder owns this state. Executor contexts retain the first request;
// a duplex connection's later responses get independent logical transactions.
type responsesWebsocketTelemetry struct {
	parent, ctx              context.Context
	current, last            *observability.Request
	successor, tokenObserved bool
	responseID               string
}

func newResponsesWebsocketTelemetry(c *gin.Context, parent context.Context, model string) *responsesWebsocketTelemetry {
	ctx, request := observability.Begin(parent, c.FullPath(), "WS", logging.GetRequestID(c.Request.Context()), c.GetHeader("sentry-trace"))
	observability.ConfigureRequest(ctx, model, true)
	logging.SetSentryRequestContext(c, ctx)
	return &responsesWebsocketTelemetry{parent: parent, ctx: ctx, current: request, last: request}
}

func (t *responsesWebsocketTelemetry) beforePayload(c *gin.Context, payload []byte) {
	if t == nil || t.last == nil {
		return
	}
	if websocketPayloadEventType(payload) == "response.created" {
		if t.current == nil {
			t.ctx, t.current = t.last.Next(t.parent)
			t.last = t.current
			t.successor, t.tokenObserved = true, false
			logging.SetSentryRequestContext(c, t.ctx)
		}
		t.responseID = gjson.GetBytes(payload, "response.id").String()
	}
	// Later executor usage reporters retain the connection context. Observe their
	// first token here, relative to response.created, without changing that context.
	if t.current != nil && t.successor && !t.tokenObserved && helps.IsResponsesTokenEvent(payload) {
		observability.FirstResponse(t.ctx, time.Now(), "token")
		t.tokenObserved = true
	}
}

func (t *responsesWebsocketTelemetry) afterPayload(c *gin.Context, payload []byte) {
	if t == nil || t.current == nil {
		return
	}
	// A queued create can fail while another response is still running. Its
	// rejection must not finalize that active response. IDs stay local only.
	if id := gjson.GetBytes(payload, "response.id").String(); id != "" && t.responseID != "" && id != t.responseID {
		return
	}
	switch websocketPayloadEventType(payload) {
	case "response.completed", "response.done":
		t.finish(c, nil, nil)
	case "response.incomplete":
		if gjson.GetBytes(payload, "response.incomplete_details.reason").String() == "steered" {
			t.finish(c, &interfaces.ErrorMessage{StatusCode: http.StatusOK, Error: context.Canceled}, nil)
		} else {
			t.finish(c, nil, nil)
		}
	case "response.failed":
		t.finish(c, responsesWebsocketErrorMessageFromPayload(payload), nil)
	}
}

func (t *responsesWebsocketTelemetry) closed(c *gin.Context, errs <-chan *interfaces.ErrorMessage) {
	if t == nil || t.current == nil {
		return
	}
	// The producer sends its error before closing data, but select may observe
	// the closed data channel first. Preserve a buffered failure in either order.
	select {
	case upstream := <-errs:
		if upstream != nil {
			t.finish(c, upstream, nil)
			return
		}
	default:
	}
	if err := c.Request.Context().Err(); err != nil {
		t.finish(c, nil, err)
		return
	}
	// A connection that ends without a terminal response is an upstream failure,
	// even if the forwarder subsequently sends a normal downstream close frame.
	t.finish(c, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: io.ErrUnexpectedEOF}, nil)
}

func (t *responsesWebsocketTelemetry) finish(c *gin.Context, upstream *interfaces.ErrorMessage, forwardErr error) {
	if t == nil || t.current == nil {
		return
	}
	status, cancelled := http.StatusOK, false
	if upstream != nil {
		// A server-sent close is also returned when reporting an upstream failure.
		// Preserve that failure instead of treating ErrCloseSent as a client abort.
		status = responsesWebsocketErrorStatus(upstream)
		observability.Outcome(t.ctx, upstream.Error, status)
	} else if forwardErr != nil {
		cancelled = errors.Is(forwardErr, context.Canceled) || errors.Is(forwardErr, websocket.ErrCloseSent) || isWebsocketConnectionClosedError(forwardErr)
		status = http.StatusInternalServerError
		observability.Outcome(t.ctx, forwardErr, 0)
	}
	t.current.Finish(status, cancelled)
	t.current = nil
	logging.SetSentryRequestContext(c, nil)
}
