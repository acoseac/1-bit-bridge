## 2026-11-28 — B1053: CI keeps the pinned kernel files in its Actions cache, and names a rate-limited download

Branch `fix/b1053-kernel-file-cache`, from main `762e53d6`. CI and scripts only: no product code
changes. The PR merges after the 0.2.4 tag.

### The failure

Main's gate on 762e53d6 (run 46050180223, 2026-11-27 18:32Z) failed its first attempt: `alsa-abi
(amd64)`, `alsa-abi (arm64)` and `dac-profiles (kernel seed)` each got HTTP 429 from
raw.githubusercontent.com on every retry for about four minutes, with the job's token; the rerun
passed. Every run of those jobs fetched a file at the tag `internal/alsapcm/uapi.go:18` pins
(`v6.12`): the kernel's `include/uapi/sound/asound.h`, once a leg, and its `sound/usb/quirks.c`. A
rate limit could so fail any run, whatever it changed, and its log read like any other failed
download.

### The change

- `scripts/alsa-abi/check.sh` and `scripts/dac-profiles/kernel-check.sh` take one new variable
  each, `ALSA_UAPI_HEADER_SAVE` and `DACPROFILE_KERNEL_QUIRKS_SAVE`: after a fetch that succeeded,
  the script copies the fetched bytes to that path (`mkdir -p` its directory, a `mktemp` file beside
  it, `mv`), prints one line naming the path, and fails the run if it cannot. A copy the override
  variable named (`ALSA_UAPI_HEADER`, `DACPROFILE_KERNEL_QUIRKS`) is never saved, and a variable set
  empty is unset, as the overrides are read. The save comes right after the fetch, before the
  comparison, so that it can be run offline; CI keeps the copy only when the job succeeds.
- The fetch reads curl's last HTTP status with `-w '%{http_code}'`, the file still going to `-o`,
  the assignment guarded (`code=$(…) || rc=$?`) so that `set -euo pipefail` does not end the script
  before the status is read (`check.sh:76-93`, `kernel-check.sh:59-76`). A 429 prints
  `<script>: the upstream host rate-limited the download of <url> (HTTP 429) after curl's retries;
  this is not a definitions difference: rerun the job`; any other failure prints curl's exit status
  and the HTTP status, or says there was none (`000`, a timeout or a connection failure). The
  partial file is removed and the run exits 1. The token's handling is as it was.
- `gate.yml`'s two jobs: a step reads the tag as the scripts read it (`sed` on `uapi.go`), fails on
  an empty one and outputs the key, `kernel-files-<tag>-asound.h-v1` (both `alsa-abi` legs) or
  `kernel-files-<tag>-quirks.c-v1`; `actions/cache` at the pin every job uses
  (`55cc8345863c7cc4c66a329aec7e433d2d1c52a9`, v6.1.0) keeps `${{ runner.temp }}/kernel-files`
  under that key, with no `restore-keys`; the check step, its paths through `env:`, takes the copy
  only where `cache-hit` is `true` and the file a non-empty regular file, and otherwise sets the
  save variable, warning with the key where an exact hit holds no file.
- What the pinned action does, read from its sources at that SHA: `action.yml:44` runs the save on
  `post-if: "success()"`, so a failed job caches nothing; `src/saveImpl.ts:50-55` saves nothing
  after an exact hit ("Cache hit occurred on the primary key …, not saving cache."), so an entry
  that holds no file is mended only by deleting it or by moving the keys' `-v1` on;
  `src/restoreImpl.ts:53-55` leaves `cache-hit` unset on a miss, and `:74-79` sets it to whether the
  restored entry's key is the key asked for.
- Docs: both scripts' header comments, both READMEs, the two jobs' comments; the cache's rule in
  `.claude/rules/build-ci-tests.md`, whose `paths:` now take `kernel-check.sh` too, with its long
  form; `player.md`'s pinned-and-checked rule and its long form, which now say what "never vendored"
  means.

### What the owner may decline

`quirks.c` is kernel driver source and carries no Linux-syscall-note, which `asound.h` does
(`GPL-2.0+ WITH Linux-syscall-note`). The change reads "never vendored" as never in the
repository, a build, a release archive, a log or the database: the unmodified files sit only in
this repository's Actions cache, keyed by the tag. Declining the CI copy of `quirks.c` drops the
`dac-profiles` job's tag and cache steps and its check step's choice; `asound.h` stays cached.

### Runs

Linux amd64, Go 1.27.3, offline. A stub `curl` first on `PATH`, under `.tmp/` and never
committed, prints a status only when given `-w '%{http_code}'`, writes partial bytes to `-o` before
a failure, and records whether `--config` was passed (never its content) and whether
`GITHUB_TOKEN` reached it; a logging `rm` beside it records each removal. Every case ran with each
script, against main's scripts (a `git archive` of 762e53d6) and the changed ones, through both
fetch branches (an empty `GITHUB_TOKEN`, so no `--config`, and a dummy one), `TMPDIR` under the
worktree's `.tmp`: 36 runs on main's scripts, 84 of their checks failing, and 38 on the changed
ones, none failing.

- 429, the stub exiting 22: main's scripts exit 22 with no line of their own, curl given no `-w`.
  The changed ones exit 1 with the rate-limit line, the partial file removed (an `rm -f` of it
  before the trap's `rm -rf`), nothing at the save path and its directory not made.
- 404 (exit 22) and a timeout (exit 28, status `000`): main's scripts print no line of their own;
  the changed ones print `failed with curl exit status 22 and HTTP status 404` and `failed with
  curl exit status 28 and no HTTP status`.
- A fetch with the save variable set (the stub serving known bytes): main's scripts save nothing;
  the changed ones save a file `cmp` finds identical, nothing beside it, and print the line naming
  its path.
- The override, a sentinel at the save path and the stub failing if it runs: on both, curl never
  ran, the sentinel stayed byte for byte, and the override's git blob id was printed.
- The save variable unset or empty, and the override: the changed scripts print what main's print,
  stdout and stderr line for line, the trees' paths and temporary names aside.
- A save that cannot be made (its directory refusing writes): the changed scripts stop at the save,
  before the comparison, exit 1, nothing at the path or beside it.
- The dummy token in no stdout, stderr, stub log or `rm` log, and neither curl nor any `rm` the
  scripts ran had `GITHUB_TOKEN` in its environment.
- Where the runs stopped: `check.sh` at the probe's compile, the stub's bytes being no C;
  `kernel-check.sh` at `TestTheKernelSeedIsThePinnedTags` ("the file does not read: the DSD format
  quirk is defined 0 times"). With the stub serving Ubuntu 26.04's own `/usr/include/sound/asound.h`,
  a run that saved it and a run that took the saved copy as the override each passed
  `TestUAPIMatchesTheKernelHeader`, git blob `421ab7d7…` both times, curl never run in the second.
- The stub's model of curl, held to the host's own curl 8.18.0 with the scripts' flags but
  `--proto` (the server being plain http) against a throwaway server on 127.0.0.1: a 429 retried
  twice printed `429` on stdout once, after the third request, and exited 22, `-o` writing
  nothing; a 404, not retried, printed `404` and exited 22; a 200 printed `200`, exited 0 and wrote
  the file; a refused connection printed `000` and exited 7; a stall past `--max-time`, retried
  once, printed `000` and exited 28.
- `gate.yml` parsed with PyYAML, and its new steps ran under `bash --noprofile --norc -eo
  pipefail`, each `${{ }}` in `env:` resolved by hand. That is how the runner runs a step with
  `shell: bash`; these steps name no shell, so the Ubuntu runner runs them as `bash -e {0}`, and as
  they hold no pipeline the two agree for them (Round 2's runs use `bash -e`). The runs covered
  the tag step's outputs (`v6.12` and its key) and its failure on a `uapi.go` without the line; the
  check step through a miss, an exact hit, a hit whose file was missing or empty (the warning
  naming the key, then a fetch and a save) and a `cache-hit` of `false` with another key's file
  present (a fetch over it); for `alsa-abi` each run to the comparison's end, which passed, the
  hit's blob id the miss's.
- Controls, each on a copy of the tree and never on a tracked file, predicted before they ran, the
  copy unmutated green first: all fourteen went red. In `check.sh`: curl given no `-w` (the 429
  and 404 runs lost their status), the assignment unguarded (`set -e` ended the failed runs with
  curl's status and no line of their own), the override branch saving its copy (the sentinel
  overwritten), no removal of the partial file, the partial file saved in its place (a file at the
  save path), and an empty save variable read as set (those runs no longer printing what main's
  print). Three of them again in `kernel-check.sh`: no `-w`, the override saving, the partial file
  saved. In `gate.yml`: the check step without its `-s`, in either job (an empty copy taken,
  nothing fetched), or without its `cache-hit` test (another key's file taken), `restore-keys`
  added, and the tag step without its empty check (an empty key output, the step passing).
- `bash -n` on both scripts; shellcheck is not installed here. `go test -tags
  ts_omit_webclient,ts_omit_debugeventbus ./internal/alsapcm ./internal/player/dacprofile
  -count=1` passed (38.6 s and 0.06 s), both kernel-file tests skipping without their flags; `make
  fmt` rewrote nothing; `make vet`, `make check-build-tags` and `make docs-check` passed, and so
  did the tests that read these files: `cmd/bridge`'s citation and docblock guards (a test name
  planted in this entry turned the citation guard red, naming the entry),
  `internal/player/qualification` (which reads `gate.yml`), `scripts/third-party-licenses` and
  `scripts/dac-profiles/ios-export`, with its `-check`.

### What CI is to show

On the PR, each cold key's first passing job saves it once: the two `alsa-abi` legs can both miss
and fetch, and one of their saves makes the entry they share. A rerun on the PR then has both legs
and `dac-profiles` take the override, print the saved file's git blob id, and pass their
comparisons. Main's first run after the merge can be cold again: a PR's cache belongs to its merge
ref, which main does not read.

### Round 2

Five gaps in `ac89883a`, each shown red there first with the harness above (the stub `curl`, and
now `mktemp`, `cp` and `mv` stand-ins that each fail only the save's own call, so that a failed
save does not rest on a permission a privileged process ignores), with both scripts and both
token branches, then green on the change:

- **Only a final 200 is a download.** The scripts read curl's exit alone, so a 206 or a 204,
  which `-f` lets through with an exit of 0, reached the save and the comparison. A download now
  needs an exit of 0 and a final status of 200 (`[ "$rc" -ne 0 ] || [ "$code" != 200 ]` enters
  the classifier), and the classifier is as it was: a 429 keeps its rate-limit line, `000` or no
  status the no-HTTP-status line, any other status the other-failure line.
- **A link is never a hit.** `[ -f ]` and `[ -s ]` follow links, so an entry holding the file as
  a link to the runner's installed header, or a `kernel-files` directory that is a link, would
  have been compared under the pinned tag. Both check steps now take the copy only where neither
  the file nor its directory is a link (`[ ! -L ]`, the directory as `${KERNEL_FILE%/*}`, a
  parameter expansion, so that no command but the script runs with the token in its
  environment).
- **An exact hit sets no save variable, and a save path that is no regular file is refused.** The
  check steps set the save variable on an exact hit too, where actions/cache saves nothing
  (`src/saveImpl.ts`), and `mv -f` into a directory at the path moved the temporary file inside
  it with a status of 0, the script then saying it had saved; a link or a fifo there was
  replaced. The save variable is now set only where `cache-hit` is not `true` and the directory
  is no link, a linked one warning, and the scripts refuse a path that is a link, dangling or
  not, tested on its own, or that is there and is no regular file, before anything is written.
- **Every failed save prints its line.** `mkdir -p` and the `mktemp` assignment ran outside the
  guarded condition, so under `set -e` their failure ended the run with no could-not-save line.
  Both are in the one condition now, `save_tmp` empty first: whichever step fails prints the line,
  the temporary file it made is removed, and a file already at the path is kept; a directory a
  successful `mkdir -p` made may remain.
- **The simulation's shell.** The first runs above used `bash --noprofile --norc -eo pipefail`,
  corrected in place; these run each step as the runner runs one with no `shell:`, `bash -e`.

The runs, 74 on `ac89883a` and 74 on the change (each new case with both scripts and both
branches):

- A 206 after partial bytes, with an exit of 0: `ac89883a` saved the partial bytes and compared
  them, with no line of its own; the change says `failed with curl exit status 0 and HTTP status
  206`, exits 1, removes the partial file, saves nothing, and the comparison never begins.
- A 200 cut short (exit 18) after partial bytes: already right on `ac89883a`, which enters the
  classifier on any non-zero exit (`failed with curl exit status 18 and HTTP status 200`).
- A directory at the save path: `ac89883a` moved `.asound.h.<random>` into it and printed the
  save; the change prints the could-not-save line and exits 1, the directory empty, nothing beside
  it, the comparison never begun. A dangling link, a link to a regular file and a fifo at the path:
  `ac89883a` replaced each with the copy; the change refuses each, the dangling link still
  dangling, the linked file's bytes kept, nothing beside it.
- `mktemp` refused for the save's template, the script's first `mktemp -d` let through: `ac89883a`
  ended at `mktemp` with no line; the change prints it and exits 1, the directory `mkdir -p` made
  left empty. The save path under a regular file, where `mkdir -p` fails (`Not a directory`):
  `ac89883a` ended there with no line; the change prints it, the file in the way unchanged.
- `cp` refused for the temporary file, and `mv` refused over a file already at the path: already
  right on `ac89883a`, whose `cp` and `mv` were guarded; the temporary file removed, the file at the
  path kept.
- Every earlier case passed on both trees, and with the save variable unset or empty, and with the
  override, the changed scripts print what main's print (36 of 36 streams).
- The check steps, through 8 states each, both jobs, `alsa-abi`'s to the comparison's end: on
  `ac89883a` an exact hit with the file missing or empty saved, a save the cache would never keep;
  a hit whose file or whose directory is a link took the copy, curl never running; and a miss
  whose directory is a link saved through it into its target. On the change each of those warns,
  fetches, saves nothing and leaves the fixture and its target as they were: the missing file
  absent, the empty file empty, each link as it was. A miss, a hit and a `false` restore are as
  they were.

Controls, each predicted before it ran, on a copy of the tree, the copy unmutated green first: all
eighteen red, in both scripts and both jobs. In the scripts: no 200 check (the 206 case), no `-L`
refusal (both link cases), no refusal of a path that is there and is no regular file (the
directory and the fifo), `mktemp` moved out of the condition (its case), and `mkdir -p` moved out
(its case). In the check steps: no `[ ! -L "$KERNEL_FILE" ]` (the linked file); the save variable
set on an exact hit (the missing, empty and linked files, and one case past the prediction, the
linked directory, which the save went through); no directory check on a hit (the linked
directory), and none on a miss (the miss through it).

Checks, on the change: `bash -n` on both scripts (shellcheck is not installed here); PyYAML and
the simulation above; `go test -tags ts_omit_webclient,ts_omit_debugeventbus ./internal/alsapcm
./internal/player/dacprofile -count=1` passed (38.9 s and 0.06 s), both kernel-file tests
skipping without their flags; `make fmt` rewrote nothing; `make vet`, `make check-build-tags` and
`make docs-check` passed; and so did `cmd/bridge`'s citation, docblock and blank-keeper guards,
which `make docs-check` does not run, `internal/player/qualification`,
`scripts/third-party-licenses`, and `scripts/dac-profiles/ios-export` with its `-check`.
