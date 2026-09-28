package loggingtest_test

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestInstallersRestoreTheStandardLogger pins that Record and ParkOn put
// back everything slog.SetDefault changed when they installed their
// handler, the standard library's logger included.
//
// slog.SetDefault does more than swap the slog default. Given any handler
// but slog's own, it also points log's output at that handler and zeroes
// log's flags, and setting slog's own default back does not undo either
// (log/slog's SetDefault skips the redirect for its own handler). So the
// restore that put back only the slog default left the standard logger
// writing into the Recorder of a test that had ended. After the first
// Record in a test binary a log.Print went there, and so did every line
// logged through slog.Default, whose own handler writes through the
// standard logger: no later test's log lines reached the output.
//
// Two defaults for an installer to find. slog's own is the one every test
// binary starts with, and the one whose restore leaves the standard logger
// alone. A handler the test set is the other shape: restoring IT points the
// standard logger at it again, so the saved writer has to be set back after
// the slog default, or that restore has the last word.
//
// Each installer runs in a subtest, whose cleanups have all run by the
// time t.Run returns, so what is checked after it is the state a later
// test in the same binary inherits.
func TestInstallersRestoreTheStandardLogger(t *testing.T) {
	installers := []struct {
		name    string
		install func(t *testing.T) *loggingtest.Recorder
	}{
		{"Record", func(t *testing.T) *loggingtest.Recorder { return loggingtest.Record(t) }},
		{"ParkOn", func(t *testing.T) *loggingtest.Recorder {
			return &loggingtest.ParkOn(t, "a message this test never logs").Recorder
		}},
	}
	priors := []struct {
		name     string
		slogsOwn bool
	}{
		{"over slog's own default", true},
		{"over a default the test set", false},
	}
	for _, in := range installers {
		for _, prior := range priors {
			t.Run(in.name+" "+prior.name, func(t *testing.T) {
				var out, viaSlog bytes.Buffer
				prevOut, prevFlags, prevDefault := log.Writer(), log.Flags(), slog.Default()
				t.Cleanup(func() {
					slog.SetDefault(prevDefault)
					log.SetOutput(prevOut)
					log.SetFlags(prevFlags)
				})
				if !prior.slogsOwn {
					slog.SetDefault(slog.New(slog.NewTextHandler(&viaSlog, nil)))
				}
				before := slog.Default()
				log.SetOutput(&out)
				// A flag SetDefault zeroes, so a restore that misses the
				// flags shows. Lmsgprefix with no prefix adds nothing to a
				// line.
				const flags = log.Lmsgprefix
				log.SetFlags(flags)

				if prior.slogsOwn {
					// The premise: slog's own default writes through the
					// standard logger, so a line logged through slog.Default
					// is lost with it.
					slog.Info("before the installer")
					if !strings.Contains(out.String(), "INFO before the installer\n") {
						t.Fatalf("slog.Default does not write through the standard logger "+
							"here, so this case cannot see the defect; the writer holds %q", out.String())
					}
				}

				var rec *loggingtest.Recorder
				t.Run("installed", func(t *testing.T) {
					rec = in.install(t)
					log.Print("while installed")
				})
				// And the redirect is real while the installer is in place:
				// the log.Print went to its handler, not to the writer.
				if got := rec.Lines("while installed"); len(got) != 1 {
					t.Fatalf("while installed, the Recorder kept %q of a log.Print; "+
						"slog.SetDefault no longer redirects the standard logger, so this "+
						"test no longer shows what it restores", got)
				}

				out.Reset()
				log.Print("after the installer")
				want := []string{"after the installer\n"}
				if prior.slogsOwn {
					slog.Info("after the installer, through slog")
					want = append(want, "INFO after the installer, through slog\n")
				}
				for _, w := range want {
					if !strings.Contains(out.String(), w) {
						t.Errorf("%q did not reach the standard logger's writer after the "+
							"installer's cleanup; the writer holds %q", strings.TrimSpace(w), out.String())
					}
				}
				if got := rec.Lines("after the installer"); len(got) != 0 {
					t.Errorf("the ended test's Recorder still receives log.Print: %q", got)
				}
				if log.Writer() != &out {
					t.Errorf("log.Writer() is %T after the cleanup, want the writer the test set", log.Writer())
				}
				if got := log.Flags(); got != flags {
					t.Errorf("log.Flags() = %d after the cleanup, want %d", got, flags)
				}
				if slog.Default() != before {
					t.Error("slog.Default() is not the logger the installer found")
				}
			})
		}
	}
}
