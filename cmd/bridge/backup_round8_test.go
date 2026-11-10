package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestTheReportersKeepWriteProgressWithoutAFoundVerdict pins every consumer
// at the outcome handoff, where the check's result may not be available.
func TestTheReportersKeepWriteProgressWithoutAFoundVerdict(t *testing.T) {
	for _, tc := range []struct {
		write backup.WriteOutcome
		words string
	}{
		{backup.WriteRecorded, "it wrote its record"},
		{backup.WriteNotDurable, "was written and is not known to survive a crash"},
		{backup.WriteUnfinished, "its write did not finish"},
		{backup.WriteUnknown, "its write outcome is unknown"},
	} {
		t.Run(tc.write.String(), func(t *testing.T) {
			v := backup.VerifyResult{Write: tc.write}
			err := errors.New("the caller stopped waiting for the worker's outcome")
			var out, errs bytes.Buffer
			if reportVerdict(&out, &errs, "", v, err) {
				t.Error("an unread record passed")
			}
			cli := out.String() + errs.String()
			out.Reset()
			errs.Reset()
			reportTickerVerdict(context.Background(), &out, &errs, "startup", backup.Result{Dir: "snapshot", Verify: v, VerifyErr: err})
			for _, words := range []string{cli, out.String() + errs.String(), keptWords(v), tickerKeptWords(v)} {
				if !strings.Contains(words, tc.words) || strings.Contains(words, "not recorded") || strings.Contains(words, "wrote nothing") {
					t.Errorf("lost %s progress: %s", tc.write, words)
				}
			}
			if tc.write == backup.WriteUnknown && writeWords(tc.write) != tc.words {
				t.Error("unknown write used the default wording")
			}
		})
	}
}
