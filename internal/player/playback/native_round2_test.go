package playback

import (
	"context"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"testing"
)

func TestNativeDroppedChunkContinuesProducer(t *testing.T) {
	b, _ := New(Config{Resolver: nativeFilesForRefusal{}, Locator: refuseLocator{}})
	s := newSession(b, 1)
	defer s.cancel()
	o := &occ{dropped: true, nativeFirst: [][]byte{{0}, {0}}}
	o.nativeFormat.Frames = 8
	if !s.writeNativeChunk(o) {
		t.Fatal("successful native cut terminated producer")
	}
	s.releasing = true
	if s.writeNativeChunk(&occ{nativeFirst: [][]byte{{0}, {0}}, nativeFormat: o.nativeFormat}) {
		t.Fatal("release continued producer")
	}
}

// Keep this fixture independent of any device: only the producer's cut branch runs.
type nativeFilesForRefusal struct{}

func (nativeFilesForRefusal) Open(context.Context, engine.Reference) (Content, error) {
	panic("unexpected content open")
}

type refuseLocator struct{}

func (refuseLocator) Locate(context.Context) (Unit, error) { panic("unexpected hardware resolution") }
