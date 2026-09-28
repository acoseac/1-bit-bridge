package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trayManagedRun builds one tray per case with the shipped buildFeatureTray,
// lets the settings snapshot land, then changes every row's control, and
// reports what the tray showed at build, after the snapshot, and what each
// change sent.
const trayManagedRun = `
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
const rowsOf = (tray) => {
  const rows = [];
  walk(tray, (n) => {
    if (n.className === "tray-row") {
      let input = null;
      walk(n, (m) => { if (m.dataset.field) input = m; });
      rows.push({ field: input.dataset.field, hidden: n.hidden, disabled: input.disabled, input });
    }
    if (n.className === "tray-note") rows.push({ note: true, hidden: n.hidden });
  });
  return rows;
};
const plain = (rows) => rows.map(({ input, ...r }) => r);
const out = [];
for (const c of cases) {
  traySettings = null;
  traySettingsPromise = null;
  trayManaged = c.knownBefore ? new Set(c.knownBefore) : null;
  mountedTrays.clear();
  settingsAnswer = { ...c.settings, managedSettings: c.managed };
  patchAnswer = { restartRequired: false, fields: {} };
  const { button, tray } = buildFeatureTray({ title: "Tray", rows: c.rows });
  if (c.openFirst) { tray.hidden = false; button.setAttribute("aria-expanded", "true"); }
  const atBuild = { gearHidden: button.hidden, rows: plain(rowsOf(tray)) };
  await new Promise((r) => setTimeout(r, 0));
  const afterSnapshot = {
    gearHidden: button.hidden, trayHidden: tray.hidden,
    expanded: button.attributes["aria-expanded"], rows: plain(rowsOf(tray)),
  };
  const sends = [];
  for (const row of rowsOf(tray)) {
    if (row.note) continue;
    patches = [];
    if (row.input.type === "checkbox") row.input.checked = !row.input.checked;
    else row.input.value = "7";
    await row.input.dispatch("change");
    sends.push({
      field: row.field, sent: patches.map((p) => Object.keys(p)),
      shows: row.input.type === "checkbox" ? row.input.checked : row.input.value,
    });
  }
  out.push({ name: c.name, atBuild, afterSnapshot, sends });
}
console.log(JSON.stringify(out));
`

// trayManagedCase is one tray the harness builds: its rows, the fields the
// settings answer calls managed, the snapshot's values, whether a snapshot
// had already told the document the managed set before this build, and
// whether the reader opened the tray before its snapshot landed.
type trayManagedCase struct {
	Name        string           `json:"name"`
	Rows        []map[string]any `json:"rows"`
	Managed     []string         `json:"managed"`
	Settings    map[string]any   `json:"settings"`
	KnownBefore []string         `json:"knownBefore,omitempty"`
	OpenFirst   bool             `json:"openFirst,omitempty"`
}

// trayManagedRow is one row as the harness read it.
type trayManagedRow struct {
	Field    string `json:"field"`
	Note     bool   `json:"note"`
	Hidden   bool   `json:"hidden"`
	Disabled bool   `json:"disabled"`
}

// trayManagedResult is what one case's tray showed and sent.
type trayManagedResult struct {
	Name    string `json:"name"`
	AtBuild struct {
		GearHidden bool             `json:"gearHidden"`
		Rows       []trayManagedRow `json:"rows"`
	} `json:"atBuild"`
	AfterSnapshot struct {
		GearHidden bool             `json:"gearHidden"`
		TrayHidden bool             `json:"trayHidden"`
		Expanded   string           `json:"expanded"`
		Rows       []trayManagedRow `json:"rows"`
	} `json:"afterSnapshot"`
	Sends []struct {
		Field string     `json:"field"`
		Sent  [][]string `json:"sent"`
		Shows any        `json:"shows"`
	} `json:"sends"`
}

