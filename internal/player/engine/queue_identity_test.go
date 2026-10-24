package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestQueueNamesItsWriterAndRejectsAnotherIdentity(t *testing.T) {
	for _, kind := range []string{"same", "foreign", "foreign_stuck", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert("original")
			h.shutdown()
			data, err := os.ReadFile(h.queuePath())
			h.must("read", err)
			var f map[string]any
			h.must("decode", json.Unmarshal(data, &f))
			if f["version"] != float64(2) || f["uuid"] != h.config().PlayerUUID {
				t.Errorf("queue does not name its writer: version %v uuid %v", f["version"], f["uuid"])
			}
			switch kind {
			case "foreign", "foreign_stuck":
				h.tweak = func(c *Config) { c.PlayerUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7" }
			case "legacy":
				f["version"] = 1
				delete(f, "uuid")
				data, err = json.Marshal(f)
				h.must("encode", err)
				h.must("legacy", os.WriteFile(h.queuePath(), data, 0600))
			}
			if kind == "foreign_stuck" {
				h.files.renameErr = errors.New("cannot move")
			}
			h.boot()
			if kind == "same" || kind == "legacy" {
				got, ok := h.snap().Read(ids[0])
				if !ok || got.Ref.String() != "original" {
					t.Fatal("own queue was lost")
				}
				return
			}
			if h.snap().Len() != 0 {
				t.Error("foreign entries exposed under the running identity")
			}
			if n := h.logs.count("queue_identity_mismatch"); n != 1 {
				t.Errorf("typed mismatch logged %d times: %v", n, h.logs.all())
			}
			aside, err := filepath.Glob(h.queuePath() + ".bad-*")
			h.must("aside", err)
			if kind == "foreign_stuck" {
				_, err := h.eng.InsertAfter(bg, 0, ref("new"), nil)
				h.refused("insert", err, ReasonFileStuck)
				got, err := os.ReadFile(h.queuePath())
				h.must("read stuck", err)
				if string(got) != string(data) {
					t.Fatal("stuck foreign queue overwritten")
				}
			} else {
				if len(aside) != 1 {
					t.Fatalf("foreign queue not moved aside: %v", aside)
				}
				got, err := os.ReadFile(aside[0])
				h.must("read aside", err)
				if string(got) != string(data) {
					t.Fatal("foreign queue bytes changed")
				}
				h.insert("new")
				h.shutdown()
				h.boot()
				if h.logs.count("queue_identity_mismatch") != 1 {
					t.Fatal("mismatch logged again")
				}
			}
		})
	}
}
