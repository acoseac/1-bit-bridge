package dsdconv

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageResponse is the zero-phase amplitude of a symmetric stage g (scale
// 2^30) at j·step for j = 0..n-1, at sample rate fs, rotating one phasor per
// tap: the response read from the coefficients themselves.
func stageResponse(g []int64, fs, step float64, n int) []float64 {
	c := (len(g) - 1) / 2
	out := make([]float64, n)
	for j := 0; j < n; j++ {
		w := 2 * math.Pi * float64(j) * step / fs
		cr, ci := math.Cos(w), math.Sin(w)
		pr, pi := 1.0, 0.0
		a := float64(g[c])
		for k := 1; k <= c; k++ {
			pr, pi = pr*cr-pi*ci, pr*ci+pi*cr
			a += 2 * float64(g[c+k]) * pr
		}
		out[j] = a / (1 << scaleBits)
	}
	return out
}

// foldIndex maps a grid index onto [0, p/2] of a stage periodic in p.
func foldIndex(j, p int) int {
	j %= p
	if j > p/2 {
		j = p - j
	}
	return j
}

// stage1Taps is g1 without its zero slot: the symmetric filter it holds.
func stage1Taps(g *geometry) []int64 { return g.g1[1:] }

// TestTheResponseMeetsItsMask measures the composite from the coefficients,
// for every DSD rate and plan: the three stages are one filter at the
// one-bit rate followed by one decimation by D (the noble identities), so a
// tone at f anywhere up to the one-bit Nyquist leaves at |H1·H2·H3|(f),
// folded. The mask is §4.2's: within ±0.01 dB to 20 kHz and 0.1 dB to the
// passband's 30 kHz, and at least 100 dB down from the stopband's start
// (40 kHz, or 22.05 kHz at 44.1 kHz) to the one-bit Nyquist. Each stage is
// symmetric (linear phase) and sums to exactly 1.0 (unity gain).
func TestTheResponseMeetsItsMask(t *testing.T) {
	const step = 10.0 // divides 176.4 and 352.8 kHz, so every stage folds onto the grid
	for _, rate := range dsdRates {
		var h1 []float64
		for _, p := range allPlans {
			g, err := geometryFor(rate, p)
			if err != nil {
				t.Fatalf("%s %s: %v", rateName(rate), p, err)
			}
			for name, taps := range map[string][]int64{"stage 1": stage1Taps(g), "stage 2": g.g2, "stage 3": g.g3} {
				var sum int64
				for i, v := range taps {
					sum += v
					if v != taps[len(taps)-1-i] {
						t.Fatalf("%s %s: %s is not symmetric at tap %d", rateName(rate), p, name, i)
					}
				}
				if sum != 1<<scaleBits || len(taps)%2 != 1 {
					t.Fatalf("%s %s: %s has %d taps summing to %d, want an odd count summing to 2^30", rateName(rate), p, name, len(taps), sum)
				}
			}
			if g.g1[0] != 0 || len(g.g1) != 8*g.t || g.t%2 != 0 {
				t.Fatalf("%s %s: stage 1's window is %d slots of %d tables, slot 0 %d", rateName(rate), p, len(g.g1), g.t, g.g1[0])
			}
			if h1 == nil {
				h1 = stageResponse(stage1Taps(g), float64(rate), step, int(float64(rate)/2/step)+1)
			}
			h2 := stageResponse(g.g2, 352800, step, int(176400/step)+1)
			h3 := stageResponse(g.g3, 176400, step, int(88200/step)+1)
			resp := func(j int) float64 {
				return h1[j] * h2[foldIndex(j, int(352800/step))] * h3[foldIndex(j, int(176400/step))]
			}
			var dev20, devPass float64
			for j := 0; float64(j)*step <= float64(p.PassbandHz()); j++ {
				v := math.Abs(20 * math.Log10(math.Abs(resp(j))))
				if float64(j)*step <= 20000 {
					dev20 = math.Max(dev20, v)
				}
				devPass = math.Max(devPass, v)
			}
			worst, worstHz := -400.0, 0.0
			for j := int(float64(p.StopbandHz()) / step); j < len(h1); j++ {
				if v := 20 * math.Log10(math.Abs(resp(j))+1e-300); v > worst {
					worst, worstHz = v, float64(j)*step
				}
			}
			t.Logf("%s → %s: stage 1 %d taps, stage 2 %d, stage 3 %d; within %.6f dB to 20 kHz and %.6f dB to %d Hz; stopband at worst %.2f dB at %.0f Hz; H %d frames",
				rateName(rate), p, len(stage1Taps(g)), len(g.g2), len(g.g3), dev20, devPass, p.PassbandHz(), worst, worstHz, g.h)
			if dev20 > 0.01 {
				t.Errorf("%s %s: the passband is %.6f dB off to 20 kHz, over 0.01 dB", rateName(rate), p, dev20)
			}
			if devPass > 0.1 {
				t.Errorf("%s %s: the passband is %.6f dB off to %d Hz, over 0.1 dB", rateName(rate), p, devPass, p.PassbandHz())
			}
			if worst > -100 {
				t.Errorf("%s %s: the stopband reaches %.2f dB at %.0f Hz, above -100 dB", rateName(rate), p, worst, worstHz)
			}
		}
	}
}

// TestNoSumCanOverflow bounds every value the integer chain forms, from the
// coefficients: a stage-1 table entry fits an int32, and stage 3's sum, the
// largest, stays under 2^63 for any bits at all (each stage's output is at
// most its taps' absolute sum times its input's bound) with room for the
// dither (under 2^45) and the rounding's addend (under 2^45). The bound is
// about 4.6 x 2^60 under the 44.1 kHz plans, whose stage 3 is longest.
func TestNoSumCanOverflow(t *testing.T) {
	abs := func(taps []int64) *big.Int {
		s := new(big.Int)
		for _, v := range taps {
			s.Add(s, big.NewInt(int64(math.Abs(float64(v)))))
		}
		return s
	}
	limit := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 63), new(big.Int).Lsh(big.NewInt(1), 46))
	for _, rate := range dsdRates {
		for _, p := range allPlans {
			g, err := geometryFor(rate, p)
			if err != nil {
				t.Fatalf("%s %s: %v", rateName(rate), p, err)
			}
			for j := 0; j < g.t; j++ {
				if s := abs(g.g1[8*j : 8*j+8]); s.Cmp(big.NewInt(math.MaxInt32)) > 0 {
					t.Errorf("%s %s: table %d's taps sum to %v, past an int32", rateName(rate), p, j, s)
				}
			}
			s1 := abs(g.g1) // 2^30 scale
			s2 := new(big.Int).Mul(abs(g.g2), s1)
			s2.Rsh(s2, scaleBits)
			s2.Add(s2, big.NewInt(1)) // the rounding
			s3 := new(big.Int).Mul(abs(g.g3), s2)
			if s3.Cmp(limit) >= 0 {
				t.Errorf("%s %s: stage 3's sum can reach %v, past 2^63 less the dither's and the rounding's room", rateName(rate), p, s3)
			}
		}
	}
}

