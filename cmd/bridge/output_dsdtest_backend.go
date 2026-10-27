package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

type lifecycleLocator struct {
	config string
	check  func(alsapcm.NodeInfo) error
}

func (l lifecycleLocator) Locate(ctx context.Context) (playback.Unit, error) {
	ep, check, err := resolveNativeTest(ctx, l.config)
	if err != nil {
		return playback.Unit{}, err
	}
	return playback.Unit{Endpoint: ep.Endpoint, Subdevice: ep.Subdevice, Admit: func(n alsapcm.NodeInfo) error {
		if err := check(n); err != nil {
			return err
		}
		return l.check(n)
	}}, nil
}
func executeBackendLifecycle(ctx context.Context, o nativeOptions, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error, fixtures map[string]qualification.Fixture, keys <-chan byte, out io.Writer, report *nativeReport) (result error) {
	candidate, err := qualification.ReadCandidate(filepath.Dir(o.provisional), filepath.Base(o.provisional), report.Identity)
	if err != nil {
		return err
	}
	if len(candidate.Body.Policies) != 1 || candidate.Body.Policies[0].Tuple != report.Tuple || candidate.Body.ManifestHash != report.ManifestHash || candidate.Body.Environment != report.Environment {
		return errors.New("candidate does not match explicit invocation")
	}
	var files []string
	var approvedFiles []qualification.Fixture
	for _, id := range o.list {
		files = append(files, filepath.Join(o.fixtures, fixtures[id].Path))
		approvedFiles = append(approvedFiles, fixtures[id])
	}
	state, err := os.MkdirTemp(filepath.Dir(o.report), "lifecycle-state-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	evidence := &lifecycleEvidence{entries: map[engine.OccurrenceID]int{}, lengths: map[engine.OccurrenceID]int64{}, complete: make([]bool, len(files)), actions: map[string]bool{}}
	defer evidence.copy(report)
	b, err := playback.NewQualificationBackend(playback.Config{Notify: func(n playback.Note) {
		evidence.mu.Lock()
		evidence.lastPCM = fmt.Sprintf("pcm/%s/%d/%d/%d/zero", n.Transport.Format, n.Transport.RateHz, n.Transport.Channels, n.Transport.ValidBits)
		if n.Kind == playback.NoteOpened || n.Kind == playback.NoteReopened {
			evidence.opened = append(evidence.opened, n.Params)
		}
		if n.Kind == playback.NoteReleased {
			evidence.actions["released"] = true
		}
		if n.Kind == playback.NoteReopened {
			evidence.actions["reopened"] = true
		}
		evidence.mu.Unlock()
	}, Resolver: lifecycleFiles{root: o.fixtures, fixtures: approvedFiles}, Locator: lifecycleLocator{o.config, check}, Authorization: &playback.Authorization{Mode: "native", Record: filepath.Base(o.provisional), StateDir: filepath.Dir(o.provisional), Sysfs: alsa.SysfsReader{Root: hostTrees.sysfs}}})
	if err != nil {
		return err
	}
	eng, err := engine.New(engine.Config{Backend: lifecycleBackend{b, evidence}, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: state, QueuePath: filepath.Join(state, "queue.json"), DSD: "native"})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(runCtx) }()
	defer func() {
		cancel()
		bound, stop := context.WithTimeout(context.Background(), engine.ReleaseTimeout+5*time.Second)
		defer stop()
		select {
		case <-done:
		case <-bound.Done():
			result = errors.Join(result, bound.Err())
		}
		result = errors.Join(result, b.Wait(bound))
	}()
	var after uint32
	for i := range files {
		ref, _ := engine.NewReference(fmt.Sprintf("file:%d", i+1))
		after, err = eng.InsertAfter(ctx, after, ref, nil)
		if err != nil {
			return err
		}
	}
	if err = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
		return err
	}
	if err = eng.Play(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "Real engine/backend: space play/pause/resume, n next, p previous, x Stop; finish each listed fixture naturally. Stop ends the run without approval.")
	report.Completed = make([]bool, len(files))

	for {
		changed := eng.Changed()
		snap := eng.Snapshot()
		if len(snap.Faults()) > 0 || len(snap.Skips()) > 0 {
			return errors.New("backend lifecycle fault or refusal")
		}
		if snap.DeviceReleased() {
			evidence.action("released")
		}
		if evidence.copy(report) && snap.State() == engine.Stopped {
			report.End = "complete"
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case key, ok := <-keys:
			if !ok {
				keys = nil
				continue
			}
			var e error
			switch key {
			case 'x':
				e = eng.Stop(ctx)
				evidence.action("stop")
			case ' ':
				if snap.State() != engine.Playing {
					e = eng.Play(ctx)
					evidence.action("resume")
				} else {
					e = eng.Pause(ctx)
					evidence.action("pause")
				}
			case 'n':
				e = eng.Next(ctx)
				evidence.action("next")
			case 'p':
				e = eng.Previous(ctx)
				evidence.action("previous")
			}
			if e != nil {
				return e
			}
		}
	}
}

type lifecycleEvidence struct {
	mu                sync.Mutex
	entries           map[engine.OccurrenceID]int
	lengths           map[engine.OccurrenceID]int64
	complete          []bool
	actions           map[string]bool
	opened            []alsapcm.Params
	lastEdge, lastPCM string
	edges             []qualification.Edge
}

