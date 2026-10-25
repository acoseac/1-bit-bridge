package main

import (
	"bytes"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestServeRefusesConsoleSavesWhenItsBootCouldNotResolveTheConfig: a serve
// whose boot read could resolve the config's name at none of its reads boots
// on the bytes it read through the name, and refuses every console save,
// saying it could not resolve the file the name opens when it started and to
// restart once the name resolves: it cannot say which file it read, so it
// saves into none, and the file keeps what it held. A save made once the name
// resolves again is refused the same way, until a restart reads the file
// again. Until B506's fifth round the boot recorded no file, and the console
// saved into whatever file the name opened at the save.
func TestServeRefusesConsoleSavesWhenItsBootCouldNotResolveTheConfig(t *testing.T) {
	restore := config.SetBootResolveForTest(func(string) (string, error) {
		return "", errors.New("the config's name cannot be resolved (for the test)")
	})
	t.Cleanup(restore)
	read := make(chan struct{})
	b := startConsoleBridge(t, "libraryName: Before\n", nil, func(o *serveOpts) {
		o.afterConfigRead = func() { close(read) }
	})
	// The boot read is over: nothing reads the seam after it.
	<-read
	cfgPath := filepath.Join(filepath.Dir(b.dataDir), "bridge.yaml")
	before := readBytes(t, cfgPath)
	for _, when := range []string{"while the name resolves nowhere", "once the name resolves"} {
		if when == "once the name resolves" {
			restore()
		}
		status, body := patchSettings(t, b, `{"libraryName":"After"}`)
		if status == http.StatusOK || !strings.Contains(body, "could not resolve") || !strings.Contains(body, "restart") {
			t.Errorf("a console save %s: %d %s, want it refused, saying the boot could not resolve the config and to restart",
				when, status, body)
		}
		if strings.Contains(body, "Before") {
			t.Errorf("the refusal %s quotes the config: %s", when, body)
		}
		if after := readBytes(t, cfgPath); !bytes.Equal(after, before) {
			t.Errorf("the console save %s wrote the config:\n%s", when, after)
		}
	}
}
