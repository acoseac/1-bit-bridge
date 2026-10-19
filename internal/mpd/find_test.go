package mpd

import (
	"context"
	"strings"
	"testing"
)

// TestFindQuotingRoundTrips is the two quoting levels MPD 0.24.4 applies
// to find. doc/protocol.rst "Escaping String Values" gives the worked
// example, and src/song/Filter.cxx ExpectQuoted is the filter level: a
// backslash escapes the next byte inside either quote. The protocol
// level is the same escape, around the whole expression, because the
// expression is one argument (the password line already quotes that
// way). A path is recovered only when both levels are undone.
func TestFindQuotingRoundTrips(t *testing.T) {
	cases := []struct {
		path string
		wire string
	}{
		{
			path: `foo'bar"`,
			wire: "find \"(file == \\\"foo\\\\'bar\\\\\\\"\\\")\"\n",
		},
		{path: `a"b`, wire: "find \"(file == \\\"a\\\\\\\"b\\\")\"\n"},
		{path: `a\b`, wire: "find \"(file == \\\"a\\\\\\\\b\\\")\"\n"},
		{path: `a'b`, wire: "find \"(file == \\\"a\\\\'b\\\")\"\n"},
		{path: `a)b`, wire: "find \"(file == \\\"a)b\\\")\"\n"},
		{
			path: `x") OR (file == "y`,
			wire: "find \"(file == \\\"x\\\\\\\") OR (file == \\\\\\\"y\\\")\"\n",
		},
	}
	for _, tc := range cases {
		line, err := commandLine("find", tc.path)
		if err != nil {
			t.Fatalf("find %q: %v", tc.path, err)
		}
		if string(line) != tc.wire {
			t.Errorf("find %q\n got %q\nwant %q", tc.path, line, tc.wire)
		}
		got, err := recoverFindPath(string(line))
		if err != nil {
			t.Fatalf("recover %q: %v", tc.path, err)
		}
		if got != tc.path {
			t.Errorf("recovered %q, want %q", got, tc.path)
		}
	}
}

func TestFindRefusesALineBreakOrNUL(t *testing.T) {
	fx := loadReplies(t)
	replies := fx.replies()
	replies["find"] = "OK\n"
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: replies})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	for _, path := range []string{"a\nb", "a\rb", "a\x00b", "has\nline"} {
		line, err := commandLine("find", path)
		if err == nil || err.Error() != "mpd: path refused" {
			t.Fatalf("find %q: line %q err %v", path, line, err)
		}
		srv.clear()
		if _, err := c.Find(context.Background(), path); err == nil || err.Error() != "mpd: path refused" {
			t.Fatalf("Find %q: %v", path, err)
		}
		if got := srv.lines(); len(got) != 0 {
			t.Fatalf("Find %q was written: %q", path, got)
		}
	}
}

func TestFindWritesTheQuotedLine(t *testing.T) {
	fx := loadReplies(t)
	replies := fx.replies()
	replies["find"] = "file: kept\nOK\n"
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: replies})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	path := `x") OR (file == "y`
	reply, err := c.Find(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if file, ok := reply.Get("file"); !ok || file != "kept" {
		t.Fatalf("reply = %#v", reply)
	}
	want, err := commandLine("find", path)
	if err != nil {
		t.Fatal(err)
	}
	got := srv.lines()
	if len(got) != 1 || got[0]+"\n" != string(want) {
		t.Fatalf("wire = %q, want %q", got, want)
	}
	if cmds := srv.commands(); len(cmds) != 1 || cmds[0] != "find" {
		t.Fatalf("commands = %v", cmds)
	}
}

// recoverFindPath undoes the protocol argument quote and then
// ExpectQuoted, the two readers in MPD 0.24.4. It is the test's check
// that a value such as x") OR (file == "y stays one string.
func recoverFindPath(line string) (string, error) {
	line = strings.TrimSuffix(line, "\n")
	name, arg, ok := strings.Cut(line, " ")
	if !ok || name != "find" {
		return "", errFindRecover
	}
	expr, err := unquoteProtocol(arg)
	if err != nil {
		return "", err
	}
	return unquoteFilterFile(expr)
}

var errFindRecover = errTest("mpd: not a find line")

type errTest string

func (e errTest) Error() string { return string(e) }

func unquoteProtocol(arg string) (string, error) {
	if len(arg) < 2 || arg[0] != '"' || arg[len(arg)-1] != '"' {
		return "", errTest("mpd: find argument is not quoted")
	}
	var b strings.Builder
	for i := 1; i < len(arg)-1; i++ {
		if arg[i] == '\\' {
			i++
			if i >= len(arg)-1 {
				return "", errTest("mpd: trailing escape")
			}
		}
		b.WriteByte(arg[i])
	}
	return b.String(), nil
}

// unquoteFilterFile reads (file == "...") the way ExpectQuoted does:
// the value runs until the matching quote, and a backslash takes the
// next byte literally.
func unquoteFilterFile(expr string) (string, error) {
	const prefix = `(file == "`
	if !strings.HasPrefix(expr, prefix) || !strings.HasSuffix(expr, `")`) {
		return "", errTest("mpd: expression is not one file comparison")
	}
	body := expr[len(prefix) : len(expr)-2]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' {
			i++
			if i >= len(body) {
				return "", errTest("mpd: filter escape at end")
			}
			b.WriteByte(body[i])
			continue
		}
		if body[i] == '"' {
			return "", errTest("mpd: filter string ended early")
		}
		b.WriteByte(body[i])
	}
	return b.String(), nil
}
