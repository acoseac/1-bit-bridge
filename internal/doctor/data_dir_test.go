package doctor

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"testing"
)

// TestRunPrintsTheDataDirLineAfterTheConfigDir pins where the report carries
// the line on the data directory as bridge serve prepares it
// (Deps.DataDirPreparation, B659): asked once, right after config-dir and
// before tls-cert, the TLS pair a data directory holds by default; a nil func
// adds no line.
func TestRunPrintsTheDataDirLineAfterTheConfigDir(t *testing.T) {
	line := Check{Name: "data-dir", Status: Fail, Summary: "bridge serve will not start: the data directory /srv/data is a regular file, not a directory"}
	asked := 0
	rep := Run(context.Background(), Deps{DataDirPreparation: func(context.Context) Check { asked++; return line }})
	i := slices.IndexFunc(rep.Checks, func(c Check) bool { return c.Name == checkNameConfigDir })
	switch {
	case asked != 1:
		t.Fatalf("the data-dir line was asked %d times, want once", asked)
	case i < 0 || i+2 >= len(rep.Checks):
		t.Fatalf("the report has no config-dir line with two after it: %+v", rep.Checks)
	case rep.Checks[i+1] != line:
		t.Errorf("the line after config-dir is %+v, want %+v", rep.Checks[i+1], line)
	case rep.Checks[i+2].Name != checkNameTLSCert:
		t.Errorf("the line after the data-dir line is %s, want %s", rep.Checks[i+2].Name, checkNameTLSCert)
	}
	base := Run(context.Background(), Deps{})
	if len(rep.Checks) != len(base.Checks)+1 {
		t.Errorf("the data-dir line added %d checks, want 1", len(rep.Checks)-len(base.Checks))
	}
	if slices.ContainsFunc(base.Checks, func(c Check) bool { return c.Name == "data-dir" }) {
		t.Errorf("a report with no data-dir func carries a data-dir line: %+v", base.Checks)
	}
}

// TestNotGradedReasonSaysWhyAConfigWasNotGraded: the reason a line that
// grades what the config sets gives for running without it is config-file's,
// a permission failure first, in the words the port checks and tls-cert give
// theirs; none where the config loaded, none was found, or nothing was looked
// up.
func TestNotGradedReasonSaysWhyAConfigWasNotGraded(t *testing.T) {
	const p = "/srv/bridge/bridge.yaml"
	for _, tc := range []struct {
		name string
		c    *ConfigFile
		want string
	}{
		{"no lookup", nil, ""},
		{"a config that loaded", &ConfigFile{Path: p}, ""},
		{"none found", &ConfigFile{Tried: []string{p}}, ""},
		{"a config this user cannot read", &ConfigFile{Path: p, LoadErr: &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}},
			"the config that sets the data directory is not readable by this user"},
		{"a named config that is not there", &ConfigFile{Path: p, LoadErr: &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}},
			"the named config does not exist"},
		{"a config that does not load", &ConfigFile{Path: p, LoadErr: errors.New("line 3: field noSuchSetting not found in type config.Config")},
			"the config that sets the data directory does not load"},
	} {
		if got := tc.c.NotGradedReason("the data directory"); got != tc.want {
			t.Errorf("%s: NotGradedReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
