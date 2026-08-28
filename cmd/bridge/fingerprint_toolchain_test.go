package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestFingerprintToolchainLogsEachReasonOnce pins the two-flag shape.
//
// The feature is hot-applied now, so an operator can fix one prerequisite
// and immediately hit the other: register a key, then discover fpcalc is
// missing — or install fpcalc and discover there is no key. A single
// shared `logged` flag would permanently suppress whichever warning came
// second, for the lifetime of the process, at exactly the moment it
// became the useful one.
//
// The cache is driven directly with a pre-set probe result rather than
// through a real `fpcalc -version`: the point is the logging policy, and
// a test that depends on whether the developer's machine has fpcalc
// installed would assert something different on every host.
func TestFingerprintToolchainLogsEachReasonOnce(t *testing.T) {
	var out strings.Builder
	c := &fingerprintToolchainCache{stderr: &out}

	// Probe failed: fpcalc is missing. Repeat calls must log once — this
	// is consulted on every sweep and every Jobs-card render.
	c.at, c.err = time.Now(), errors.New("exec: fpcalc not found")
	for i := 0; i < 5; i++ {
		if ok, reason := c.ready(true); ok || reason != "fpcalc_missing" {
			t.Fatalf("ready() = (%v, %q), want (false, fpcalc_missing)", ok, reason)
		}
	}
	if n := strings.Count(out.String(), "fpcalc is not available"); n != 1 {
		t.Errorf("fpcalc warning logged %d times, want 1 — it is consulted per sweep "+
			"and per card render, so an unconditional line is per-minute spam", n)
	}

	// Operator installs fpcalc, and NOW the missing key surfaces. With
	// one shared flag this second warning never appears.
	c.at, c.err = time.Now(), nil
	if ok, reason := c.ready(false); ok || reason != "no_api_key" {
		t.Fatalf("ready() = (%v, %q), want (false, no_api_key)", ok, reason)
	}
	if !strings.Contains(out.String(), "no AcoustID API key") {
		t.Error("the key warning was suppressed by the earlier fpcalc warning — the two " +
			"prerequisites need independent flags now that the feature is hot")
	}
	// And it too logs only once.
	for i := 0; i < 5; i++ {
		c.ready(false)
	}
	if n := strings.Count(out.String(), "no AcoustID API key"); n != 1 {
		t.Errorf("key warning logged %d times, want 1", n)
	}

	// Both fixed: ready, and nothing further logged.
	before := out.Len()
	if ok, reason := c.ready(true); !ok || reason != "" {
		t.Errorf("ready() = (%v, %q), want (true, \"\")", ok, reason)
	}
	if out.Len() != before {
		t.Errorf("a healthy probe logged: %q", out.String()[before:])
	}
}

// TestFingerprintToolchainCachesTheProbe — the probe is a fork-exec and
// every sweep plus every card render asks, so a TTL is what keeps that
// from becoming one process spawn per consult.
func TestFingerprintToolchainCachesTheProbe(t *testing.T) {
	c := &fingerprintToolchainCache{}
	// Seed a fresh failed probe; a cached read must not re-run it.
	sentinel := errors.New("seeded")
	c.at, c.err = time.Now(), sentinel
	if _, reason := c.ready(true); reason != "fpcalc_missing" {
		t.Fatalf("reason = %q, want fpcalc_missing from the cached probe", reason)
	}
	if c.err != sentinel {
		t.Error("the probe re-ran inside the TTL window")
	}
}
