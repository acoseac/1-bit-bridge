package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
func confirmNative(ctx context.Context, r *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprintln(out, prompt)
	ch := make(chan struct {
		s string
		e error
	}, 1)
	go func() {
		s, e := r.ReadString('\n')
		ch <- struct {
			s string
			e error
		}{strings.TrimSpace(s), e}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case v := <-ch:
		return v.s, v.e
	}
}
func executeNativeTest(parent context.Context, o nativeOptions, env outputDSDTestEnv, ep alsapcm.Endpoint, check func(alsapcm.NodeInfo) error, m qualification.Manifest, fixtures map[string]qualification.Fixture, in *bufio.Reader, out io.Writer, report *nativeReport) error {
	for _, id := range o.list {
		if e := verifyNativeFixture(o.fixtures, fixtures[id]); e != nil {
			return e
		}
		if e := verifyNativeFixture(o.fixtures, fixtures[fixtures[id].PCMReference]); e != nil {
			return e
		}
	}
	if o.scenario == "level-check" {
		typed, e := confirmNative(parent, in, out, "Type exactly: "+qualification.CalibrationPhrase)
		if e != nil {
			return e
		}
		if typed != qualification.CalibrationPhrase {
			return errors.New("calibration not confirmed")
		}
		report.OwnerConfirmation = typed
	}
	// One deadline from first open; keys, pause, reopens and track changes never
	// reset it. Cancellation stops admission; sink cleanup has its own 2 s bound.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ticks, stop := env.at(env.now().Add(time.Duration(o.seconds) * time.Second))
	defer stop()
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

	if o.scenario == "level-check" {
		f := fixtures[o.list[0]]
		pcm := fixtures[f.PCMReference]
		fmt.Fprintln(out, "Headphones off ears. Start at minimum; raise PCM until just audible off-ear, then keep that position. Stop for louder DSD or gain bypass.")
		runErr = env.playPCM(ctx, filepath.Join(o.fixtures, pcm.Path), ep)
		var audible, knob, matched string
		if runErr == nil {
			audible, runErr = confirmNative(ctx, in, out, "Type exactly: "+qualification.PCMConfirmation)
			if runErr == nil && audible != qualification.PCMConfirmation {
				runErr = errors.New("PCM not confirmed audible")
			}
		}
		if runErr == nil {
			knob, runErr = confirmNative(ctx, in, out, "Type the recorded knob position:")
			if knob == "" {
				runErr = errors.New("missing knob position")
			}
		}
		if runErr == nil {
			one := o
			one.list = one.list[:1]
			runErr = runNativeQueue(ctx, one, env, ep, check, fixtures, nil, report)
		}
		if runErr == nil {
			matched, runErr = confirmNative(ctx, in, out, "At the SAME position DSD must be audible, not louder; type exactly: "+qualification.DSDConfirmation)
			if runErr == nil && matched != qualification.DSDConfirmation {
				runErr = errors.New("DSD not matched")
			}
		}
		if runErr == nil {
			report.Contact = &qualification.Contact{SchemaVersion: 1, QualificationVersion: qualification.Version, Tuple: report.Tuple, Attachment: report.Attachment, MixerHash: report.Before.Hash(), ManifestHash: report.ManifestHash, KnobPosition: knob, PCMAudible: audible, DSDMatched: matched}
			report.End = "complete"
		}
	} else {
		var keys <-chan byte
		if env.tty {
			ch := make(chan byte)
			keys = ch
			go func() {
				defer close(ch)
				for {
					b, e := in.ReadByte()
					if e != nil {
						return
					}
					select {
					case ch <- b:
					case <-ctx.Done():
						return
					}
					if b == 'q' || b == 's' {
						return
					}
				}
			}()
		}
		if o.scenario == "matrix" {
			pcm := fixtures[fixtures[o.list[0]].PCMReference]
			runErr = env.playPCM(ctx, filepath.Join(o.fixtures, pcm.Path), ep)
		}
		if runErr == nil {
			runErr = runNativeQueue(ctx, o, env, ep, check, fixtures, keys, report)
		}
		if runErr == nil && o.scenario == "matrix" {
			pcm := fixtures[fixtures[o.list[len(o.list)-1]].PCMReference]
			runErr = env.playPCM(ctx, filepath.Join(o.fixtures, pcm.Path), ep)
		}
	}
	if expired.Load() {
		report.End = "deadline"
		return errors.Join(runErr, context.DeadlineExceeded)
	}
	return runErr
}
func feedNative(ctx context.Context, s nativeTestSink, o nativeOptions, fixtures map[string]qualification.Fixture, start int) error {
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
	cfg := alsapcm.DSDConfig{Endpoint: ep, Format: format.ALSAFormat(o.format), BitRate: o.rate, Pause: pause, Tail: time.Duration(o.tail) * time.Millisecond, Prefill: time.Duration(o.prefill) * time.Millisecond, Check: check}
	at := 0
	var pendingKeys []byte
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		s, e := env.open(ctx, cfg)
		if e != nil {
			return e
		}
		report.Opened = append(report.Opened, s.Params())
		if e = s.Activate(ctx, 1); e != nil {
			ce := s.Release(context.Background(), alsapcm.Immediate)
			return errors.Join(e, ce)
		}
		feedCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func(start int) { done <- feedNative(feedCtx, s, o, fixtures, start) }(at)
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
				stop()
				ce := s.Release(context.Background(), alsapcm.Immediate)
				if e == nil && ce == nil {
					report.End = "complete"
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
					current := at
					played := s.Position().Played
					for current < len(o.list)-1 && played >= fixtures[o.list[current]].Frames {
						played -= fixtures[o.list[current]].Frames
						current++
					}
					next = current + 1
					if key == 'p' {
						next = max(0, current-1)
					}
					stop()
					stopped, queued, re := nativeTrackRelease(ctx, s, keys, report)
					if len(pendingKeys)+len(queued) > 16 {
						return errors.New("native command queue exceeded")
					}
					pendingKeys = append(pendingKeys, queued...)
					if re != nil || stopped {
						return re
					}
					break commandLoop
				case 's', 'q':
					stop()
					e = s.Release(context.Background(), alsapcm.Immediate)
					if e == nil {
						report.End = "stopped"
					}
					return e
				}
			}
		}
		if next >= len(o.list) {
			report.End = "complete"
			return nil
		}
		at = next
	}
}

