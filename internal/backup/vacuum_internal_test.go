package backup

import (
	"strings"
	"testing"
)

// TestTheVacuumStatementCallsItsTargetFunction pins vacuumIntoSQL, one
// literal so SonarCloud's S2077 stays quiet, to the function init
// registers: a statement naming another function would fail with "no such
// function" on every snapshot.
func TestTheVacuumStatementCallsItsTargetFunction(t *testing.T) {
	if !strings.Contains(vacuumIntoSQL, "INTO "+vacuumTargetFunc+"(") {
		t.Errorf("vacuumIntoSQL = %q, want its INTO target to call %s", vacuumIntoSQL, vacuumTargetFunc)
	}
}
