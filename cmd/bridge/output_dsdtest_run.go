package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeSource(root string, f qualification.Fixture) (decode.Source, *os.File, error) {
	if !filepath.IsLocal(f.Path) {
		return nil, nil, errors.New("fixture escaped root")
	}
	path, e := filepath.EvalSymlinks(filepath.Join(root, f.Path))
	if e != nil {
		return nil, nil, e
	}
	base, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, nil, e
	}
	rel, e := filepath.Rel(base, path)
	if e != nil || !filepath.IsLocal(rel) {
		return nil, nil, errors.New("fixture escaped root")
	}
	file, _, e := fsutil.OpenAsFile(path)
	if e != nil {
		return nil, nil, e
	}
	h := sha256.New()
	_, e = io.Copy(h, io.LimitReader(file, 64<<20+1))
	if e != nil || fmt.Sprintf("%x", h.Sum(nil)) != f.SHA256 {
		file.Close()
		return nil, nil, errors.New("opened fixture hash mismatch")
	}
	if _, e = file.Seek(0, io.SeekStart); e != nil {
		file.Close()
		return nil, nil, e
	}
	src, e := decode.Open(file)
	if e != nil {
		file.Close()
		return nil, nil, e
	}
	return src, file, nil
}
func verifyNativeFixture(root string, f qualification.Fixture) error {
	src, file, e := nativeSource(root, f)
	if e != nil {
		return e
	}
	defer file.Close()
	defer src.Close()
	if f.Container == "WAV" {
		d, ok := src.(decode.Decoder)
		if !ok {
			return errors.New("PCM reference is not PCM")
		}
		p := d.Format()
		if p.RateHz != f.Rate || p.Channels != f.Channels || p.Frames != f.Frames {
			return errors.New("PCM reference header disagrees with manifest")
		}
		return nil
	}
	d, ok := src.(decode.DSDDecoder)
	if !ok {
		return errors.New("manifest DSD fixture is not DSD")
	}
	p := d.Format()
	if p.BitRateHz != f.Rate || p.Channels != f.Channels || p.Frames != f.Frames || !strings.EqualFold(string(p.Container), f.Container) {
		return errors.New("DSD fixture header disagrees with manifest")
	}
	return nil
}
func executeNativeTest(ctx context.Context, cancelRun context.CancelCauseFunc, o nativeOptions, env outputDSDTestEnv, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error, m qualification.Manifest, fixtures map[string]qualification.Fixture, approved *approvedTransition, in io.Reader, out io.Writer, report *nativeReport) (runResult error) {
	defer cancelRun(nil)
	var echo io.Writer
	if env.tty {
		echo = out
	}
	console := newNativeConsole(ctx, in, cancelRun, o.scenario == "level-check", echo)
	defer console.close()
	if console.err != nil {
		return console.err
	}
	defer func() {
		var stopped *nativeStop
		if errors.As(context.Cause(ctx), &stopped) {
			report.End = stopped.Error()
			report.Contact = nil
			runResult = withoutNativeCancellation(runResult)
		}
	}()

	stopDeadline := func() {}
	defer func() {
		defer stopDeadline()
		// No reads, successors, or fresh cleanup allowance after an unconfirmed
		// release. The worker or sink's completion handle still owns cleanup.
		if !errors.Is(runResult, alsapcm.ErrReleaseUnconfirmed) && ctx.Err() == nil {
			after, e := validateNativeFinal(ctx, env, ep.Card, report.Attachment, report.Before.Hash())
			report.After = after
			runResult = errors.Join(runResult, e)
		}
		if ctx.Err() != nil {
			runResult = errors.Join(runResult, ctx.Err())
		}
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			report.End = "deadline"
		}
		if runResult == nil && report.End == "complete" {
			report.Contact = report.pendingContact
		} else {
			report.Contact = nil
		}
		report.pendingContact = nil
	}()

	for _, id := range o.list {
		if e := verifyNativeFixture(o.fixtures, fixtures[id]); e != nil {
			return e
		}
		if fixtures[id].Container == "WAV" {
			continue
		}
		if e := verifyNativeFixture(o.fixtures, fixtures[fixtures[id].PCMReference]); e != nil {
			return e
		}
	}
	if o.scenario == "level-check" {
		typed, e := console.confirm(ctx, out, "Type exactly: "+qualification.CalibrationPhrase)
		if e != nil {
			return e
		}
		if typed != qualification.CalibrationPhrase {
			return errors.New("calibration not confirmed")
		}
		report.OwnerConfirmation = typed
	}
	// One deadline from first open; keys, pause, reopens and track changes never
	// reset it. Cancellation stops admission; active native sink cleanup joins its
	// existing 2 s bound, while opener/PCM waits cannot extend this run deadline.
	cancel := func() { cancelRun(context.DeadlineExceeded) }
	env.deadline = env.now().Add(time.Duration(o.seconds) * time.Second)
	ticks, stop := env.at(env.deadline)
	stopDeadline = stop
	var expired atomic.Bool
	go func() {
		select {
		case <-ticks:
			expired.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()
	var runErr error
	if o.scenario == "backend-lifecycle" || o.scenario == qualification.TransitionScenario {
		// A DoP scenario's evidence, complete, still needs the DAC's DSD
		// indication and the listening: the backend's opens prove neither.
		if e := executeBackendLifecycle(ctx, o, env, ep, check, fixtures, console, out, report, approved); e != nil || o.transport != "dop" || report.End != "complete" {
			return e
		}
		report.DoP, runErr = confirmDoP(ctx, console, out)
		return runErr
	}

	if o.scenario == "level-check" {
		f := fixtures[o.list[0]]
		pcm := fixtures[f.PCMReference]
		fmt.Fprintln(out, "Headphones off ears. Start at minimum; raise PCM until just audible off-ear, then keep that position. Stop for louder DSD or gain bypass.")
		runErr = playNativePCM(ctx, env, o.fixtures, pcm, ep, check)
		var audible, knob, matched string
		if runErr == nil {
			audible, runErr = console.confirm(ctx, out, "Type exactly: "+qualification.PCMConfirmation)
			if runErr == nil && audible != qualification.PCMConfirmation {
				runErr = errors.New("PCM not confirmed audible")
			}
		}
		if runErr == nil {
			knob, runErr = console.confirm(ctx, out, "Type the recorded knob position:")
			if runErr == nil && knob == "" {
				runErr = errors.New("missing knob position")
			}
		}
		if runErr == nil {
			runErr = runNativeQueue(ctx, o, env, ep, check, fixtures, console.keys, report)
		}
		if runErr == nil {
			matched, runErr = console.confirm(ctx, out, "At the SAME position DSD must be audible, not louder; type exactly: "+qualification.DSDConfirmation)
			if runErr == nil && matched != qualification.DSDConfirmation {
				runErr = errors.New("DSD not matched")
			}
		}
		if runErr == nil && o.transport == "dop" {
			report.DoP, runErr = confirmDoP(ctx, console, out)
		}
		if runErr == nil {
			report.pendingContact = &qualification.Contact{Device: &report.Device, Subdevice: &report.Subdevice, SchemaVersion: 2, QualificationVersion: qualification.Version, Tuple: report.Tuple, Attachment: report.Attachment, MixerHash: report.Before.Hash(), ManifestHash: report.ManifestHash, KnobPosition: knob, PCMAudible: audible, DSDMatched: matched}
			if o.transport == "dop" {
				report.pendingContact.SchemaVersion = 3
				report.pendingContact.DoP = report.DoP
			}
			report.End = "complete"
		}
	} else {
		keys := console.keys
		if o.scenario == "matrix" {
			pcm := fixtures[fixtures[o.list[0]].PCMReference]
			runErr = playNativePCM(ctx, env, o.fixtures, pcm, ep, check)
		}
		if runErr == nil {
			runErr = runNativeQueue(ctx, o, env, ep, check, fixtures, keys, report)
		}
		if runErr == nil && ctx.Err() == nil && report.End == "complete" && o.scenario == "matrix" {
			pcm := fixtures[fixtures[o.list[len(o.list)-1]].PCMReference]
			runErr = playNativePCM(ctx, env, o.fixtures, pcm, ep, check)
		}
	}
	if runErr == nil && o.transport == "dop" && o.scenario != "level-check" && report.End == "complete" {
		report.DoP, runErr = confirmDoP(ctx, console, out)
	}
	if expired.Load() {
		report.End = "deadline"
		return errors.Join(runErr, context.DeadlineExceeded)
	}
	return runErr
}
func feedNative(ctx context.Context, s nativeTestSink, o nativeOptions, fixtures map[string]qualification.Fixture, start int, fed []atomic.Int64) error {
	planes := [][]byte{make([]byte, 16384), make([]byte, 16384)}
	for i := start; i < len(o.list); i++ {
		f := fixtures[o.list[i]]
		src, file, e := nativeSource(o.fixtures, f)
		if e != nil {
			return e
		}
		d, ok := src.(decode.DSDDecoder)
		if !ok {
			src.Close()
			file.Close()
			return errors.New("fixture changed type")
		}
		remain := d.Format().Frames
		for remain > 0 {
			n, re := d.ReadDSD(ctx, planes)
			bits := int(min(int64(n)*8, remain))
			if bits > 0 {
				accepted, we := s.WriteDSD(ctx, uint64(i+1), planes[0][:n], planes[1][:n], 0, bits)
				fed[i].Add(int64(accepted))
				remain -= int64(accepted)
				if we != nil {
					src.Close()
					file.Close()
					return we
				}
				if accepted != bits {
					src.Close()
					file.Close()
					return io.ErrShortWrite
				}
			}
			if re != nil {
				if re != io.EOF || remain != 0 {
					src.Close()
					file.Close()
					return re
				}
				break
			}
			if n == 0 {
				src.Close()
				file.Close()
				return io.ErrNoProgress
			}
		}
		src.Close()
		file.Close()
	}
	return s.Finish(ctx)
}
func runNativeQueue(ctx context.Context, o nativeOptions, env outputDSDTestEnv, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error, fixtures map[string]qualification.Fixture, keys <-chan byte, report *nativeReport) error {
	pause := alsapcm.PauseKernel
	if o.pause == "silence" {
		pause = alsapcm.PauseZeros
	}
	kind := format.KindNativeDSD
	if o.transport == "dop" {
		kind = format.KindDoP
	}
	cfg := alsapcm.DSDConfig{Kind: kind, Endpoint: ep, Format: format.ALSAFormat(o.format), BitRate: o.rate, Pause: pause, Tail: time.Duration(o.tail) * time.Millisecond, Prefill: time.Duration(o.prefill) * time.Millisecond, Check: check}
	at := 0
	report.Completed = make([]bool, len(o.list))
	complete := func() error {
		if o.scenario != "interactive" {
			if report.Navigated {
				return errors.New("qualification navigation leaves evidence incomplete")
			}
			for _, played := range report.Completed {
				if !played {
					return errors.New("required fixture occurrence did not play to its natural end")
				}
			}
		}
		report.End = "complete"
		return nil
	}
	var pendingKeys []byte
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		s, e := nativeOpenCall(ctx, env, cfg)
		if e != nil {
			return e
		}
		report.Opened = append(report.Opened, s.Params())
		if e = s.Activate(ctx, 1); e != nil {
			ce := s.Release(context.Background(), alsapcm.Immediate)
			return errors.Join(e, ce)
		}
		fed := make([]atomic.Int64, len(o.list))
		// Played is cumulative only within this sink. Each occurrence is capped
		// by both its own content length and the bytes this feeder accepted.
		coverage := func(played int64) int {
			current := at
			for i := at; i < len(o.list); i++ {
				frames := fixtures[o.list[i]].Frames
				accepted := min(frames, fed[i].Load())
				heard := min(max(played, 0), accepted)
				if heard == frames {
					report.Completed[i] = true
				}
				if heard > 0 {
					current = i
				}
				played -= accepted
				if accepted < frames || played <= 0 {
					break
				}
			}
			return current
		}
		feedCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func(start int) { done <- feedNative(feedCtx, s, o, fixtures, start, fed) }(at)
		paused := false
		next := -1
	commandLoop:
		for {
			commandKeys := keys
			replay := len(pendingKeys) > 0
			if replay {
				ch := make(chan byte, 1)
				ch <- pendingKeys[0]
				commandKeys = ch
			}
			select {
			case <-ctx.Done():
				stop()
				ce := s.Release(context.Background(), alsapcm.Immediate)
				return errors.Join(ctx.Err(), ce)
			case e := <-done:
				current := coverage(s.Position().Played)
				stop()
				ce := s.Release(context.Background(), alsapcm.Immediate)
				if ce == nil && errors.Is(e, context.Canceled) {
					e = nativeConfirmedCancellation(e)
				}
				if e == nil && ce == nil {
					// Inspect the whole queued batch: a space must not conceal
					// navigation that raced natural completion.
					queued := len(keys)
					for range queued {
						key := <-keys
						if key == 'n' || key == 'p' {
							if key == 'p' && current == 0 {
								report.IgnoredKeys = append(report.IgnoredKeys, "p:first-fixture")
							} else {
								report.Navigated = true
							}
						}
					}
					e = complete()
				}
				return errors.Join(e, ce)
			case key, ok := <-commandKeys:
				if replay {
					pendingKeys = pendingKeys[1:]
				}
				if !ok {
					keys = nil
					continue
				}
				switch key {
				case ' ':
					if o.transport == "dop" && o.scenario == "level-check" {
						stop()
						return errors.Join(errors.New("contact pause invalidates evidence"), s.Release(context.Background(), alsapcm.Immediate))
					}
					if paused {
						e = s.Resume(ctx)
					} else {
						e = s.Pause(ctx)
					}
					if e != nil {
						stop()
						ce := s.Release(context.Background(), alsapcm.Immediate)
						return errors.Join(e, ce)
					}
					paused = !paused
				case 'n', 'p':
					if o.transport == "dop" && o.scenario == "level-check" {
						stop()
						return errors.Join(errors.New("contact navigation invalidates evidence"), s.Release(context.Background(), alsapcm.Immediate))
					}
					current := coverage(s.Position().Played)
					if key == 'p' && current == 0 {
						report.IgnoredKeys = append(report.IgnoredKeys, "p:first-fixture")
						continue
					}
					report.Navigated = true
					next = current + 1
					if key == 'p' {
						next = max(0, current-1)
					}
					s.BeginRelease(alsapcm.TrackChange)
					stop()
					stopped, queued, re := nativeTrackRelease(ctx, s, keys, report)
					if len(pendingKeys)+len(queued) > 16 {
						return errors.New("native command queue exceeded")
					}
					pendingKeys = append(pendingKeys, queued...)
					if re != nil || stopped {
						return re
					}
					if o.scenario != "interactive" {
						return errors.New("qualification navigation leaves evidence incomplete")
					}
					break commandLoop
				case 's', 'q':
					stop()
					e = s.Release(context.Background(), alsapcm.Immediate)
					if e == nil {
						report.End = (&nativeStop{key}).Error()
					}
					return e
				}
			}
		}
		if next >= len(o.list) {
			return complete()
		}
		at = next
	}
}

