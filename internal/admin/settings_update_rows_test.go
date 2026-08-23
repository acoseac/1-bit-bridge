package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// stubUpdater drives the two conditional Update rows on the Settings page.
type stubUpdater struct{ st UpdateStatus }

func (s stubUpdater) Status() UpdateStatus                  { return s.st }
func (s stubUpdater) CheckNow(context.Context) UpdateStatus { return s.st }
func (s stubUpdater) Install(context.Context, bool) (UpdateStatus, error) {
	return s.st, nil
}
func (s stubUpdater) Rollback(bool) error { return nil }

// dtBeforeDD matches a <dt> immediately followed by the <dd> carrying the
// given id, allowing only whitespace between them. Group 1 is the <dt>'s
// opening tag, so a caller can inspect the label's own attributes.
func dtBeforeDD(id string) *regexp.Regexp {
	return regexp.MustCompile(`(<dt[^>]*>)[^<]*</dt>\s*<dd id="` + regexp.QuoteMeta(id) + `"`)
}

// TestSettingsUpdateRowsRenderOnceAndKeepDTAdjacency pins the shape the
// Updates panel's two conditional rows must hold in BOTH states.
//
// The rows used to be written as {{if}}/{{else}} pairs that repeated the whole
// <dt>/<dd> in each arm. That renders correctly but repeats the id in the
// template source, which SonarCloud reports as a duplicate-id reliability bug
// and fails the gate on. Rewriting them as a single element with a conditional
// `hidden` attribute fixes that — but only if two things stay true, neither of
// which the source-level guard in templates_ids_test.go can check:
//
//   - The row must still exist when it has nothing to show. app.js resolves it
//     by id and toggles `hidden`; if the element is simply omitted in the empty
//     state, a later SSE frame carrying an error has nothing to reveal and the
//     operator never sees it.
//   - The <dt> must remain the <dd>'s immediately preceding ELEMENT. app.js
//     reaches the label through previousElementSibling to hide and reveal it in
//     step with the value. Template comments and whitespace are text nodes and
//     safe; slipping any real element between them would silently strand the
//     label visible above a hidden value.
func TestSettingsUpdateRowsRenderOnceAndKeepDTAdjacency(t *testing.T) {
	// Each row carries its OWN expectation, and the table includes the two
	// one-sided cases. A shared want-hidden would let both rows be wired to
	// the same field — a realistic copy-paste slip in a rewrite that made
	// these two rows structurally identical — and the test would still pass:
	// verified by pointing update-deferred at LastError, which the shared
	// form accepted and this form rejects.
	cases := []struct {
		name              string
		st                UpdateStatus
		wantErrorHidden   bool
		wantDeferalHidden bool
	}{
		{
			name:              "nothing to report",
			st:                UpdateStatus{CurrentVersion: "v0.1.9", Channel: "stable"},
			wantErrorHidden:   true,
			wantDeferalHidden: true,
		},
		{
			name: "error and deferral present",
			st: UpdateStatus{
				CurrentVersion: "v0.1.9",
				Channel:        "stable",
				LastError:      "dial tcp: connection refused",
				DeferredReason: "outside quiet hours",
			},
			wantErrorHidden:   false,
			wantDeferalHidden: false,
		},
		{
			// Discriminates the two rows: only an error.
			name: "error only",
			st: UpdateStatus{
				CurrentVersion: "v0.1.9",
				Channel:        "stable",
				LastError:      "dial tcp: connection refused",
			},
			wantErrorHidden:   false,
			wantDeferalHidden: true,
		},
		{
			// The mirror case: only a deferral.
			name: "deferral only",
			st: UpdateStatus{
				CurrentVersion: "v0.1.9",
				Channel:        "stable",
				DeferredReason: "outside quiet hours",
			},
			wantErrorHidden:   true,
			wantDeferalHidden: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.deps.Updater = stubUpdater{st: tc.st}
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/settings")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			body := string(raw)

			assertUpdateRow(t, body, "update-last-error", tc.wantErrorHidden)
			assertUpdateRow(t, body, "update-deferred", tc.wantDeferalHidden)
		})
	}
}

// assertUpdateRow checks one conditional Update row in a rendered page.
//
// Extracted from the table body rather than inlined: with two ids x three
// assertions x a two-case table the loop measured cognitive complexity 16
// against SonarCloud's limit of 15. That is a smell rather than a gate
// failure on its own, but it is the kind that tips a gate later, when the
// next edit to this file adds one more branch.
func assertUpdateRow(t *testing.T, body, id string, wantHidden bool) {
	t.Helper()

	attr := `id="` + id + `"`
	if n := strings.Count(body, attr); n != 1 {
		t.Errorf("%s appears %d times in the rendered page, want exactly 1 — "+
			"app.js resolves it with getElementById, which sees only the first, "+
			"and a repeat also trips SonarCloud Web:S7930", attr, n)
		return
	}

	m := dtBeforeDD(id).FindStringSubmatch(body)
	if m == nil {
		t.Errorf("no <dt> immediately precedes <dd %s> — app.js hides and reveals "+
			"the label via previousElementSibling, so an element slipped between "+
			"them leaves the label stranded", attr)
		return
	}

	// Both halves of the row have to track the data, and independently:
	// the template sets `hidden` on the dt and the dd separately, so a
	// change that hides only the value leaves an empty label sitting in the
	// list. Checking the dd alone accepts that — verified by dropping the
	// dt's condition, which passed until this assertion existed.
	dd := body[strings.Index(body, attr):]
	dd = dd[:strings.Index(dd, ">")]
	for _, part := range []struct {
		what string
		tag  string
	}{
		{"<dt> label", m[1]},
		{"<dd> value", dd},
	} {
		if got := strings.Contains(part.tag, "hidden"); got != wantHidden {
			t.Errorf("%s of row %s: hidden=%v, want %v (rendered tag: %q)",
				part.what, attr, got, wantHidden, part.tag)
		}
	}
}
