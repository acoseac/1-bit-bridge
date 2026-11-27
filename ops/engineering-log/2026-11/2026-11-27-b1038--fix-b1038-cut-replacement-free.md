## 2026-11-27 — B1038: the cut-replacement test frees its fake DAC before the resume

Branch `fix/b1038-cut-replacement-free`, from main `e1f86c4a`. Test-only: one statement of
`internal/player/playback/native_integration_test.go` moves, with a comment; no production file
changes. The file builds only under the native overlay (`scripts/native-integration-overlay.py`), as
CI's native qualification job runs it: a bare `go test ./internal/player/playback/` runs none of its
tests.

### The deadlock

`TestNativeIntegrationSuccessfulCutReplacement` held the fake DAC until after `b.Resume(ctx, 1)`. Its
queue ends after occurrence 3, so once the resumed writer has written what the ring holds it begins
the end of queue's release (`beginRelease(TrackChange)`, `internal/alsapcm/sink_dsd_writer.go`). Where
the resume's caller reads its reply only after that, its `settle` (`sink_dsd.go`) finds the release
begun and waits for its outcome (`releaseOutcome`): the writer's end or the release's budget. On a
held DAC the release never ends, and the budget's timer runs on the fake's clock
(`FakeKernel.OpenNativeSink` passes `nativeAt`), which a held DAC never moves, so the test's own 5 s
context ended the call: "player: the resume was refused, and the session stays paused: device_error:
the device could not be opened again: context deadline exceeded". CI's native qualification job
failed it so twice (backlog B1038). The words are `resumeError`'s default code, which it gives any
error it does not classify: no reopen was made, the sink having never been released in the pause.
Production arms the budget on a real 2 s timer (`openDSDSink`'s default `at`, `nativeReleaseBudget`)
and its DAC plays, so the deadlock is the harness's, not the player's.

### The fix

`k.Free()` moves to just above the resume. Freed there, the DAC plays nothing before the resume: the
sink is paused, so its writer's writes are cancelled (`nativeContext`), and its idle wait polls for
no event, which the fake ends only on a grant, freed or not (`modelPoll`). The cut and both Prepares
still run with the DAC held, which is what the test exercises.

### Runs

macOS arm64, Go 1.27.3, every run with `-race`, the release tags and the overlay. The overlay copies
the test file when it is made, so it was made again after the edit and each run's copy compared with
the file.

- Forced: a 20 ms `time.Sleep` before `await`'s select in `internal/alsapcm/sink_dsd.go`, never
  committed. Main's test failed 10 of 10, each at the resume with the words above after 5.00 s; the
  fixed test passed 10 of 10. A trace, also never committed, showed the order: with main's test the
  resume's settle found the release begun and waited on it; with the fix the release had begun and
  ended on the freed DAC by then, and the resume answered `continued`.
- Unforced, at `-cpu 1`: main's test failed 100 of 200 the same way; the fixed test passed 500 of
  500.
- `-count=500` at the default GOMAXPROCS (12 here): 500 of 500.
- The overlay's backend suite, `-run '^TestNativeIntegration'` as CI runs it: 33 of 33. `go test
  -race ./internal/player/playback/`, `go vet` with the overlay, the pinned `gofmt -l` and
  `make docs-check`: clean.
