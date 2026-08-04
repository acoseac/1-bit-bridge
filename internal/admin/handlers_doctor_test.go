package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getDoctor(t *testing.T, s *Server) doctorResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/doctor", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.apiDoctor(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out doctorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestDoctorUnwiredSaysSoRatherThanLooksClean is the assertion that
// matters. An unwired doctor returning an empty report would render as
// "no problems found", which is the single most dangerous thing a health
// panel can say when it has not actually checked anything.
func TestDoctorUnwiredSaysSoRatherThanLooksClean(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.DoctorRun = nil

	out := getDoctor(t, s)
	if out.Available {
		t.Error("available = true with no DoctorRun wired")
	}
	if out.Reason == "" {
		t.Error("no reason given; the UI would have nothing to show but an empty panel")
	}
	if out.Report != nil {
		t.Error("a report was returned by an unwired doctor")
	}
}

// TestDoctorNilReportIsNotAPass — same hazard by a different route: a
// wired closure that returns nil must not read as a clean run.
func TestDoctorNilReportIsNotAPass(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.DoctorRun = func(context.Context) *DoctorReport { return nil }

	out := getDoctor(t, s)
	if out.Available || out.Report != nil {
		t.Errorf("nil report reported as available=%v report=%v", out.Available, out.Report)
	}
}

// TestDoctorPassesThroughReport pins the shape the UI renders, including
// that a failing check keeps its hint — the hint is the only part that
// tells the operator what to do about it.
func TestDoctorPassesThroughReport(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.DoctorRun = func(context.Context) *DoctorReport {
		return &DoctorReport{
			Checks: []DoctorCheck{
				{Name: "platform", Status: "ok", Summary: "darwin/arm64"},
				{Name: "port-api", Status: "fail", Summary: ":443 in use", Hint: "stop the other process"},
			},
			OK: 1, Fail: 1,
		}
	}

	out := getDoctor(t, s)
	if !out.Available || out.Report == nil {
		t.Fatalf("available=%v report=%v, want a delivered report", out.Available, out.Report)
	}
	if len(out.Report.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(out.Report.Checks))
	}
	if out.Report.Fail != 1 {
		t.Errorf("fail count = %d, want 1", out.Report.Fail)
	}
	if out.Report.Checks[1].Hint == "" {
		t.Error("the failing check lost its hint; the hint is the actionable half of a failure")
	}
}

// TestDoctorIsNotCached — the panel exists to answer "is it healthy NOW",
// and a browser cache hit would answer a question about the past.
func TestDoctorIsNotCached(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.DoctorRun = func(context.Context) *DoctorReport { return &DoctorReport{} }
	req := httptest.NewRequest(http.MethodGet, "/api/doctor", nil)
	rec := httptest.NewRecorder()
	s.apiDoctor(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
