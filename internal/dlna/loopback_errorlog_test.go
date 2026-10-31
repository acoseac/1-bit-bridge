package dlna

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/handshakelog/handshaketest"
)

// TestALoopbackRoutePanicIsLoggedWithoutItsClient: net/http recovers a
// panic in a handler and logs it with the address of the client it was
// serving ("http: panic serving <addr>: …"), and the loopback track route's
// server had no ErrorLog, so the line went to the standard logger with
// MPD's address in it. It takes handshakelog's, as serve's servers and the
// OpenHome surface do, so the line keeps the panic and stands
// handshakelog.ClientPlaceholder where the address was. The real server,
// over a real connection, its handler made to panic by the gate it asks
// first.
func TestALoopbackRoutePanicIsLoggedWithoutItsClient(t *testing.T) {
	log := handshaketest.CaptureStdLog(t)
	s := NewLoopbackServer(LoopbackConfig{Enabled: func() bool { panic(testPanicValue) }})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	requireAPanicLineWithoutItsClient(t, log, s.BaseURL()+FilePathPrefix+"0123456789abcdef")
}
