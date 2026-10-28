package playback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"os"
	"path/filepath"
	"testing"
)

// TestDoPSourceRefusals: content the decoders refuse (mono, six channels, a
// 48 kHz-family rate) never reaches DoP admission, and DSD512, which the
// decoder takes, is refused at admission with its typed reason; neither is
// located nor opened, and the decoder still decodes DSD512.
func TestDoPSourceRefusals(t *testing.T) {
	golden := func(t *testing.T, name string) []byte {
		t.Helper()
		b, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", name))
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	atRate := func(t *testing.T, hz uint32) []byte {
		b := golden(t, "lsb_dsd64_partial_block.dsf")
		binary.LittleEndian.PutUint32(b[56:60], hz) // the fmt chunk's sampling frequency
		return b
	}
	for _, tc := range []struct {
		name     string
		content  func(*testing.T) []byte
		reason   engine.Reason
		admitted bool
	}{
		{"mono", func(t *testing.T) []byte { return golden(t, "mono.dsf") }, engine.Reason(decode.ReasonUnsupportedChannels), false},
		{"six_channels", func(t *testing.T) []byte { return golden(t, "six_channel.dsf") }, engine.Reason(decode.ReasonUnsupportedChannels), false},
		{"rate48", func(t *testing.T) []byte { return atRate(t, 3072000) }, engine.Reason(decode.ReasonUnsupportedLayout), false},
		{"DSD512", func(t *testing.T) []byte { return atRate(t, 22579200) }, ReasonRate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.content(t)
			locates, opens, admissions := 0, 0, 0
			b, e := New(Config{Resolver: edgeContent(data), Locator: edgeLocator{&locates}})
			if e != nil {
				t.Fatal(e)
			}
			b.nativeAdmission = func(d format.NativeDSD) (engine.NativePlan, error) {
				admissions++
				return engine.NativePlan{Tuple: engine.NativeTuple{Transport: "dop", Format: "S32_LE", BitRate: d.BitRateHz}, PolicyID: "dop-test", Pause: "kernel"}, nil
			}
			b.nativeOpen = func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				opens++
				return nil, errors.New("unexpected DoP open")
			}
			ref, _ := engine.NewReference("content")
			e = b.Start(context.Background(), engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}})
			var u *engine.Unplayable
			if !errors.As(e, &u) || u.Reason != tc.reason {
				t.Fatalf("start %v, want the refusal %s", e, tc.reason)
			}
			if locates != 0 || opens != 0 {
				t.Fatalf("the refusal located %d and opened %d times", locates, opens)
			}
			if (admissions == 1) != tc.admitted {
				t.Fatalf("%d admissions", admissions)
			}
			if e = b.Release(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if tc.name != "DSD512" {
				return
			}
			src, e := decode.Open(bytes.NewReader(data))
			if e != nil {
				t.Fatal("the decoder no longer takes DSD512:", e)
			}
			defer src.Close()
			if d, ok := src.(decode.DSDDecoder); !ok || d.Format().BitRateHz != 22579200 {
				t.Fatalf("decoded %T, want DSD512", src)
			}
		})
	}
}

func TestDoPProductionDisabled(t *testing.T) {
	for _, name := range []string{"selection", "publication"} {
		t.Run(name, func(t *testing.T) {
			for _, mixed := range []bool{false, true} {
				cfg, a := approvedBackendFixture(t)
				a.Body.Environment = qualification.Digest([]string{"linux/arm64", "test-go", "test-kernel"})
				for _, ev := range []*qualification.Evidence{&a.Body.Policies[0].Matrix, &a.Body.Policies[0].Lifecycle} {
					path := filepath.Join(cfg.Authorization.StateDir, ev.Path)
					data, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					var r map[string]any
					json.Unmarshal(data, &r)
					r["Environment"] = a.Body.Environment
					data, _ = json.Marshal(r)
					if e = os.WriteFile(path, data, 0600); e != nil {
						t.Fatal(e)
					}
					ev.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
				}
				p := a.Body.Policies[0]
				p.ID = "dop"
				p.Tuple.Transport = "dop"
				p.Tuple.Format = "S32_LE"
				yes := true
				for _, ev := range []*qualification.Evidence{&p.Matrix, &p.Lifecycle} {
					data, e := os.ReadFile(filepath.Join(cfg.Authorization.StateDir, ev.Path))
					if e != nil {
						t.Fatal(e)
					}
					var r map[string]any
					if e = json.Unmarshal(data, &r); e != nil {
						t.Fatal(e)
					}
					r["Tuple"] = p.Tuple
					r["NativeTuple"] = p.Tuple
					r["SchemaVersion"] = 2
					r["PackingVersion"] = 1
					r["SourceRate"] = 2822400
					r["CarrierRate"] = 176400
					r["Kernel"] = "test-kernel"
					r["Platform"] = "linux/arm64"
					r["GoVersion"] = "test-go"
					r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": 176400, "Channels": 2, "Msbits": 32, "PeriodFrames": 8820, "BufferFrames": 35280, "RateNum": 176400, "RateDen": 1}}

					r["DoP"] = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
					ev.Path = "dop-" + ev.Path
					data, _ = json.Marshal(r)
					if e = os.WriteFile(filepath.Join(cfg.Authorization.StateDir, ev.Path), data, 0600); e != nil {
						t.Fatal(e)
					}
					ev.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
				}
				if mixed {
					a.Body.Policies = append(a.Body.Policies, p)
				} else {
					a.Body.Policies = []qualification.ApprovedPolicy{p}
				}
				saveApprovedFixture(t, cfg.Authorization, a)
				// Real reader must accept before the independent production selector guard.
				if _, e := qualification.ReadApproval(cfg.Authorization.StateDir, "approved.json", a.Body.Identity); e != nil {
					t.Fatal("synthetic approved fixture:", e)
				}
				cfg.Resolver = edgeContent(nil)
				b, e := New(cfg)
				if e != nil {
					t.Fatal(e)
				}
				permission, e := b.readPermission(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				if name == "selection" {
					plan, e := permission.selectNative(format.NativeDSD{BitRateHz: 2822400, Channels: 2})
					if mixed {
						if e != nil || plan.Tuple.Format == "S32_LE" {
							t.Fatal("native mixed policy unusable", plan, e)
						}
					} else if e == nil {
						t.Fatal("DoP selected", plan)
					}
				} else {
					b.publishPermission(permission, "")
					pol := b.Policy()
					for _, v := range pol.Tuples {
						if v.Tuple.Format == "S32_LE" {
							t.Fatal("DoP published")
						}
					}
					if mixed && len(pol.Tuples) != 1 {
						t.Fatal("native publication lost")
					}
				}
				a.Body.Confirmation = "CLI candidate; not approved"
				saveApprovedFixture(t, cfg.Authorization, a)
				if _, e = qualification.ReadCandidate(cfg.Authorization.StateDir, "approved.json", a.Body.Identity); e != nil {
					t.Fatal("valid candidate rejected", e)
				}
				if _, e = b.readPermission(context.Background()); e == nil {
					t.Fatal("candidate became production permission")
				}
			}
		})
	}
}
