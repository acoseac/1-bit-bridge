# Long form: CLAUDE.md's build and release-wiring entries

> Moved verbatim from `CLAUDE.md` on 2026-10-18 so it stops loading on every request. **These entries are still binding.** CLAUDE.md keeps each one as a single line ending `→ History-Build.md`; change an entry here and its line there in the same commit. Each `##` names the section the entries came from.

## Build

- **Fuzz targets are fuzzed NIGHTLY in CI, and are NOT fuzzed by `make test`.**
  `.github/workflows/fuzz.yml` fans every target out as a parallel matrix job at
  `-fuzztime 5m -fuzzminimizetime 1s`; the target list is DISCOVERED from the tree via
  `go test -list 'Fuzz.*' ./...`, so adding a target needs no workflow edit. Fan-out, not
  rotation: one-target-per-night by day-of-year would give each target five minutes a
  MONTH. A crasher fails that matrix leg and uploads `testdata/fuzz/**` as an artifact —
  deliberately not auto-committed, since a corpus commit from CI is noise while a crasher
  deserves a human-reviewed PR. Locally, `make test` still runs seed corpora only. **44** targets across **ten** packages —
  `internal/{manifest,fs,dlna,dlna/discovery,upnp,enrich,dupes,lyrics,upload,atlasharvest}` —
  and `atlasharvest`'s `FuzzMatchRelease` lives in
  `lyrics_test.go` rather than a `fuzz_*_test.go` file, so a census that
  greps only the latter undercounts. (This entry said 37 across nine until
  2026-09-10: the sixth stale claim of the kind, and in the paragraph that
  warns about them; 38 until 2026-09-12, when `internal/lyrics` gained
  `FuzzTextCandidateClassification`; 39 until 2026-09-28, when
  `internal/manifest` gained `FuzzSACDExpandUnderAReadFault`; 40 until
  2026-09-29, when it gained `FuzzExtractOGG`, 41 until later that day,
  when it gained `FuzzID3v2WalkAgreesWithDhowden`, 42 until later still, when
  it gained `FuzzOggFLACReadsBackTheMetadataItCarries`, and 43 until backlog B117 added
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes`.) They cover
  the five untrusted-input surfaces: the audio extractors (whole-file + the pure
  chunk-body parsers + the SACD ISO reader), the LAN-facing UNAUTHENTICATED parsers (SSDP /
  SOAP / DIDL / device description), `fs.Resolver`, the web-upload path validation
  (`internal/upload`, which this list omitted until 2026-09-09), and the Atlas
  release matcher (`internal/atlasharvest`).
  **Count them by file:name pair** to get 44 targets. A function-name-only
  census (`grep -h '^func Fuzz' | sort -u`) reports 43, because
  `FuzzNormalize` exists in both `internal/dupes` and `internal/lyrics`. Without `-fuzz` they run their seed
  corpora as ordinary tests, so the normal suite absorbs them for free. To actually fuzz:
  `go test ./internal/fs/ -run XXX -fuzz FuzzResolveContainment -fuzztime 60s -fuzzminimizetime 1s`
  (one target at a time — Go permits only one `-fuzz` per invocation).
  **`-fuzzminimizetime` is not optional on a target that keeps finding new coverage.** It
  defaults to 60s, minimization burns CPU *without incrementing the reported `execs`*, and the
  run still says `PASS` — so the failure mode is a target that looks like it ran and did not.
  Measured on `FuzzFoldForMatch`: `-fuzztime 60s` alone executes **19,003** inputs and then
  sits at 0/sec for 43 seconds; adding `-fuzzminimizetime 1s` executes **1,302,362** in half
  the wall clock. Twenty-five carry PROPERTY assertions worth keeping green rather than merely
  not-crashing: `FuzzResolveContainment` (a successful `Resolve` must land inside a root —
  asymmetric, so only a real escape fails it), `FuzzFoldForMatch` (the documented
  `foldNameNoArticle == stripLeadingArticle∘foldName` identity `pickBestArtist` depends on),
  `FuzzParseRetryAfter` (the `maxRetryAfter` cap), `FuzzSACDVirtualPathRoundTrip` (a
  rendered virtual path must parse back to the same index and container — the renderer and the
  parser disagreeing is a row-reaping bug, since the deletion pass keys on
  `IsSACDVirtualPath`), `FuzzSACDExpandUnderAReadFault` (an SACD image expanded through
  one failing read answers what it answers fault-free, or an error: never "not an SACD"
  where the fault-free read found an album, the answer that retires its rows; the rule
  is under **Scanner**), the four lyrics targets (`FuzzParseSYLTToLRC`, `FuzzNormalize`,
  `FuzzPickIsShuffleInvariant`, `FuzzTextCandidateClassification`; their properties are
  under **Lyrics**), `FuzzMatchRelease` (a claimed match is an entry the listing holds),
  `FuzzKeyFor` (the dupe key is deterministic and every field reaches `Key.ID`),
  `FuzzValidateRelPath` (an accepted upload path meets every invariant the commit relies
  on), `FuzzAcceptedExt` (an audio extension is always accepted) and
  `FuzzParseDeviceDescription` (every service URL the parser keeps, re-parsed, is http(s)
  with a host, stays on the description's host when discovered, names this machine or a
  link-local address only from a description URL that does too, and never names a cloud
  metadata address; it fuzzes the base URL as well as the XML), the eight whole-file
  extractor targets (`FuzzExtract{AIFF,WAV,DFF,DSF,FLAC,OGG,M4A,MP3}`, through
  `fuzzExtractOnce`: one extraction allocates no more than `extractionAllocLimit`, 64 MiB
  plus 64 bytes per input byte; the rule is under **Scanner**),
  `FuzzID3v2WalkAgreesWithDhowden` (wherever dhowden reads an ID3v2 tag, the ID3v2 guard's
  walk counts what dhowden stores, per frame id; the rule is under **Scanner** too),
  `FuzzOggFLACReadsBackTheMetadataItCarries` (a FLAC stream's metadata laid out as Ogg FLAC
  pages of any size reads back byte for byte as a .flac file's; also under **Scanner**) and
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes` (the page buffer dhowden reads through holds
  the bytes its stream holds, where it holds them, over any tape of reads and seeks; under
  **Scanner**). This said "Four" until 2026-09-28, while eight more were added beside
  them, then "Twelve" and "Thirteen" that same day, as two more joined, "Fourteen" until
  2026-09-29, "Twenty-two" until later that day, "Twenty-three" until later still, and
  "Twenty-four" until backlog B117: **count them
  by the assertions in each `f.Fuzz` body**, not from this list. **A crash found by the extractor
  targets is a REAL defect, not a nicety** — `runScanWorker`'s per-iteration `recover()` means
  a panicking file is skipped, so it silently never reaches the manifest, and a throw (out of
  memory) is not recovered at all. Baseline at
  introduction: ~41M executions total, zero panics, zero escapes.
  **The nightly job runs every fuzz process under a 5 GiB address-space limit**
  (`prlimit --as` through `go test -exec`, so the compiler and linker are not held to it;
  2026-09-29, backlog B99). Without it an input that made a worker allocate gigabytes did
  not fail the run: the four workers re-zeroed the address space on every execution until
  GitHub shut the runner down ("The runner has received a shutdown signal", exit 143) before
  the upload step ran, so the leg read as an infrastructure failure and saved nothing.
  `FuzzExtractFLAC` died that way on 8 of the 28 nights from 2026-09-02, from 09-07 on, not
  (as the backlog entry first read) since #991. Under the limit the allocation fails, the
  runtime exits with status 2, and Go records the input: on dido (a 4.5 GiB limit), 23 s in,
  a 40-byte FLAC whose PICTURE block declared 4,281,344,048 bytes of data. **Not a cgroup cap**: the kernel kills a
  worker with SIGKILL, and Go records a worker a signal killed only when it dies MINIMIZING
  an input (`internal/fuzz/worker.go`); killed while fuzzing, the run fails with "terminated
  by unexpected signal; no crash will be recorded". **Size the limit from the measured
  reservation, not from RAM**: RLIMIT_AS counts reserved address space, and a
  `FuzzExtractFLAC` process reserves most of its size before it allocates (go1.26.6 on
  Linux, in decimal units: 2.2 to 2.6 GB for a worker, up to 4.3 GB for the coordinator,
  which maps each worker's shared memory), so 5 GiB (5.37 GB) leaves a worker at least
  2.8 GB of heap and the coordinator about 1.1 GB; a coordinator that reaches the limit fails its leg with its own out-of-memory,
  which is visible, and raising the limit gives each of the four workers that much more of
  the runner's 16 GB. The
  limit is the backstop; the allocation property is what names a bomb the limit would let
  through. **The extractor targets carry no per-execution time budget, on measurement**
  (2026-09-29, backlog B101): a tag at the ID3v2 guard's bounds runs about 0.25 s on a
  loaded dido (the slowest of 3.3 M FuzzExtractMP3 executions, 253 ms, none past 1 s), so a
  budget under a second would flake, and one above it catches only superlinear work that does
  not allocate, while work that allocates (B101's renaming) already fails the allocation
  property. Go's engine bounds no execution itself; whether a budget belongs to B103's answer
  is B103's question.

- `make fmt vet test build-all` is the pre-push gate, now mirrored by CI (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all). Run `make check` (fmt + vet + race test, skips build-all) in the inner loop; `make build-all` once before pushing. On a RAM-constrained box the `-race` + 6-target cross-compile peak can OOM — the Makefile caps Go's `-p` parallelism via `P` (default 4; `make test P=2` to go lower, `P=$(sysctl -n hw.ncpu)` for a roomy box). See `CONTRIBUTING.md`.

- **On a host whose Go is newer than `go.mod`'s, `make fmt` rewrites files CI calls clean.** CI's gofmt check runs `go.mod`'s toolchain (`go-version-file: go.mod`, 1.26.6 as of 2026-09-25), and gofmt 1.27 indents a composite literal in a multi-value `return` differently: on a 1.27.1 host `make fmt` re-indents `internal/manifest/favorites_test.go` and `internal/atlasharvest/lyrics_test.go`, and 1.26.6's `gofmt -l` reports both rewrites. Never commit such a rewrite. Restore those files, and check your own with the pinned gofmt: `$(GOTOOLCHAIN=go1.26.6 go env GOROOT)/bin/gofmt -l <files>`.

- **A test that removes or replaces a library fixture after a request that can start a
  background scan waits for the server's background scans first, and fails if they do not
  settle** (2026-10-14, backlog B384). On Windows a file a scan holds open cannot be removed:
  `TestACancelledWipeDropsTheSavedDates` failed four Windows runs in one day, on PRs that did
  not touch it, because a failed root flip's compensating scan was still reading the song its
  `readdedKeeps` removes. macOS and Linux remove an open file, so it never failed there. Wait
  with `srvBgScansWait(t, srv)`, which fails after 30 s, and the server fixture's cleanup
  reports a scan still running. Until backlog B404 (2026-10-15) the helper returned silently
  at a 10 s bound, which turns a slow scan back into the same flake.

## Repo clean-up

CI now runs the gate on every PR (`.github/workflows/gofmt.yml` = the fmt check, `gate.yml` = vet + test + build-all on a runner that doesn't OOM under the `-race` peak). Still run `make fmt vet test build-all` (or `make check` in the inner loop) locally first and paste the clean output into the PR body — local-green keeps reviewers off the runner's critical path, and the CI check backstops it. The CI workflows deliberately mirror the local gate command-for-command; keep them in lockstep if either changes.

Releases *are* wired up: `.github/workflows/release.yml` runs goreleaser on tag push (`git tag v0.1.0 && git push --tags`), producing signed+notarized darwin archives and unsigned linux / windows archives as a draft GitHub Release. Windows Authenticode signing is pending — tracked against the next release once SignPath Foundation approval lands. Edit the auto-generated release notes and publish; the `README.md` install recipe works for end users from that point. **Alongside it, `.github/workflows/docker.yml` publishes the multi-arch image `ghcr.io/acoseac/1-bit-bridge` (linux/amd64 + arm64; tags `X.Y.Z` / `X.Y` / `latest`) to GHCR on the same tag push** — the package is public (set once with v0.1.7), so future releases need no manual step. Keep `Dockerfile`'s `ARG GO_VERSION` in step with go.mod's `go` directive or the image build fails (the alpine golang image runs `GOTOOLCHAIN=local`; this broke the first v0.1.7 image build). To publish an image for a frozen tag whose own `Dockerfile` predates a fix, dispatch `docker.yml` with `tag=<ver>` + `ref=main`. Full procedure + per-release hygiene: `docs/release-process.md` → **Container image (GHCR)**.
