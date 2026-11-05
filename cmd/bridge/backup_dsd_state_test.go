package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// dsdStateBeforeTheBackup is a player-dsd.json as the box holds it when the
// backup is taken: a migrated explicit off (defaultMode off, its marker and
// its warning), a DAC the app chose for, with a DoP confirmation, a migrated
// acknowledgement of DSD64 alone that playback then observed, two listener
// answers and what this box observed of it, and a second DAC of which this
// box observed something and nothing else.
const dsdStateBeforeTheBackup = `{
  "schemaVersion": 1,
  "defaultMode": "off",
  "dacs": [
    {
      "key": {"vendorId": "20b1", "productId": "3009", "interface": 1, "product": "GO bar"},
      "choice": {"mode": "native", "maxBitRate": 5644800, "revision": 3, "setAt": "2026-11-05T10:00:00Z", "setBy": "app"},
      "dopConfirmed": {"bcdDevice": "0123", "confirmedAt": "2026-11-05T10:01:00Z", "setBy": "app"},
      "firstUse": [{"transport": "native", "bcdDevice": "0123", "bitRates": [2822400], "acknowledgedAt": "2026-11-05T10:02:00Z", "observedAt": "2026-11-05T10:03:00Z"}],
      "listener": [
        {"transport": "native", "bitRate": 2822400, "bcdDevice": "0123", "answer": "clean", "on": "2026-11-04"},
        {"transport": "dop", "bitRate": 5644800, "bcdDevice": "0123", "answer": "noise", "on": "2026-11-05"}
      ],
      "evidence": [{"transport": "native", "format": "DSD_U32_BE", "bitRate": 2822400, "bcdDevice": "0123", "driverAcceptedAt": "2026-11-05T10:04:00Z", "kernel": "6.12.47-v8-16k+", "streamingObservedAt": "2026-11-05T10:05:00Z"}]
    },
    {
      "key": {"vendorId": "262a", "productId": "9302", "interface": null, "product": null},
      "choice": null,
      "dopConfirmed": null,
      "firstUse": [],
      "listener": [],
      "evidence": [{"transport": "dop", "format": "S32_LE", "bitRate": 2822400, "bcdDevice": "0100", "driverAcceptedAt": "2026-11-05T10:06:00Z", "kernel": "6.12.47-v8-16k+"}]
    }
  ],
  "migratedFrom": {"from": "keys", "dsdMode": "off"},
  "warnings": [{"code": "migrated_off", "at": "2026-11-05T09:00:00Z"}]
}
`

