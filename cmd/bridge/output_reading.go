package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// outputRowReader is the one store read the reading's row lookup makes: a
// row's format and the size and modification time the scanner's skip gate
// compares, from one statement (manifest.Store.GetTrackWithStat), so one
// version of the row. It has no other method, so the lookup cannot read the
// row in two reads a scan could commit between.
type outputRowReader interface {
	GetTrackWithStat(ctx context.Context, path string) (*manifest.Track, int64, int64, error)
}

// outputTrackLookup is the reading's view of one of this bridge's rows
// (GET /v1/outputs/{id}/reading, B473 B3): its format from the track, and
// the size and modification time from the columns GetTrackStat reads,
// which say whether the file on disk is still the one the row describes,
// all from one call of rows.
func outputTrackLookup(rows outputRowReader) func(context.Context, string) (api.OutputTrack, bool, error) {
	return func(ctx context.Context, path string) (api.OutputTrack, bool, error) {
		t, size, mtimeNS, err := rows.GetTrackWithStat(ctx, path)
		if err != nil || t == nil {
			return api.OutputTrack{}, false, err
		}
		return api.OutputTrack{
			Codec:         t.Codec,
			SampleRate:    t.SampleRate,
			BitsPerSample: t.BitsPerSample,
			Channels:      t.Channels,
			IsDSD:         t.IsDSD,
			Size:          size,
			MTimeNS:       mtimeNS,
		}, true, nil
	}
}

// loopbackTrackLookup is the row a loopback track route id names and the
// file the route serves for it, read from the index the route itself
// reads, so the reading names the song MPD fetches from that route as the
// route resolves it. A routed track (no file of this bridge's) is none.
func loopbackTrackLookup(lib dlna.TrackLookup) func(string) (string, string, bool) {
	return func(trackID string) (string, string, bool) {
		info, ok := lib.GetTrackInfo(trackID)
		if !ok || info.AbsolutePath == "" || info.RelativePath == "" {
			return "", "", false
		}
		return info.RelativePath, info.AbsolutePath, true
	}
}
