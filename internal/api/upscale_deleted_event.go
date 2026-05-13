// Package api — wire shape for the SSE `upscale.deleted` topic.
//
// Mirror-PR pair invariant (see CLAUDE.md): this struct is the wire
// contract between bridge and iOS clients. JSON tag changes are
// breaking and require a `ProtocolVersion` bump + coordinated PR
// pair. Additive fields (with `omitempty`) are safe.
package api

import "time"

// UpscaleDeletedEvent is published on the SSE `upscale.deleted` topic
// every time one or more variant rows disappear from `track_variants`,
// from any of three triggers:
//
//  1. Operator-driven DELETE /v1/upscale/variants (admin UI / iOS-future).
//  2. Reactive serve-side cleanup: a `/v1/download?variant=...` GET
//     observes the sidecar missing on disk, drops the DB row, and
//     publishes the event so iOS clients reconcile immediately.
//  3. Periodic integrity sweep (1 h default ticker): the
//     `internal/integrity.Watcher` walks `track_variants`, stats each
//     sidecar, batches the misses into a single event per sweep.
//
// iOS uses `Paths` to route the event to the relevant `Track` rows via
// the (shareID, Track.path) → trackID reverse index in
// `BridgeUpscaleService.pathIndex`. For each match it filters the
// `Track.bridgeVariants` JSON blob to remove the deleted `VariantIDs`
// and saves; in the playback-fallback case (currently rendering the
// upscaled variant of one of the listed tracks) it captures elapsed,
// reloads at the source path, and resumes — same machinery the
// user-driven variant-switch uses.
//
// **Critical**: `Paths[i]` must equal the manifest's `Track.path`
// field byte-for-byte — same contract as UpscaleCompleteEvent. The
// bridge serialises from the variant rows' `source_path` column
// (which the manifest scanner also wrote); do not reformat,
// normalise, or transform between the SQL projection and this
// struct. Any drift breaks iOS's constant-time reverse lookup.
//
// `Paths` and `VariantIDs` are positional: a single event may carry
// rows belonging to multiple source paths AND multiple variant IDs
// per source path; the two slices are NOT zipped 1:1 — iOS treats
// `VariantIDs` as the set of IDs that disappeared somewhere in the
// `Paths` set. Pre-feature bridges never emit this event.
//
// `DeletedAt` is the wall-clock timestamp of the publish, mostly
// for admin-console / debug surfaces; iOS doesn't gate behaviour on
// it.
type UpscaleDeletedEvent struct {
	Paths      []string  `json:"paths"`
	VariantIDs []string  `json:"variantIds"`
	DeletedAt  time.Time `json:"deletedAt"`
}

// publishUpscaleDeleted is the single emission helper shared between
// the operator-driven delete handler, the serve-side reactive
// cleanup, and the integrity watcher's batch path. Callers MUST
// invoke with a non-empty `paths` slice (the no-op case is filtered
// upstream) and a coherent `variantIDs` set — `paths[i]` and
// `variantIDs[j]` are NOT zipped, just the union of what disappeared.
//
// `publisher` is the api.EventPublisher (the broker or a nop). The
// nop drops silently — test harnesses and pre-broker bridges get
// the same no-op behaviour every other publish helper relies on.
func publishUpscaleDeleted(publisher EventPublisher, paths, variantIDs []string) {
	if publisher == nil || len(paths) == 0 {
		return
	}
	publisher.Publish("upscale.deleted", UpscaleDeletedEvent{
		Paths:      paths,
		VariantIDs: variantIDs,
		DeletedAt:  time.Now(),
	})
}
