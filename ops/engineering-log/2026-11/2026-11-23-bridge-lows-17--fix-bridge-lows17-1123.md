## 2026-11-23 — Bridge lows 17: B997's review Lows (tests and records)

Branch `fix/bridge-lows17-1123`, from main at `a1c35128` (B997's merge, #1557). Four Lows of B997's
review, each checked by line at `a1c35128`, the brief's five binding notes (X1 to X5) deciding
where they differ from the items. No production behaviour changes: the one production file touched,
`internal/api/api.go`, renames a test seam and rewords its comments. Five commits, this record the
last:
- `e76c307c`, item 1 and X2: the pairing silence checks see every write;
- `f0bda46d`, item 2: each address family is a subtest of its own;
- `828225b6`, item 3: the API's status hook says the lock was found held;
- `36d0cb76`, item 4: B997's record says what `45d70d5a` held;
- this record.

Every Go command here ran with `GOTOOLCHAIN=go1.27.3`, `GOPROXY=off`, the release tags
(`ts_omit_webclient,ts_omit_debugeventbus`) and `TMPDIR` on disk below the checkout's `.tmp/`. Each
control was predicted in writing before it ran and is an overlay of its package (`go test -overlay`,
`-vet=off`): nothing tracked was written, and `git status` was read after each.

### 1. The silence checks see a status written alone

`requireNothingWritten` (`internal/admin/pairing_cancel_test.go`) failed only on a body, and so did
the discovery case's check (`pairing_discovery_test.go`, X2), so a handler that called `WriteHeader`
and wrote no body passed both: `httptest.NewRecorder` reports 200 before anything is written, and a
`WriteHeader(200)` with no body moves neither `Body`, `Flushed` nor the header map (X1). Both checks
read `WriteCallRecorder`'s `Writes` now: an `httptest.ResponseRecorder` embedded, its `WriteHeader`,
`Write`, `WriteString` and `Flush` each overridden to note the call, in order, before passing it on,
so no promoted method writes unnoted (`Header` and `Result` stay promoted, being no write). It is
declared in the package's internal tests and exported, so the discovery case, in `package
admin_test`, takes the same recorder.

The middleware (X5): the lock case and the discovery case send their requests through `Handler()`
(the case of the paths that read no list calls the handlers directly), whose chain on the loopback
socket, `boundaryMiddleware`, `loopbackOnly`, `loopbackHostOnly`, `csrfGuard` and
`sessionMiddleware` (a pass in loopback mode), then the `ServeMux`, hands the writer on as it got
it: none asserts its type or wraps it, and none sets a header there (`setHSTS` runs in public mode
over TLS alone). The token routes wrap nothing either: the package's only assertions on a writer are
the event stream's `http.Flusher` and the upload transport's `ResponseController`. So the checks
count the calls, and the header map plays no part.

The controls ran three tests each time: L,
`TestAPairingRequestEndedWhileItWaitsForTheLockWritesNothing` (2 subtests); N,
`TestAnEndedPairingRequestOnAPathThatReadsNoListWritesNothing` (6); D,
`TestAPairingRequestThatEndsWhileItsListWaitsChangesNoCredential` (2). Each came out as predicted:
- C1, both handlers writing `w.WriteHeader(http.StatusOK)` alone where their context has ended
  under the lock: L and N red, eight lines "the <route> wrote [WriteHeader(200)] to a request that
  had ended, want nothing written", D green. With both pairing test files as at `a1c35128`: all
  green, the gap the item names.
- C2, the same status written where the endpoint list's wait ends with the request, X2's path: D
  red, its two lines, L and N green. With the files as at `a1c35128`: all green.
- C3, `Flush` alone under the lock: L and N red, "wrote [Flush()]".
- C4, an empty `WriteString` through the writer's own method under the lock: L and N red,
  `wrote [WriteString("")]`.
- C5, C4 with the recorder's `WriteString` override removed: all green, the empty string reaching
  the embedded recorder unnoted, which is why each promoted method is overridden.
- No mutation: all green.

### 2. Each address family a subtest

`TestTheExpectedSetTakesBothAddressTypesInBothFamilies`
(`internal/consolelan/hostmodel_reading_test.go`) runs each family in a subtest of its own, `ipv4`
and `ipv6`, every assertion and message as it was.
The control gave the unnumbered interface one address of each family, from the documentation
ranges, so that each family's first `Fatalf` fires: with the subtests both families failed, each in
its own words; with the loop as at `a1c35128`, the IPv4 line alone, IPv6 never checked. The package
vets clean for windows/amd64 and windows/arm64 and its test binary builds for both (`go test -c`);
the live Windows test beside the file (`hold_windows_test.go`) runs on CI's windows-latest leg
alone.

### 3. The API's status hook: found the lock held

`lockTailscaleStatus` (`internal/api/api.go`) calls its test hook after a failed `TryLock` and
before `Lock`, so the hook shows that a read found the status cache's lock held, never that it has
begun to wait for it: the holder can let it go in between, and it may be another read or a setter
(`WithTailscaleStatus` and `SetTailscaleStatus` take the same lock). The hook keeps that meaning
(X4), and its words say it: it is `OnTailscaleStatusLockFoundHeldForTest` now
(`OnTailscaleStatusWaitForTest` in B997's record), its field `tailscaleStatusLockFoundHeld`, and its
comments and `lockTailscaleStatus`'s say that the lock was found held. The queued-waiter case,
`TestACancelledEndpointsWaitLeavesTheHealthReadWhole`, claims no more: neither request ends before
the second's read has found the lock held, which the first's read holds in the held status until
the case lets it go, and its failure says how many reads "found the API's status lock held". The
item's other option, a hook that fires only for a caller that will block, was not taken: nothing a
`sync.Mutex` answers says that a `Lock` will block. With no hook, as in production, the lock is
taken as before.

### 4. B997's record of what `45d70d5a` held

B997's record said its round-1 runs were made on `45d70d5a`, "which holds every change of code";
`3bebf762`, after it, changed item 4's test data, whose runs are that record's next paragraph. The
sentence now says so (its line 143). Its round-2 sentence (line 239) holds as written: `310d18ac`
holds that round's code, and `274d0e9b` after it changes the record alone.

### Runs

At `36d0cb76`, on linux/amd64 (16 CPUs), whose tree held every change of code: the last commit that
changed code or tests is `828225b6`, and none after it changed any, `36d0cb76` changing B997's
entry alone (bridge lows 19's correction: this line named `36d0cb76` the last commit of code):
- In the shared build slot, one session, taken at once (14:12 UTC): `make fmt` rewrote nothing
  (`git status` clean after it); `make vet` clean; `go vet` of `internal/admin`, `internal/api` and
  `internal/consolelan` for windows/amd64 and windows/arm64 clean; those three packages' tests as
  `make test` runs them (`-race`, the release tags, `-timeout 45m`, `-count=1`, `-p 3`): ok, admin
  1,338.6 s, api 148.1 s, consolelan 1.0 s.
- Outside the slot, one package at a time: the queued-waiter case `-race -count=20` as `828225b6`
  holds it, 20 of 20; then the four changed admin tests (L, N, D and the queued-waiter case)
  `-race -count=50` at `GOMAXPROCS=4`, beside the slot's run: 200 of 200 passed, no race reported,
  442 s;
  `internal/consolelan` whole `-race -count=50`: passed, the two families' subtests 100 of 100
  (`TestARoutedClientInsideThePrefixGetsNoConsole` skips here, needing root: CI's routed job runs
  it); consolelan's vets for windows/amd64 and windows/arm64, and its test binary built for both.
- cmd/bridge's source guards (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`,
  `TestNoBlankKeepers`, `TestEveryBackgroundGoroutineDrainsOnCleanup`): clean before this record,
  and the first two again with it staged, a citation of a test that does not exist planted in it
  turning `TestEveryCitedTestNameExists` red naming this file, its bytes then put back.
- `make docs-check`: clean before this record and with it (44 entries since the archive).
- `go run ./scripts/qualification-version -base a1c35128 -head HEAD`: "qualification version OK".

What only CI verifies: consolelan's live Windows reading (`hold_windows_test.go`) on windows-latest,
and the macOS legs. PROTOCOL.md is not touched.