// playNativeReference uses the unchanged PCM sink; it is separately injected in
// harness tests and cannot open hardware from a test binary.
func playNativeReference(ctx context.Context, path string, ep alsapcm.Endpoint) error {
	if testing.Testing() {
		return errors.New("PCM reference hardware prohibited in tests")
	}
	file, _, e := fsutil.OpenAsFile(path)
	if e != nil {
		return e
	}
	defer file.Close()
	src, e := decode.Open(file)
	if e != nil {
		return e
	}
	defer src.Close()
	d, ok := src.(decode.Decoder)
	if !ok {
		return errors.New("reference must be PCM")
	}
	p := d.Format()
	f := format.S32LE
	if p.ValidBits == 16 {
		f = format.S16LE
	}
	if p.ValidBits == 24 {
		f = format.S243LE
	}
	s, e := alsapcm.OpenSink(ctx, alsapcm.SinkConfig{Endpoint: ep, Transport: alsapcm.Transport{Kind: format.KindPCM, Format: f, RateHz: p.RateHz, Channels: p.Channels, ValidBits: p.ValidBits}})
	if e != nil {
		return e
	}
	defer s.Close()
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
func nativePCMCall(ctx context.Context, play func(context.Context) error) error {
	answer := make(chan error, 1)
	go func() { answer <- play(ctx) }()
	select {
	case e := <-answer:
		return e
	case <-ctx.Done():
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case e := <-answer:
		return errors.Join(ctx.Err(), e)
	case <-timer.C:
		return errors.Join(alsapcm.ErrReleaseUnconfirmed, ctx.Err())
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
					report.End = "stopped"
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
