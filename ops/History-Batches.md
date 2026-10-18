# Long form: the dated review records

> Moved verbatim from `CLAUDE.md` on 2026-10-18 so it stops loading on every request. **These entries are still binding.** CLAUDE.md keeps each one as a single line ending `→ History-Batches.md`; change an entry here and its line there in the same commit. Each `##` names the section the entries came from.

## Things that have bitten before

### <a name="review-2026-09-22-fixes"></a>2026-09-22 — review of the #959–#966 fix window

The eight PRs that closed the window below had merged the same day with
no review as a window. A pass over `9f1289f`..`9b1a5a8` found eight
defects plus one adjacent; shipped as #967–#972. The record is in
`ops/engineering-log.md`.

**Every defect was a behaviour the existing tests still accept**, and
THREE were introduced by the fixes they sit in — one release or one
commit from the rule they broke. That is the thing to expect when
reviewing a hardening batch: the new code is where the new defects are,
and its own tests were written by the same reasoning that missed them.

- **A fix placed in FRONT of a check can destroy that check's input.**
  v13's SSND narrowing ran ahead of v12's sentinel refusal and handed it
  `0xFFFFFFF7`. Neither PR was wrong on its own; the ORDER was.
- **A range gate does not protect a value that can wrap into the range.**
  A bot found an `int` overflow landing on exactly 3600 — inside the
  plausibility band the same PR was adding.
- **An escape hatch re-opens the failure its own PR fixes.** The
  empty-store exception on the variant delete took two more rounds to
  narrow: an unlink outside the store explained the store, then an
  unlink before a mid-request unmount did. Both were bot findings on my
  own fix, and the second is the sharper: "we emptied it" and "it
  unmounted" are the same stat.
- **Four of eleven accepted findings were on my own fixes.** Two rounds
  is the documented floor; #968 took five.
- **Three findings were declined by MEASUREMENT, not argument** — escape
  analysis for a claimed allocation, two red tests for a proposed
  `os.Lstat`, and the top-level `ReadDir(1)` for a claimed symlink
  escape. One more was declined with its premise CONCEDED: the config
  really is validated, but the invariant is split across two validators
  chosen by deployment mode, and the failure mode of dropping the guard
  is the vacuous pass the PR existed to fix.
- **"No new findings" is not "reviewed", and I reported it as such
  before checking.** The verdict lives in the walkthrough comment, not
  `pulls/N/reviews`. Rule and the check that works under **### Build, CI,
  and test discipline**.
- **Disjointness decays across review rounds.** Six branches were
  file-disjoint at open and I said so; by round 3 two of them shared a
  test file. Re-derive the overlap matrix before merging.
- **Commit BEFORE the negative control — hit again**, one commit after
  reading the rule, and a tree that did not build was pushed.

### <a name="review-2026-09-22"></a>2026-09-22 — full review of the post-v0.2.0 window

