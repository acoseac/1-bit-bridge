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

// TestTheTickerSilencesOnlyCancellationWithoutAnotherFailure pins shutdown's
// cancellation-only silence, including an outer-expiration wrapper.
func TestTheTickerSilencesOnlyCancellationWithoutAnotherFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"cancellation alone", context.Canceled, ""},
		{"outer expiration", backup.VerifyExpiryErrorForTest(context.Canceled), ""},
		{"given-up step", errors.Join(context.Canceled, backup.ErrCheckGivenUp), "was given up"},
		{"unread record", errors.Join(context.Canceled, backup.ErrRecordNotReadBack), "record could not be read back"},
		{"write failure", errors.Join(context.Canceled, errors.New("injected write failure")), "injected write failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			reportTickerVerdict(ctx, &out, &errs, "shutdown", backup.Result{
				Dir: "snapshot", Verify: backup.VerifyResult{Write: backup.WriteUnknown}, VerifyErr: tc.err,
			})
			words := out.String() + errs.String()
			if tc.want == "" {
				if words != "" {
					t.Errorf("cancellation-only shutdown printed %q", words)
				}
			} else if !strings.Contains(words, tc.want) || !strings.Contains(words, "its write outcome is unknown") {
				t.Errorf("shutdown lost accompanying failure %q or write outcome: %s", tc.want, words)
			}
		})
	}
}

// TestTheReportersSayWhenAnIncompletePublicationWasNeverReadBack uses
// abandonPending's actual result shape: a published INCOMPLETE, no read made.
func TestTheReportersSayWhenAnIncompletePublicationWasNeverReadBack(t *testing.T) {
	v := backup.VerifyResult{
		Found: backup.Verification{
			DatabaseState: backup.StateIncomplete, DatabaseReasons: []backup.Reason{backup.ReasonCancelled},
			FilesState: backup.StateIncomplete, FilesReasons: []backup.Reason{backup.ReasonCancelled},
		},
		Write: backup.WriteRecorded, Recorded: true,
	}
	var out, errs bytes.Buffer
	if reportVerdict(&out, &errs, "", v, context.Canceled) {
		t.Error("an unread record passed")
	}
	cli := out.String() + errs.String()
	out.Reset()
	errs.Reset()
	// A live ticker reports a cancelled operation; shutdown-only silence is
	// covered separately above.
	reportTickerVerdict(context.Background(), &out, &errs, "startup", backup.Result{Dir: "snapshot", Verify: v, VerifyErr: context.Canceled})
	for _, words := range []string{cli, out.String() + errs.String(), keptWords(v), tickerKeptWords(v)} {
		if !strings.Contains(words, "it wrote its record") || !strings.Contains(words, "its record was not read back") || strings.Contains(words, "could not be read back") {
			t.Errorf("publication with no read-back misreported: %s", words)
		}
	}
}
