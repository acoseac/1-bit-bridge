package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// Backlog B142: serve's shared sox probe (soxToolchainCache) ran `sox
// --help` under its lock on the request path whenever its 30 s TTL had run
// out, and cached a probe that timed out as no sox for the TTL: on a host
// whose sox is only slow to start, the live upscale and analysis gates
// closed, /v1/health said upscaleEnabled=false and the manifests lost every
// track's variants for 30 s, and the request that ran the probe waited up to
// 2 s with every concurrent one behind it.

var probedUsableSox = transcode.SoxInfo{Path: "sox", Version: "v14.4.2", Formats: []string{"flac", "wav"},
	FormatsKnown: true, HasFLAC: true}

// scriptedSoxProbe answers one probe after another from answers, repeating
// the last; calls counts the probes.
type scriptedSoxProbe struct {
	mu      sync.Mutex
	answers []func() (transcode.SoxInfo, error)
	calls   int
}

func (p *scriptedSoxProbe) probe(context.Context) (transcode.SoxInfo, error) {
	p.mu.Lock()
	i := min(p.calls, len(p.answers)-1)
	p.calls++
	answer := p.answers[i]
	p.mu.Unlock()
	return answer()
}

func (p *scriptedSoxProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func soxAnswers(answers ...func() (transcode.SoxInfo, error)) *scriptedSoxProbe {
	return &scriptedSoxProbe{answers: answers}
}

func probeSaysUsable() (transcode.SoxInfo, error) { return probedUsableSox, nil }

func probeTimesOut() (transcode.SoxInfo, error) {
	return transcode.SoxInfo{Path: "sox"}, transcode.ErrSoxProbeTimeout
}

func probeSaysGone() (transcode.SoxInfo, error) { return transcode.SoxInfo{}, transcode.ErrSoxMissing }

// expireSoxVerdict ages the cached verdict past the TTL.
func expireSoxVerdict(c *soxToolchainCache) {
	c.mu.Lock()
	c.at = time.Now().Add(-adminSoxTTL - time.Second)
	c.mu.Unlock()
}

// waitSoxProbe waits for the background probe a snapshot started to land.
func waitSoxProbe(t *testing.T, c *soxToolchainCache) {
	t.Helper()
	c.mu.Lock()
	done := c.refreshing
	c.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the background sox probe never landed")
	}
}

// probeAfterTTL ages the verdict, asks once (which starts the probe), waits
// for the probe, and returns what a snapshot then says.
func probeAfterTTL(t *testing.T, c *soxToolchainCache) error {
	t.Helper()
	expireSoxVerdict(c)
	_ = c.precheck()
	waitSoxProbe(t, c)
	return c.precheck()
}

// TestASoxProbeTimeoutKeepsTheLastAnswer is the defect: a probe that does not
// answer is no verdict, so the last answer stands through a run of timeouts,
// which is said once; a probe that answers ends the run, and the next run is
// said again.
func TestASoxProbeTimeoutKeepsTheLastAnswer(t *testing.T) {
	var logs bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&logs, nil)))
	p := soxAnswers(probeSaysUsable, probeTimesOut, probeTimesOut, probeSaysUsable, probeTimesOut)
	c := &soxToolchainCache{probe: p.probe}
	if err := c.precheck(); err != nil {
		t.Fatalf("premise: the first probe answered usable, the cache says %v", err)
	}
	for round := 1; round <= 4; round++ {
		if err := probeAfterTTL(t, c); err != nil {
			t.Fatalf("probe %d: the cache says %v, want the last answer (sox usable) through a timeout", round+1, err)
		}
	}
	if n := strings.Count(logs.String(), "did not answer in time"); n != 2 {
		t.Errorf("%d timeout lines, want one per run of timeouts (2):\n%s", n, logs.String())
	}
	if p.count() != 5 {
		t.Errorf("premise: %d probes, want 5", p.count())
	}
}

// TestASoxThatLeftThePathClosesTheGatesWhenItsProbeAnswers: a missing sox is
// a verdict the moment its probe answers, never kept back like a timeout.
func TestASoxThatLeftThePathClosesTheGatesWhenItsProbeAnswers(t *testing.T) {
	c := &soxToolchainCache{probe: soxAnswers(probeSaysUsable, probeSaysGone).probe}
	if err := c.precheck(); err != nil {
		t.Fatalf("premise: %v", err)
	}
	if err := probeAfterTTL(t, c); !errors.Is(err, transcode.ErrSoxMissing) {
		t.Errorf("after the probe that found no sox the cache says %v, want ErrSoxMissing", err)
	}
}

// TestASnapshotAfterTheTTLDoesNotWaitForTheProbe: once there is a verdict, a
// snapshot after the TTL answers from it at once, and the probe it started
// replaces the verdict when it lands.
func TestASnapshotAfterTheTTLDoesNotWaitForTheProbe(t *testing.T) {
	release := make(chan struct{})
	slowGone := func() (transcode.SoxInfo, error) {
		select {
		case <-release:
		case <-time.After(3 * time.Second): // a bound for main, where the snapshot itself waits
		}
		return transcode.SoxInfo{}, transcode.ErrSoxMissing
	}
	c := &soxToolchainCache{probe: soxAnswers(probeSaysUsable, slowGone).probe}
	if err := c.precheck(); err != nil {
		t.Fatalf("premise: %v", err)
	}
	expireSoxVerdict(c)
	start := time.Now()
	err := c.precheck()
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("the snapshot after the TTL took %v: it waited for the probe", took)
	}
	if err != nil {
		t.Errorf("the snapshot after the TTL says %v, want the last verdict while the probe runs", err)
	}
	close(release)
	waitSoxProbe(t, c)
	if err := c.precheck(); !errors.Is(err, transcode.ErrSoxMissing) {
		t.Errorf("after the probe landed the cache says %v, want its answer", err)
	}
}

// TestWithNoAnswerYetATimeoutIsTheVerdict: with nothing answered to keep, a
// first probe that times out closes the gates as before, until one answers.
func TestWithNoAnswerYetATimeoutIsTheVerdict(t *testing.T) {
	c := &soxToolchainCache{probe: soxAnswers(probeTimesOut, probeSaysUsable).probe}
	if err := c.precheck(); !errors.Is(err, transcode.ErrSoxProbeTimeout) {
		t.Fatalf("the first probe timed out and the cache says %v, want the timeout", err)
	}
	if err := probeAfterTTL(t, c); err != nil {
		t.Errorf("after a probe answered usable the cache says %v", err)
	}
}

// TestConcurrentFirstSnapshotsShareOneProbe: the first snapshots, asked at
// once, wait for one probe.
func TestConcurrentFirstSnapshotsShareOneProbe(t *testing.T) {
	gate := make(chan struct{})
	p := soxAnswers(func() (transcode.SoxInfo, error) { <-gate; return probedUsableSox, nil })
	c := &soxToolchainCache{probe: p.probe}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = c.precheck() }(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("snapshot %d: %v", i, err)
		}
	}
	if n := p.count(); n != 1 {
		t.Errorf("%d probes for 8 first snapshots, want 1", n)
	}
}
