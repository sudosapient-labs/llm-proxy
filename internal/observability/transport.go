package observability

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	log "github.com/sirupsen/logrus"
)

// queuedTransport never waits for network I/O on an inference goroutine. A
// single worker bounds concurrency even when the destination stops responding.
type queuedTransport struct {
	inner   sentry.Transport
	queue   chan *sentry.Event
	mu      sync.Mutex
	closed  bool
	done    chan struct{}
	dropped atomic.Uint64
	warned  atomic.Bool
	cancel  context.CancelFunc
}

func (t *queuedTransport) Configure(options sentry.ClientOptions) {
	t.inner.Configure(options)
	go func() {
		defer close(t.done)
		for event := range t.queue {
			if t.dropped.Load() > 0 && t.warned.CompareAndSwap(false, true) {
				log.WithField("capacity", cap(t.queue)).Warn("Sentry telemetry queue full; dropping events")
			}
			t.inner.SendEvent(event)
		}
	}()
}

func (t *queuedTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		t.dropped.Add(1)
		return
	}
	select {
	case t.queue <- event:
	default:
		t.dropped.Add(1)
	}
}

func (t *queuedTransport) Flush(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return t.FlushWithContext(ctx)
}

func (t *queuedTransport) FlushWithContext(ctx context.Context) bool {
	// A barrier is only used during shutdown/tests, never on the request path.
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.queue)
	}
	t.mu.Unlock()
	select {
	case <-t.done:
		return t.inner.FlushWithContext(ctx)
	case <-ctx.Done():
		return false
	}
}

func (t *queuedTransport) Close() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.queue)
	}
	t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	t.inner.Close()
}
