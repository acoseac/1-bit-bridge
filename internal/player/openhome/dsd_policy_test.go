package openhome

import (
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"strings"
	"testing"
)

func TestDSDPolicyProtocolInfo(t *testing.T) {
	const pcm = "http-get:*:audio/x-flac:*,http-get:*:audio/wav:*,http-get:*:audio/aiff:*,http-get:*:audio/mp4:*"
	for _, state := range []string{"off", "missing", "stale", "empty", "dop_only", "binding_mismatch", "revoked", "valid"} {
		t.Run(state, func(t *testing.T) {
			b := playbacktest.Approved(t, state)
			p := newTestPlayer(t)
			h := p.handler("")
			calls := 0
			h.dsdPolicy = func() bool { calls++; return len(b.Policy().Tuples) > 0 }
			value := read(t, h, playlist, "ProtocolInfo")["Value"]
			if calls != 1 {
				t.Fatalf("policy callback calls=%d", calls)
			}
			if state == "valid" {
				if !strings.HasPrefix(value, pcm+",") || !strings.Contains(value, "dsf") || !strings.Contains(value, "dff") {
					t.Fatal(value)
				}
			} else if value != pcm {
				t.Fatalf("%s changed PCM bytes: %s", state, value)
			}
		})
	}
}
