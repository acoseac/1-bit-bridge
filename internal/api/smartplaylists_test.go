package api

import (
	"encoding/json"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestParseLocalHour(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"", 0, false}, {"8", 8, true}, {"0", 0, true}, {"23", 23, true},
		{"24", 0, false}, {"-1", 0, false}, {"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseLocalHour(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseLocalHour(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTimeOfDayTitle(t *testing.T) {
	cases := map[int]string{
		7: "Good Morning", 11: "This Afternoon", 16: "This Afternoon",
		19: "This Evening", 2: "Late Night", 23: "Late Night", 4: "Late Night",
	}
	for h, want := range cases {
		if got := timeOfDayTitle(h); got != want {
			t.Errorf("timeOfDayTitle(%d) = %q want %q", h, got, want)
		}
	}
}

func TestResolveTimeOfDayItems_WindowUnionDedupWrap(t *testing.T) {
	it := func(p string) manifest.SmartPlaylistItem { return manifest.SmartPlaylistItem{Path: p, Title: "T" + p} }
	hourly := map[int][]manifest.SmartPlaylistItem{
		7: {it("d")},
		8: {it("a"), it("b")},
		9: {it("c"), it("a")}, // 'a' dup across hours
	}
	// center 8 first, then 7, then 9 → a,b,d,c (a deduped)
	got := resolveTimeOfDayItems(hourly, 8, 1, 100)
	want := []string{"a", "b", "d", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d items want %d: %+v", len(got), len(want), got)
	}
	for i, p := range want {
		if got[i].Path != p || got[i].Position != i {
			t.Fatalf("item %d = %+v want path %s pos %d", i, got[i], p, i)
		}
	}

	// cap
	if capped := resolveTimeOfDayItems(hourly, 8, 1, 2); len(capped) != 2 {
		t.Errorf("cap not applied: got %d", len(capped))
	}

	// wrap: hour 0, window 1 → visits 0, 23, 1
	wrap := map[int][]manifest.SmartPlaylistItem{23: {it("late")}, 0: {it("mid")}, 1: {it("early")}}
	gw := resolveTimeOfDayItems(wrap, 0, 1, 100)
	if len(gw) != 3 || gw[0].Path != "mid" {
		t.Fatalf("wrap window: %+v", gw)
	}

	if resolveTimeOfDayItems(nil, 8, 1, 100) != nil {
		t.Errorf("empty pools should yield nil")
	}
}

func TestBuildSmartPlaylistDTO_FlatFamily(t *testing.T) {
	items := []manifest.SmartPlaylistItem{{Position: 0, Path: "/a.flac", Title: "A", Artist: "X"}, {Position: 1, Path: "/b.flac"}}
	blob := mustMarshal(t, items)
	row := manifest.StoredSmartPlaylist{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Subtitle: "sub", RefreshedAt: 99, ItemsJSON: blob}

	dto, ok := buildSmartPlaylistDTO(row, 8, 0, false)
	if !ok {
		t.Fatal("flat family should always build")
	}
	if dto.Title != "Heavy Rotation" || dto.Kind != "heavyRotation" || dto.RefreshedAt != 99 {
		t.Errorf("dto meta wrong: %+v", dto)
	}
	if len(dto.Items) != 2 || dto.Items[0].Path != "/a.flac" || dto.Items[0].Artist != "X" {
		t.Errorf("items wrong: %+v", dto.Items)
	}
}

func TestBuildSmartPlaylistDTO_TimeOfDay(t *testing.T) {
	blob := mustMarshal(t, manifest.SmartPlaylistHourlyBlob{Hourly: map[int][]manifest.SmartPlaylistItem{
		8: {{Path: "/a.flac", Title: "A"}},
	}})
	row := manifest.StoredSmartPlaylist{Slug: "time-of-day", Kind: "timeOfDay", Title: "For Right Now", RefreshedAt: 5, ItemsJSON: blob}

	// current UTC hour 8 + local hour 14 → titled "This Afternoon", item present
	dto, ok := buildSmartPlaylistDTO(row, 8, 14, true)
	if !ok {
		t.Fatal("time-of-day with a matching hour should build")
	}
	if dto.Title != "This Afternoon" {
		t.Errorf("title = %q want local-hour title", dto.Title)
	}
	if len(dto.Items) != 1 || dto.Items[0].Path != "/a.flac" {
		t.Errorf("items wrong: %+v", dto.Items)
	}

	// without local hour, keep the stored title
	dto2, _ := buildSmartPlaylistDTO(row, 8, 0, false)
	if dto2.Title != "For Right Now" {
		t.Errorf("no-local-hour title = %q want stored", dto2.Title)
	}

	// current UTC hour 20 (no habit in [19,20,21]) → omitted
	if _, ok := buildSmartPlaylistDTO(row, 20, 0, false); ok {
		t.Error("empty time-of-day window should omit the family")
	}
}