// TestOutputFrameKIsCentredOnBitKD: with the group delay compensated, output
// frame k is centred on input bit k·D. One bit flipped in DSD silence at bit
// k0·D changes the output by the composite's impulse response, which is
// symmetric about frame k0 and largest there.
func TestOutputFrameKIsCentredOnBitKD(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {88200, 24}, {44100, 24}} {
			d, err := p.Decimation(rate)
			if err != nil {
				t.Fatal(err)
			}
			h, _ := p.Window(rate)
			k0 := 4 * h
			n := (8*h + 1) * d
			base := repeatByte(0x69, n)
			flipped := append([]byte(nil), base...)
			flipped[k0*d] ^= 1
			a, _ := convert(t, [2][]byte{base, base}, rate, p, 4096)
			b, _ := convert(t, [2][]byte{flipped, base}, rate, p, 4096)
			diff := make([]int64, len(a)/2)
			peak := 0
			for k := range diff {
				diff[k] = int64(b[2*k]) - int64(a[2*k])
				if abs64(diff[k]) > abs64(diff[peak]) {
					peak = k
				}
				if b[2*k+1] != a[2*k+1] {
					t.Fatalf("%s %s: the other channel changed at frame %d", rateName(rate), p, k)
				}
			}
			if peak != k0 {
				t.Errorf("%s %s: flipping bit %d moved frame %d most, want frame %d", rateName(rate), p, k0*d, peak, k0)
			}
			for j := 1; j <= h; j++ {
				if abs64(diff[k0-j]-diff[k0+j]) > 2 {
					t.Errorf("%s %s: the response is %d before frame %d and %d after, not symmetric", rateName(rate), p, diff[k0-j], k0, diff[k0+j])
					break
				}
			}
		}
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestTheDesignIsTheSameOnEveryArchitecture holds the package to integer
// kernels and a design no architecture computes differently, from its
// source, so the claim does not rest on two runs on one machine (the
// orchestrator compares TestTheOutputIsDeterministic's digests from amd64
// and arm64 too). Go may fuse a product into the add or subtract that uses
// it, across statements too, wherever no explicit conversion rounds it
// (arm64 does; amd64 does not by default), so in kaiser.go, the one file
// with floating point in it, every product is converted to float64 where
// it is formed; its only math functions are the ones IEEE 754 or Go defines
// exactly; and no other file holds a floating-point type but in a result.
func TestTheDesignIsTheSameOnEveryArchitecture(t *testing.T) {
	if _, err := os.Stat("kaiser.go"); errors.Is(err, fs.ErrNotExist) {
		// A test binary built here and run on another machine (the Pi the
		// orchestrator compares digests on) has no source beside it.
		t.Skip("the package's source is not in the working directory")
	}
	exact := map[string]bool{"Sqrt": true, "Floor": true, "Round": true, "Abs": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		results := map[ast.Node]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if ft, ok := n.(*ast.FuncType); ok && ft.Results != nil {
				for _, r := range ft.Results.List {
					results[r.Type] = true
				}
			}
			return true
		})
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			parent := ast.Node(nil)
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.Ident:
				if (x.Name == "float64" || x.Name == "float32") && name != "kaiser.go" && !results[x] {
					t.Errorf("%s: %s outside kaiser.go", fset.Position(x.Pos()), x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.FLOAT && name != "kaiser.go" {
					t.Errorf("%s: a floating-point literal outside kaiser.go", fset.Position(x.Pos()))
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "math" {
					if call, ok := parent.(*ast.CallExpr); ok && call.Fun == x && !exact[x.Sel.Name] {
						t.Errorf("%s: math.%s, which an architecture may compute differently", fset.Position(x.Pos()), x.Sel.Name)
					}
				}
			case *ast.BinaryExpr:
				if name != "kaiser.go" || x.Op != token.MUL {
					break
				}
				seen = true
				call, ok := parent.(*ast.CallExpr)
				if fn, isIdent := callee(call); !ok || !isIdent || fn != "float64" {
					t.Errorf("%s: a product not converted to float64 where it is formed", fset.Position(x.Pos()))
				}
			case *ast.AssignStmt:
				if name == "kaiser.go" && (x.Tok == token.MUL_ASSIGN) {
					t.Errorf("%s: *= rounds nothing on its own", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
	if !seen {
		t.Error("kaiser.go holds no product: the check read nothing")
	}
}

func callee(c *ast.CallExpr) (string, bool) {
	if c == nil {
		return "", false
	}
	id, ok := c.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}
