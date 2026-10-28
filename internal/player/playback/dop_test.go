package playback

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"os"
	"path/filepath"
	"testing"
)

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
