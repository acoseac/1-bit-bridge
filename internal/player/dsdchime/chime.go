// Package dsdchime will make the chime the bridge player's DSD check plays
// (B728 B12). This is its names-only shim: the names its tests use, with
// nothing built, so the tests compile and fail before the chime exists.
package dsdchime

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

const level = 0.0

const (
	padFrames     = 22050
	sectionFrames = 30870
	leadFrames    = 441
	rampFrames    = 882
	chimeFrames   = 2*padFrames + 2*sectionFrames
)

var rates [3]struct {
	tone [2][2]float64
	ramp [2]float64
	tail [2]int
}

var (
	errRate   = errors.New("dsdchime: not built")
	errBounds = errors.New("dsdchime: not built")
)

const bound = 0.0

// Chime is one rate's chime.
type Chime struct {
	bitRate int
	planes  [2][]byte
}

// Format is nothing yet.
func (c *Chime) Format() format.NativeDSD { return format.NativeDSD{} }

// ReadAt reads nothing yet.
func (c *Chime) ReadAt(dst [][]byte, off int64) (int, error) { return 0, io.EOF }

type cache struct {
	build   func(context.Context, int) (*Chime, error)
	waiting func()
	mu      sync.Mutex
	slots   [3]struct {
		chime  *Chime
		flight chan struct{}
	}
}

var chimes = &cache{build: build}

// Get builds nothing yet.
func Get(ctx context.Context, bitRateHz int) (*Chime, error) { return chimes.get(ctx, bitRateHz) }

func (c *cache) get(context.Context, int) (*Chime, error) { return nil, errRate }

func build(context.Context, int) (*Chime, error) { return nil, errRate }

func toneOf(i, ch, tail int) tone { return tone{} }

type tone struct {
	amp, c, s, rc, rs, limit float64
	lead, n, ramp            int
}

func (t tone) modulate(ctx context.Context, dst []byte) error { return nil }
