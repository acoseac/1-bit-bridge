package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// B882L: the redacted log export and the bug-report bundle drop a DAC's
// product string from the player's lines that name a DAC (the dac attribute
// of cmd/bridge's DSD migration lines), keeping its USB IDs, whichever of
// slog's handlers wrote the line; the unredacted export is left as it is.

// dacLineIDs is the USB IDs a dac attribute begins with.
const dacLineIDs = "20b1:3009"

// dacLineProducts are product strings a unit's descriptor may hold, as the
// lines written before B882L carried them after the IDs: with a quote, a
// trailing backslash, an escaped quote, a quoted path with text after it, and
// a serial. dacprofile's checkProduct refuses none of them.
var dacLineProducts = []string{
	`Model "Quoted" DAC`,
	`Model Trailing\`,
	`Model \"Both\" DAC`,
	`Model "/srv/Music" TAIL`,
	`GO bar GOBAR-7Q2X-91KZ-4471`,
}

// dacLineHandlers are slog's two handlers, as logging.Init installs them.
var dacLineHandlers = map[string]func(*bytes.Buffer) slog.Handler{
	"text": func(b *bytes.Buffer) slog.Handler {
		return slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo})
	},
	"json": func(b *bytes.Buffer) slog.Handler {
		return slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo})
	},
}

// dacLine is the line the handler kind writes for a migration line naming
// the DAC dac, at at, with attributes before it and after it, as the player's
// component logger writes it.
func dacLine(t *testing.T, kind string, at time.Time, dac string) string {
	t.Helper()
	var b bytes.Buffer
	h := dacLineHandlers[kind](&b).WithAttrs([]slog.Attr{slog.String("component", "player")})
	r := slog.NewRecord(at, slog.LevelInfo, "player: the legacy DSD keys were migrated into the player's DSD state", 0)
	r.AddAttrs(slog.String("defaultMode", "auto"), slog.String("dac", dac), slog.String("choice", "native"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// dacLineRedacted is the line a redaction must give for one written with dac
// IDs and a product string: the line written with the IDs alone, which the
// text handler leaves unquoted, with its value quoted as the line holds it.
func dacLineRedacted(t *testing.T, kind string, at time.Time) string {
	t.Helper()
	ids := dacLine(t, kind, at, dacLineIDs)
	if kind == "json" {
		return ids
	}
	bare := " dac=" + dacLineIDs + " "
	if strings.Count(ids, bare) != 1 {
		t.Fatalf("the text handler wrote %q, want the IDs bare once", ids)
	}
	return strings.Replace(ids, bare, ` dac="`+dacLineIDs+`" `, 1)
}

// dacLogLines are the lines of a download that a handler wrote: everything
// but the bundle's sections and the footers.
func dacLogLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "time=") || strings.HasPrefix(l, "{") {
			out = append(out, l)
		}
	}
	return out
}

// dacLogFixture is a log of every product string's line from both handlers,
// then each handler's line with the IDs alone, which B882L's lines write
// where they drop the product string; and the lines a redaction must give,
// in the same order.
func dacLogFixture(t *testing.T) (log string, written, redacted []string) {
	t.Helper()
	at := time.Now().Add(-time.Minute)
	for _, kind := range []string{"text", "json"} {
		for _, p := range dacLineProducts {
			written = append(written, dacLine(t, kind, at, dacLineIDs+" "+p))
			redacted = append(redacted, dacLineRedacted(t, kind, at))
		}
		ids := dacLine(t, kind, at, dacLineIDs)
		written, redacted = append(written, ids), append(redacted, ids)
	}
	return strings.Join(written, "\n") + "\n", written, redacted
}

// requireDACLines compares a download's log lines with want, one by one, and
// requires that no product string, or a part of one, is left in them.
func requireDACLines(t *testing.T, download, body string, want []string) {
	t.Helper()
	got := dacLogLines(body)
	if len(got) != len(want) {
		t.Fatalf("the %s holds %d log lines, want %d:\n%s", download, len(got), len(want), body)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("the %s's line %d is\n%s\nwant\n%s", download, i, got[i], want[i])
		}
	}
	lines := strings.Join(got, "\n")
	for _, part := range []string{"Model", "Quoted", "Trailing", "Both", "TAIL", "srv", "<path>", "GO bar", "GOBAR", "7Q2X"} {
		if strings.Contains(lines, part) {
			t.Errorf("the %s keeps %q of a product string:\n%s", download, part, lines)
		}
	}
}

