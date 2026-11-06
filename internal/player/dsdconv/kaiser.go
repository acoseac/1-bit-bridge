package dsdconv

import "math"

// The filter design: the one place with floating point in it. Go may fuse a
// product into the add or subtract that uses it, across statements too,
// wherever no explicit conversion rounds it first (arm64 does, amd64 does
// not by default), so every product here is converted to float64 where it
// is formed, which rounds it on its own as IEEE 754 says; the only math
// functions are Sqrt, Floor, Round and Abs, which IEEE 754 and Go define
// exactly, and the sine is this file's own. Every architecture therefore
// designs the same coefficients, and from them on the chain is integer.
// TestTheDesignIsTheSameOnEveryArchitecture holds the file to that.

// twoPi is 2π, rounded once.
var twoPi = float64(2 * math.Pi)

// taylorDen are (2k)(2k+1) for k = 1..13, the sine series' steps.
var taylorDen = [...]float64{6, 20, 42, 72, 110, 156, 210, 272, 342, 420, 506, 600, 702}

// sinTurns is sin(2π·t): t reduced to a quarter turn, then the series,
// whose last term at a quarter turn is under 1e-20.
func sinTurns(t float64) float64 {
	t = t - math.Floor(t)
	neg := t >= 0.5
	if neg {
		t = t - 0.5
	}
	if t > 0.25 {
		t = 0.5 - t
	}
	x := float64(twoPi * t)
	x2 := float64(x * x)
	term, sum := x, x
	for k, den := range taylorDen {
		term = float64(term*x2) / den
		if k%2 == 0 {
			sum = sum - term
		} else {
			sum = sum + term
		}
	}
	if neg {
		return -sum
	}
	return sum
}

// besselI0 is the modified Bessel function of the first kind, order zero,
// by its series, to a relative 1e-21.
func besselI0(x float64) float64 {
	h := x / 2
	q := float64(h * h)
	term, sum := 1.0, 1.0
	for k := 1; k < 500; k++ {
		term = float64(term*q) / float64(k*k)
		sum = sum + term
		if term < sum/1e21 {
			break
		}
	}
	return sum
}

// kaiserBeta is Kaiser's window parameter for a stopband attenuationDB
// down (over 50 dB): 0.1102 (A - 8.7).
func kaiserBeta(attenuationDB int) float64 {
	return float64(0.1102 * (float64(attenuationDB) - 8.7))
}

// kaiserTaps is Kaiser's estimate of the taps a low-pass needs for
// attenuationDB of stopband over a transition of transitionHz at rateHz:
// N - 1 = (A - 7.95) / (2.285 · 2π · Δf / fs), rounded up.
func kaiserTaps(attenuationDB, transitionHz, rateHz int) int {
	dw := float64(twoPi*float64(transitionHz)) / float64(rateHz)
	n := (float64(attenuationDB) - 7.95) / float64(2.285*dw)
	t := int(math.Floor(n))
	if float64(t) < n {
		t++
	}
	return t + 1
}

// lowpass is an odd-length, taps-tap Kaiser-windowed sinc, its cutoff
// cutNum/cutDen of the sample rate and its window for attenuationDB,
// quantized to 2^30 for 1.0 with its centre tap made to bring the sum to
// exactly 2^30: unity gain. Symmetric by construction, since each pair of
// taps is computed from the same |m|.
func lowpass(taps, cutNum, cutDen, attenuationDB int) []int64 {
	c := (taps - 1) / 2
	fc := float64(cutNum) / float64(cutDen)
	beta := kaiserBeta(attenuationDB)
	ib := besselI0(beta)
	h := make([]float64, c+1) // h[k] is taps c-k and c+k
	var sum float64
	for k := 0; k <= c; k++ {
		v := float64(2 * fc)
		if k > 0 {
			m := float64(k)
			v = sinTurns(float64(fc*m)) / float64(math.Pi*m)
		}
		r := float64(k) / float64(c)
		w := besselI0(float64(beta*math.Sqrt(1-float64(r*r)))) / ib
		h[k] = float64(v * w)
		if k == 0 {
			sum = sum + h[k]
		} else {
			sum = sum + float64(2*h[k])
		}
	}
	g := make([]int64, taps)
	var total int64
	for k := 0; k <= c; k++ {
		q := int64(math.Round(float64(float64(h[k]/sum) * float64(1<<scaleBits))))
		g[c-k], g[c+k] = q, q
		if k == 0 {
			total += q
		} else {
			total += q + q
		}
	}
	g[c] += 1<<scaleBits - total
	return g
}
