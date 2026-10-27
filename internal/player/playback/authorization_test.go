package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevokeRetiredDoesNotCreateSession(t *testing.T) {
	b := &Backend{sessions: map[engine.SessionID]*session{}}
	b.Revoke(engine.RevokeInput{Session: 99, Reason: "late monitor"})
	if len(b.sessions) != 0 {
		t.Fatal("late revoke created a retired session")
	}
}
func TestRevokeKeepsPCMWithPreparedDSD(t *testing.T) {
	b := &Backend{sessions: map[engine.SessionID]*session{}}
	s := newSession(b, 1)
	b.sessions[1] = s
	s.chain = []*occ{{id: 1}, {id: 2, plan: &engine.NativePlan{}, done: make(chan struct{})}}
	close(s.chain[1].done)
	b.Revoke(engine.RevokeInput{Session: 1, Reason: "record deleted"})
	if s.fault != nil {
		t.Fatal("PCM faulted for prepared DSD")
	}
	if !s.chain[1].dropped {
		t.Fatal("prepared DSD survived revocation")
	}
	s.cancel()
}
func TestApprovalAbsentNeverLocates(t *testing.T) {
	for _, mode := range []string{"off", "native", "auto", "dop"} {
		t.Run(mode, func(t *testing.T) {
			b := &Backend{cfg: Config{Authorization: &Authorization{Mode: mode, StateDir: t.TempDir(), Record: "missing.json"}}}
			if _, err := b.readPermission(context.Background()); err == nil {
				t.Fatal("missing approval admitted")
			}
		})
	}
}

type edgeLocator struct{ calls *int }

func (l edgeLocator) Locate(context.Context) (Unit, error) {
	*l.calls++
	return Unit{}, errors.New("unexpected incoming hardware open")
}
func TestUnapprovedEdgesNeverOpen(t *testing.T) {
	for _, mode := range []string{"natural", "navigation"} {
		t.Run(mode, func(t *testing.T) {
			calls, opens := 0, 0
			b := &Backend{cfg: Config{Locator: edgeLocator{&calls}}, sessions: map[engine.SessionID]*session{}, nativeOpen: func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				opens++
				return nil, errors.New("unexpected open")
			}}
			from := &engine.NativePlan{Tuple: engine.NativeTuple{Format: "DSD_U32_BE", BitRate: 2822400}, PolicyID: "p64"}
			to := &engine.NativePlan{Tuple: engine.NativeTuple{Format: "DSD_U32_BE", BitRate: 5644800}, PolicyID: "p128"}
			b.rememberEdge(nativeTransport(*from), from, &permission{})
			s := newSession(b, 2)
			defer s.cancel()
			o := &occ{plan: to, transport: nativeTransport(*to), permission: &permission{}}
			if mode == "natural" {
				if s.transition(o) {
					t.Fatal("unapproved natural edge advanced")
				}
			} else {
				data, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", "dsd64.dff"))
				if e != nil {
					t.Fatal(e)
				}
				b.cfg.Resolver = edgeContent(data)
				b.nativeAdmission = func(format.NativeDSD) (engine.NativePlan, error) { return *to, nil }
				ref, _ := engine.NewReference("incoming")
				if e := b.Start(context.Background(), engine.StartRequest{Session: 2, First: engine.Occurrence{ID: 2, Ref: ref}}); e == nil {
					t.Fatal("unapproved navigation advanced")
				}
			}
			if calls != 0 || opens != 0 {
				t.Fatalf("incoming calls=%d opens=%d", calls, opens)
			}
			to.Edges = []engine.TransitionEdge{{From: edgeKey(nativeTransport(*from), from), To: edgeKey(o.transport, to)}}
			if e := b.checkEdge(o.transport, to, o.permission); e != nil {
				t.Fatal("approved directional edge refused", e)
			}
			b.rememberEdge(o.transport, to, o.permission)
			if e := b.checkEdge(nativeTransport(*from), from, &permission{}); e == nil {
				t.Fatal("reverse edge inferred")
			}
		})
	}
}
func TestPlaybackObservationStates(t *testing.T) {
	b := &Backend{policy: PlayerPolicy{BootID: "boot", Generation: 8}, sessions: map[engine.SessionID]*session{}}
	s := newSession(b, 1)
	defer s.cancel()
	b.sessions[1] = s
	out := &output{transport: alsapcm.Transport{Format: "DSD_U32_BE", RateHz: 88200, LogicalRate: 2822400, Channels: 2, ValidBits: 1}, policyID: "p"}
	o := &occ{id: 3}
	s.observeOccurrence(o, out)
	_, f := b.Facts()
	if f.State != "open" || f.Occurrence != 3 || f.RateHz != 88200 || f.LogicalRate != 2822400 || f.BootID != "boot" || f.PolicyGeneration != 8 {
		t.Fatalf("live facts=%+v", f)
	}
	b.notify(Note{Session: 1, Kind: NoteReleased})
	_, f = b.Facts()
	if f.State != "released" {
		t.Fatal("released observed open")
	}
	s.observeOccurrence(o, out)
	b.Revoke(engine.RevokeInput{Session: 1})
	b.notify(Note{Session: 1, Kind: NoteClosed})
	p, f := b.Facts()
	if f.State != "revoked" || len(p.Tuples) != 0 {
		t.Fatal("revocation lost")
	}
}

