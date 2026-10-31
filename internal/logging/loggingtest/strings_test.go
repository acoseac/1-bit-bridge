package loggingtest

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// secretValuer is a LogValuer whose own text names nothing: only the value
// it resolves to holds the secret, as a guarded type's would not.
type secretValuer struct{ secret string }

func (v secretValuer) LogValue() slog.Value { return slog.StringValue("resolved " + v.secret) }

// detail is a struct a line could carry whole, formatted with %v or %+v.
type detail struct {
	Where string
	Count int
}

// TestStringsSaysEverythingAHandlerCouldWrite pins Strings and Carrying: a
// value is found in a record's message, a plain attribute, an error's text,
// each error errors.Join joins, a LogValuer's resolved value, a group's
// attributes, a struct as %v and %+v print it, a map as JSON encodes it, an
// attribute a logger binds with With, the name of a group it opens with
// WithGroup, and an attribute bound inside such a group, and a record that
// carries none of the values is not returned.
func TestStringsSaysEverythingAHandlerCouldWrite(t *testing.T) {
	cases := []struct {
		name  string
		log   func(*slog.Logger)
		value string
	}{
		{"the message", func(l *slog.Logger) { l.Info("refused /abs/message") }, "/abs/message"},
		{"a string attribute", func(l *slog.Logger) { l.Info("m", "path", "/abs/string") }, "/abs/string"},
		{"an error", func(l *slog.Logger) { l.Warn("m", "err", errors.New("read /abs/error: EIO")) }, "/abs/error"},
		{"an error errors.Join joins", func(l *slog.Logger) {
			l.Warn("m", "err", errors.Join(errors.New("first"), errors.New("open /abs/joined: EIO")))
		}, "/abs/joined"},
		{"a LogValuer's resolved value", func(l *slog.Logger) { l.Debug("m", "v", secretValuer{"/abs/valuer"}) }, "/abs/valuer"},
		{"an attribute in a group", func(l *slog.Logger) {
			l.Info("m", slog.Group("g", slog.String("dir", "/abs/group")))
		}, "/abs/group"},
		{"a struct as %v prints it", func(l *slog.Logger) { l.Info("m", "d", detail{Where: "/abs/struct", Count: 2}) }, "/abs/struct"},
		{"a map as JSON encodes it", func(l *slog.Logger) {
			l.Info("m", "m", map[string]string{"dir": "/abs/map"})
		}, "/abs/map"},
		{"an attribute With binds", func(l *slog.Logger) { l.With("path", "/abs/with").Info("refused") }, "/abs/with"},
		{"a group WithGroup opens", func(l *slog.Logger) { l.WithGroup("abs-group").Info("m", "k", "v") }, "abs-group"},
		{"an attribute bound inside a group, between two Withs", func(l *slog.Logger) {
			l.With("outer", "a").WithGroup("g").With("inner", "/abs/nested").Info("m", "k", "v")
		}, "/abs/nested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRecorder()
			tc.log(r.Logger())
			r.Logger().Info("an unrelated line", "path", "Album/01.flac")
			got := r.Carrying(tc.value, "")
			if len(got) != 1 || strings.Contains(got[0], "an unrelated line") {
				t.Errorf("Carrying(%q) = %q, want the one record that carries it", tc.value, got)
			}
			if len(r.Records()) != 2 {
				t.Errorf("Records holds %d records, want 2", len(r.Records()))
			}
		})
	}
}

// TestALoggerOfARecorderKeepsEveryLevelAndChangesNoDefault pins that a
// Recorder's Logger keeps Debug records, and that NewRecorder installs
// nothing: slog.Default is what it was.
func TestALoggerOfARecorderKeepsEveryLevelAndChangesNoDefault(t *testing.T) {
	t.Parallel()
	before := slog.Default()
	r := NewRecorder()
	l := r.Logger()
	if !l.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("a Recorder's Logger does not take Debug records")
	}
	l.Debug("a debug line")
	if got := r.Lines("a debug line"); len(got) != 1 {
		t.Errorf("the debug line was kept %d times, want once", len(got))
	}
	if slog.Default() != before {
		t.Error("NewRecorder or Logger changed slog.Default")
	}
}