// TestABackupCarriesTheChoicesAndRestoreDropsThisBoxsEvidence: bridge backup
// carries the player's DSD state as its bytes, and bridge restore installs
// in its place, over what the box holds since, what was chosen and heard
// (the design's section 2.5): defaultMode, the migration's marker and the
// warnings travel, so a migrated off stays off and the restored config's
// legacy keys never migrate again; a DAC's choice, its DoP confirmation, its
// first-use acknowledgements with the rates they cover and its listener
// answers travel; what was observed on the box the backup was taken on does
// not (driverAcceptedAt, kernel, streamingObservedAt and first use's
// observedAt), and a DAC left with nothing is not kept as a shell. Through
// the real CLI, as an operator runs both.
func TestABackupCarriesTheChoicesAndRestoreDropsThisBoxsEvidence(t *testing.T) {
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	persistentMedium(t)
	cfgPath, dataDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	live := filepath.Join(dataDir, "player-dsd.json")
	if err := os.WriteFile(live, []byte(dsdStateBeforeTheBackup), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, cfgPath)
	var m backup.Manifest
	if err := json.Unmarshal(readFileT(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Files, "player-dsd.json") {
		t.Fatalf("the snapshot lists %v, without player-dsd.json", m.Files)
	}
	if got := readFileT(t, filepath.Join(snap, "player-dsd.json")); string(got) != dsdStateBeforeTheBackup {
		t.Fatalf("the snapshot's player-dsd.json is not the box's bytes:\n%s", got)
	}

	// The box moves on: another state, which the restore replaces whole.
	const since = `{"schemaVersion": 1, "defaultMode": "auto", "dacs": [{"key": {"vendorId": "152a", "productId": "85de", "interface": 2, "product": "Brooklyn DAC+"}, "choice": {"mode": "convert", "maxBitRate": 0, "revision": 1, "setAt": "2026-11-06T08:00:00Z", "setBy": "console"}, "dopConfirmed": null, "firstUse": [], "listener": [], "evidence": []}], "migratedFrom": null, "warnings": []}`
	if err := os.WriteFile(live, []byte(since), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runRestoreYes(t, cfgPath, snap); code != 0 {
		t.Fatalf("restore = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	raw := readFileT(t, live)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the restored state does not parse: %v\n%s", err, raw)
	}
	for _, observed := range []string{"driverAcceptedAt", "kernel", "streamingObservedAt", "observedAt"} {
		if path, ok := jsonKeyPath(got, observed); ok {
			t.Errorf("the restored state keeps %s, observed on the box the backup was taken on, at %s:\n%s", observed, path, raw)
		}
	}
	if got["defaultMode"] != "off" {
		t.Errorf("the restored defaultMode is %v, want the migrated off:\n%s", got["defaultMode"], raw)
	}
	if want := map[string]any{"from": "keys", "dsdMode": "off"}; !reflect.DeepEqual(got["migratedFrom"], want) {
		t.Errorf("the restored marker is %v, want %v, so the restored config is not migrated again:\n%s", got["migratedFrom"], want, raw)
	}
	if want := []any{map[string]any{"code": "migrated_off", "at": "2026-11-05T09:00:00Z"}}; !reflect.DeepEqual(got["warnings"], want) {
		t.Errorf("the restored warnings are %v, want %v:\n%s", got["warnings"], want, raw)
	}
	dacs, _ := got["dacs"].([]any)
	if len(dacs) != 1 {
		t.Fatalf("the restored state holds %d DACs, want the one with what was chosen and heard, neither the box's since nor a shell of evidence:\n%s", len(dacs), raw)
	}
	var before map[string]any
	if err := json.Unmarshal([]byte(dsdStateBeforeTheBackup), &before); err != nil {
		t.Fatal(err)
	}
	kept := before["dacs"].([]any)[0].(map[string]any)
	for _, field := range []string{"key", "choice", "dopConfirmed", "listener"} {
		if !reflect.DeepEqual(dacs[0].(map[string]any)[field], kept[field]) {
			t.Errorf("the restored DAC's %s is %v, want %v as the backup had it", field, dacs[0].(map[string]any)[field], kept[field])
		}
	}
	firstUse := map[string]any{"transport": "native", "bcdDevice": "0123", "bitRates": []any{2822400.0}, "acknowledgedAt": "2026-11-05T10:02:00Z"}
	if want := []any{firstUse}; !reflect.DeepEqual(dacs[0].(map[string]any)["firstUse"], want) {
		t.Errorf("the restored first use is %v, want %v: the acknowledgement and its rates, not when this box observed it", dacs[0].(map[string]any)["firstUse"], want)
	}
	if ev, ok := dacs[0].(map[string]any)["evidence"].([]any); !ok || len(ev) != 0 {
		t.Errorf("the restored evidence is %v, want []", dacs[0].(map[string]any)["evidence"])
	}
}

// jsonKeyPath reports where key is a key of an object in v, decoded JSON.
func jsonKeyPath(v any, key string) (string, bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == key {
				return k, true
			}
			if p, ok := jsonKeyPath(e, key); ok {
				return k + "." + p, true
			}
		}
	case []any:
		for _, e := range x {
			if p, ok := jsonKeyPath(e, key); ok {
				return "[]." + p, true
			}
		}
	}
	return "", false
}
