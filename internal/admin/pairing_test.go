package admin

import (
	"net/url"
	"strings"
	"testing"
)

// TestBuildPairURLOmitsUrlsWhenOnlyPrimary keeps the QR payload small
// and byte-identical to pre-v1.x builds when the bridge only knows
// about one endpoint — older iOS clients that don't handle `urls`
// still see exactly what they've always seen.
func TestBuildPairURLOmitsUrlsWhenOnlyPrimary(t *testing.T) {
	out := buildPairURL("https://host:7788", "tok", "AB:CD", "Home", []string{"https://host:7788"})
	if strings.Contains(out, "urls=") {
		t.Errorf("urls= should be omitted when alternates is just the primary: %s", out)
	}
	if !strings.Contains(out, "url=https") {
		t.Errorf("url= must still be present: %s", out)
	}
}

func TestBuildPairURLEmitsUrlsWhenAlternatesPresent(t *testing.T) {
	alts := []string{
		"https://192.168.1.10:7788",
		"https://homepc.local:7788",
		"https://100.64.5.9:7788",
	}
	out := buildPairURL("https://192.168.1.10:7788", "tok", "AB:CD", "Home", alts)
	if !strings.Contains(out, "urls=") {
		t.Fatalf("urls= missing: %s", out)
	}
	// Parse the query and confirm every alternate round-trips through
	// newline-joined encoding.
	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := u.Query().Get("urls")
	want := strings.Join(alts, "\n")
	if got != want {
		t.Errorf("urls = %q, want %q", got, want)
	}
}

func TestBuildPairURLPrimaryStaysFirst(t *testing.T) {
	// Even when the caller lists the primary in the middle of
	// alternates, the `url=` field is what we pass explicitly — the
	// iOS fallback path (older builds ignoring `urls`) reads only
	// `url`, so it has to be the operator's chosen primary, not
	// whatever advertise.URLs returned first.
	out := buildPairURL("https://pick-me:7788", "tok", "AB:CD", "Home",
		[]string{"https://otherhost:7788", "https://pick-me:7788"})
	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("url"); got != "https://pick-me:7788" {
		t.Errorf("url= = %q, want https://pick-me:7788", got)
	}
}

func TestPairAlternatesPrependsPrimary(t *testing.T) {
	// advertise.URLs doesn't know about the operator's override URL —
	// our pairAlternates helper is what ensures it lands first. Test
	// with a non-default listen address just to exercise the port
	// parse.
	got := pairAlternates("https://user-chose-this:9999", "127.0.0.1:7788")
	if len(got) == 0 {
		t.Fatal("expected non-empty alternates")
	}
	if got[0] != "https://user-chose-this:9999" {
		t.Errorf("first alternate = %q, want the operator primary", got[0])
	}
	// And the primary isn't duplicated inside the advertise-derived
	// entries.
	count := 0
	for _, u := range got {
		if u == "https://user-chose-this:9999" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times; want exactly once", count)
	}
}

func TestEnsurePrimaryFirstHappyPathPassthrough(t *testing.T) {
	primary := "https://primary:7788"
	in := []string{primary, "https://b:7788", "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if len(got) != len(in) || got[0] != primary {
		t.Errorf("ensurePrimaryFirst pass-through changed slice: in=%v out=%v", in, got)
	}
}

func TestEnsurePrimaryFirstDedupsPrimaryEvenWhenAlreadyAtHead(t *testing.T) {
	// CodeRabbit round 2 on PR #101: a duplicate primary anywhere in
	// the input must collapse to a single primary at the head. The
	// pre-fix early-return on `alternates[0] == primary` let the
	// duplicate slip through unchanged.
	primary := "https://primary:7788"
	in := []string{primary, "https://b:7788", primary}
	got := ensurePrimaryFirst(primary, in)
	want := []string{primary, "https://b:7788"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, u := range want {
		if got[i] != u {
			t.Errorf("got[%d] = %q, want %q (full got=%v)", i, got[i], u, got)
		}
	}
	count := 0
	for _, u := range got {
		if u == primary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times after dedup; want exactly once (got=%v)", count, got)
	}
}

func TestEnsurePrimaryFirstReordersWhenPrimaryNotHead(t *testing.T) {
	// CodeRabbit defence-in-depth: if a future helper change ever
	// returns the primary in a non-head position, the response-
	// boundary helper restores the contract.
	primary := "https://primary:7788"
	in := []string{"https://b:7788", primary, "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if got[0] != primary {
		t.Errorf("ensurePrimaryFirst did not move primary to head: %v", got)
	}
	count := 0
	for _, u := range got {
		if u == primary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times after normalization; want exactly once", count)
	}
}

func TestEnsurePrimaryFirstPrependsWhenPrimaryMissing(t *testing.T) {
	primary := "https://primary:7788"
	in := []string{"https://b:7788", "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if got[0] != primary {
		t.Errorf("ensurePrimaryFirst did not prepend missing primary: %v", got)
	}
	if len(got) != len(in)+1 {
		t.Errorf("len = %d, want %d (one more than input)", len(got), len(in)+1)
	}
}

func TestEnsurePrimaryFirstEmptyInputYieldsPrimaryOnly(t *testing.T) {
	primary := "https://primary:7788"
	got := ensurePrimaryFirst(primary, nil)
	if len(got) != 1 || got[0] != primary {
		t.Errorf("ensurePrimaryFirst on nil input = %v, want [%q]", got, primary)
	}
}