Twenty-four PRs (#934–#958, ~19.5k lines) had merged since the `v0.2.0`
tag with no review as a window and nothing deployed. Three parallel
sweeps plus direct verification of every finding; shipped as #959–#966.
The record, with every measurement, is in `ops/engineering-log.md`.

**The build was healthy and the wire contract intact** — `gofmt`, `vet`,
the whole suite, the race suite on every changed package, `PROTOCOL.md`
byte-identical with the iOS mirror, `ExtractorVersion` correctly bumped,
nightly fuzz green. The adversarial pass over #935/#956's new
MP3/MP4/AIFF/WAV parsers found no overflow, no negative-length slice and
no division by zero. **Every defect was one the suite cannot see**, which
is the shape to expect in a window whose own tests all pass.

- **The enumeration failure, again, and this time on the destructive
  site.** #937 named three reapers; there were five, and the two it
  missed were `DELETE /v1/upscale/variants` and the serve reap's mount
  check. #940's shared walker did not get the root resolution #937 had
  given its read-only twin, so `--gc` could unlink the variants directory
  itself. **Grep for the PATTERN, not the symptom** — and when a rule
  names its sites, the count is the thing most likely to be wrong.
- **A fix's own blast radius is a thing to check.** Resolving the walk
  root would have made every known sidecar look like an orphan, because
  the known set keys on the configured spelling. The safe fix needed a
  second half the finding did not mention.
- **Three tests asserted the defect.** `TestPlausibleDuration_TruthTable`
  required `{"tiny positive", 1e-9, true}`; `TestCheckSidecarPaths`
  required the stale `analyze --force` advice; `TestRejectedCertGrading…`
  listed the clamping line among its ACCEPTED forms. A want-list encodes
  a policy as readily as a fix does.
- **A docblock that matches its code exactly can still be the bug.**
  `plausibleDuration` defended the ceiling and said so precisely. The gap
  was in the policy, so neither the comment nor a test could show it.
- **Bot findings earned their rounds, and one extended a fix of mine.**
  CodeRabbit found that the round-1 SSND fix missed the 8-byte-prefix
  spelling of "empty" — reproduced before it was believed — and that
  `certValidityText` tested a duration where the question is a calendar.
  Two proposals were DECLINED on evidence: defaulting a bare
  customEndpoint to the active listen port would re-open #953/#936, and
  `bridge doctor --rate-limit` does not exist.
- **Merge order from file overlap, not instinct.** Seven of the eight
  branches were mutually disjoint; the docblock PR shared files with four
  of them, so merging it LAST cost one merge-from-main instead of four
  rebases. `main` merged INTO the branch, not a force-push, so bot-review
  history stayed addressable.

### <a name="review-2026-09-18"></a>2026-09-18 — findings review on the post-#899 window

An external full-tree pass (the 2026-09-10 invariants re-checked
mechanically, plus targeted reads of everything after #899) came back with
four bugs and eight quick wins; every one was verified against the code
before acting, and one proposed fix was declined on evidence. Shipped as
PRs #915–#921 plus a field report folded in (#920). The named classes from the
2026-09-10 sweep — `indexed_at`, `enriched_at`, `LIKE` on path predicates,
`WipeFilesystemTracks`, empty-set GC, `loadCLIConfig`, feature gates,
`SetPostScanHook` — are still closed. The record is in
`ops/engineering-log.md`.

- **A same-week fix that does not match the wire the UI actually sends.**
  #913's gate read the parameter, the player always sends it, and the test
  used an empty query. Gate on the PARSED value, and test with the client's
  real request — a fixture must be the value the transformation would
  actually change, and here that value was the default one.
- **A sibling loop that missed the guard.** The lyrics sweep's pass two had
  no scan latch, and the test that pinned pass one could not reach it. The
  same enumeration failure the 2026-09-09 batch names, one function wide.
- **One consumer that never went live when its writers did.** The integrity
  watchers kept a boot-time `variantsDir` while every other consumer had
  been made live, and the Jobs chips then reported them off while they
  ticked. "Never split a field's halves", with the display half following.
- **Verify a proposed fix's MECHANISM before taking it.** `QueryEscape`
  before an html/template URL attribute double-encodes; bounding the
  ticket's shape was the right thing. Same rule as the 2026-09-10 note on
  the lyrics-cooldown patch.
- **Stale claims corrected:** AGENTS.md still said `WipeAllTracks` on a
  root flip — a THIRD copy of the claim CLAUDE.md's top list corrected on
  2026-09-06; the pairing bullet denied a limiter PROTOCOL.md documents;
  the `csrfGuard` docblock said `Origin: null` is allowed while
  `originMatchesAdmin` refuses it; two artwork comments described a test
  that ACCEPTS PNG as one that rejects it. When you correct a claim, grep
  for its twins in every file that restates the rule — AGENTS.md is one.

### <a name="review-2026-09-10"></a>2026-09-10 — full-codebase review

Three parallel sweeps (the unreviewed Atlas lyrics tier; the least-audited
packages; a mechanical invariant sweep across all 46) plus a pass on the exposed
API/admin surface. Shipped as PRs #892–#899. **The worst defect was not in the
new code**: `internal/trash` had never worked on a multi-root bridge, and in the
overlapping-names case it trashed a real file and reaped the manifest row of the
one still on disk. The unswept window supplied the rest.

- **The mechanical sweeps came back CLEAN, and that is worth as much as the
  findings.** `indexed_at` (17 sites), `LIKE` on path predicates (104 + 24),
  cutoff arithmetic (20), ticker validation (52), goroutine lifecycle (55),
  unbounded caches, error-string status parsing (8), `http.Client` probe/mutation
  separation (24), defer ordering (11). Don't re-derive these; the record is in
  `ops/engineering-log.md`.
- **Where the defects actually were: code nobody had reviewed, and the seam
  between two subsystems.** Six of the seven PRs fixed something in the
  2026-09-09 lyrics window or at a boundary — trash↔fs, upload↔trash,
  console↔sweeper, one GC's guard versus its sibling's. A rule held inside each
  package and did not cross.
- **The same enumeration failure, three more times.** A fix that lists the sites
  it covers misses one: the empty-set guard existed in two of five reapers, the
  positional-scope guard in five of seven commands, the `omitempty` time rule in
  three of thirteen structs. Every one was fixed by grepping for the PATTERN and
  writing the sweep test in the same PR — and each sweep found a site the
  enumeration had missed (`artwork --gc`, `internal/upload`).
- **Two tests asserted the defect in their own comments.** `TestAtlasLyricsStats`
  said "a/3 is addressable and still has no row" about an instrumental track that
  the candidate query will never offer again;
  `TestATransientReleaseFetchFailureWritesNoVerdict` asserted `wantErr: true` on
  a sweep-ending bug, with one candidate seeded so it could not tell "skipped"
  from "over". A test can encode the bug and pass forever.
- **Three findings came from bots and were real**, and one suggested patch was
  wrong in both halves — Gemini saw a redundant lock acquisition in the lyrics
  cooldown and the defect underneath it was that the cooldown re-armed itself
  every tick and never expired; the proposed guard compared a WRAPPED error with
  `==` and inverted the condition. Take the observation, verify the mechanism,
  write your own fix.

### <a name="loupe-2026-09-09"></a>2026-09-09 — LOUPE on the 2026-09-04..09 window

The window's own three hardening batches (#849-851 lyrics, #852-858 CLI,
and #859-862 compaction) refuted almost everything, as expected. **Every
confirmed finding was in the unswept half** — the DSD renditions, the login
tickets, the export, managed controls — and the two worst were sites where
a fix that landed IN THIS WINDOW did not reach a sibling.

- **When a fix enumerates the sites it covers, the enumeration is the
  defect.** #852 restored the live feature gate on `POST /v1/upscale` and
  `DELETE /v1/upscale/variants` and its own test file says "**Both**
  mutation handlers" — there are three, and the third,
  `POST /v1/upscale/batch`, walks the WHOLE LIBRARY. Any bearer-token holder
  could start a library-wide sox run on a bridge reporting
  `upscaleEnabled: false`. #856 added the `fs.NArg()` positional guard to
  `enrichment retry` and left the four `--filter` commands unswept, so
  `bridge render "Kind of Blue"` rendered the library. **Grep for the
  pattern, not the symptom, and write the sweep test in the same PR.**
- **The suite proved both holes rather than catching them.**
  `batchFixtureWith` never called `WithUpscale`, so every batch test ran
  against a bridge with the feature OFF and asserted 202. **A fixture that
  omits a live gate describes a different bridge than production** — the
  flags belong in the BASE fixture, and a decorate hook can still override.
- **A behaviour-preserving fix needs a test that fails without it.**
  Reverting the `CanDecodeVia` extension gate left the whole package green.
  Count what must not happen (`LookPath` calls) rather than timing anything,
  and check the equivalence the fix rests on instead of asserting it in a
  comment.
- **`ExtractorVersion` was not bumped for the lyrics batch**, so #849/#850/
  #851's three changes to what extraction PRODUCES were inert on every
  already-scanned track, forever, with no error and no log line. Their own
  tests pass because they call the extractor directly. **A test that calls
  the extractor cannot see the skip gate.**
- **A managed CONTROL must own the settings FIELD that performs it.** #876
  gated the actions at the route table; `PATCH /api/settings` has no control
  gate and `managedFieldsIn` consulted only `managedSettings`, so one
  authenticated PATCH of `updateAutoInstall` reached the binary swap and the
  process restart that `updates` and `restart` both exist to refuse. Derive
  the implied field set FROM the control — telling the control plane to also
  list them makes correctness depend on a second program getting a list
  right, which is what route-table placement was chosen to avoid. **And
  report the widened set to the console**, or every save on a managed bridge
  supplies a refused field and the PATCH fails whole.
- **The one unauthenticated endpoint deserves a second look at every write.**
  `GET /login/ticket`'s miss branch rewrote the ticket file unconditionally
  while its comment said "when pruning removed something" — `prunedTickets`
  gave no way to ask. Measured 3.93 ms/req against 159 µs idle, and eight
  flooding clients took an authenticated `GET /api/stats` from 278 µs to
  33.1 ms, under the mutex `ValidateSession` takes. It was also an oracle for
  "a login link is live now". (`prunedTickets` is gone since 2026-09-28: each
  ticket is a file of its own, a miss writes nothing at all, and pruning is a
  mint's; the ticket bullet under Auth, pairing, TLS.)
- **`mux.HandleFunc("GET …")` also matches HEAD.** A HEAD burned the
  one-time ticket, because redemption deletes before judging. Anything that
  probes a link before the human clicks — a mail scanner, an unfurler, a
  proxy — spends it.
- **`truncated` must be a fact about the DATA, not about the loop.** The
  export's cap check ran BEFORE each fetch with a page size that divided the
  cap, so `len` could never exceed it: the trim was unreachable, its comment
  described an impossible overshoot, and a history of exactly 100,000 rows
  shipped `truncated: true` about an export that omitted nothing. The page
  size now deliberately does NOT divide the cap, and a test pins that — if
  they become commensurate the distinction collapses silently.
- **A test seam is per-SERVER, never a package var.** Shrinking the export
  cap through package globals is a write the race detector can pair with a
  live handler's read, and `buildExport` read the cap twice, so a change
  between them could panic the slice. Read once into locals; put the seam on
  the struct. (A second case, 2026-09-28: the reachability probe's stat
  seam, whose restore raced a parked probe; under **Build, CI, and test
  discipline**.)
- **The scratch pre-flight is sized for every LANE**, not the largest single
  job — the pool runs `EffectiveWorkers()` concurrently on distinct dedup
  keys and holds that many Stage A intermediates at once. The docblock
  asserted the opposite. The auto-optimize sweeper had it right all along.
- **`FFmpegInfo`'s zero value granted the MP4 fallback** against its own "the
  zero value grants nothing" docstring — a nil `MissingBinaries` has
  `len() == 0`. Not live, but a trap for the extension gate, which passes
  exactly that value.
- **`pickPlayableVariant` had no `pcm-` arm**, so a DSD track whose only
  rendition is the faithful tier read as unplayable in the web player — with
  the FLAC sitting right there. `optimized-` covers `optimized-dsd-` by
  prefix, which is why eight other prefix sites were swept and this one was
  not.
- **`BRIDGE_DSD_FIXTURE_TESTS` appeared nowhere outside the test file that
  reads it**, so the alias-rejection differential, the level-parity clip
  pin and the CCIF check had never run since they were written — while this
  file cites their numbers as settling the decimation design. They take 2.8
  seconds. Run on Debian trixie with apt sox 14.4.2 + ffmpeg they reproduce
  every number here to the decimal (stopband −116.84 dB, alias delta +0.00 /
  +0.38 dB, rendition −0.97 dBTP at +5.00 dB applied gain) — a VERIFICATION,
  not a correction. `make measure-dsd` and a `dsd-measure` CI job now run
  them. **A gated test that skips looks exactly like one that passed**, which
  is why the job PRINTS the decoder list.
- **The fuzz-target package list here omitted `internal/upload`** (2 targets),
  and with it the web-upload path validation from the "three untrusted-input
  surfaces" enumeration. Counting targets needs the file:name pair —
  `grep '^func Fuzz' | sort -u` says 36, because `FuzzNormalize` exists in
  both `internal/dupes` and `internal/lyrics`.
- **`stripGoComments` blanks string LITERALS as well as comments.** A sweep
  test anchored on a flag NAME (`fs.String("filter"`) matches nothing and
  passes vacuously; anchor on the identifier. Only the `checked == 0` floor
  caught it — every scan-based guard needs one.
- **Two process failures worth more than the fixes.** I ran a negative
  control BEFORE committing that round, and the control's `git checkout --`
  restore took the uncommitted helper with it; the `git add -A` that followed
  pushed a tree that did not build. *Commit before the control, always.* And
  three DSD tests failed on a machine with 6.8 GB free and a 20 GB Go build
  cache — `df -h /` before reading the failures, as this file already says.
  With the lane multiplier those fixtures wanted 11.4 GB, so they were also
  scaled down: **a unit test must not depend on the host's free disk.**
