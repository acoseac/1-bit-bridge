package openhome

import (
	"strings"
	"testing"
)

func TestDSDPolicyProtocolInfo(t *testing.T) {
	const pcm = "http-get:*:audio/x-flac:*,http-get:*:audio/wav:*,http-get:*:audio/aiff:*,http-get:*:audio/mp4:*"
	for _, reason := range []string{"off", "missing", "stale", "empty", "dop_only", "binding_mismatch", "revoked"} {
		t.Run(reason, func(t *testing.T) {
			values, f := reads["Playlist"]["ProtocolInfo"](readRequest{})
			if f != nil || string(values[0]) != pcm {
				t.Fatalf("PCM bytes changed: %q %v", values, f)
			}
		})
	}
	values, f := reads["Playlist"]["ProtocolInfo"](readRequest{dsd: true})
	if f != nil || !strings.HasPrefix(string(values[0]), pcm+",") || !strings.Contains(string(values[0]), "dsf") || !strings.Contains(string(values[0]), "dff") {
		t.Fatalf("approved MIME set=%q", values)
	}
}
