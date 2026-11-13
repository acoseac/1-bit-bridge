package doctor

import (
	"context"
	"slices"
	"testing"
)

// TestRunPrintsTheInstallLayoutLineAfterTheDataDirLine pins where the report
// carries the line on the bridge's own files inside a library root
// (Deps.InstallLayout, backlog B884): asked once, right after the data-dir
// line, a fail there included, and before tls-cert; a func that returns no
// line adds none, so a healthy layout's report is the report without it; a
// nil func adds none either; and a managed bridge never asks it, its files
// being the host's to move.
func TestRunPrintsTheInstallLayoutLineAfterTheDataDirLine(t *testing.T) {
	dataDir := Check{Name: "data-dir", Status: Fail, Summary: "bridge serve will not start: the data directory /m/data is a regular file, not a directory"}
	layout := Check{Name: "install-layout", Status: Warn, Summary: "the data directory /m/data is inside library root /m (libraryRoots[0])"}
	asked := 0
	d := Deps{
		DataDirPreparation: func(context.Context) Check { return dataDir },
		InstallLayout:      func(context.Context) []Check { asked++; return []Check{layout} },
	}
	rep := Run(context.Background(), d)
	i := slices.IndexFunc(rep.Checks, func(c Check) bool { return c.Name == "data-dir" })
	switch {
	case asked != 1:
		t.Fatalf("the install-layout line was asked %d times, want once", asked)
	case i < 0 || i+2 >= len(rep.Checks):
		t.Fatalf("the report has no data-dir line with two after it: %+v", rep.Checks)
	case rep.Checks[i+1] != layout:
		t.Errorf("the line after data-dir is %+v, want %+v", rep.Checks[i+1], layout)
	case rep.Checks[i+2].Name != checkNameTLSCert:
		t.Errorf("the line after the install-layout line is %s, want %s", rep.Checks[i+2].Name, checkNameTLSCert)
	}
	if !rep.HasFail() || rep.WarnCount() < 1 {
		t.Errorf("the report counts %d fail and %d warn, want the data-dir fail and the layout's warn", rep.FailCount(), rep.WarnCount())
	}

	without := Run(context.Background(), Deps{DataDirPreparation: d.DataDirPreparation})
	healthy := Run(context.Background(), Deps{DataDirPreparation: d.DataDirPreparation,
		InstallLayout: func(context.Context) []Check { return nil }})
	if !slices.Equal(checkNamesOf(healthy.Checks), checkNamesOf(without.Checks)) {
		t.Errorf("a layout with no line changed the report:\n%v\nwant\n%v", checkNamesOf(healthy.Checks), checkNamesOf(without.Checks))
	}

	asked = 0
	d.Managed = true
	managed := Run(context.Background(), d)
	if asked != 0 || slices.ContainsFunc(managed.Checks, func(c Check) bool { return c.Name == layout.Name }) {
		t.Errorf("a managed bridge's report asked the layout %d times and carries it: %v", asked, checkNamesOf(managed.Checks))
	}
}

func checkNamesOf(checks []Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}
