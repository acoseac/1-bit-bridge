package config

import (
	"strings"
	"testing"
)

func TestDuplicatesEffectiveFilter(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", DuplicatesFilterHighestQuality, false}, // default is ON, highest-quality
		{"highest-quality", DuplicatesFilterHighestQuality, false},
		{"same-format", DuplicatesFilterSameFormat, false},
		{"off", DuplicatesFilterOff, false},
		{"  Highest-Quality  ", DuplicatesFilterHighestQuality, false}, // case/whitespace tolerant
		{"OFF", DuplicatesFilterOff, false},
		{"hihgest-quality", "", true}, // typo must fail loudly, not default silently
	}
	for _, c := range cases {
		got, err := DuplicatesConfig{Filter: c.in}.EffectiveFilter()
		if c.wantErr {
			if err == nil {
				t.Errorf("EffectiveFilter(%q): want error, got %q", c.in, got)
			} else if !strings.Contains(err.Error(), c.in) {
				// The TailscaleMode convention: the error preserves the
				// operator's original input verbatim.
				t.Errorf("EffectiveFilter(%q) error must quote the input: %v", c.in, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("EffectiveFilter(%q) = (%q, %v), want %q", c.in, got, err, c.want)
		}
	}
}

// TestValidateRejectsUnknownDuplicatesFilter: a typo'd policy fails at
// config load, not silently at serve time.
func TestValidateRejectsUnknownDuplicatesFilter(t *testing.T) {
	cfg := &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788",
		AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	cfg.Duplicates.Filter = "bogus"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates.filter") {
		t.Fatalf("Validate must reject an unknown duplicates.filter, got %v", err)
	}
	cfg.Duplicates.Filter = "same-format"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid filter must pass Validate: %v", err)
	}
}
