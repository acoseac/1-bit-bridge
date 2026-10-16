package manifest

// A rendition COVERS its track only while it is FRESH (backlog B100): a
// batch walks past a track only when one of its renditions of the kind still
// describes the file, and the coverage bars count only such a track. Before
// B100 both counted a track holding ANY rendition of the kind, so after a
// retag and a scan a batch rendered nothing for the stale renditions, and
// the bars read 100% for renditions the serve path answers 410 for. The CLI
// (`upscaleResumeDecision`), the on-demand path (`finalizeAndEnqueue`) and
// the auto-optimize sweep already treated a stale rendition as work.
//
// What stays as it was: the KIND test is the version-agnostic id prefix
// (`upscaled-%`, `optimized-%`, `pcm-%`; see VariantKindPrefix*), so any
// rendition of the family counts, an older schema's included (only the
// sweep asks for the current DSD schema); and the SIZES count every
// rendition, fresh or stale, because the disk a stale one occupies is real.

// renditionFreshSQL is the ONE test of whether a rendition is fresh: the
// version of its source that a `track_variants` row `tv` recorded when it
// was rendered (`source_mtime_ns`, `source_size`) is the version the track
// row `t` records now. Against the TRACK ROW, never a stat of the file
// (#1077's one clock): every rendition writer stamps the row, so a render
// is fresh here the moment it lands and stays fresh until a scan reads a
// change to the file. Both columns, because a retag can keep either one.
//
// Every coverage read composes it, through the EXISTS below or, where a
// read aggregates `track_variants` rows itself (variantPresenceSelect),
// directly. Never write the comparison out at a site.
const renditionFreshSQL = `(tv.source_mtime_ns = t.mtime_ns AND tv.source_size = t.size)`

// freshRenditionOfKind / freshRenditionOfKindEnd are the ONE "this track
// holds a fresh rendition of the kind" test, in two halves around the
// kind's id pattern: the projection the batch walks (HasVariant), the
// browse rows' per-track marks, the folder and subtree numerators, and the
// covered arm of every covered-or-eligible denominator all compose it
// through the four constants below. References a `tracks` row aliased `t`.
const (
	freshRenditionOfKind = `EXISTS(SELECT 1 FROM track_variants tv
	        WHERE tv.source_path = t.path
	          AND tv.variant_id LIKE `
	freshRenditionOfKindEnd = `
	          AND ` + renditionFreshSQL + `)`
)

const (
	freshUpscaledSQL  = freshRenditionOfKind + `'upscaled-%'` + freshRenditionOfKindEnd
	freshOptimizedSQL = freshRenditionOfKind + `'optimized-%'` + freshRenditionOfKindEnd
	freshPCMSQL       = freshRenditionOfKind + `'pcm-%'` + freshRenditionOfKindEnd
	// freshOfBoundKindSQL takes the kind's id pattern as a bind, the
	// projection's `<prefix>-%` (trackProjectionSelect).
	freshOfBoundKindSQL = freshRenditionOfKind + `?` + freshRenditionOfKindEnd
)
