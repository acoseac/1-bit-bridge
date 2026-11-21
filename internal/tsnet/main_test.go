package tsnet

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// TestMain puts a stand-in in the place of logtail's switch for the whole
// suite, which counts what reaches it, and fails the binary when anything
// did: logtail.Disable lasts the process the suite runs in, so a case that
// turns the switch (Server.AbandonStart) puts its own stand-in in place for
// its length (standInForLogtail), and nothing else may reach it.
func TestMain(m *testing.M) {
	var reached atomic.Int32
	restore := SetLogtailDisableForTest(func() { reached.Add(1) })
	code := m.Run()
	restore()
	if n := reached.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "the suite turned logtail's switch %d time(s) with no stand-in of a case's own in place, "+
			"which no test may: logtail.Disable lasts the process\n", n)
		code = 1
	}
	os.Exit(code)
}
