package admin

import "testing"

// TestCsvSafe_LeadingWhitespaceStillEscaped pins the formula-injection
// bypass. Excel and LibreOffice strip leading whitespace before deciding
// whether a cell is a formula, so "  =1+1" executes exactly like "=1+1"
// — but a bare s[0] check sees ' ' and passes it through.
//
// The values here reach the CSV export from file tags (track title,
// artist), so they are attacker-influenced in the realistic case of a
// downloaded or shared library.
func TestCsvSafe_LeadingWhitespaceStillEscaped(t *testing.T) {
	dangerous := []string{
		"=1+1",
		" =1+1",
		"  =cmd|' /c calc'!A0",
		"\t=1+1",
		"\t\t+1",
		" -1+1",
		"   @SUM(A1)",
		"\r\n=1+1",
		"\v=1+1",
		"\f=1+1",
	}
	for _, in := range dangerous {
		got := csvSafe(in)
		if len(got) == 0 || got[0] != '\'' {
			t.Errorf("csvSafe(%q) = %q — want a leading apostrophe; "+
				"spreadsheets trim leading whitespace before evaluating", in, got)
		}
	}
}

// TestCsvSafe_LeavesOrdinaryValuesAlone guards the other direction: the
// trim must not start quoting normal metadata.
func TestCsvSafe_LeavesOrdinaryValuesAlone(t *testing.T) {
	safe := []string{
		"",
		"Kind of Blue",
		"  Miles Davis", // leading space, but not a formula
		"Ella & Louis",
		"track 1 - intro",
		"3+3 (Live)", // '+' not in first non-space position
		"A#",
	}
	for _, in := range safe {
		if got := csvSafe(in); got != in {
			t.Errorf("csvSafe(%q) = %q, want it unchanged", in, got)
		}
	}
}
