package auth

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// EndGrace is how long a stream a recheck ended has to write its refusal to
// its client before its writes are cut off.
const EndGrace = 5 * time.Second

type cutOffKey struct{}

// cutOff ends the writes of one request's response, until the request's
// handler has returned, and does nothing after.
type cutOff struct {
	mu       sync.Mutex
	rc       *http.ResponseController
	set      bool
	returned bool
}

func (c *cutOff) after(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.returned {
		return
	}
	if c.rc.SetWriteDeadline(time.Now().Add(d)) == nil {
		c.set = true
	}
}

func (c *cutOff) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.returned = true
	// The connection may serve another request; the deadline was this
	// one's.
	if c.set {
		_ = c.rc.SetWriteDeadline(time.Time{})
	}
}

// CutOff lets a stream Hold ends stop writing: a handler blocked writing to
// a client that reads nothing sees no context end, so once a recheck
// refuses the stream its writes fail after EndGrace. A client that still
// reads has its refusal within that. Wrap every listener's handler in it.
func CutOff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &cutOff{rc: http.NewResponseController(w)}
		defer c.done()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cutOffKey{}, c)))
	})
}

// cutOffWrites ends the writes of the response ctx belongs to after d, if
// CutOff serves it.
func cutOffWrites(ctx context.Context, d time.Duration) {
	if c, ok := ctx.Value(cutOffKey{}).(*cutOff); ok {
		c.after(d)
	}
}
