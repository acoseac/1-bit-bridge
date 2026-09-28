package adminauth

import (
	"errors"
	"testing"
)

// The tests in this file open more than one store on one adminauth.json, as
// store_crossprocess_test.go does: `bridge admin login-link` mints in a
// process of its own (cli below) and the running bridge redeems (a), and
// Store.mu serialises neither against the other. Until 2026-09-28 every live
// ticket shared one file, which each process rewrote whole from its own
// earlier read, so a write landing inside another process's window undid it.
// A test hook lands the other process's step inside that window, and every
// outcome is checked through a store opened afterwards, as the next process
// to look would see it.

// requireRedeems opens a fresh store on path and requires raw to redeem there
// as admin.
func requireRedeems(t *testing.T, path, raw, what string) {
	t.Helper()
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if user, err := c.RedeemLoginTicket(raw); err != nil || user != "admin" {
		t.Errorf("%s = (%q, %v) in a store opened afterwards, want (admin, nil)", what, user, err)
	}
}

// requireSpent opens a fresh store on path and requires raw not to redeem.
func requireSpent(t *testing.T, path, raw, what string) {
	t.Helper()
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if user, err := c.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
		t.Errorf("%s = (%q, %v) in a store opened afterwards, want ErrTicketInvalid: "+
			"a ticket spent once redeemed again", what, user, err)
	}
}

// TestAMintCannotRestoreATicketSpentDuringItsWrite is the reported defect: a
// mint whose read of the shared file predated a redemption renamed the spent
// ticket back into place, live again for the rest of its lifetime (up to
// MaxLoginTicketTTL). The redemption lands inside the mint's write, between
// its staging and its rename. Two rows, because the shared file had two
// branches: a redemption beside another live ticket rewrote the file, and one
// of the only ticket removed it. A mint now writes its own ticket's file and
// nothing else, so both the ticket spent and every other stay as they were.
func TestAMintCannotRestoreATicketSpentDuringItsWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		beside bool
	}{
		{"beside another live ticket", true},
		{"the only ticket", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, _, _ := runningBridge(t)
			cli, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			var other string
			if tc.beside {
				if other, err = cli.MintLoginTicket("admin"); err != nil {
					t.Fatal(err)
				}
			}
			spent, err := cli.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}

			fired := false
			var redeemErr error
			beforeTicketCommitHook = func() {
				if fired {
					return
				}
				fired = true
				_, redeemErr = a.RedeemLoginTicket(spent)
			}
			t.Cleanup(func() { beforeTicketCommitHook = nil })

			minted, err := cli.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the redemption never ran: the mint did not reach its rename")
			}
			if redeemErr != nil {
				t.Fatalf("the redemption inside the mint's write failed: %v", redeemErr)
			}
			requireSpent(t, path, spent, "the ticket redeemed while the mint was writing")
			requireRedeems(t, path, minted, "the ticket minted around the redemption")
			if tc.beside {
				requireRedeems(t, path, other, "the ticket beside the one redeemed")
			}
		})
	}
}

// TestARedemptionCannotDropATicketMintedDuringIt is the same window from the
// other side. A redemption read the shared file, then wrote it back without
// the ticket it spent (or removed it, when that ticket was the only one), so a
// mint landing between the read and the write was dropped, and the link it
// printed never redeemed. A redemption now removes its own ticket's file and
// nothing else, so the mint landing inside it survives. The tail mints once
// more after the spend, from the process that minted the spent ticket itself:
// no process writes back a ticket it knew of, even one it minted.
func TestARedemptionCannotDropATicketMintedDuringIt(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	cli, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	spent, err := cli.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}

	fired := false
	var minted string
	var mintErr error
	beforeTicketSpendHook = func() {
		if fired {
			return
		}
		fired = true
		minted, mintErr = cli.MintLoginTicket("admin")
	}
	t.Cleanup(func() { beforeTicketSpendHook = nil })

	if user, err := a.RedeemLoginTicket(spent); err != nil || user != "admin" {
		t.Fatalf("the redemption = (%q, %v), want (admin, nil)", user, err)
	}
	if !fired {
		t.Fatal("the mint never ran: the redemption did not reach its spend")
	}
	if mintErr != nil {
		t.Fatalf("the mint inside the redemption failed: %v", mintErr)
	}
	after, err := cli.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}

	requireSpent(t, path, spent, "the ticket redeemed")
	requireRedeems(t, path, minted, "the ticket minted while the redemption was spending another")
	requireRedeems(t, path, after, "the ticket minted after the spend")
}

// TestTwoInterleavedMintsBothLand: two mints whose writes overlap both leave
// a ticket that redeems. The shared file lost one of them, the one whose
// rename came first, and its docblock called that survivable: mint again. A
// link printed by `bridge admin login-link` that never redeems is not
// survivable to the person holding it, who cannot tell it from an expired
// one. Each mint now writes a file of its own.
func TestTwoInterleavedMintsBothLand(t *testing.T) {
	_, path, _, _ := runningBridge(t)
	first, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	fired := false
	var inner string
	var innerErr error
	beforeTicketCommitHook = func() {
		if fired {
			return
		}
		fired = true
		inner, innerErr = second.MintLoginTicket("admin")
	}
	t.Cleanup(func() { beforeTicketCommitHook = nil })

	outer, err := first.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the second mint never ran: the first did not reach its rename")
	}
	if innerErr != nil {
		t.Fatalf("the mint inside the other's write failed: %v", innerErr)
	}
	requireRedeems(t, path, outer, "the mint whose write the other landed inside")
	requireRedeems(t, path, inner, "the mint that landed inside the other's write")
}
