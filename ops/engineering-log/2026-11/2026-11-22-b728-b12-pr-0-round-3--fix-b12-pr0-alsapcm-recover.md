## 2026-11-22 — B728 B12 PR 0 round 3: DROP's error survives HW_FREE's panic, and each panic's error is counted

Branch `fix/b12-pr0-alsapcm-recover` (PR #1547), round 2's head `23057cb7`. Rounds 1 and 2 are
`2026-11-22-b728-b12-pr-0--fix-b12-pr0-alsapcm-recover.md` and
`2026-11-22-b728-b12-pr-0-round-2--fix-b12-pr0-alsapcm-recover.md`; this entry records round 3
alone, three items and the binding review folded into its brief (Q1, Q2), which wins where they
differ. Its bound was `releaseNative` and its cases; nothing past them changed.

### What was built

6 production lines added (3 code, 3 comments) and 3 removed, all in `releaseNative`
(`internal/alsapcm/device_native.go`), by `git diff -U0 23057cb7` over the non-test Go files; the
qualification version stays 57.

- **DROP's error survives HW_FREE's panic** (item 1). `releaseNative` answered
  `errors.Join(d.ioctlNone(ioctlDrop), d.ioctlNone(ioctlHWFree))` in one `return`: when DROP
  answered an error and HW_FREE then panicked, that join never ran, so the result never held DROP's
  error, and the recover and the deferred close saw HW_FREE's panic and the close's outcome alone.
  Each ioctl's result is now kept in the named result before the next is made (`err = DROP`, then
  `err = errors.Join(err, HW_FREE)`), so the three are there together. A case of its own, "a release
  whose DROP fails, its HW_FREE panicking": DROP answers EIO (`errDropFailed`), HW_FREE panics, and
  `CleanupResult` carries DROP's errno and one error of HW_FREE's panic, unconfirmed, the node closed
  once (`checkClosedEach`), one line logged.
- **The double-panic case proves both failures** (item 2, Q1). Two recovered panics answer `*Error`
  values with the same details and the same `ErrPanicked`, so `errors.Is` cannot tell one from two,
  and the case of DROP's panic with its close's asserted two log lines while its cleanup needed one
  `ErrPanicked`. `panickedErrors` walks the cleanup's tree through both unwrap forms
  (`Unwrap() error` and `Unwrap() []error`) and returns each distinct `*Error`, by identity, that is
  itself typed `ReasonPanicked`; every case of a release whose ioctl panics now requires one for each
  of its panics, two for that case.
- **Item 3, as round 2's exit left it**: no second hook on the worker. The History says the
  cancellation arm's control, C8, is not deterministic (red 18 times in 60 alone under `-race`), a
  known limit of that control, not of the fix.

### Red first

The round's tests over `23057cb7`'s `device_native.go`, laid by `go test -overlay` with no shim (the
round adds no production name), the prediction written first: the new case red by assertion, "the
cleanup is native release unconfirmed … want DROP's failure beside HW_FREE's panic", never by death;
the 32 other cases green, the double-panic case with its new count among them, since `23057cb7`
kept both panics' errors: item 2 is the case's strength, which its controls show. The same overlay
run on the new case alone 20 times under `-race`: red 20 times of 20.

### Controls

Each predicted in writing first, case by case (green, died, red by assertion), an exact-once
mutation of `c9442ae2`'s `device_native.go` laid by `go test -overlay`, both log calls kept, the four
tests run once (`-watchdog 10s`), no tracked file touched; both went as predicted and nowhere else,
and each tripped the double-panic case's count ("holds 1 errors of a recovered panic, want 2").

- Qa, the ioctl panic's error omitted (its recover keeps `recovered[0](r)`, the log call, and drops
  its answer): red by assertion, the five cases of a release whose ioctl panics; every other case
  green. Alone under `-race`, the double-panic case red 20 times of 20, each at the count, and the
  new case 20 of 20.
- Qb, the close panic's error omitted the same way: red by assertion, the fifteen cases whose close
  panics (the release's, after the writer panicked, at a pause's release, the four reopen refusals,
  a reopen's setup with its close panicking, a refused reopen's, DROP's with its close's, the check
  after open and its close, the kernel's setup and its close, a refusal's close, and the backend's
  release and refused reopen); every other case green. Alone under `-race`, the double-panic case red
  20 times of 20, each at the count.
- At `c9442ae2`, alone under `-race`, the new case and the double-panic case green 20 times of 20
  each.

### Runs

- One slot session at `df92e2b3` (go1.27.3, the release tags), 4,452 s, the tree clean after it:
  - `make fmt` rewrote nothing; `make vet` clean, and `go vet ./...` clean with `GOOS=darwin` and
    with `GOOS=windows`.
  - `go test -race -count=20 -timeout 150m ./internal/alsapcm ./internal/player/playback`: ok,
    alsapcm 2,278.9 s and playback 46.7 s; beside it `go test -race -count=1 ./cmd/bridge`, which
    stays out of the twenty (one race run of it takes near half an hour): ok, 1,676.4 s.
  - The native-integration overlays, as `gate.yml`'s two steps: `./cmd/bridge` 16 passes and
    `./internal/player/playback` 33, none skipped, every name the workflow checks passing.
  - `make test` (P=4, `TEST_PKGS='-count=1 -timeout 45m ./...'`): ok in 2,087.9 s, 92 packages and
    none failed (cmd/bridge 1,734.1 s, internal/manifest 1,598.1 s, internal/admin 1,240.8 s,
    internal/backup 658.2 s, internal/alsapcm 118.5 s); `make build-all` built the release targets
    (24.6 s); `make docs-check` ok.
- Outside the slot: the qualification tool, `go run -tags ts_omit_webclient,ts_omit_debugeventbus
  ./scripts/qualification-version -base a14a26db -head HEAD`, "qualification version OK:
  a14a26db..HEAD" (version 57, raised no further); `verify_moves.py --base c9442ae2 --edit` naming
  the B12 PR 0 long form (its rule line, unchanged, opens alike and does not count): PASS, 0
  citations broken; the citation guard, `TestEveryCitedTestNameExists`, and `make docs-check` again
  with this entry staged. The pinned gofmt (`go run cmd/gofmt -l`, go1.27.3) lists neither of the
  round's Go files.

### What stays open

The residuals are backlog B987.