// TestTheRedactedExportDropsADACsProductString: the lines written before
// B882L, which name a DAC by its USB IDs and its product string, come out of
// the redacted export with the IDs alone, from slog's text handler and its
// JSON handler, whatever quotes and backslashes the product string holds and
// a quoted path inside it included; the message and the attributes on both
// sides of dac come out as they were, and a line naming a DAC by its IDs
// alone, the text handler's unquoted, is left as it is.
func TestTheRedactedExportDropsADACsProductString(t *testing.T) {
	log, _, want := dacLogFixture(t)
	s := logTestServer(t, log)
	rec := doGet(t, s, "/api/logs/export?level=all&since=all&redact=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	requireDACLines(t, "redacted export", rec.Body.String(), want)
}

// TestTheBundleDropsADACsProductString: the bug-report bundle, redacted by
// default, gives the same lines as the redacted export.
func TestTheBundleDropsADACsProductString(t *testing.T) {
	log, _, want := dacLogFixture(t)
	s := logTestServer(t, log)
	rec := doGet(t, s, "/api/logs/bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	requireDACLines(t, "bundle", rec.Body.String(), want)
}

// TestTheUnredactedExportKeepsADACsProductString: the export without
// redaction, the operator's own choice and its default, and the bundle with
// redaction turned off, give every line as it was written, byte for byte.
func TestTheUnredactedExportKeepsADACsProductString(t *testing.T) {
	log, written, _ := dacLogFixture(t)
	s := logTestServer(t, log)
	for _, path := range []string{"/api/logs/export?level=all&since=all", "/api/logs/export?level=all&since=all&redact=false", "/api/logs/bundle?redact=false"} {
		got := dacLogLines(doGet(t, s, path).Body.String())
		if strings.Join(got, "\n") != strings.Join(written, "\n") {
			t.Errorf("%s gives\n%s\nwant the lines as written\n%s", path, strings.Join(got, "\n"), strings.Join(written, "\n"))
		}
	}
}

// TestTheDACRuleReadsTheWholeQuotedValue: the rule takes any spacing around
// the key and the = or the :, slog's handlers writing none, reads the quoted
// value to its closing quote, escapes included, runs before the path rule,
// which still redacts a path outside it, and leaves a dac value that is not
// quoted, or does not begin with the USB IDs, as it is. These lines are
// written by hand: no slog handler writes their spacing.
func TestTheDACRuleReadsTheWholeQuotedValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`msg=x dac = "20b1:3009 GO bar" err=y`, `msg=x dac = "20b1:3009" err=y`},
		{`msg=x dac= "20b1:3009 GO bar" err=y`, `msg=x dac= "20b1:3009" err=y`},
		{"msg=x\tdac =\"20b1:3009 GO bar\" err=y", "msg=x\tdac =\"20b1:3009\" err=y"},
		{`dac="20b1:3009 GO bar" err=y`, `dac="20b1:3009" err=y`},
		{`{"msg":"x", "dac" : "20b1:3009 GO bar", "err":"y"}`, `{"msg":"x", "dac" : "20b1:3009", "err":"y"}`},
		{`{"dac":"20b1:3009 Model \"/srv/Music\" TAIL","err":"y"}`, `{"dac":"20b1:3009","err":"y"}`},
		{`msg=x dac="20b1:3009 Model \"/srv/Music\" TAIL" state="/srv/data/player-dsd.json"`, `msg=x dac="20b1:3009" state="<path>"`},
		{`msg=x dac="20b1:3009 a\\" b="c d"`, `msg=x dac="20b1:3009" b="c d"`},
		{`msg=x dac=20b1:3009 err=y`, `msg=x dac=20b1:3009 err=y`},
		{`{"dac":"20b1:3009","err":"y"}`, `{"dac":"20b1:3009","err":"y"}`},
		{`msg=x dac="some other words" err=y`, `msg=x dac="some other words" err=y`},
		{`msg="a \"dac\":\"20b1:3009 X\" b" err=y`, `msg="a \"dac\":\"20b1:3009 X\" b" err=y`},
		{`msg="a dac=\"20b1:3009 X\" b" err=y`, `msg="a dac=\"20b1:3009 X\" b" err=y`},
		{`msg=x xdac="20b1:3009 GO bar"`, `msg=x xdac="20b1:3009 GO bar"`},
	} {
		if got := redactLogLine(tc.in); got != tc.want {
			t.Errorf("redactLogLine(%q)\n= %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// TestADACLineFixtureIsWhatTheHandlersWrite pins the fixture's own premise:
// each line it writes with a product string holds that product string as
// the handler escaped it, inside one quoted value that reads back whole.
func TestADACLineFixtureIsWhatTheHandlersWrite(t *testing.T) {
	at := time.Now()
	for _, p := range dacLineProducts {
		text := dacLine(t, "text", at, dacLineIDs+" "+p)
		if !strings.Contains(text, " dac="+strconv.Quote(dacLineIDs+" "+p)+" ") {
			t.Errorf("the text handler wrote %q, want the value quoted whole", text)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(dacLine(t, "json", at, dacLineIDs+" "+p)), &m); err != nil || m["dac"] != dacLineIDs+" "+p {
			t.Errorf("the JSON handler's line reads back %v (%v), want the label whole", m["dac"], err)
		}
	}
}
