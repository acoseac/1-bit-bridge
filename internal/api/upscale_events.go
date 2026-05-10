// Package api — wire shape for the SSE `upscale.complete` topic.
//
// Mirror-PR pair invariant (see CLAUDE.md): this struct is the wire
// contract between bridge and iOS clients. JSON tag changes are
// breaking and require a `ProtocolVersion` bump + coordinated PR
// pair. Additive fields (with `omitempty`) are safe.
package api

import "time"

// UpscaleCompleteEvent is published on the SSE `upscale.complete`
// topic exactly once per successful transcode-pool job, AFTER the
// SQLite `UpsertVariant` transaction commits. iOS uses the `path`
// field to route the event to the in-flight upscale job in
// `BridgeUpscaleService` via its (shareID, path) → trackID reverse
// index, then fires a silent delta scan; the manifest reconcile
// promotes the wand chrome to "Ready" without waiting for the
// ladder's 8 s first rung.
//
// **Critical**: `Path` must equal the manifest's `Track.path` field
// byte-for-byte. The bridge serializes it from `JobSpec.SourceLibraryRel`
// (the same field the manifest scanner uses) — do not reformat,
// normalise, or transform it anywhere between the worker and this
// struct. Any drift breaks iOS's constant-time reverse lookup.
//
// Ordering invariant: emitted only AFTER `manifest.Store.UpsertVariant`
// returns nil — the SQLite transaction is committed at that point,
// so the iOS-triggered manifest re-sync that follows is guaranteed
// to see the new `track_variants` row and bumped `tracks.indexed_at`.
type UpscaleCompleteEvent struct {
	Path          string    `json:"path"`
	VariantID     string    `json:"variantId"`
	SampleRate    int       `json:"sampleRate"`
	BitsPerSample int       `json:"bitsPerSample"`
	CompletedAt   time.Time `json:"completedAt"`
}
