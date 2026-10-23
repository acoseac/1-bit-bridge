package signalpath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
)

// Exercise the real loader through the verdict, with the same synthetic
// files the ALSA package uses for resolution and stage assertions.
func TestPCMValueFixturesKeepWarnings(t *testing.T) {
	for _, name := range []string{"float", "negative-real", "negative-integer", "path", "word", "numeric-strings", "byte-strings"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "alsa", "testdata", "asound-values", name+".conf"))
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			c := alsa.LoadPCMConfig(root)
			f := pcmOnTheGoBar()
			f.MPD.Output.SampleStage = c.SampleStage("listen")
			code := "alsa_softvol"
			if name == "float" {
				code = "alsa_route"
			}
			wantVerdict(t, name, Judge(f), FormatPreserved, nil, Warning{Code: code, Source: FromALSAConf})
		})
	}
}

func TestPCMSpacedDottedRouteKeepsWarning(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "pcm.direct { type hw card 2 } pcm.x { type plug slave .pcm direct ttable.0 .1 0.5 }"
	if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := alsa.LoadPCMConfig(root)
	f := pcmOnTheGoBar()
	f.MPD.Output.SampleStage = c.SampleStage("x")
	wantVerdict(t, "spaced dotted route", Judge(f), FormatPreserved, nil, Warning{Code: "alsa_route", Source: FromALSAConf})
}
