package packaging

import (
	"runtime"
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

// A literal percent in a path must be doubled to %% so systemd doesn't
// treat it as a specifier prefix (%h, %u, …) and fail unit parsing.
func TestRenderSystemdTemplate_EscapesPercent(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/opt/Top_100_10%_Off/bridge.yaml",
		WorkingDir: "/opt/Top_100_10%_Off/data",
		LogPath:    "/var/log/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "Top_100_10%_Off") {
		t.Errorf("rendered systemd left a bare %% (specifier prefix) in a path\n--\n%s", s)
	}
	if !strings.Contains(s, "Top_100_10%%_Off") {
		t.Errorf("rendered systemd missing the doubled %%%% escape\n--\n%s", s)
	}
}

// TestRenderSystemdTemplate_EscapesDollarInExecOnly pins the Exec-vs-path
// split. systemd applies environment-variable substitution ($VAR / ${VAR})
// to the arguments of the Exec* command lines AFTER quote removal (the
// surrounding double quotes do NOT suppress it), so a literal `$` in the
// binary/config path MUST be doubled to `$$`. But WorkingDirectory /
// StandardOutput / StandardError undergo specifier expansion only (no
// $-substitution), so a `$` there must survive verbatim — doubling it
// would corrupt the path.
func TestRenderSystemdTemplate_EscapesDollarInExecOnly(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: "/opt/My$Music/bridge",
		ConfigPath: "/opt/My$Music/bridge.yaml",
		WorkingDir: "/opt/My$Music/data",
		LogPath:    "/var/log/My$Music/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	// Exec* args: `$` doubled to `$$` (env-var substitution applies).
	if !strings.Contains(s, `ExecStart="/opt/My$$Music/bridge" serve --config "/opt/My$$Music/bridge.yaml"`) {
		t.Errorf("ExecStart did not double $ in path\n--\n%s", s)
	}
	// WorkingDirectory: single `$` preserved (specifier-only expansion).
	if !strings.Contains(s, `WorkingDirectory="/opt/My$Music/data"`) {
		t.Errorf("WorkingDirectory should keep a single $ (no env-var substitution there)\n--\n%s", s)
	}
	if strings.Contains(s, `WorkingDirectory="/opt/My$$Music/data"`) {
		t.Errorf("WorkingDirectory must NOT double $\n--\n%s", s)
	}
	// StandardOutput/StandardError are also non-Exec path settings.
	if !strings.Contains(s, `StandardOutput=append:"/var/log/My$Music/bridge.log"`) {
		t.Errorf("StandardOutput should keep a single $\n--\n%s", s)
	}
	if strings.Contains(s, `/var/log/My$$Music/bridge.log`) {
		t.Errorf("StandardOutput/StandardError must NOT double $\n--\n%s", s)
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

// TestRenderStartupCmd_EscapesPercent pins the batch-file percent escape:
// cmd.exe expands %VAR% even inside double quotes, so a path with a literal
// `%` (e.g. "C:\Music 50% Off\") must be doubled to `%%` in the .cmd file
// (cmd collapses `%%`→`%` at parse time, yielding the correct literal path).
func TestRenderStartupCmd_EscapesPercent(t *testing.T) {
	body, err := render("startup.cmd.tmpl", Params{
		BinaryPath: `C:\Music 50% Off\bridge.exe`,
		ConfigPath: `C:\config\bridge.yaml`,
		LogPath:    `C:\log\bridge.log`,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	// The bare single-% form (which cmd would treat as a var-expansion
	// trigger) must not survive; the doubled form must appear.
	if strings.Contains(s, `50% Off`) {
		t.Errorf("rendered startup.cmd left a bare %% (var-expansion trigger) in a path\n--\n%s", s)
	}
	if !strings.Contains(s, `C:\Music 50%% Off\bridge.exe`) {
		t.Errorf("rendered startup.cmd missing the doubled %%%% escape\n--\n%s", s)
	}
}

// TestCmdEscape_DoublesPercent pins the unit directly: the batch-file
// escaper doubles `%` (and still doubles `"`), so a path with both is safe
// inside a `.cmd` double-quoted argument.
func TestCmdEscape_DoublesPercent(t *testing.T) {
	got := CmdEscape(`50% "x"`)
	if got != `50%% ""x""` {
		t.Errorf("CmdEscape(`50%% \"x\"`) = %q, want `50%%%% \"\"x\"\"`", got)
	}
}

// TestCmdArgEscape_DoesNotDoublePercent pins the `cmd /c` command-line
// escaper used by SpawnDetached: unlike the batch-file CmdEscape it must
// NOT double `%` — cmd.exe doesn't collapse `%%` for a `/c` command line,
// so doubling would corrupt the path. Quotes are still doubled.
func TestCmdArgEscape_DoesNotDoublePercent(t *testing.T) {
	got := cmdArgEscape(`C:\Music 50% Off\br"idge.exe`)
	if strings.Contains(got, `%%`) {
		t.Errorf("cmdArgEscape doubled %% (wrong for cmd /c): %q", got)
	}
	if !strings.Contains(got, `50% Off`) {
		t.Errorf("cmdArgEscape should keep a single %%: %q", got)
	}
	if !strings.Contains(got, `br""idge.exe`) {
		t.Errorf("cmdArgEscape should still double the quote: %q", got)
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

// TestInstallStartupOnNonWindowsIsNoOp pins the cross-platform
// contract: on non-Windows, InstallStartup returns ("", nil) so the
// wizard's unified call site doesn't need a runtime.GOOS branch
// at every site that wants strict-Startup-folder semantics. The
// macOS / Linux paths each have a single install mode, so the
// "Startup folder" distinction has no analogue there.
func TestInstallStartupOnNonWindowsIsNoOp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows-only contract")
	}
	path, err := InstallStartup(Params{
		Label:      ServiceLabel,
		BinaryPath: "/tmp/bridge",
		ConfigPath: "/tmp/bridge.yaml",
		WorkingDir: "/tmp",
		LogPath:    "/tmp/bridge.log",
	})
	if err != nil {
		t.Errorf("InstallStartup error on %s: %v", runtime.GOOS, err)
	}
	if path != "" {
		t.Errorf("InstallStartup path on %s = %q, want empty", runtime.GOOS, path)
	}
}
