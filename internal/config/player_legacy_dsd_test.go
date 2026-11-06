package config

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The legacy DSD keys (B728 B6): dsdMode, allowDoP and dsdQualification
// still load, turn no player off, and are warned of once in a start's scope.

// TestTheLegacyDSDKeysAreTheOnesTheBlockSets: LegacyDSDKeys names each
// legacy key the block sets, in the block's order, and a key written with no
// value ("" or null) is not set.
func TestTheLegacyDSDKeysAreTheOnesTheBlockSets(t *testing.T) {
	for _, tc := range []struct {
		block string
		want  string
	}{
		{"    enabled: true\n", ""},
		{"    dsdMode: off\n", "dsdMode"},
		{"    dsdMode: \"\"\n    allowDoP: null\n    dsdQualification: \"\"\n", ""},
		{"    allowDoP: false\n", "allowDoP"},
		{"    allowDoP: true\n    dsdQualification: approved.json\n    dsdMode: auto\n", "dsdMode,dsdQualification,allowDoP"},
	} {
		cfg, err := Load(writePlayerConfig(t, "  player:\n"+tc.block))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cfg.Output.Player.LegacyDSDKeys(), ","); got != tc.want {
			t.Errorf("%q: the legacy keys %q, want %q", tc.block, got, tc.want)
		}
	}
}

// TestALegacyDSDBlockKeepsThePlayerOn: a block of legacy keys, in each value
// they take, loads, is refused by nothing, and keeps the player as it is: on
// where it is enabled, its block read. The keys stay among those the block
// takes, since an unknown key would leave the block unread and the player
// off.
func TestALegacyDSDBlockKeepsThePlayerOn(t *testing.T) {
	forgetPlayerOffReason(t)
	for _, block := range []string{
		"    dsdMode: off\n", "    dsdMode: native\n    dsdQualification: approved.json\n", "    dsdMode: dop\n    allowDoP: false\n",
		"    dsdMode: dop\n    allowDoP: true\n    dsdQualification: elsewhere/approved.json\n", "    dsdMode: auto\n", "    allowDoP: true\n",
		"    dsdQualification: approved.json\n",
	} {
		cfg, err := Load(writePlayerConfig(t, "  player:\n"+boundPlayerBlock+block))
		if err != nil {
			t.Fatalf("%q: the load failed: %v", block, err)
		}
		if err := cfg.Output.Player.Unread(); err != nil {
			t.Errorf("%q: the block did not read: %v", block, err)
		}
		if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
			t.Errorf("%q: PlayerEnabled = %v, %v; want the player on", block, on, why)
		}
	}
}

// boundPlayerBlock is a player enabled and bound, the block's other keys.
const boundPlayerBlock = "    enabled: true\n    device:\n      binding: uniqueModel\n      vendorId: \"20b1\"\n      productId: \"3009\"\n"

// TestAStartsScopeWarnsOfEachLegacyKeyOnce: a config loaded with no scope
// warns of no legacy key, at its Load or any Validate; given a start's scope,
// it warns of each key it sets once, and Validate on it, on a clone, or on a
// clone saved with a change, the console's save, warns of none again; a
// second start's scope warns of each once more. Nothing of it is the
// process's: the two scopes are apart.
func TestAStartsScopeWarnsOfEachLegacyKeyOnce(t *testing.T) {
	forgetPlayerOffReason(t)
	rec := loggingtest.Record(t)
	path := writePlayerConfig(t, "  player:\n"+boundPlayerBlock+"    dsdMode: dop\n    allowDoP: true\n    dsdQualification: approved.json\n")
	lines := func(t *testing.T, want int) {
		t.Helper()
		got := rec.Failures(legacyDSDMessage)
		if len(got) != want {
			t.Fatalf("logged %d legacy-key lines, want %d:\n%s", len(got), want, strings.Join(got, "\n"))
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	lines(t, 0)

	start := &LegacyDSDWarnings{}
	cfg.WarnLegacyDSDKeysIn(start)
	lines(t, 3)
	for i, key := range []string{"dsdMode", "dsdQualification", "allowDoP"} {
		if got := rec.Failures(legacyDSDMessage)[i]; !strings.Contains(got, "key=output.player."+key) || strings.Contains(got, "approved.json") || strings.Contains(got, "dop") && !strings.Contains(got, "dsdMode") {
			t.Errorf("line %d is %q, want the key %s named and no value", i+1, got, key)
		}
	}
	next := Clone(cfg)
	next.LibraryName = "Study"
	for _, c := range []*Config{cfg, next} {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := c.NormalizeAndValidate(); err != nil {
			t.Fatal(err)
		}
		if err := Clone(c).NormalizeAndValidateQuiet(); err != nil {
			t.Fatal(err)
		}
	}
	lines(t, 3)

	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	again.WarnLegacyDSDKeysIn(&LegacyDSDWarnings{})
	lines(t, 6)
	again.WarnLegacyDSDKeysIn(nil)
	if err := again.Validate(); err != nil {
		t.Fatal(err)
	}
	lines(t, 6)
}

// TestTheConfigWordsSayNoKeyConverts (B728, the design's section 3.6): the
// refusals of a DSD mode the legacy key does not take and of a key that
// would convert no longer say the player never converts, which the per-DAC
// choice of the player's DSD state will, and still say no key here does.
func TestTheConfigWordsSayNoKeyConverts(t *testing.T) {
	forgetPlayerOffReason(t)
	for _, tc := range []struct{ block, says string }{
		{"    dsdMode: pcm\n", "dsdMode: that value is not a DSD mode (auto, native, dop or off)"},
		{"    dsd2pcm: true\n", "what the player does with DSD is each DAC's choice in the player's DSD state, never a key here"},
		{"    dsdToPCM: true\n", "no player setting resamples, converts"},
	} {
		cfg, err := Load(writePlayerConfig(t, "  player:\n"+boundPlayerBlock+tc.block))
		if err != nil {
			t.Fatal(err)
		}
		_, why := cfg.PlayerEnabled("linux")
		if why == nil || !strings.Contains(why.Error(), tc.says) || strings.Contains(why.Error(), "never converts") {
			t.Errorf("%q: the reason %v, want %q and no claim that the player never converts", tc.block, why, tc.says)
		}
	}
}
