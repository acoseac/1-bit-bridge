package doctor

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestTLSCertIsNotCheckedWhereNothingSaysWhereTheCertificateIs: a report
// with no data dir and no certificate path has nothing for tls-cert to
// grade, and the reason is a line above it: config-file's, about a config
// that was named or found and could not be graded, or config-dir's, about a
// config directory that could not be resolved. #1022's rule for a check that
// declines for a reason another line reports: ok, "not checked", and why,
// as the port checks and config-dir answer. It warned "no data dir set" until
// 2026-09-29 (backlog B61), a second warn about the one fact config-file or
// config-dir already reports.
func TestTLSCertIsNotCheckedWhereNothingSaysWhereTheCertificateIs(t *testing.T) {
	const path = "/srv/bridge/bridge.yaml"
	for _, tc := range []struct {
		name string
		d    Deps
		// why is the reason the summary must give.
		why string
	}{
		{"a config this user cannot read", Deps{ConfigDir: "/srv/bridge", ConfigFile: &ConfigFile{Path: path,
			LoadErr: &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}}},
			"not readable by this user"},
		{"a named config that is not there", Deps{ConfigDir: "/srv/bridge", ConfigFile: &ConfigFile{Path: path,
			LoadErr: &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}}},
			"does not exist"},
		{"a config that does not load", Deps{ConfigDir: "/srv/bridge", ConfigFile: &ConfigFile{Path: path,
			LoadErr: errors.New(`yaml: unmarshal errors: field lisenAddress not found`)}},
			"does not load"},
		{"no config found, and no config directory", Deps{ConfigFile: &ConfigFile{Tried: []string{path}}},
			"no config directory"},
		{"no lookup and no data dir", Deps{}, "nothing says where"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := checkTLSCert(t.Context(), tc.d)
			if c.Status != OK || !strings.HasPrefix(c.Summary, "not checked: ") || !strings.Contains(c.Summary, tc.why) {
				t.Errorf("tls-cert = %s %q, want ok \"not checked: …%s…\"", c.Status, c.Summary, tc.why)
			}
			if c.Hint != "" {
				t.Errorf("an ok carries a hint nobody is shown: %q", c.Hint)
			}
		})
	}
}
