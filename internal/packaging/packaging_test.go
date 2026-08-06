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
		// ExecStart is QUOTED — it is one of the settings systemd
		// documents as taking a shell-style quoted command line, so
		// backslashes, CR/LF, and embedded quotes go through
		// systemdEscapeExec before landing between the quotes.
		`ExecStart="/usr/local/bin/bridge" serve --config "/home/me/.config/1-bit-bridge/bridge.yaml"`,
		// The path settings are UNQUOTED — they get no quote removal, so
		// a leading `"` would land inside the path and make the unit fail
		// to load. See TestRenderSystemdTemplate_PathSettingsAreUnquoted.
		`WorkingDirectory=/home/me/.config/1-bit-bridge/data`,
		`StandardOutput=append:/home/me/.local/state/1-bit-bridge/bridge.log`,
		`StandardError=append:/home/me/.local/state/1-bit-bridge/bridge.log`,
		"WantedBy=default.target",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing %q\n--\n%s", want, s)
		}
	}
}

// TestRenderSystemdTemplate_EscapesBadChars pins the escape contract
// for values that would otherwise break the unit-file parser: a path
// containing a double quote, a backslash, and a newline.
//
// The two settings families are handled differently BY DESIGN. Exec* is
// written between double quotes and does get systemd's quote removal +
// C-unescaping, so `"` and `\` are translated to the `\x` escapes its
// parser expects. The path settings are written unquoted and get NO
// unescaping layer, so those same bytes are ordinary path characters and
// must survive verbatim — only CR/LF/NUL are stripped, because a raw
// newline would end the directive and let the tail be parsed as a second
// one.
func TestRenderSystemdTemplate_EscapesBadChars(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: `/usr/local/bin/br"idge`,
		ConfigPath: `/weird\path/bridge.yaml`,
		WorkingDir: "/home/bad\npath",
		LogPath:    "/var/log/car\riage.log",
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
		`/usr/local/bin/br"idge`, // quote unescaped would terminate the Exec value
		"bad\npath",              // raw LF inside a path = second-directive injection
		"car\riage",              // raw CR, same class
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("rendered systemd still contains unescaped %q\n--\n%s", forbidden, s)
		}
	}
	// Exec*: the in-quote escaped forms must appear.
	for _, want := range []string{
		`br\"idge`,
		`/weird\\path`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing Exec escape %q\n--\n%s", want, s)
		}
	}
	// Path settings: CR/LF are STRIPPED, not translated to a `\n` /
	// `\r` escape — nothing unescapes those here, so the escape form
	// would land as two literal characters inside the path.
	for _, want := range []string{
		`WorkingDirectory=/home/badpath`,
		`StandardOutput=append:/var/log/cariage.log`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing stripped path form %q\n--\n%s", want, s)
		}
	}
	if strings.Contains(s, `bad\npath`) {
		t.Errorf("path setting emitted a literal `\\n` escape; nothing unescapes it there\n--\n%s", s)
	}
}

