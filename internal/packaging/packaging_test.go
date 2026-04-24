package packaging

import (
	"strings"
	"testing"
)

func TestRenderLaunchdTemplate(t *testing.T) {
	body, err := render("launchd.plist.tmpl", Params{
		Label:      ServiceLabel,
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/Users/me/Library/Application Support/1-bit-bridge/bridge.yaml",
		WorkingDir: "/Users/me/Library/Application Support/1-bit-bridge/data",
		LogPath:    "/Users/me/Library/Logs/1-bit-bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"<plist version=\"1.0\">",
		ServiceLabel,
		"<string>serve</string>",
		"--config",
		"/Users/me/Library/Application Support/1-bit-bridge/bridge.yaml",
		"KeepAlive",
		"RunAtLoad",
		"SuccessfulExit",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered launchd missing %q\n--\n%s", want, s)
		}
	}
}

func TestRenderSystemdTemplate(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/home/me/.config/1-bit-bridge/bridge.yaml",
		WorkingDir: "/home/me/.config/1-bit-bridge/data",
		LogPath:    "/home/me/.local/state/1-bit-bridge/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Restart=always",
		// Quoted form pins the escaping contract: backslashes, CR/LF,
		// and embedded quotes go through systemdEscape before landing
		// between the quotes, so systemd can parse the unquoted value
		// unambiguously. Plain ASCII paths like this one pass through
		// verbatim.
		`ExecStart="/usr/local/bin/bridge" serve --config "/home/me/.config/1-bit-bridge/bridge.yaml"`,
		`WorkingDirectory="/home/me/.config/1-bit-bridge/data"`,
		`StandardOutput=append:"/home/me/.local/state/1-bit-bridge/bridge.log"`,
		"WantedBy=default.target",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing %q\n--\n%s", want, s)
		}
	}
}

// TestRenderSystemdTemplate_EscapesBadChars pins the escape contract
// for values that would otherwise break the unit-file parser: a path
// containing a double quote, a backslash, and a newline. Each must be
// translated to the systemd `\x` escape rather than surviving raw.
func TestRenderSystemdTemplate_EscapesBadChars(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: `/usr/local/bin/br"idge`,
		ConfigPath: `/weird\path/bridge.yaml`,
		WorkingDir: "/home/bad\npath",
		LogPath:    "/var/log/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	// Literal raw characters must NOT leak through. "bad\npath" with a
	// real LF is the escape-leakage signature we care about; the check
	// is intentionally specific so the template's own newlines between
	// directives don't trip it.
	for _, forbidden := range []string{
		`/usr/local/bin/br"idge`, // quote unescaped would terminate the value
		"bad\npath",              // raw LF inside the value = escape leaked
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("rendered systemd still contains unescaped %q\n--\n%s", forbidden, s)
		}
	}
	// Escaped forms must appear.
	for _, want := range []string{
		`br\"idge`,
		`/weird\\path`,
		`bad\npath`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing escape %q\n--\n%s", want, s)
		}
	}
}

func TestRenderStartupCmd_Windows(t *testing.T) {
	body, err := render("startup.cmd.tmpl", Params{
		BinaryPath: `C:\Program Files\1-bit-bridge\bridge.exe`,
		ConfigPath: `C:\Users\me\AppData\Local\1-bit-bridge\bridge.yaml`,
		LogPath:    `C:\Users\me\AppData\Local\1-bit-bridge\bridge.log`,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`@echo off`,
		`start "1-bit-bridge" /min`,
		`"C:\Program Files\1-bit-bridge\bridge.exe"`,
		`"C:\Users\me\AppData\Local\1-bit-bridge\bridge.yaml"`,
		`1>>"C:\Users\me\AppData\Local\1-bit-bridge\bridge.log"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered startup.cmd missing %q\n--\n%s", want, s)
		}
	}
}

func TestRenderStartupCmd_EscapesEmbeddedQuote(t *testing.T) {
	// Pathological path — unlikely but cheap to guard. cmd.exe's in-
	// quote escape for `"` is `""` (not backslash), so an embedded
	// quote must appear as `""` in the rendered launcher.
	body, err := render("startup.cmd.tmpl", Params{
		BinaryPath: `C:\weird\br"idge.exe`,
		ConfigPath: `C:\config\bridge.yaml`,
		LogPath:    `C:\log\bridge.log`,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `br""idge.exe`) {
		t.Errorf("expected doubled-quote escape, got:\n%s", s)
	}
}

func TestDefaultConfigDirNotEmpty(t *testing.T) {
	d, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if d == "" {
		t.Error("DefaultConfigDir empty")
	}
	if !strings.Contains(d, "1-bit-bridge") {
		t.Errorf("DefaultConfigDir = %q, missing project name", d)
	}
}

func TestDefaultLogPathNotEmpty(t *testing.T) {
	p, err := DefaultLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if p == "" || !strings.Contains(p, "1-bit-bridge") {
		t.Errorf("DefaultLogPath = %q", p)
	}
}
