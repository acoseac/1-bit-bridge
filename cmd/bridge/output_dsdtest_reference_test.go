package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// referenceFixture writes a stereo WAV reference of bits-bit samples and
// frames frames at 44.1 kHz in a directory of its own, and returns the
// directory, its fixture and the samples' bytes as the file holds them.
func referenceFixture(t *testing.T, bits, frames int) (string, qualification.Fixture, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ref.wav")
	data := writePCMWAV(t, path, 44100, bits, frames, 0x33)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return dir, qualification.Fixture{ID: "ref", Path: "ref.wav", SHA256: fmt.Sprintf("%x", sha256.Sum256(b)), Container: "WAV",
		Rate: 44100, Channels: 2, Frames: int64(frames)}, data
}

// recordingPCMSink keeps the transport a reference opened and what it
// wrote, and counts its drains and closes.
type recordingPCMSink struct {
	transport      alsapcm.Transport
	written        []byte
	drains, closes int
}

func (s *recordingPCMSink) Write(_ context.Context, b []byte) (int, error) {
	s.written = append(s.written, b...)
	return len(b), nil
}
func (s *recordingPCMSink) Drain(context.Context) error { s.drains++; return nil }
func (s *recordingPCMSink) Close() error                { s.closes++; return nil }

// TestTheNativeReferenceIsOfferedTheBackendsPackings pins B735 in the DSD
// qualification harness: a PCM reference is offered its depth's packings in
// the backend's order (playback.Packings), each the DAC refuses as
// parameters_not_supported or parameters_mismatch passed over for the next,
// so a DAC with no S16_LE, or no S24_3LE, plays a 16- or 24-bit reference
// widened into S32_LE, each sample its own bytes at the container's top with
// zero bytes below them; a DAC that takes the depth's own container is asked
// for it alone, as before; any other refusal ends the reference at once;
// and a DoP reference is S32_LE alone, and still refused from 16 bits.
func TestTheNativeReferenceIsOfferedTheBackendsPackings(t *testing.T) {
	refuse := func(asked int, f format.ALSAFormat) error {
		reason := alsapcm.ReasonNotSupported
		if asked%2 == 0 {
			reason = alsapcm.ReasonMismatch
		}
		return &alsapcm.Error{Reason: reason, Op: "hw_refine", Endpoint: alsapcm.Endpoint{Card: 5}, Detail: "the DAC does not take " + string(f)}
	}
	for _, c := range []struct {
		bits  int
		takes format.ALSAFormat
		asked []format.ALSAFormat
		dop   bool
	}{
		{16, format.S16LE, []format.ALSAFormat{format.S16LE}, false},
		{16, format.S32LE, []format.ALSAFormat{format.S16LE, format.S243LE, format.S24LE, format.S32LE}, false},
		{24, format.S243LE, []format.ALSAFormat{format.S243LE}, false},
		{24, format.S32LE, []format.ALSAFormat{format.S243LE, format.S24LE, format.S32LE}, false},
		{24, format.S32LE, []format.ALSAFormat{format.S32LE}, true},
	} {
		t.Run(fmt.Sprintf("%d bits, the DAC takes %s, DoP %v", c.bits, c.takes, c.dop), func(t *testing.T) {
			dir, fixture, data := referenceFixture(t, c.bits, 1000)
			var asked []format.ALSAFormat
			sink := &recordingPCMSink{}
			open := func(_ context.Context, cfg alsapcm.SinkConfig) (nativePCMSink, error) {
				asked = append(asked, cfg.Transport.Format)
				if cfg.Transport.Format != c.takes {
					return nil, refuse(len(asked), cfg.Transport.Format)
				}
				sink.transport = cfg.Transport
				return sink, nil
			}
			if err := playNativeReference(context.Background(), dir, fixture, alsapcm.Endpoint{Card: 5}, nil, open, c.dop); err != nil {
				t.Fatalf("the reference: %v", err)
			}
			if !slices.Equal(asked, c.asked) {
				t.Errorf("the DAC was asked for %v, want %v", asked, c.asked)
			}
			if want := (alsapcm.Transport{Kind: format.KindPCM, RateHz: 44100, Channels: 2, Format: c.takes, ValidBits: c.bits}); sink.transport != want {
				t.Errorf("the reference opened %v, want %v", sink.transport, want)
			}
			want := data
			if c.takes == format.S32LE {
				size := c.bits / 8
				want = nil
				for i := 0; i+size <= len(data); i += size {
					want = append(append(want, make([]byte, 4-size)...), data[i:i+size]...)
				}
			}
			if !bytes.Equal(sink.written, want) {
				t.Errorf("the DAC got %d bytes that are not the reference's %d-bit samples in %s, %d bytes", len(sink.written), c.bits, c.takes, len(want))
			}
			if sink.drains != 1 || sink.closes != 1 {
				t.Errorf("%d drains and %d closes, want one each", sink.drains, sink.closes)
			}
		})
	}
	t.Run("any other refusal ends the reference", func(t *testing.T) {
		dir, fixture, _ := referenceFixture(t, 16, 1000)
		var asked int
		busy := &alsapcm.Error{Reason: alsapcm.ReasonBusy, Op: "open", Endpoint: alsapcm.Endpoint{Card: 5}}
		err := playNativeReference(context.Background(), dir, fixture, alsapcm.Endpoint{Card: 5}, nil,
			func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) { asked++; return nil, busy })
		if alsapcm.ReasonOf(err) != alsapcm.ReasonBusy || asked != 1 {
			t.Errorf("a busy DAC: %v after %d opens, want busy after one", err, asked)
		}
	})
	t.Run("a DoP reference of 16 bits is refused", func(t *testing.T) {
		dir, fixture, _ := referenceFixture(t, 16, 1000)
		var asked int
		err := playNativeReference(context.Background(), dir, fixture, alsapcm.Endpoint{Card: 5}, nil,
			func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) {
				asked++
				return &recordingPCMSink{}, nil
			}, true)
		if err == nil || !strings.Contains(err.Error(), "DoP reference requires 24 or 32-bit PCM for S32_LE") || asked != 0 {
			t.Errorf("a 16-bit DoP reference: %v after %d opens, want refused before an open", err, asked)
		}
	})
}