// productionNativePCMOpen guards only hardware. Verification and admission
// stay above the injected sink seam, so tests exercise the shipping helper.
func productionNativePCMOpen(ctx context.Context, c alsapcm.SinkConfig) (nativePCMSink, error) {
	if testing.Testing() {
		return nil, errors.New("PCM reference hardware prohibited in tests")
	}
	return alsapcm.OpenSink(ctx, c)
}
func playNativePCM(ctx context.Context, env outputDSDTestEnv, root string, f qualification.Fixture, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error) error {
	return nativePCMCall(ctx, env, func(ctx context.Context) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return playNativeReference(ctx, root, f, ep, check, env.openPCM, env.forceS32)
	})
}
func playNativeReference(ctx context.Context, root string, fixture qualification.Fixture, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error, open func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error), forceS32 ...bool) (result error) {
	src, file, e := nativeSource(root, fixture)
	if e != nil {
		return e
	}
	defer file.Close()
	defer src.Close()
	d, ok := src.(decode.Decoder)
	if !ok {
		return errors.New("reference must be PCM")
	}
	p := d.Format()
	if p.RateHz != fixture.Rate || p.Channels != fixture.Channels || p.Frames != fixture.Frames || fixture.Container != "WAV" {
		return errors.New("opened PCM geometry disagrees with manifest")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	f := format.S32LE
	if p.ValidBits == 16 {
		f = format.S16LE
	}
	if p.ValidBits == 24 {
		f = format.S243LE
	}
	if len(forceS32) > 0 && forceS32[0] {
		if p.ValidBits != 24 && p.ValidBits != 32 {
			return errors.New("DoP reference requires 24 or 32-bit PCM for S32_LE")
		}
		f = format.S32LE
	}
	s, e := open(ctx, alsapcm.SinkConfig{Endpoint: ep, Admit: check, Transport: alsapcm.Transport{Kind: format.KindPCM, Format: f, RateHz: p.RateHz, Channels: p.Channels, ValidBits: p.ValidBits}})
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, s.Close()) }()
	samples := make([]int32, 8192)
	packed := make([]byte, format.PackedBytes(len(samples), f))
	for {
		n, re := d.ReadFrames(ctx, samples)
		if n > 0 {
			size, e := format.Pack(packed, samples[:n], p.ValidBits, f)
			if e != nil {
				return e
			}
			if _, e = s.Write(ctx, packed[:size]); e != nil {
				return e
			}
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return re
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return s.Drain(ctx)
}

// nativePCMCall bounds only this provisional harness. The existing PCM sink
// retains its teardown semantics and owns cleanup until the worker returns.
func nativePCMCall(ctx context.Context, env outputDSDTestEnv, play func(context.Context) error) error {
	answer := make(chan error, 1)
	done := make(chan struct{})
	go func() { e := play(ctx); close(done); answer <- e }()
	select {
	case e := <-answer:
		return e
	case <-ctx.Done():
	}
	ticks, stop := nativeCleanupTimer(ctx, env)
	defer stop()
	select {
	case e := <-answer:
		return errors.Join(ctx.Err(), e)
	case <-ticks:
		select {
		case e := <-answer:
			return errors.Join(ctx.Err(), e)
		default:
		}
		return &nativePendingWorker{cause: ctx.Err(), done: done}
	}
}

// nativeTrackRelease keeps Stop/quit live while the same sink drains its bounded
// prefix. Other keys are retained in a bounded queue for the next command loop.
func nativeTrackRelease(ctx context.Context, s nativeTestSink, keys <-chan byte, report *nativeReport) (bool, []byte, error) {
	released := make(chan error, 1)
	go func() { released <- s.Release(ctx, alsapcm.TrackChange) }()
	var pending []byte
	for {
		select {
		case e := <-released:
			return false, pending, e
		case <-ctx.Done():
			return true, nil, errors.Join(ctx.Err(), s.Release(context.Background(), alsapcm.Immediate))
		case key, ok := <-keys:
			if !ok {
				keys = nil
				continue
			}
			if key == 's' || key == 'q' {
				e := s.Release(context.Background(), alsapcm.Immediate)
				if e == nil {
					report.End = (&nativeStop{key}).Error()
				}
				return true, nil, e
			}
			if key == ' ' || key == 'n' || key == 'p' {
				if len(pending) == 16 {
					return true, nil, errors.Join(errors.New("native command queue exceeded"), s.Release(context.Background(), alsapcm.Immediate))
				}
				pending = append(pending, key)
			}
		}
	}
}

// Final validation remains inside the run deadline and publishes no partial
// result if a read is parked or cancellation wins.
func validateNativeFinal(ctx context.Context, env outputDSDTestEnv, card int, want qualification.Attachment, hash string) (alsactl.Snapshot, error) {
	type result struct {
		snapshot alsactl.Snapshot
		err      error
	}
	answer := make(chan result, 1)
	go func() {
		a, e := env.attachment(card)
		if e != nil {
			answer <- result{err: e}
			return
		}
		if e := ctx.Err(); e != nil {
			answer <- result{err: e}
			return
		}
		s, e := env.controls(card)
		if e == nil && (a != want || s.Hash() != hash) {
			e = errors.New("final binding/mixer reference changed")
		}
		answer <- result{s, e}
	}()
	select {
	case r := <-answer:
		if ctx.Err() != nil {
			return alsactl.Snapshot{}, ctx.Err()
		}
		return r.snapshot, r.err
	case <-ctx.Done():
		return alsactl.Snapshot{}, ctx.Err()
	}
}

// nativeOpenCall retains an explicit worker fence, including an opener's own
// admission/cleanup worker. Cancellation gets one allowance, never two.
func nativeOpenCall(ctx context.Context, env outputDSDTestEnv, cfg alsapcm.DSDConfig) (nativeTestSink, error) {
	type result struct {
		sink nativeTestSink
		err  error
	}
	// A successful send transfers cleanup ownership. Until then the worker owns
	// the sink, even if cancellation lands just after open's last context check.
	answer := make(chan result)
	done := make(chan struct{})
	var workerErr error
	go func() {
		defer close(done)
		s, e := env.open(ctx, cfg)
		var pending interface{ Completion() <-chan struct{} }
		if errors.As(e, &pending) {
			<-pending.Completion()
			if result, ok := pending.(interface{ CompletionResult() error }); ok {
				e = result.CompletionResult()
			}
		}
		select {
		case answer <- result{s, e}:
			return
		case <-ctx.Done():
			if s != nil {
				e = errors.Join(e, s.Release(context.Background(), alsapcm.Immediate))
				<-s.Done()
			}
			workerErr = e
		}
	}()
	select {
	case r := <-answer:
		if ctx.Err() == nil && r.err == nil {
			return r.sink, nil
		}
		if r.sink != nil {
			e := nativePCMCall(ctx, env, func(context.Context) error {
				e := r.sink.Release(context.Background(), alsapcm.Immediate)
				<-r.sink.Done()
				return e
			})
			return nil, errors.Join(ctx.Err(), r.err, e)
		}
		return nil, errors.Join(ctx.Err(), r.err)
	case <-ctx.Done():
	}
	ticks, stop := nativeCleanupTimer(ctx, env)
	defer stop()
	select {
	case <-done:
		return nil, errors.Join(ctx.Err(), workerErr)
	case <-ticks:
		select {
		case <-done:
			return nil, errors.Join(ctx.Err(), workerErr)
		default:
			return nil, &nativePendingWorker{cause: ctx.Err(), done: done}
		}
	}
}

// The opener/PCM worker gets only the remainder of the run deadline, capped at
// two seconds from cancellation. Deadline cancellation never rearms an allowance.
func nativeCleanupTimer(ctx context.Context, env outputDSDTestEnv) (<-chan time.Time, func()) {
	deadline := env.now().Add(2 * time.Second)
	if !env.deadline.IsZero() && env.deadline.Before(deadline) {
		deadline = env.deadline
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) || !env.now().Before(deadline) {
		expired := make(chan time.Time)
		close(expired)
		return expired, func() {}
	}
	return env.at(deadline)
}

type nativePendingWorker struct {
	cause error
	done  <-chan struct{}
}

func (e *nativePendingWorker) Error() string {
	return errors.Join(alsapcm.ErrReleaseUnconfirmed, e.cause).Error()
}
func (e *nativePendingWorker) Unwrap() []error {
	return []error{alsapcm.ErrReleaseUnconfirmed, e.cause}
}
func (e *nativePendingWorker) Completion() <-chan struct{} { return e.done }

// A cancelled Finish may report unconfirmed before cleanup has had its budget.
// Successful Release confirms that fence; other errors remain in the report.
func nativeConfirmedCancellation(e error) error {
	if e == alsapcm.ErrReleaseUnconfirmed {
		return nil
	}
	if joined, ok := e.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, v := range joined.Unwrap() {
			kept = append(kept, nativeConfirmedCancellation(v))
		}
		return errors.Join(kept...)
	}
	return e
}

// confirmDoP never infers decoder lock from the PCM carrier's successful open.
func confirmDoP(ctx context.Context, console *nativeConsole, out io.Writer) (*qualification.DoPObservations, error) {
	indicator, e := console.confirm(ctx, out, "Type exactly: DAC DSD indicator confirmed")
	if e != nil {
		return nil, e
	}
	dopIndicatorConfirmed := indicator == "DAC DSD indicator confirmed"
	if !dopIndicatorConfirmed {
		return nil, errors.New("DoP DSD indicator unconfirmed")
	}
	listening, e := console.confirm(ctx, out, "Type exactly: L/R pitch and duration correct")
	if e != nil {
		return nil, e
	}
	correct := listening == "L/R pitch and duration correct"
	o := &qualification.DoPObservations{Indicator: &dopIndicatorConfirmed, Channels: &correct, PitchDuration: &correct}
	return o, o.Validate()
}
