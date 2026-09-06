package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nonTestGoFilesInPackage lists the package's production .go files by base
// name. A directory read rather than a hardcoded list: a new file that
// reintroduces the bug should be caught without anyone remembering to add it.
func nonTestGoFilesInPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	if len(out) < 20 {
		t.Fatalf("only %d production files found — the scan is not seeing the package", len(out))
	}
	return out
}

// readPackageFile returns one production file's source with // and /* */
// comments stripped, so the guard cannot fire on prose. This package's
// commentary names the very symbols it discusses — including, now, this one.
func readPackageFile(t *testing.T, base string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(base))
	if err != nil {
		t.Fatalf("read %s: %v", base, err)
	}
	return stripGoComments(string(b))
}

// stripGoComments removes block and line comments while respecting string and
// rune literals — Go raw strings contain both `//` and `/*` freely, and a
// naive strip would corrupt the code around them.
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	const (
		code = iota
		lineComment
		blockComment
		str
		rawStr
		runeLit
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				i++
			case c == '"':
				state = str
				b.WriteByte(c)
			case c == '`':
				state = rawStr
				b.WriteByte(c)
			case c == '\'':
				state = runeLit
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				b.WriteByte(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = code
				i++
			}
		case str, runeLit:
			// Contents dropped, delimiters kept. A production file whose
			// error text or log message happens to contain "config.Load("
			// is not a caller, and a guard that cries wolf gets deleted —
			// which is the outcome this exists to prevent.
			q := byte('"')
			if state == runeLit {
				q = '\''
			}
			if c == '\\' && i+1 < len(src) {
				i++
			} else if c == q {
				state = code
				b.WriteByte(c)
			}
		case rawStr:
			if c == '`' {
				state = code
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// The `--config` flag defaults to the EMPTY string on every subcommand.
// Sixteen files in this package therefore route through loadCLIConfig, which
// resolves that empty default per resolveConfigPath (explicit → ./bridge.yaml
// → the platform config dir). Two shared tails did not, and handed "" straight
// to config.Load — so `bridge token {list,rotate,expire,revoke}`,
// `bridge upscale` and `bridge optimize` all died with
//
//	config load failed: read config "": open : no such file or directory
//
// on a host with a perfectly good platform install, while their own flag help
// promised exactly the fallback they were not doing. `bridge token revoke` is
// the documented recovery path for an orphaned token, which makes it the one
// command an operator reaches for when something is already wrong.
//
// These tests assert on the CONFIG-RESOLUTION step reaching the platform
// install, not on each command succeeding outright: `bridge optimize` still
// legitimately refuses when `upscale.enabled` is false, and conflating the two
// refusals is how the original defect stayed readable as "expected".

// TestStripGoCommentsDropsProseAndLiterals pins the two things the guard's
// scanner has to get right, because both were review findings rather than
// hypotheticals.
//
// Comments: this package's commentary names the symbols it discusses — the very
// fix that prompted the guard is explained in a comment mentioning
// config.Load — so a raw text scan reports its own documentation as a caller.
//
// String contents: a production file whose error text or log message happens to
// contain the call spelling is not a caller either. A guard that cries wolf gets
// deleted, which is the outcome it exists to prevent. (Gemini, PR #853.)
func TestStripGoCommentsDropsProseAndLiterals(t *testing.T) {
	src := "package p\n" +
		"// we deliberately avoid config.Load( here\n" +
		"/* and config.Load( in a block comment */\n" +
		"var msg = \"use config.Load( carefully\"\n" +
		"var raw = `also config.Load( in a raw string`\n" +
		"func f() { realCall() }\n"
	got := stripGoComments(src)
	if strings.Contains(got, "config.Load(") {
		t.Errorf("stripGoComments left a config.Load( mention that is prose or literal text:\n%s", got)
	}
	if !strings.Contains(got, "realCall()") {
		t.Errorf("stripGoComments dropped actual code:\n%s", got)
	}

	// And it must NOT hide a real call — the guard is worthless if the
	// stripper eats the thing it is looking for.
	if !strings.Contains(stripGoComments("package p\nfunc f() { config.Load(x) }\n"), "config.Load(") {
		t.Error("stripGoComments removed a genuine call")
	}
}

// TestTokenCmdResolvesPlatformConfigWithoutExplicitFlag pins the token tail.
func TestTokenCmdResolvesPlatformConfigWithoutExplicitFlag(t *testing.T) {
	_, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "some-track.flac")

	var so, se bytes.Buffer
	code := tokenListCmd(nil, &so, &se)
	if code != 0 {
		t.Fatalf("token list exit %d with a platform install present and no --config; stderr:\n%s",
			code, se.String())
	}
	if strings.Contains(se.String(), `read config ""`) {
		t.Errorf("token list passed the flag's empty default straight to config.Load:\n%s", se.String())
	}
}

// TestTranscodeCmdResolvesPlatformConfigWithoutExplicitFlag pins the tail
// shared by `bridge upscale` and `bridge optimize`.
//
// The install written here leaves `upscale.enabled` false, so the command is
// expected to refuse — the assertion is that it refuses for the RIGHT reason,
// having found and read the platform config first.
func TestTranscodeCmdResolvesPlatformConfigWithoutExplicitFlag(t *testing.T) {
	_, platform := isolateConfigEnv(t)
	writeInstallAt(t, platform, "some-track.flac")

	var se bytes.Buffer
	_, code := bootstrapTranscodeCmd(context.Background(), &se, "", "", false)
	if code == 0 {
		t.Fatalf("expected a refusal (the fixture leaves upscale.enabled false), got 0")
	}
	if strings.Contains(se.String(), `read config ""`) {
		t.Errorf("the transcode tail passed the flag's empty default straight to config.Load:\n%s", se.String())
	}
	if !strings.Contains(se.String(), "disabled in bridge.yaml") {
		t.Errorf("expected the feature-gate refusal, meaning the platform config was found and read; got:\n%s",
			se.String())
	}
}

// TestNoSubcommandTailBypassesLoadCLIConfig is the class guard. Both defects
// above were one call each, in a shared tail, invisible from the subcommand
// that owned the flag — so a fix that only corrects today's two sites leaves
// the next one to be found in production.
//
// The allowlist is the set of sites that legitimately call config.Load with an
// ALREADY-RESOLVED concrete path (loadCLIConfig's own body, plus the three
// callers that resolve first and pass the result). Anything else handing a
// flag value to config.Load is the bug this file exists for.
func TestNoSubcommandTailBypassesLoadCLIConfig(t *testing.T) {
	allowed := map[string]bool{
		// loadCLIConfig itself — the one place that MAY call config.Load
		// with a path it just resolved.
		"configpath.go": true,
		// These pass a concrete path they already hold, never a flag value:
		// init.go writes the file then reads it back; doctor.go probes each
		// candidate path in turn; menu.go holds packaging.IsInitialized()'s
		// platform path.
		"init.go":   true,
		"doctor.go": true,
		"menu.go":   true,
	}
	for _, f := range nonTestGoFilesInPackage(t) {
		if allowed[f] {
			continue
		}
		body := readPackageFile(t, f)
		if strings.Contains(body, "config.Load(") {
			t.Errorf("%s calls config.Load directly. A subcommand's --config flag defaults to "+
				"the empty string, so config.Load(*configPath) resolves nothing and the command "+
				"fails on any host where the operator did not pass --config explicitly. Use "+
				"loadCLIConfig, which implements the ./bridge.yaml-then-platform fallback the "+
				"flag help already promises. If this site genuinely holds an already-resolved "+
				"concrete path, add it to the allowlist above with the reason.", f)
		}
	}
}