type edgeContent []byte

func (c edgeContent) Open(context.Context, engine.Reference) (Content, error) {
	return Content{Data: bytes.NewReader(c), Size: int64(len(c))}, nil
}

func TestApprovedTupleSelection(t *testing.T) {
	p := &permission{}
	for _, f := range []string{"DSD_U16_LE", "DSD_U32_LE", "DSD_U32_BE"} {
		p.record.Body.Policies = append(p.record.Body.Policies, qualification.ApprovedPolicy{ID: f, Tuple: qualification.Tuple{Format: f, BitRate: 2822400}})
	}
	f := format.NativeDSD{BitRateHz: 2822400, Channels: 2}
	for _, want := range []string{"DSD_U32_BE", "DSD_U32_LE", "DSD_U16_LE"} {
		got, e := p.selectNative(f)
		if e != nil || got.Tuple.Format != want {
			t.Fatalf("selection=%+v %v", got, e)
		}
		p.record.Body.Policies = p.record.Body.Policies[:len(p.record.Body.Policies)-1]
	}
	if _, e := p.selectNative(f); e == nil {
		t.Fatal("empty approved tuple set admitted")
	}
	p.record.Body.Policies = []qualification.ApprovedPolicy{{Tuple: qualification.Tuple{Format: "DSD_U32_BE", BitRate: 5644800}}}
	if _, e := p.selectNative(f); e == nil {
		t.Fatal("invented format/rate cross product")
	}
	p.record.Body.Policies[0].Tuple = qualification.Tuple{Format: "S32_LE", BitRate: 2822400}
	b := &Backend{}
	b.publishPermission(p, "")
	if len(b.Policy().Tuples) != 0 || b.Policy().Reason == "" {
		t.Fatal("DoP-only capability advertised")
	}
	if _, e := p.selectNative(f); e == nil {
		t.Fatal("unimplemented DoP selected")
	}
}

func TestApprovedConstructorContentGate(t *testing.T) {
	for _, mode := range []string{"dff_dst", "dff_missing", "dff_duplicate", "dff_short", "dsf_mono", "dsf_invalid_fmt"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _ := approvedBackendFixture(t)
			name := "dsd64.dff"
			if strings.HasPrefix(mode, "dsf_") {
				name = "lsb_dsd64_partial_block.dsf"
			}
			data, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", name))
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "dff_dst":
				at := bytes.Index(data, []byte("CMPR"))
				copy(data[at+12:], "DST ")
			case "dff_missing":
				at := bytes.Index(data, []byte("CMPR"))
				copy(data[at:], "XXXX")
			case "dff_short":
				at := bytes.Index(data, []byte("CMPR"))
				binary.BigEndian.PutUint64(data[at+4:at+12], 2)
			case "dff_duplicate":
				at := bytes.Index(data, []byte("PROP"))
				size := int(binary.BigEndian.Uint64(data[at+4 : at+12]))
				end := at + 12 + size
				extra := append([]byte("CMPR\x00\x00\x00\x00\x00\x00\x00\x04"), []byte("DSD ")...)
				data = append(append(append([]byte(nil), data[:end]...), extra...), data[end:]...)
				binary.BigEndian.PutUint64(data[at+4:at+12], uint64(size+len(extra)))
				binary.BigEndian.PutUint64(data[4:12], uint64(len(data)-12))
			case "dsf_mono":
				binary.LittleEndian.PutUint32(data[48:52], 1)
				binary.LittleEndian.PutUint32(data[52:56], 1)
			case "dsf_invalid_fmt":
				binary.LittleEndian.PutUint32(data[48:52], 99)
			}
			cfg.Resolver = edgeContent(data)
			b, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			opens := 0
			b.nativeOpen = func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				opens++
				return nil, errors.New("unexpected native open")
			}
			ref, _ := engine.NewReference("content")
			if e = b.Start(context.Background(), engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}}); e == nil {
				t.Fatal("ineligible content admitted")
			}
			if opens != 0 {
				t.Fatal("content refusal reached hardware open/write")
			}
			if e = b.Release(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
		})
	}
}