// runManagedTraysUnderNode runs trayManagedRun over the cases with the tray
// functions extracted from the shipped app.js.
func runManagedTraysUnderNode(t *testing.T, node string, cases []trayManagedCase) []trayManagedResult {
	t.Helper()
	src := readFile(t, "static/app.js")
	var script strings.Builder
	script.WriteString(trayHarnessPreamble)
	for _, name := range trayHarnessFunctions {
		script.WriteString(extractJSFunction(t, src, name))
		script.WriteString("\n")
	}
	script.WriteString(trayManagedRun)
	dir := t.TempDir()
	path := filepath.Join(dir, "tray.mjs")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(casesPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, path, casesPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var results []trayManagedResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(results) != len(cases) {
		t.Fatalf("the harness built %d trays for %d cases", len(results), len(cases))
	}
	return results
}

// TestATrayOffersNoSwitchTheControlPlaneOwns runs the shipped
// buildFeatureTray under node and pins what a tray does with a field
// `/api/settings` names in `managedSettings`.
//
// Trays ignored the list until 2026-09-28 (backlog B35), so on a managed
// bridge each offered switches the settings PATCH refuses: in a browser,
// the album page's Variants gear offered PCM upscaling and CarPlay, and a
// click answered "Save failed: these settings are managed by the control
// plane…", and the Jobs page's Backups and Update checks gears offered
// nothing else. The console must send only what it showed, and hides a
// managed field as the Settings page does, so a managed row is hidden and
// its input disabled, a change dispatched to it anyway sends nothing, and a
// tray with nothing left to show loses its gear (a note row is something
// to show).
func TestATrayOffersNoSwitchTheControlPlaneOwns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	sw := func(field string) map[string]any {
		return map[string]any{"field": field, "type": "switch", "label": field}
	}
	num := func(field string) map[string]any {
		return map[string]any{"field": field, "type": "number", "label": field}
	}
	note := map[string]any{"type": "note", "text": "A note for the reader."}
	settings := map[string]any{
		"upscaleEnabled": false, "optimizeEnabled": false,
		"backupIntervalHours": 24, "backupKeep": 7, "smartPlaylistsEnabled": true,
	}
	cases := []trayManagedCase{
		{Name: "one of two managed", Rows: []map[string]any{sw("upscaleEnabled"), sw("optimizeEnabled")},
			Managed: []string{"upscaleEnabled"}, Settings: settings},
		{Name: "all managed", Rows: []map[string]any{num("backupIntervalHours"), num("backupKeep")},
			Managed: []string{"backupIntervalHours", "backupKeep"}, Settings: settings},
		{Name: "all managed beside a note", Rows: []map[string]any{note, sw("smartPlaylistsEnabled")},
			Managed: []string{"smartPlaylistsEnabled"}, Settings: settings},
		{Name: "opened before the snapshot", Rows: []map[string]any{sw("upscaleEnabled")},
			Managed: []string{"upscaleEnabled"}, Settings: settings, OpenFirst: true},
		{Name: "managed set known at build", Rows: []map[string]any{sw("upscaleEnabled"), sw("optimizeEnabled")},
			Managed: []string{"upscaleEnabled"}, Settings: settings, KnownBefore: []string{"upscaleEnabled"}},
		{Name: "nothing managed", Rows: []map[string]any{sw("upscaleEnabled"), sw("optimizeEnabled")},
			Settings: settings},
	}
	results := runManagedTraysUnderNode(t, node, cases)

	for i, r := range results {
		c := cases[i]
		managed := map[string]bool{}
		for _, f := range c.Managed {
			managed[f] = true
		}
		offered := false
		for _, row := range r.AfterSnapshot.Rows {
			if row.Note {
				offered = true
				continue
			}
			if row.Hidden != managed[row.Field] || row.Disabled != managed[row.Field] {
				t.Errorf("%s: after the snapshot %s is hidden=%v disabled=%v, want both %v",
					r.Name, row.Field, row.Hidden, row.Disabled, managed[row.Field])
			}
			if !managed[row.Field] {
				offered = true
			}
		}
		if r.AfterSnapshot.GearHidden == offered {
			t.Errorf("%s: the gear is hidden=%v with offered rows=%v: a gear must show exactly "+
				"while its tray has a row to show", r.Name, r.AfterSnapshot.GearHidden, offered)
		}
		if !offered && (!r.AfterSnapshot.TrayHidden || r.AfterSnapshot.Expanded != "false") {
			t.Errorf("%s: nothing is left to show, and the tray is hidden=%v with aria-expanded=%q",
				r.Name, r.AfterSnapshot.TrayHidden, r.AfterSnapshot.Expanded)
		}
		for _, s := range r.Sends {
			switch {
			case managed[s.Field] && len(s.Sent) != 0:
				t.Errorf("%s: a change to the managed %s sent %v; the PATCH refuses it whole",
					r.Name, s.Field, s.Sent)
			case managed[s.Field] && fmt.Sprint(s.Shows) != fmt.Sprint(settings[s.Field]):
				t.Errorf("%s: after the refused change %s shows %v, want the stored %v back",
					r.Name, s.Field, s.Shows, settings[s.Field])
			case !managed[s.Field] && (len(s.Sent) != 1 || len(s.Sent[0]) != 1 || s.Sent[0][0] != s.Field):
				t.Errorf("%s: a change to %s sent %v, want one PATCH of that field alone",
					r.Name, s.Field, s.Sent)
			}
		}
	}

	// Known at build: the managed row never paints, and the gear stays.
	known := results[4]
	for _, row := range known.AtBuild.Rows {
		if row.Field == "upscaleEnabled" && !row.Hidden {
			t.Errorf("%s: upscaleEnabled showed at build, before its own snapshot, although an "+
				"earlier snapshot had named it managed", known.Name)
		}
	}
	// Unknown at build: every row paints, disabled, until the snapshot says.
	first := results[0]
	for _, row := range first.AtBuild.Rows {
		if row.Hidden || !row.Disabled {
			t.Errorf("%s: at build, before any snapshot, %s is hidden=%v disabled=%v, want "+
				"shown and disabled", first.Name, row.Field, row.Hidden, row.Disabled)
		}
	}
}
