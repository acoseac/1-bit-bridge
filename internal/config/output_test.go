package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOutputOnlyConfigHasNoRoots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\ndataDir: " + filepath.Join(dir, "data") + "\noutput:\n  mpd: on\n  endpoint: 127.0.0.1:6600\n  password: \"s3cret-value\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Output.Mode() != "on" || cfg.Output.Endpoint != "127.0.0.1:6600" || cfg.Output.Password != "s3cret-value" {
		t.Fatalf("output = %#v", cfg.Output)
	}
	if len(cfg.LibraryRoots) != 0 {
		t.Fatalf("roots = %v", cfg.LibraryRoots)
	}
	if errs := cfg.CheckLibraryRootsAccessible(); len(errs) != 0 {
		t.Fatalf("accessibility: %v", errs)
	}
	if strings.Contains(errString(cfg.Validate()), "s3cret-value") {
		t.Fatal("validate named the password")
	}
}

// TestOutputLoopbackPort: the loopback track route's port is 7791 when the
// config does not name one, 0 asks the kernel for one, and a value that is
// not a port is refused at load. MPD keeps the route's URLs in its queue,
// so the default is a fixed port, not a kernel pick.
func TestOutputLoopbackPort(t *testing.T) {
	base := Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"}}
	if got := base.Output.EffectiveLoopbackPort(); got != 7791 || DefaultLoopbackPort != 7791 {
		t.Fatalf("default port %d (const %d), want 7791", got, DefaultLoopbackPort)
	}
	for _, p := range []int{0, 1, 7801, 65535} {
		cfg := base
		v := p
		cfg.Output.LoopbackPort = &v
		if err := cfg.Validate(); err != nil {
			t.Fatalf("port %d: %v", p, err)
		}
		if got := cfg.Output.EffectiveLoopbackPort(); got != p {
			t.Fatalf("port %d read back as %d", p, got)
		}
	}
	for _, p := range []int{-1, 65536, 1 << 20} {
		cfg := base
		v := p
		cfg.Output.LoopbackPort = &v
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "output.loopbackPort") {
			t.Fatalf("port %d: validate %v, want a refusal naming output.loopbackPort", p, err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\nlibraryRoots: [" + dir + "]\ndataDir: " + filepath.Join(dir, "data") + "\noutput:\n  loopbackPort: 7801\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Output.EffectiveLoopbackPort(); got != 7801 {
		t.Fatalf("yaml port %d, want 7801", got)
	}
	t.Setenv("BRIDGE_OUTPUT_LOOPBACK_PORT", "7802")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if got := cfg.Output.EffectiveLoopbackPort(); got != 7802 {
		t.Fatalf("env port %d, want 7802", got)
	}
}

func TestOutputModeRejectsAnythingElse(t *testing.T) {
	cfg := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"}, Output: OutputConfig{MPD: "yes", Password: "s3cret-value"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "output.mpd") {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatal("the error names the password")
	}
	for _, mode := range []string{"", "auto", "AUTO", " on ", "on", "off"} {
		cfg.Output.MPD = mode
		want := "auto"
		if mode == " on " || mode == "on" {
			want = "on"
		} else if mode == "off" {
			want = "off"
		}
		if cfg.Output.Mode() != want {
			t.Fatalf("mode %q -> %q", mode, cfg.Output.Mode())
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate %q: %v", mode, err)
		}
	}
}

// TestOutputRefusalsCarryNoValue: Validate's refusals of the output block
// name the setting and never its value, since what is refused is often
// something pasted onto the wrong line (MPD's password or a DAC's serial on
// output.mpd's, a serial of digits on output.loopbackPort's), and bridge
// serve, bridge doctor and init print the refusal; init's rewrite took its
// warning from it (B505's round-2 review). Through Validate and through Load,
// and through Load for a value yaml.v3 itself refuses, whose own error quoted
// a value of ten bytes or fewer whole and the first seven of a longer one,
// a value tagged !!map whole, a value its explicit tag does not fit whole,
// and the alias that an unquoted password starting with * is (round 3).
// Each of those is checked to carry the value in yaml.v3's own error first,
// so the case is one the refusal has to change.
func TestOutputRefusalsCarryNoValue(t *testing.T) {
	digits, past, negative := 202311050044, 70000, -5
	base := Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"}}
	for _, tc := range []struct {
		name            string
		output          OutputConfig
		setting, secret string
	}{
		{"MPD's password on mpd's line", OutputConfig{MPD: "mpd-kitchen-pw-8812"}, "output.mpd", "mpd-kitchen-pw-8812"},
		{"a serial on mpd's line", OutputConfig{MPD: playerSerial}, "output.mpd", playerSerial},
		{"a word mpd does not take", OutputConfig{MPD: "maybe"}, "output.mpd", "maybe"},
		{"a serial of digits as the port", OutputConfig{LoopbackPort: &digits}, "output.loopbackPort", "202311050044"},
		{"a port past 65535", OutputConfig{LoopbackPort: &past}, "output.loopbackPort", "70000"},
		{"a negative port", OutputConfig{LoopbackPort: &negative}, "output.loopbackPort", "-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Output = tc.output
			err := cfg.Validate()
			if err == nil || !strings.HasPrefix(err.Error(), tc.setting+": ") {
				t.Fatalf("Validate = %v, want a refusal naming %s", err, tc.setting)
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("the refusal carries the value: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name, output, secret string
		// yamlRefuses is whether yaml.v3 refuses the block itself, and so
		// whether its own error must carry the value for the case to count.
		yamlRefuses bool
		says        string
	}{
		{"MPD's password on mpd's line", "\n  mpd: mpd-kitchen-pw-8812", "mpd-kitchen-pw-8812", false, "output.mpd: that value is not auto, on or off"},
		{"a password of ten bytes or fewer as the port", "\n  loopbackPort: p4ssw0rd", "p4ssw0rd", true, "cannot unmarshal a !!str value into int"},
		{"a longer password as the port", "\n  loopbackPort: mpd-kitchen-pw-8812", "mpd-kitchen-pw-8812", true, "cannot unmarshal a !!str value into int"},
		{"a lettered serial as the port", "\n  loopbackPort: " + playerSerial, playerSerial, true, "cannot unmarshal a !!str value into int"},
		{"a password as the whole block", " p4ssw0rd", "p4ssw0rd", true, "cannot unmarshal a !!str value into config.OutputConfig"},
		{"a longer password as the whole block", " mpd-kitchen-pw-8812", "mpd-kitchen-pw-8812", true, "cannot unmarshal a !!str value into config.OutputConfig"},
		{"a password over two lines as the port", "\n  loopbackPort: |\n    p4ss\n    w0rd", "p4ss\nw0rd", true, "cannot unmarshal a !!str value into int"},
		{"a password tagged as a block on the port's line", "\n  loopbackPort: !!map p4ssw0rd", "p4ssw0rd", true, "cannot unmarshal that value into int"},
		{"a password starting with ! on the port's line", "\n  loopbackPort: !p4ssw0rd", "p4ssw0rd", true, "cannot unmarshal that value into int"},
		{"a password its tag does not fit", "\n  password: !!int p4ssw0rd", "p4ssw0rd", true, "cannot decode a !!str value as a !!int"},
		{"a password starting with * left unquoted", "\n  password: *p4ssw0rd", "p4ssw0rd", true, "an alias names an anchor the file does not define"},
		{"a password written alone in a flow block", " {mpd: on, p4ssw0rd}", "p4ssw0rd", true, "that key is not a field of type config.OutputConfig"},
	} {
		t.Run("Load: "+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\nlibraryRoots: [" + dir + "]\ndataDir: " + filepath.Join(dir, "data") +
				"\noutput:" + tc.output + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.yamlRefuses {
				dec := yaml.NewDecoder(strings.NewReader(body))
				dec.KnownFields(true)
				if err := dec.Decode(&Config{}); err == nil || !carriesPartOf(err.Error(), tc.secret) {
					t.Fatalf("yaml.v3's own error is %v: the case does not carry the value there, so it pins nothing", err)
				}
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Load = %v, want a refusal saying %q", err, tc.says)
			}
			if carriesPartOf(refusalText(err, path), tc.secret) {
				t.Errorf("Load's refusal carries part of the value: %v", err)
			}
		})
	}
}

func TestAutoStillRequiresARoot(t *testing.T) {
	cfg := &Config{ListenAddress: ":7788", ScanIntervalSec: 3600, Output: OutputConfig{MPD: "auto"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "libraryRoots") {
		t.Fatalf("auto with no roots: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
