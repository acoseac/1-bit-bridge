package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
	"github.com/google/uuid"
)

// TestAQueuedRenderRunsOnlyWhileItsFamilysGateIsOpen pins what serve's
// transcode pool asks when a worker takes a job (backlog B155): the live gate
// of the rendition family the job renders, the gate every enqueuer of that
// family passes. A family's job is shared: the pool dedups on (source,
// variant), so a phone's request for a rendition a sweep or a batch already
// queued rides that job. So the take-time gate is the family's, never the
// enqueuer's: dropping a job on a gate only one of its enqueuers passed (the
// pre-generation switch, a batch) would drop the phone's request with it.
//
// Each case is checked under all eight answers of the three kind gates,
// which the cases hold apart here though production nests them (each
// includes the upscale gate).
func TestAQueuedRenderRunsOnlyWhileItsFamilysGateIsOpen(t *testing.T) {
	type answers struct{ upscale, optimize, pcm bool }
	cases := []struct {
		name string
		spec transcode.JobSpec
		want func(a answers) bool
	}{
		{"an upscale job", transcode.JobSpec{Kind: transcode.JobKindUpscale},
			func(a answers) bool { return a.upscale }},
		{"a job with no kind (the batch's upscale)", transcode.JobSpec{},
			func(a answers) bool { return a.upscale }},
		{"a CarPlay optimize of a PCM source", transcode.JobSpec{Kind: transcode.JobKindOptimize},
			func(a answers) bool { return a.optimize }},
		{"a pre-generation optimize (background, no batch)", transcode.JobSpec{Kind: transcode.JobKindOptimize, Background: true},
			func(a answers) bool { return a.optimize }},
		{"a batch's optimize", transcode.JobSpec{Kind: transcode.JobKindOptimize, BatchID: uuid.New()},
			func(a answers) bool { return a.optimize }},
		{"the compact tier of a DSD source", transcode.JobSpec{Kind: transcode.JobKindOptimize, SourceIsDSD: true},
			func(a answers) bool { return a.optimize && a.pcm }},
		{"the faithful tier of a DSD source", transcode.JobSpec{Kind: transcode.JobKindPCMRender, SourceIsDSD: true},
			func(a answers) bool { return a.pcm }},
		{"a kind serve does not render", transcode.JobSpec{Kind: "remix"},
			func(answers) bool { return false }},
	}
	for _, tc := range cases {
		for mask := 0; mask < 8; mask++ {
			a := answers{upscale: mask&1 != 0, optimize: mask&2 != 0, pcm: mask&4 != 0}
			gates := renditionGates{
				upscale:  func() bool { return a.upscale },
				optimize: func() bool { return a.optimize },
				pcm:      func() bool { return a.pcm },
			}
			if got, want := gates.admits(tc.spec), tc.want(a); got != want {
				t.Errorf("%s with gates %+v: admits = %v, want %v", tc.name, a, got, want)
			}
		}
	}
	if (renditionGates{}).admits(transcode.JobSpec{Kind: transcode.JobKindUpscale}) {
		t.Error("admits with no gate wired = true, want false: a nil gate is closed")
	}
}

// TestThePoolsTakeJobsUnderTheV1Gates pins the wiring the tests above stand
// in for: serve's analysis pool asks the closure /v1 reads as the analysis
// gate (api.WithAnalysis's first argument), and its transcode pool asks the
// one renditionGates runServe builds, the closures POST /v1/upscale reads per
// kind (TestAStaleDownloadRendersUnderTheV1KindGates pins its fields). A pool
// with no gate runs whatever is queued after a switch-off, which is B155.
func TestThePoolsTakeJobsUnderTheV1Gates(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	var (
		v1Analysis    []string // WithAnalysis's first argument on the api.New chain
		poolGates     []string // analyze.WithLiveGate's argument inside analyze.NewPool
		gatesVars     []string // the variable the renditionGates literal is assigned to
		transcodeGate []string // the argument of upscalePool.SetLiveGate
		staleOpen     []string // staleRerender's open:
	)
	name := func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				return id.Name + "." + x.Sel.Name
			}
		}
		return fmt.Sprintf("a %T", e)
	}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "WithAnalysis" && len(x.Args) > 0 && chainStartsAtAPINew(sel.X):
				v1Analysis = append(v1Analysis, name(x.Args[0]))
			case sel.Sel.Name == "NewPool" && name(sel.X) == "analyze":
				for _, arg := range x.Args {
					if opt, ok := arg.(*ast.CallExpr); ok && name(opt.Fun) == "analyze.WithLiveGate" && len(opt.Args) == 1 {
						poolGates = append(poolGates, name(opt.Args[0]))
					}
				}
				if len(poolGates) == 0 {
					poolGates = append(poolGates, "none")
				}
			case sel.Sel.Name == "SetLiveGate" && name(sel.X) == "upscalePool" && len(x.Args) == 1:
				transcodeGate = append(transcodeGate, name(x.Args[0]))
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
				if lit, ok := x.Rhs[0].(*ast.CompositeLit); ok && name(lit.Type) == "renditionGates" {
					gatesVars = append(gatesVars, name(x.Lhs[0]))
				}
			}
		case *ast.CompositeLit:
			if name(x.Type) == "staleRerender" {
				forEachKeyedElement(x, func(key *ast.Ident, value ast.Expr) {
					if key.Name == "open" {
						staleOpen = append(staleOpen, name(value))
					}
				})
			}
		}
		return true
	})
	if len(v1Analysis) != 1 {
		t.Fatalf("the /v1 chain calls WithAnalysis %d times (%v), want once", len(v1Analysis), v1Analysis)
	}
	if len(poolGates) != 1 || poolGates[0] != v1Analysis[0] {
		t.Errorf("serve's analysis pool takes jobs under %v, want [%s], the gate /v1 reads: "+
			"a queued analysis would run after analysis is switched off", poolGates, v1Analysis[0])
	}
	if len(gatesVars) != 1 {
		t.Fatalf("runServe assigns the renditionGates literal to %v, want one variable", gatesVars)
	}
	if want := gatesVars[0] + ".admits"; len(transcodeGate) != 1 || transcodeGate[0] != want {
		t.Errorf("serve's transcode pool takes jobs under %v, want [%s]: "+
			"a queued render would run after its kind is switched off", transcodeGate, want)
	}
	if want := gatesVars[0] + ".open"; len(staleOpen) != 1 || staleOpen[0] != want {
		t.Errorf("staleRerender's open is %v, want [%s]: the stale heal and the pool must read the same gates",
			staleOpen, want)
	}
}
