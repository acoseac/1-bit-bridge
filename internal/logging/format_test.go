package logging

import (
	"os"
	"testing"
)

// TestJSONRequested pins the format decision.
//
// The default is conditional rather than fixed because the two
// deployments want opposite things and neither should have to say so: a
// person running `bridge serve` in a terminal is READING these lines,
// and a container ships them to a collector that parses them.
//
// The strongest evidence that text was the wrong default off-terminal
// is in this repo already — the log-export handler reconstructs
// structure out of the text format with a strict `time=` + `level=`
// prefix scan, precisely because a whole-line search for `level=`
// matches text inside a quoted msg.
func TestJSONRequested(t *testing.T) {
	// A non-file writer is never a terminal and must not be probed as
	// one. Text keeps every existing test's expectations.
	t.Run("non-file writer defaults to text", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "")
		if jsonRequested(&nopWriter{}) {
			t.Error("a buffer got the JSON handler")
		}
	})

	t.Run("a pipe is not a terminal, so json", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "")
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if !jsonRequested(w) {
			t.Error("a redirected stderr must default to JSON — this is the container " +
				"case, where a collector has to parse what we write")
		}
	})

	t.Run("explicit text wins over the pipe default", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "text")
		r, w, _ := os.Pipe()
		defer r.Close()
		defer w.Close()
		if jsonRequested(w) {
			t.Error("BRIDGE_LOG_FORMAT=text was ignored")
		}
	})

	t.Run("explicit json wins over a non-file writer", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "json")
		if !jsonRequested(&nopWriter{}) {
			t.Error("BRIDGE_LOG_FORMAT=json was ignored")
		}
	})

	t.Run("case and whitespace tolerated", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "  JSON ")
		if !jsonRequested(&nopWriter{}) {
			t.Error("an operator's stray whitespace defeated the setting")
		}
	})

	t.Run("an unrecognised value falls through to the default", func(t *testing.T) {
		t.Setenv(FormatEnvVar, "logfmt")
		if jsonRequested(&nopWriter{}) {
			t.Error("an unknown format must fall through, not be treated as json")
		}
	})
}

type nopWriter struct{}

func (*nopWriter) Write(p []byte) (int, error) { return len(p), nil }
