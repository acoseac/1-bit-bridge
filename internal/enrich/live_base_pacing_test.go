package enrich

import (
	"testing"
	"time"
)

// TestPacingFollowsTheLiveBase is the safety test for this whole change.
//
// The base URL DERIVES the pacing — that is the documented invariant in
// pacing.go, and it is deliberately not a separate knob. Making the base
// live without making the interval follow it produces the one mistake in
// this package that reaches a third party: an operator clears the Atlas
// mirror URL, the client starts calling public MusicBrainz, and it does
// so at the self-hosted 150 ms — roughly 6.7 rps against a service that
// asks anonymous clients for one.
//
// So this asserts the interval changes WITH the base, in both directions,
// on a live client that was constructed pointing the other way.
func TestPacingFollowsTheLiveBase(t *testing.T) {
	t.Run("musicbrainz", func(t *testing.T) {
		base := "https://atlas.example.test/ws/2"
		// Constructed against the PUBLIC default, so a pass cannot come
		// from the captured value happening to be right.
		c := NewMusicBrainzClient("", "ua", nil).
			WithLiveBase(func() string { return base })

		if got := c.MinInterval(); got != SelfHostedMinInterval {
			t.Errorf("mirror base: interval = %v, want %v", got, SelfHostedMinInterval)
		}
		base = "https://musicbrainz.org/ws/2"
		if got := c.MinInterval(); got != PublicMBMinInterval {
			t.Errorf("after clearing the mirror: interval = %v, want the public %v — "+
				"a frozen interval here is ~%0.1f rps against MusicBrainz",
				got, PublicMBMinInterval, float64(time.Second)/float64(SelfHostedMinInterval))
		}
		// Subdomains are the same operator under the same policy.
		base = "https://beta.musicbrainz.org/ws/2"
		if got := c.MinInterval(); got != PublicMBMinInterval {
			t.Errorf("subdomain: interval = %v, want the public %v", got, PublicMBMinInterval)
		}
	})

	t.Run("coverart", func(t *testing.T) {
		base := "https://atlas.example.test"
		c := NewCoverArtClient("", "ua", nil).
			WithLiveBase(func() string { return base })

		if got := c.MinInterval(); got != SelfHostedMinInterval {
			t.Errorf("mirror base: interval = %v, want %v", got, SelfHostedMinInterval)
		}
		base = "https://coverartarchive.org"
		if got := c.MinInterval(); got != PublicCAAMinInterval {
			t.Errorf("after clearing the mirror: interval = %v, want the public %v",
				got, PublicCAAMinInterval)
		}
	})
}

// TestLiveBaseFallsBackToTheConstructedValue — a cleared config field
// arrives here as "", and an empty live value must resolve to the public
// default rather than building `"/release/…"` against no host at all.
func TestLiveBaseFallsBackToTheConstructedValue(t *testing.T) {
	live := ""
	c := NewMusicBrainzClient("", "ua", nil).WithLiveBase(func() string { return live })
	if got := c.resolveBase(); got != DefaultMusicBrainzBase {
		t.Errorf("empty live base resolved to %q, want the constructed default %q",
			got, DefaultMusicBrainzBase)
	}
	if got := c.MinInterval(); got != PublicMBMinInterval {
		t.Errorf("empty live base: interval = %v, want the public %v", got, PublicMBMinInterval)
	}
	live = "   "
	if got := c.resolveBase(); got != DefaultMusicBrainzBase {
		t.Errorf("whitespace live base resolved to %q, want the default", got)
	}
	// A trailing slash must not survive into the URL builder — the config
	// layer trims, but this is the one place that would silently emit
	// `https://host//release/…` if it did not.
	live = "https://mirror.example.test/ws/2/"
	if got := c.resolveBase(); got != "https://mirror.example.test/ws/2" {
		t.Errorf("resolveBase = %q, want the trailing slash trimmed", got)
	}
}

// TestNilLiveBaseKeepsConstructedPacing — every caller other than the
// serve path leaves the provider nil and must be byte-identical to
// before, including the pacing.
func TestNilLiveBaseKeepsConstructedPacing(t *testing.T) {
	mirror := NewMusicBrainzClient("https://atlas.example.test/ws/2", "ua", nil)
	if got := mirror.MinInterval(); got != SelfHostedMinInterval {
		t.Errorf("mirror: %v, want %v", got, SelfHostedMinInterval)
	}
	public := NewMusicBrainzClient("", "ua", nil)
	if got := public.MinInterval(); got != PublicMBMinInterval {
		t.Errorf("public: %v, want %v", got, PublicMBMinInterval)
	}
	if got := public.resolveBase(); got != DefaultMusicBrainzBase {
		t.Errorf("resolveBase = %q, want %q", got, DefaultMusicBrainzBase)
	}
}

// TestEnricherPacingFollowsTheClient — the enricher captured the interval
// into an exported field at construction, so a live client alone would
// not have been enough: the sleeps read the enricher's copy.
func TestEnricherPacingFollowsTheClient(t *testing.T) {
	base := "https://atlas.example.test/ws/2"
	mb := NewMusicBrainzClient("", "ua", nil).WithLiveBase(func() string { return base })
	caaBase := "https://atlas.example.test"
	caa := NewCoverArtClient("", "ua", nil).WithLiveBase(func() string { return caaBase })
	e := &Enricher{mb: mb, caa: caa,
		MBMinInterval: PublicMBMinInterval, CAAMinInterval: PublicCAAMinInterval}

	if got := e.mbMinInterval(); got != SelfHostedMinInterval {
		t.Errorf("mb: %v, want the client's live %v (not the captured field)",
			got, SelfHostedMinInterval)
	}
	if got := e.caaMinInterval(); got != SelfHostedMinInterval {
		t.Errorf("caa: %v, want the client's live %v", got, SelfHostedMinInterval)
	}
	base, caaBase = "https://musicbrainz.org/ws/2", "https://coverartarchive.org"
	if got := e.mbMinInterval(); got != PublicMBMinInterval {
		t.Errorf("mb after repointing: %v, want %v", got, PublicMBMinInterval)
	}
	if got := e.caaMinInterval(); got != PublicCAAMinInterval {
		t.Errorf("caa after repointing: %v, want %v", got, PublicCAAMinInterval)
	}
}

// TestEnricherKeepsTheFieldWithoutALiveBase — the exported fields are how
// tests and the CLI set pacing, and a client with no live base must not
// override them.
func TestEnricherKeepsTheFieldWithoutALiveBase(t *testing.T) {
	e := &Enricher{
		mb:             NewMusicBrainzClient("", "ua", nil),
		caa:            NewCoverArtClient("", "ua", nil),
		MBMinInterval:  7 * time.Millisecond,
		CAAMinInterval: 9 * time.Millisecond,
	}
	if got := e.mbMinInterval(); got != 7*time.Millisecond {
		t.Errorf("mb = %v, want the explicitly-set field", got)
	}
	if got := e.caaMinInterval(); got != 9*time.Millisecond {
		t.Errorf("caa = %v, want the explicitly-set field", got)
	}
	// And with no client at all.
	bare := &Enricher{MBMinInterval: 3 * time.Millisecond}
	if got := bare.mbMinInterval(); got != 3*time.Millisecond {
		t.Errorf("no client: %v, want the field", got)
	}
}
