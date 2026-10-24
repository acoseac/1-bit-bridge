package engine

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

func TestReservationNamesTheRunningIdentityAndKeepsEveryFloor(t *testing.T) {
	for _, kind := range []string{"missing", "v1", "same", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
			var ids, tokens uint32
			if kind != "missing" {
				ids, tokens = 30000, 90000
				f := Reservation{Version: 2, UUID: id, IDs: ids, Tokens: tokens}
				if kind == "v1" {
					f.Version, f.UUID = 1, ""
				}
				if kind == "foreign" {
					f.UUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
				}
				b, err := json.Marshal(f)
				h.must("encode", err)
				h.must("seed", os.WriteFile(h.reservePath(), b, 0600))
			}
			h.boot()
			f, err := ReadReservation(h.reservePath())
			h.must("read", err)
			if f.Version != 2 || f.UUID != id {
				t.Fatalf("unstamped first reservation: %+v", f)
			}
			if got := h.snap().Token(); got != tokens+4097 || got > f.Tokens {
				t.Fatalf("first token %d, floor %d, durable %+v", got, tokens, f)
			}
			if got := h.insert("first")[0]; got != ids+4097 {
				t.Fatalf("first ID %d, floor %d", got, ids)
			}
		})
	}
}

func TestReservationWritesMergeBothCountersAndStampTheWriter(t *testing.T) {
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
	for _, counter := range []string{"ids", "tokens"} {
		t.Run(counter, func(t *testing.T) {
			h := newHarness(t)
			h.be = newFakeBackend(t)
			c := h.config()
			old := Reservation{Version: 2, UUID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", IDs: 10, Tokens: 20}
			if counter == "ids" {
				old.IDs = 90000
			} else {
				old.Tokens = 90000
			}
			b, err := json.Marshal(old)
			h.must("encode", err)
			h.must("seed", os.WriteFile(h.reservePath(), b, 0600))
			h.must("merge", WriteReservation(h.dir, h.reservePath(), Reservation{UUID: c.PlayerUUID, IDs: 1000, Tokens: 2000}))
			got, err := ReadReservation(h.reservePath())
			h.must("read", err)
			if got.IDs != max(old.IDs, 1000) || got.Tokens != max(old.Tokens, 2000) || got.UUID != c.PlayerUUID {
				t.Fatalf("merged %+v over %+v", got, old)
			}
		})
	}
}

func TestTokenExhaustionRefusesEveryTokenAdvancingEdit(t *testing.T) {
	h := newHarness(t)
	writeReserveFile(t, h, 1000, math.MaxUint32-idBlock-3)
	h.boot()
	if got := h.snap().Token(); got != math.MaxUint32-2 {
		t.Fatalf("first token %d", got)
	}
	ids := h.insert("a", "b")
	if got := h.snap().Token(); got != math.MaxUint32 {
		t.Fatalf("last token %d", got)
	}
	for i := 0; i < 2; i++ {
		_, err := h.eng.InsertAfter(bg, 0, ref("c"), nil)
		h.refused("insert", err, ReasonIDsExhausted)
		h.refused("delete", h.eng.Delete(bg, ids[0]), ReasonIDsExhausted)
		h.refused("delete all", h.eng.DeleteAll(bg), ReasonIDsExhausted)
	}
	if h.snap().Token() != math.MaxUint32 || h.snap().Len() != 2 {
		t.Fatal("exhaustion changed the queue or wrapped the token")
	}
}

func TestAnExhaustedReservationRefusesStart(t *testing.T) {
	for _, counter := range []string{"ids", "tokens"} {
		t.Run(counter, func(t *testing.T) {
			h := newHarness(t)
			ids, tokens := uint32(1000), uint32(2000)
			if counter == "ids" {
				ids = math.MaxUint32 - idBlock
			} else {
				tokens = math.MaxUint32 - idBlock
			}
			writeReserveFile(t, h, ids, tokens)
			before, err := os.ReadFile(h.reservePath())
			h.must("read", err)
			h.be = newFakeBackend(t)
			if _, err := New(h.config()); err == nil {
				t.Fatal("exhausted counter did not refuse start")
			}
			after, err := os.ReadFile(h.reservePath())
			h.must("read", err)
			if string(after) != string(before) {
				t.Fatal("refused start changed the reservation")
			}
		})
	}
}