func (v *lifecycleEvidence) action(name string) { v.mu.Lock(); v.actions[name] = true; v.mu.Unlock() }
func (v *lifecycleEvidence) occurrence(o engine.Occurrence) {
	n, e := strconv.Atoi(strings.TrimPrefix(o.Ref.String(), "file:"))
	if e != nil {
		return
	}
	v.mu.Lock()
	v.entries[o.ID] = n - 1
	v.mu.Unlock()
}
func (v *lifecycleEvidence) position(p engine.Position) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n, ok := v.entries[p.Occurrence]
	if ok && n >= 0 && n < len(v.complete) && v.lengths[p.Occurrence] > 0 && p.Frames >= v.lengths[p.Occurrence] {
		v.complete[n] = true
	}
}
func (v *lifecycleEvidence) details(id engine.OccurrenceID, d engine.Details) {
	v.mu.Lock()
	v.lengths[id] = d.Frames
	v.mu.Unlock()
}
func (v *lifecycleEvidence) copy(report *nativeReport) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	report.PassedEdges = slices.Clone(v.edges)
	report.Completed = slices.Clone(v.complete)
	report.Opened = slices.Clone(v.opened)
	report.LifecycleActions = nil
	for a := range v.actions {
		report.LifecycleActions = append(report.LifecycleActions, a)
	}
	slices.Sort(report.LifecycleActions)
	for _, done := range v.complete {
		if !done {
			return false
		}
	}
	for _, a := range []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"} {
		if !v.actions[a] {
			return false
		}
	}
	return true
}

type lifecycleBackend struct {
	*playback.Backend
	evidence *lifecycleEvidence
}

func (b lifecycleBackend) Start(ctx context.Context, r engine.StartRequest) error {
	b.evidence.occurrence(r.First)
	r.Events = lifecycleEvents{r.Events, b.evidence}
	return b.Backend.Start(ctx, r)
}
func (b lifecycleBackend) Prepare(ctx context.Context, s engine.SessionID, a engine.OccurrenceID, n *engine.Occurrence) error {
	if n != nil {
		b.evidence.occurrence(*n)
	}
	return b.Backend.Prepare(ctx, s, a, n)
}

type lifecycleEvents struct {
	engine.Events
	evidence *lifecycleEvidence
}

func (v lifecycleEvents) Details(s engine.SessionID, o engine.OccurrenceID, d engine.Details) {
	v.evidence.details(o, d)
	v.Events.(engine.DetailsEvents).Details(s, o, d)
}
func (v lifecycleEvents) Position(s engine.SessionID, p engine.Position) {
	v.evidence.position(p)
	v.Events.Position(s, p)
}
func (v lifecycleEvents) Finished(s engine.SessionID) {
	v.evidence.action("natural")
	v.Events.Finished(s)
}
func (v lifecycleEvents) NativePlan(s engine.SessionID, o engine.OccurrenceID, p engine.NativePlan) {
	v.Events.(engine.NativePlanEvents).NativePlan(s, o, p)
}
func (v lifecycleEvents) CurrentOccurrence(s engine.SessionID, o engine.OccurrenceID, p *engine.NativePlan) {
	v.evidence.mu.Lock()
	key := v.evidence.lastPCM
	if p != nil {
		key = fmt.Sprintf("native/%s/%d/2/msb/%s/%s/%d/%d/30", p.Tuple.Format, p.Tuple.BitRate, p.PolicyID, p.Pause, p.Tail.Milliseconds(), p.Prefill.Milliseconds())
	}
	if v.evidence.lastEdge != "" {
		edge := qualification.Edge{From: v.evidence.lastEdge, To: key}
		if !slices.Contains(v.evidence.edges, edge) {
			v.evidence.edges = append(v.evidence.edges, edge)
		}
	}
	v.evidence.lastEdge = key
	v.evidence.mu.Unlock()
	v.Events.(engine.CurrentOccurrenceEvents).CurrentOccurrence(s, o, p)
}
func (v lifecycleEvents) Retiring(t engine.ReleaseToken, n engine.NativeEvent) {
	if n.Kind == "position" {
		v.evidence.position(n.Position)
	}
	v.Events.(engine.NativeReleaseEvents).Retiring(t, n)
}

// lifecycleFiles hashes the descriptor actually opened for each occurrence,
// including navigation/reopen, rather than trusting an earlier path check.
type lifecycleFiles struct {
	root     string
	fixtures []qualification.Fixture
}

func (f lifecycleFiles) Open(ctx context.Context, ref engine.Reference) (playback.Content, error) {
	if err := ctx.Err(); err != nil {
		return playback.Content{}, err
	}
	n, e := strconv.Atoi(strings.TrimPrefix(ref.String(), "file:"))
	if e != nil || n < 1 || n > len(f.fixtures) {
		return playback.Content{}, errors.New("fixture reference invalid")
	}
	source, file, e := nativeSource(f.root, f.fixtures[n-1])
	if e != nil {
		return playback.Content{}, e
	}
	if e = source.Close(); e != nil {
		file.Close()
		return playback.Content{}, e
	}
	st, e := file.Stat()
	if e != nil {
		file.Close()
		return playback.Content{}, e
	}
	return playback.Content{Data: file, Size: st.Size(), Closer: file}, nil
}
