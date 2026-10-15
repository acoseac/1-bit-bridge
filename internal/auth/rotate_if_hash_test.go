package auth

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// Backlog B160: the pairing-code redemption rotates the token a code names
// only while the token still carries the hash it had when the code was
// issued. `bridge token rotate` is another process, so the check has to see
// a rotation written by a sibling store, including one that lands inside
// the redemption's own commit window.

func TestRotateIfHashRotatesWhileTheTokenStillCarriesTheHash(t *testing.T) {
	f := newCommitFixture(t)
	raw, tok, err := f.running.RotateIfHash(f.victim.ID, f.victim.Hash)
	if err != nil {
		t.Fatalf("RotateIfHash with the token's own hash: %v", err)
	}
	if tok.ID != f.victim.ID || tok.Hash == f.victim.Hash {
		t.Fatalf("rotated record = %+v, want the same ID with a new hash", tok)
	}
	if _, ok := f.running.Validate(raw); !ok {
		t.Error("the fresh secret does not validate")
	}
	if _, ok := f.running.Validate(f.victimRaw); ok {
		t.Error("the old secret still validates after the rotation")
	}
}

func TestRotateIfHashRefusesATokenRotatedSince(t *testing.T) {
	f := newCommitFixture(t)
	siblingRaw, _, err := f.sibling.Rotate(f.victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.running.RotateIfHash(f.victim.ID, f.victim.Hash); !errors.Is(err, ErrTokenChanged) {
		t.Fatalf("RotateIfHash after a sibling's rotation = %v, want ErrTokenChanged", err)
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the refused rotation wrote the token store")
	}
	if _, ok := f.running.Validate(siblingRaw); !ok {
		t.Error("the sibling's secret no longer validates")
	}
}

func TestRotateIfHashRefusesAnEmptyHashAndAnUnknownToken(t *testing.T) {
	f := newCommitFixture(t)
	if _, _, err := f.running.RotateIfHash(f.victim.ID, ""); !errors.Is(err, ErrTokenChanged) {
		t.Errorf("RotateIfHash with an empty hash = %v, want ErrTokenChanged: it names no secret", err)
	}
	if _, _, err := f.running.RotateIfHash("no-such-token", f.victim.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("RotateIfHash of an unknown token = %v, want ErrNotFound", err)
	}
	if _, ok := f.running.Validate(f.victimRaw); !ok {
		t.Error("a refused rotation replaced the secret")
	}
}

// The check runs inside the commit: a sibling's rotation that lands between
// this write's staging and its rename makes the commit read the file again
// and judge the token there, so the redemption cannot rotate over it.
func TestRotateIfHashSeesARotationThatLandsInItsCommitWindow(t *testing.T) {
	f := newCommitFixture(t)
	var siblingRaw string
	staged, ran := inCommitWindow(t, 1, func() {
		var err error
		if siblingRaw, _, err = f.sibling.Rotate(f.victim.ID); err != nil {
			t.Errorf("sibling rotate: %v", err)
		}
	})
	if _, _, err := f.running.RotateIfHash(f.victim.ID, f.victim.Hash); !errors.Is(err, ErrTokenChanged) {
		t.Fatalf("RotateIfHash with a sibling's rotation in its commit window = %v, want ErrTokenChanged", err)
	}
	if ran() != 1 || staged() != 1 {
		t.Fatalf("the sibling ran %d times inside %d stagings, want 1 and 1", ran(), staged())
	}
	if _, ok := f.running.Validate(siblingRaw); !ok {
		t.Error("the sibling's secret no longer validates: the redemption rotated over it")
	}
}
