package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"

	"github.com/acoseac/1-bit-bridge/internal/tsnet"
)

// heldTsnetStatus is an embedded node whose status read waits for its
// context's end, and says when it was asked.
type heldTsnetStatus struct {
	asked chan struct{}
	once  *sync.Once
}

func (h heldTsnetStatus) Status(ctx context.Context) (*ipnstate.Status, error) {
	h.once.Do(func() { close(h.asked) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (heldTsnetStatus) CertDomains() []string { return nil }

func (heldTsnetStatus) LastStartError() error { return nil }

func (heldTsnetStatus) Uploads() tsnet.Uploads { return tsnet.Uploads{} }

func (heldTsnetStatus) LogsRequired(*ipnstate.Status) error { return nil }

// TestTheTailnetTilesStatusEndsWithItsCallersContext: the console's read of
// the embedded node's status (StatusContext, which its event stream and its
// REST route call) ends with its caller's context, which the home-network
// socket's retirement and the shutdown cancel, well inside the read's own
// bound, adminStatusTimeout (backlog B989, its second round).
func TestTheTailnetTilesStatusEndsWithItsCallersContext(t *testing.T) {
	held := heldTsnetStatus{asked: make(chan struct{}), once: &sync.Once{}}
	src := newTailscaleAdminSource(nil, held, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-held.asked:
		case <-time.After(2 * adminStatusTimeout):
		}
		cancel()
	}()
	start := time.Now()
	st := src.StatusContext(ctx)
	if took := time.Since(start); took > adminStatusTimeout/2 {
		t.Fatalf("the status read returned %v after its caller's context ended: at its own bound, not its caller's context", took)
	}
	if st.LastError == "" {
		t.Fatalf("the status of a read its caller's context ended says nothing went wrong: %+v", st)
	}
}
