package main

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// TestMain lowers the bcrypt work factor for this package's suite.
//
// `bridge admin reset-password` and the public-mode startup paths hash real
// passwords, and bcrypt's deliberate ~250 ms is multiplied by roughly eight
// under the race detector. Set once, before any test runs.
//
// The shipped cost is unchanged: adminauth.TestShippedHashCostIsUnchanged pins
// it, and TestNoProductionCodeLowersTheHashCost sweeps every non-test file to
// make sure nothing outside a suite calls this.
func TestMain(m *testing.M) {
	adminauth.SetTestHashCost(bcrypt.MinCost)
	os.Exit(m.Run())
}