// TestRenderSystemdTemplate_PathSettingsAreUnquoted is the regression gate
// for the bug this contract exists to prevent: WorkingDirectory=,
// StandardOutput=append: and StandardError=append: were emitted wrapped in
// double quotes, and systemd does NOT strip quotes from those settings —
// systemd.syntax(5) says the shell-style quoting "is not used for all
// settings, but only for those documented as such", and systemd.exec(5)
// documents it for Exec*= only. Their parsers pass the raw rvalue to
// path_simplify_and_warn with PATH_CHECK_ABSOLUTE|PATH_CHECK_FATAL, so a
// leading `"` makes the path non-absolute and the whole unit fails to
// load — `bridge init --service` on Linux then reports a
// `systemctl enable --now` failure and leaves no running service.
//
// Asserts on the RENDERED OUTPUT (not the template text) and covers the
// awkward-but-legal path shape: a space (a path setting takes the rest of
// the line verbatim, so the space needs no quoting) plus a literal `%`
// (which still must be doubled — specifier expansion DOES reach these
// settings).
func TestRenderSystemdTemplate_PathSettingsAreUnquoted(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/srv/My Music 50%/bridge.yaml",
		WorkingDir: "/srv/My Music 50%/data",
		LogPath:    "/var/log/My Music 50%/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)

	// No quote may follow the `=` (or the `append:`) on any path setting.
	for _, badPrefix := range []string{
		`WorkingDirectory="`,
		`StandardOutput=append:"`,
		`StandardError=append:"`,
	} {
		if strings.Contains(s, badPrefix) {
			t.Errorf("path setting is quoted (%s…) — systemd does not strip these quotes and the unit will fail to load\n--\n%s",
				badPrefix, s)
		}
	}

	// Spaces survive verbatim; `%` is still doubled.
	for _, want := range []string{
		`WorkingDirectory=/srv/My Music 50%%/data`,
		`StandardOutput=append:/var/log/My Music 50%%/bridge.log`,
		`StandardError=append:/var/log/My Music 50%%/bridge.log`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing %q\n--\n%s", want, s)
		}
	}

	// ExecStart keeps its quotes — it IS one of the settings systemd
	// documents as taking a quoted command line, and without them a
	// binary path containing a space would split into extra argv entries.
	if !strings.Contains(s, `ExecStart="/usr/local/bin/bridge" serve --config "/srv/My Music 50%%/bridge.yaml"`) {
		t.Errorf("ExecStart must stay quoted (and %%-doubled)\n--\n%s", s)
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
	if !strings.Contains(s, `WorkingDirectory=/opt/My$Music/data`) {
		t.Errorf("WorkingDirectory should keep a single $ (no env-var substitution there)\n--\n%s", s)
	}
	if strings.Contains(s, `WorkingDirectory=/opt/My$$Music/data`) {
		t.Errorf("WorkingDirectory must NOT double $\n--\n%s", s)
	}
	// StandardOutput/StandardError are also non-Exec path settings.
	if !strings.Contains(s, `StandardOutput=append:/var/log/My$Music/bridge.log`) {
		t.Errorf("StandardOutput should keep a single $\n--\n%s", s)
	}
	if strings.Contains(s, `/var/log/My$$Music/bridge.log`) {
		t.Errorf("StandardOutput/StandardError must NOT double $\n--\n%s", s)
	}
}

// TestRenderSystemdTemplate_RejectsTrailingBackslashPath pins the one path
// shape the unquoted form cannot represent. systemd joins a line ending in
// `\` with the next one, replacing the backslash with a space, so
// `WorkingDirectory=/srv/x\` would absorb the `Restart=always` beneath it:
// the unit loads, the working directory is wrong, and auto-restart is gone
// with no diagnostic at all.
//
// No escape exists — these settings get no unquoting layer, so `\\` still
// ends the line with a backslash — and silently trimming would hand systemd
// a path the operator never configured. Rendering must fail instead, which
// aborts `installSystemd` before it writes the file.
func TestRenderSystemdTemplate_RejectsTrailingBackslashPath(t *testing.T) {
	base := Params{
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/home/me/.config/1-bit-bridge/bridge.yaml",
		WorkingDir: "/home/me/.config/1-bit-bridge/data",
		LogPath:    "/var/log/bridge.log",
	}

	for _, tc := range []struct {
		name  string
		apply func(*Params)
	}{
		{"WorkingDirectory", func(p *Params) { p.WorkingDir = `/home/me/data\` }},
		{"StandardOutput/StandardError", func(p *Params) { p.LogPath = `/var/log/bridge\` }},
		// A doubled backslash is NOT an escape here — the line still ends
		// in one, so systemd still continues it.
		{"doubled backslash", func(p *Params) { p.WorkingDir = `/home/me/data\\` }},
		// Stripping CR/LF happens first, so a trailing backslash hiding
		// behind a newline must still be caught.
		{"backslash then newline", func(p *Params) { p.WorkingDir = "/home/me/data\\\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.apply(&p)
			body, err := render("systemd.service.tmpl", p)
			if err == nil {
				t.Fatalf("render accepted a trailing-backslash path; the unit would swallow its next directive\n--\n%s", body)
			}
			if !strings.Contains(err.Error(), "backslash") {
				t.Errorf("error should name the cause, got %v", err)
			}
		})
	}

	// An interior backslash is an ordinary path byte and must still pass
	// through verbatim — only the line-ending case is dangerous.
	p := base
	p.WorkingDir = `/home/me/we\ird/data`
	body, err := render("systemd.service.tmpl", p)
	if err != nil {
		t.Fatalf("interior backslash must be accepted: %v", err)
	}
	if !strings.Contains(string(body), `WorkingDirectory=/home/me/we\ird/data`) {
		t.Errorf("interior backslash should survive verbatim\n--\n%s", body)
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
