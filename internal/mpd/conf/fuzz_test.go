package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// FuzzParseConf holds the configuration parser to a file it does not
// trust: no panic, two parses that agree, output bounded by the input,
// and a password of any real length absent from the text, the JSON and
// the problems once the public field values have been taken out. The
// nightly job finds this by name.
func FuzzParseConf(f *testing.F) {
	b, err := os.ReadFile("../testdata/volumio-mpd.conf")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(string(b))
	f.Add("password \"fuzz-secret-value\"\nmusic_directory \"/m\"\n")
	f.Add("password\"password\"\n")
	f.Add("audio_output {\n  type \"alsa\"\n  name \"a\"\n}\n")
	f.Fuzz(func(t *testing.T, text string) {
		a, errA := Parse(text)
		b, errB := Parse(text)
		if (errA == nil) != (errB == nil) {
			t.Fatalf("parse is not deterministic")
		}
		ja := mustMarshal(t, a)
		jb := mustMarshal(t, b)
		if string(ja) != string(jb) || a.String() != b.String() {
			t.Fatalf("parse is not deterministic")
		}
		if len(ja) > 64*len(text)+4096 {
			t.Fatalf("%d bytes of input made %d bytes of JSON", len(text), len(ja))
		}
		rendered := a.String() + "\n" + string(ja) + "\n" + strings.Join(a.Problems, "\n")
		if errA != nil {
			rendered += "\n" + errA.Error()
		}
		rendered = stripFixed(stripPublic(rendered, a))
		for _, secret := range a.passwordValues() {
			if len(secret) < 8 {
				continue
			}
			if strings.Contains(rendered, secret) {
				t.Fatalf("a password of %d bytes is in the rendered conf", len(secret))
			}
		}
	})
}

func mustMarshal(t *testing.T, f File) []byte {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		if err.Error() != "mpd conf: refusing to serialise a password" && leaked(err, f) {
			t.Fatalf("marshal error carries a password")
		}
		return []byte(err.Error())
	}
	return b
}

func stripFixed(s string) string {
	for _, w := range []string{
		"passwordSet", "passwordCount", "password:",
		"refusing to serialise a password",
	} {
		s = strings.ReplaceAll(s, w, "")
	}
	return s
}

func leaked(err error, f File) bool {
	for _, secret := range f.passwordValues() {
		if len(secret) >= 8 && strings.Contains(err.Error(), secret) {
			return true
		}
	}
	return false
}

func stripPublic(s string, f File) string {
	vals := []string{
		f.Path, f.MusicDirectory, f.SamplerateConverter, f.ReplayGain,
		f.VolumeNormalization, f.AudioOutputFormat, f.ZeroconfEnabled,
		f.Resampler.Plugin, f.Resampler.Quality,
	}
	vals = append(vals, f.BindAddresses...)
	for _, o := range f.Outputs {
		vals = append(vals, o.Type, o.Name, o.Device, o.Path, o.DOP,
			o.MixerType, o.MixerControl, o.MixerDevice, o.Enabled, o.Format, o.AllowedFormats)
	}
	for _, flt := range f.Filters {
		vals = append(vals, flt.Name, flt.Plugin)
	}
	if f.Port != nil {
		vals = append(vals, fmt.Sprint(*f.Port))
	}
	for _, v := range vals {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "")
		}
	}
	return s
}
