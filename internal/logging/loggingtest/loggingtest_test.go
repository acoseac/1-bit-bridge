package loggingtest

import (
	"bytes"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// TestSetDefaultPutsBackTheLogPackage pins what a bare slog.SetDefault(prev)
// leaves behind. slog.SetDefault points the log package's output at the new
// handler and zeroes its flags, and putting back a default whose handler is
// slog's own undoes neither. That handler writes through the log package, so
// a line logged after the capture ended went into the finished test's
// buffer, and so did every later line in the binary.
func TestSetDefaultPutsBackTheLogPackage(t *testing.T) {
	leavesTheLogPackageAsItFoundIt(t, func(t testing.TB, buf *bytes.Buffer) {
		SetDefault(t, slog.New(slog.NewTextHandler(buf, nil)))
	})
}

// TestRecordAndParkOnPutBackTheLogPackage pins that the package's own
// installers go through SetDefault, and so leave nothing behind either.
func TestRecordAndParkOnPutBackTheLogPackage(t *testing.T) {
	t.Run("Record", func(t *testing.T) {
		leavesTheLogPackageAsItFoundIt(t, func(t testing.TB, _ *bytes.Buffer) { Record(t) })
	})
	t.Run("ParkOn", func(t *testing.T) {
		leavesTheLogPackageAsItFoundIt(t, func(t testing.TB, _ *bytes.Buffer) { ParkOn(t, "never logged") })
	})
}

// leavesTheLogPackageAsItFoundIt runs install inside a subtest, logs a line
// there, and requires that once the subtest has ended the log package has
// its own output and flags back and a line logged through the default
// reaches that output rather than the handler install put in place. It
// gives the log package an output of its own for the duration, so nothing
// here writes to the binary's stderr.
func leavesTheLogPackageAsItFoundIt(t *testing.T, install func(testing.TB, *bytes.Buffer)) {
	t.Helper()
	// The defect shows only when the default being put back is slog's own:
	// any other handler is re-installed by slog.SetDefault, which points the
	// log package at it again. Nothing in this package's tests leaves another.
	if got := fmt.Sprintf("%T", slog.Default().Handler()); got != "*slog.defaultHandler" {
		t.Fatalf("the default handler is %s, not slog's own, so this test cannot see the defect", got)
	}
	var own bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&own)
	log.SetFlags(log.Lmicroseconds)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	var captured bytes.Buffer
	t.Run("installed", func(t *testing.T) {
		install(t, &captured)
		slog.Info("inside the capture")
	})

	if log.Writer() != &own {
		t.Errorf("the log package's output is %T after the capture ended, not the output it had", log.Writer())
	}
	if got := log.Flags(); got != log.Lmicroseconds {
		t.Errorf("the log package's flags are %d after the capture ended, want %d", got, log.Lmicroseconds)
	}
	before := captured.Len()
	slog.Info("after the capture ended")
	if captured.Len() != before {
		t.Errorf("a line logged after the capture ended went into its buffer:\n%s", captured.String()[before:])
	}
	if !strings.Contains(own.String(), "after the capture ended") {
		t.Errorf("a line logged after the capture ended did not reach the log package's own output: %q", own.String())
	}
}
