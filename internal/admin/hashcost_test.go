package admin

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// TestMain lowers the bcrypt work factor for this package's suite.
//
// The admin tests seed and reset console passwords constantly, and every one of
// those pays bcrypt's deliberate ~250 ms — multiplied by roughly eight under the
// race detector. That, not the tests themselves, is what made CI's race job the
// twenty-minute leg of the gate.
//
// Set once, before any test runs, which is what keeps a plain variable safe
// here. The shipped cost is unchanged and guarded by
// adminauth.TestShippedHashCostIsUnchanged.
func TestMain(m *testing.M) {
	adminauth.SetTestHashCost(bcrypt.MinCost)
	os.Exit(m.Run())
}
