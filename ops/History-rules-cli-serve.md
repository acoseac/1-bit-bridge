# Long form of `.claude/rules/cli-serve.md`

> Moved verbatim from `.claude/rules/cli-serve.md` on 2026-10-18 so it stops loading with every matching file. **These entries are still binding.** The rules file keeps each one as a single line (its bold title plus its own "Don't…" sentences, ending `→ History-rules-cli-serve.md`); this file keeps the reasoning, measurements, rejected alternatives and the full don't-undo list. Change an entry here and its line in the rules file in the same commit. Each `##` below names the rules-file section the entries came from; entries inside a section keep their original order.

## The CLI and the serve wiring (`cmd/bridge`)

- **`bridge init`'s preflight grades the install that is THERE — all of
  it, not just its certs.** #951/#952 taught it to read the config at the
  target path and copied the cert fields; the `doctor.Deps` literal kept
  hard-coded `APIPort: 7788, AdminPort: 7789` and never set `OwnPIDFile`.
  So a public-mode install on `:443` was graded against two ports nothing
  uses — free, therefore `port-api: ok`, a check passing because the thing
  it guards is ABSENT — and a re-init while the operator's own bridge runs
  found 7789 bound, skipped checkPort's whole "is it us?" ladder for want
  of a pid file, and ABORTED. The call site's own comment named that
  situation as a reason to pass `--skip-doctor`, i.e. the defect worked
  around in prose inside the command whose job is to grade the install.
  `withExistingInstallDeps` carries ports and pid file too, and is named
  for what it does rather than for certs — a name that says otherwise is
  how the next field gets left out. The first-install skip keeps its own
  control. (#963) The one part of that install it leaves ungraded is a
  port a rewrite moves off (2026-09-28, the "…on a certain rewrite" bullet
  below); the certificate and the data dir are graded as ever.

- **…and grades the ports it is about to SAVE, which is a different
  question** (#970). Where the install's config loads, the preflight
  grades the install's current ports (where none loads, the next
  bullet's), all of them on a run that keeps the config and those a
  rewrite keeps on one that does. For the certificate that is right and
  deliberate — init does not rewrite the pair on disk.
  For the ports it was backwards: a rewrite wrote the run's own addresses,
  so an install on `:9090`/`:9091` was graded on those, passed, and was
  then handed `:7788` / `127.0.0.1:7789`. A second narrow pass
  (`doctor.RunPortChecks`) grades what the config will contain, BEFORE
  `Save`, so a refusal leaves the existing config intact — and only over
  the ports that CHANGED, since an unchanged one was already graded
  correctly. Since backlog B125 a same-posture rewrite that names neither
  address flag keeps the install's listen and admin addresses, so that
  first pass already grades what will be saved. A named flag or a posture
  change still writes the new addresses, and the second pass grades only
  the ports that changed.

- **…and where no install's config loads, the preflight grades the ports
  this run WRITES, from the one definition the config is built from**
  (2026-09-28). It kept init's 7788 / 7789 there, on a first install and
  over a config that does not load, whatever the run wrote. A `--public`
  run writes `:443` or `--listen-address` and 7789 or `--admin-address`, so
  another process on 7788 (a second bridge beside the operator's, say)
  refused a public first install, and a public re-init over a broken
  config, over a port neither would bind: measured with the real binary,
  exit 1 on `[FAIL] port-api :7788 in use`, and exit 0 now.
  `initAddresses` is the ONE definition of the addresses a run writes when
  it names a flag or changes posture, and of a first install's. The config
  is built from those variables, and the preflight is seeded with them,
  which `withExistingInstallDeps` still replaces with the install's own
  where the config loads (and reports whether it did). A same-posture
  rewrite that names neither address flag overlays the file's addresses
  onto the same variables before the preflight (`addressesARewriteKeeps`,
  backlog B125), so the preflight and the saved config still share one
  pair. **Don't seed the preflight from anything else, or build the
  config's addresses anywhere else**: a divergence reaches only the second
  pass, which runs after init has made its data dir. Where no config loads that pass now grades
  nothing, and its `OwnPIDPortsUnknown` exception (the "…over a config
  that is there and does not load" bullet) stays for a port that could
  differ. **A refusal on the run's ports says so under the report**
  (`portsThisInitWrites`, printed only when a port check FAILed on the
  run's ports: where no config loaded, or on a rewrite, the next
  bullet): those lines are the run's choice, not a verdict about
  an install, and the checks' own hint names a bridge.yaml, where a run
  chooses its ports with `--listen-address` and `--admin-address`. **An
  address flag the config would refuse is refused before the preflight**
  (exit 2, `initAddressFlagsError`), which has no port to grade for it; a
  public run's was refused only at the validation before Save, after a
  preflight that graded 7788 in its place. **Both flags apply in either
  posture** since 2026-09-28: a loopback run read neither, and saved
  `:7788` / `127.0.0.1:7789` with exit 0 and no word, whatever it was
  given, `0.0.0.0` and an address with no port included. **A loopback
  run's `--admin-address` must name a loopback host**
  (`config.ValidateLoopbackAddress`, the rule `Validate` holds that
  install's adminAddress to, now one function for the file and the flag):
  its console has no login, so binding loopback is its whole trust
  boundary. Don't honour a non-loopback one there and let `Validate` refuse
  it after the preflight, and don't widen the rule for the flag.

- **…and on a certain rewrite of an install whose config loads (`--yes
  --force`, or an interactive yes), the preflight grades only the
  install's ports the rewrite KEEPS** (2026-09-28). The preflight graded
  the install's old ports all the same: an install on `:X` / `:Y`, its
  bridge stopped, another process on X, and a `--yes --force` rewrite
  moving the API off X exited 1 on `[FAIL] port-api :X in use` (measured
  with the real binary, as a public run and as a loopback one), about a
  port the saved config never binds. `portsARewriteAbandons` lists
  the install's ports the rewrite binds in NEITHER role, and
  `doctor.Deps.AbandonedPorts` answers each ok "not checked: this rewrite
  moves off :X", with no probe; the second pass grades what the rewrite
  writes in their place, as before. Since backlog B125 a same-posture
  rewrite that names neither address flag keeps both, so that list is
  empty and the install's ports are graded as its own. A rewrite moves a
  port when it names `--listen-address` or `--admin-address`, or changes
  posture. **Build that list over both roles,
  never per role**: a rewrite moving the console onto the old API port
  binds that port again, and a per-role list names it, so the second pass,
  which carries the same Deps while it grades that port as the new admin
  port, answers "not checked" and saves a port a stranger holds. **A kept
  port is graded as the install's, pid file and all**, and its refusal says
  it is a port this init would write (`portsThisInitWrites`). **An
  interactive run asks "Overwrite?" before the preflight** (2026-09-29,
  backlog B61), so its yes is the same certain rewrite, and a no keeps the
  config, whose ports its preflight grades, all of them, as for `--yes`
  without `--force`, and asks for no name it would discard. The decision
  is made once (`replace` / `keep` in initCmd) and read by the posture
  check, the preflight, the name prompt and the keep branch. Until then
  the question came after the preflight and the name prompt: a stranger
  on a port a yes would move off refused the run before it could ask
  (measured: exit 1, where the same flags with `--yes --force` exited
  0), and a no discarded the name just typed. #1081 had rejected the move
  as a reorder of every interactive re-init that "still leaves a
  no-answer grading the install"; a no keeps the install, so grading it
  is the right answer, and the reorder is one question moved ahead of the
  report. **It stays AFTER the library prompt**: that prompt ends a
  loopback run on a closed stdin ("input closed; aborting."), as it did
  before, where `confirm` would take its default, a no, which keeps the
  config and installs the service on nobody's say-so (a public run asks
  for no library, and reached that keep on a closed stdin before this
  change too). **And the run refuses a config that changed after it read
  it** (`configChangedSinceRead`, exit 1, the config not written): moving
  the question ahead of the preflight and the name prompt, which wait on
  the operator, put both between the decision and the write. Measured with
  the real binary: a config another init wrote while the name prompt
  waited was written over (exit 0, its name gone), where main had asked
  "Overwrite?" after that prompt, and a rewrite answered yes lost an edit
  made meanwhile (CodeRabbit's security review of #1106 named the window).
  The run keeps the bytes it read at its start (`configAsRead`, the read
  `readPriorInstall` parses) and compares them in the statement directly
  before `cfg.Save`, which `TestTheChangedConfigCheckIsTheStepBeforeSave`
  pins: its first place, before `refuseRewrite`, left the second port pass
  and the TLS load after it, and a change during them was still written
  over (CodeRabbit's next round). A refusal there can follow a first
  install's TLS mint, which the next run loads, as it loads the pair a
  failed Save leaves. What remains is Save's own staging and rename, which
  only an interprocess lock would close (#1043 declined one for
  `tokens.json`). The check also closes the change the old order let
  through: a rewrite keeps what it read, and a config written since would
  lose what its writer wrote. **Don't compare a stat**: bytes are what the
  rewrite keeps from, and a write in the same mtime tick passes a stat
  (#1043's rule for `tokens.json`).

- **The "is it us?" fallback must NOT reach a port the run is choosing.**
  `checkPort` answers ok or warn — never fail — whenever the pid in
  `OwnPIDFile` is alive and the owner probe could not rule it out (one it
  rules out FAILs: the #1029 bullet below): an unattributable port warns,
  and one merely owned by this uid is reported **ok** (the capability-bound
  `:443` case it exists for). A live bridge binds what ITS config says, so
  it cannot legitimately own a port absent from it; left set, an occupied
  NEW port read as "our bridge is still running", `HasFail` stayed false,
  and the config saved anyway — the check passing because the thing it
  guards is absent, one level in from the defect the pass exists for.
  Clear `OwnPIDFile` for a changed port, except where no config says which
  ports the running bridge binds: there it stays, confined to attribution
  (`OwnPIDPortsUnknown`, the "…over a config that is there and does not
  load" bullet below).
  `RunPortChecks` takes WHICH ports to grade, because port 0 is a legal
  value with its own verdict and cannot double as "skip this one".

- **…and clearing it did nothing on a host without lsof, because the
  verdict read which tools the host HAS** (#1021). `checkPort` ended in
  `if !portProbeAvailable() { return warn(…) }`, goreview F9's fix (#429)
  for a LIVE bridge that a host without lsof could not attribute. #640's
  liveness arm answers that case first, so all the fallback still saw was
  a port with no live pid of ours behind it: no pid file given, none
  readable, or a dead one. lsof cannot change that answer. With no pid it
  is never even asked (a logging shim counted zero calls), and a dead pid
  holds nothing for it to find. Yet its absence turned the Fail into a
  warn. In the stock `golang:1.26.6` image `bridge init` therefore
  saved an admin port another process held, on a first install, on
  #970's second pass, and on a re-init with the bridge stopped, and
  `bridge serve` then could not bind. Gating the fallback on an empty pid
  file would have fixed the reported tests and left the stopped-bridge
  re-init open. The fallback is gone, and `portProbeAvailable` with it:
  **a missing tool may explain a verdict; it never decides one.** lsof is
  Priority `standard` on Debian 13 and Ubuntu 26.04, so their minimal
  installs and container images lack it, while CI's Linux and macOS
  runners have it. `withoutLsof` sets `lsofPath = ""`, the state
  `resolveLsof` leaves on such a host, so the no-lsof leg runs on every
  runner.

- **…and grades no port at all from a config it could not load**
  (#1022). `buildDoctorDepsFor` seeds 7788 / 7789 and replaces them only
  from a config that loads, and the pid file's path comes from the same
  config. So a config that was named or found and did not load left the
  port checks grading a guess with no pid file behind it. Run by a user
  who cannot read a service-owned config (`bridge init` makes its dir
  0700), doctor warned about the config and then FAILed both ports,
  "another process owns this port", against the live bridge's own
  listeners: on every host with lsof, and since #1021 on every host. The
  runbook's validate-before-restart run on an edit with a typo got the
  same two false FAILs beside the real one, and an install off the
  defaults read "free" about ports nothing binds. For any load error,
  `ungradedConfigPortCheck` now answers ok "not checked", with the reason
  from `ConfigFile.problem` (config-file's own classifier), naming no
  port. **It runs before OwnedPorts and the bind probe**, so the answer
  cannot depend on who holds a guessed port. **ok, not warn**: config-file
  gives the one verdict about the config at #985's severity, a check that
  declines for a reason another line reports is ok elsewhere here too
  (config-dir's "not checked", tls-cert-sans, and tls-cert's since
  2026-09-29), and no consumer reads a
  port line's status (a Gemini consult argued warn for JSON consumers;
  declined on that census). **The trigger is a load error, never an absent
  config**: with nothing named or found, doctor runs before `bridge init`
  and the defaults ARE the ports init writes, so they are graded, and a
  held one still FAILs.

- **…and config-dir does not probe as a user who cannot read the config**
  (#1023). config-dir vouches that the BRIDGE can create and write the
  directory beside its config, and its two probes, a MkdirAll and a write
  of `.doctor-probe`, answer for whoever runs doctor. A user config-file
  reports as unable to read the config is not the one the bridge runs
  as, since the bridge reads it at every start. Yet beside #1022's "not
  checked" port lines, config-dir FAILed "not writable" against the 0700
  dir `bridge init` makes, so that run exited 1 and advised `bridge init
  --skip-doctor`, and `--fix` printed "created … but chmod 0700 failed"
  about a directory it had not created. Where that user may write,
  config-dir vouched ok for a directory the bridge may not. On
  `configUnreadable`, `checkConfigDir` now answers ok "not checked: the
  config in it is not readable by this user", **before the create as
  well as the write** (a config below an untraversable parent fails the
  create), and **whatever either probe would answer**. **Only that
  error**: a config that does not load was read by this user, who can be
  the bridge's, and it names its directory as well as one that loads (the
  directory comes from the PATH). A run that found nothing is by the user
  about to run `bridge init` there. Both keep the probe and its FAIL, as
  does init's own preflight (a nil lookup). **Ask which half is wrong
  before copying a decline**: the port lines decline for all three load
  errors because their INPUT is the guess, and config-dir declines for
  the one where the PROBER is. **The launcher's doctor row is the
  exception to both declines** (`ConfigFile.PreSetup`, read through
  `ungraded`; config-file keeps `problem()` and still warns). The menu
  reads any stat error as "not installed", so over another user's
  install, the root-owned dir a `sudo bridge init` leaves, it offers
  Setup, and this row exists to preview Setup's preflight. That preflight
  looks nothing up and FAILs the directory and the held default ports as
  this user. This fix's first draft declined config-dir there too, so
  beside #1022's "not checked" ports the row read "all clear." while Setup
  refused (measured on dido); a Gemini consult caught it.
  `TestMenuDoctorPreviewsSetupOverAnInstallThisUserCannotRead`
  drives both and requires the same lines. A wrong-user `bridge doctor
  --config` run now ends "all clear." (13 ok, 4 warn, 0 fail), as the
  footer does whenever nothing FAILs, below config-file's warn saying the
  install was not graded. The same consult proposed rewording the footer
  for every run with warns, which changes every report. That was not taken
  here.

- **…and over a config that is there and does not load, init recognises
  the bridge it replaces by the pid file in the data dir it WRITES, and
  only where the probe SEES that bridge on the port** (#1027).
  `withExistingInstallDeps` returned early on any load error, so the
  preflight graded 7788 / 7789 with no pid file: a bridge live on its
  defaults whose config then broke FAILed both port checks, and `bridge
  init --yes --force`, the run that replaces that config, refused (#1022
  measured it). A public re-init refused in the second pass instead,
  because "changed" was measured against init's defaults and #970 clears
  the pid for a changed port (since 2026-09-28 the preflight grades a
  public re-init's own ports there, the bullet after #970's, in this
  mode). init always writes `<dir>/data` and serve
  records `<dataDir>/server.pid`, so the bridge the re-init replaces is
  known without its config wherever the data dir did not move. That pid
  file is wired with `doctor.Deps.OwnPIDPortsUnknown`, which confines the
  "is it us?" ladder to ATTRIBUTION (`checkChosenPort`): a held port is
  excused only when the probe sees the recorded pid listening on it.
  **Liveness, the uid arm and a failed probe excuse nothing there**,
  because no config says which ports that bridge binds: an install that
  had moved off the defaults (because something else holds 7788, say)
  has a live bridge on its own ports while another process holds the one
  init writes. With the full ladder behind that pid file the re-init
  saved `:7788` without a word, and the restarted bridge died on `bind:
  address already in use` (row B on dido, with and without lsof): #970's
  defect. The second pass keeps that pid file rather than clearing it
  (though since 2026-09-28 no port of the run reaches it there). **A
  MISSING config stays a first install**, with no pid file: nothing there
  shows an install, and a leftover pid can be stale or recycled (a Gemini
  consult agreed). **Attribution does not depend on lsof on Linux**:
  where none resolves, `isPIDListeningOnPort` reads the listener's inode
  from `/proc/net/tcp{,6}` and looks for it among `/proc/<pid>/fd`'s
  `socket:[N]` links (`pidListensOnPort`), the tables lsof itself reads.
  Without that, a host with no lsof refused the bridge's own port that a
  host with lsof accepted (NC-F, one set of facts, two verdicts). Its
  limits are lsof's: another user's process, or a `cap_net_bind_service`
  binary (dumpable=0), keeps its descriptors from a non-root observer, and
  the refusal's hint names the recorded bridge and, where the probe could
  have missed it, says to stop it first (the next bullet, #1028).
  **An ok is not proof of attribution**: on Linux the uid arm answered ok
  for a test's own listener until #1030, so `TestPortCheck_OwnPIDMatches`
  asserts the "bound by our own bridge" summary; with the `/proc` path
  removed it had passed on the uid arm.

- **…and a live recorded bridge the probe did not see is explained by what
  the probe SAW, never by the one case the arm was written for** (#1028).
  #640's liveness arm said "pid attribution blocked — capability-bound
  binary" on its ok line, and blamed `cap_net_bind_service` and dumpable=0
  in its warn hint, for every live recorded pid the probe did not name,
  and `checkChosenPort`'s refusal gave the same capability as its example.
  Measured on main, that is true of one shape, a bridge granted the
  capability. It was printed for a bridge running as another user (dido,
  both images; the Mac, where lsof run without root lists only that
  user's processes), for a port whose holder lsof had just NAMED, and on
  macOS and Windows, which have no such capability. The case reported, a
  plain bridge in the no-lsof image, already read "bound by our own
  bridge" since #1027's `/proc` attribution. `isPIDListeningOnPort` now
  returns an `ownerSighting` beside found and the error, written by the
  probe that looked (`lsofSighting`, `procSighting`,
  `listenerTableSighting`): what it saw, what it cannot see as this user
  (`blindSpot`, per platform: another user or group, or dumpable=0, on
  Linux; another user, for lsof on macOS; nothing on Windows, whose
  listener table carries every listener's pid), and whether what it saw
  RULES THE PID OUT. Only a probe that saw everything there was to see
  may: Windows' listener table, or `/proc` reading every one of the pid's
  descriptors against every socket table. **Never lsof**: it lists only
  the processes this user may inspect, so naming another holder does not
  exclude a hidden bridge listening on the same port at another address,
  and a socket table that is there and did not read leaves the pid
  possible as well (both CodeRabbit on #1028, the second by a flag rather
  than the error it proposed, which would have moved a verdict). The ok
  summary gives the account. Both hints say to stop the holder when the
  pid is ruled out, and keep the hedge and the blind spot when it is not;
  the zero `ruledOut` is the hedge, the safe fallback for a probe that sets
  nothing. **#1028 left the verdicts untouched, and pinned them apart from
  the text**: `TestPortVerdictsDoNotDependOnTheAccount` walks both ladders
  over every lsof answer and passed on main before the accounts existed,
  and `TestPortVerdictsReadOnlyRuledOutFromTheSighting` (then
  `…IgnoreTheSighting`) feeds every kind of account through
  `ownerProbeFunc`, a ruled-out one included. Only Windows and Linux make
  one for real, so without the seam a verdict keyed on it passed every
  test on a Mac (NC6). Since #1029 one verdict reads the sighting, and the
  two tests pin that it reads `ruledOut` and nothing else. #1021's rule,
  that a missing tool may explain a verdict and never decides one, has a
  second half: **the explanation is the one the probe established.** Root
  gets no blind spot, because inside a Docker container it lacks
  CAP_SYS_PTRACE and "root sees everything" is false too. #1028 found one
  ok wrong and left it for its own change, which is the next bullet.

- **…and a live recorded bridge the probe RULES OUT is not ours on that
  port, so the port FAILs** (#1029). The uid arm (#640) answered ok on
  Linux for a held port whose listener runs as this user, whenever the
  recorded pid was alive and the probe had not named it. It exists for the
  capability-bound bridge, whose descriptors no unprivileged probe can
  read, and it answered the same way for a port ANOTHER process of this
  user holds. #1028's row L4 is that shape: a bridge still running on its
  old ports, with its config edited to a port something else holds. It
  read ok, `bridge doctor --config` exited 0, and the restart could not
  bind, which is #970's defect in the ordinary ladder. The sighting
  already says when what the probe saw excludes the pid (`ruledOut`):
  `/proc` read every one of its descriptors and none is a listener on the
  port, or Windows' table names every listener and the pid is not among
  them. Such a pid holds no listener on that port on any address, so
  `checkPort` FAILs it, AHEAD of the uid arm. That is the fact the
  no-live-pid branch below it already FAILs, and `checkChosenPort` already
  refused a ruled-out pid. **FAIL, not warn**: `bridge doctor` exits 1
  only on a FAIL (a warn prints "all clear." and exits 0), and `bridge
  init`'s preflight refuses only on one. A warn would have fixed the
  wording and not the defect. On main, the re-init over L4 saved the held
  `:7788`. **A missing tool must not decide it, so on Linux the probe asks
  `/proc` after lsof** (`procSecondOpinion`). lsof lists only the
  processes this user may inspect, so its miss never rules a pid out, and
  keyed on `ruledOut` alone (NC3) L4 FAILed where lsof is absent and
  passed where it is installed. After a CLEAN lsof miss the probe reads the
  recorded pid's descriptors through `/proc`, under the kernel check
  lsof's readlinks meet. A match is a match, a ruling-out is joined to
  lsof's account, and anything else leaves lsof's account as it was. A
  failed lsof asks nothing more. On dido, L4 and L5 now FAIL in both
  images where main let both through (an ok and a warn, both exit 0), the
  capability-bound L2 keeps its ok,
  and root in a container (no CAP_SYS_PTRACE) rules nothing out. **Two
  shapes stayed open.** One, L4 over a capability-bound bridge (the NUC's
  shape, row L6), is closed by the next bullet (#1030). The other, L4 on
  macOS (row ML4), is closed by the last bullet of this chain (#1034),
  and NOT by the pid's uid, which this sentence proposed until
  2026-09-26: an lsof a sandbox blinds exits 1 as a clean miss does, and
  the uid rule FAILed a sandboxed doctor's own bridge. **A test that
  records a pid of its own choosing forces what the probe says about it**
  (`procOwnerFunc`, `withUnattributedMiss`). On Linux, pid 4242 may be a
  readable process of the test's own user, and Windows' table rules it
  out, so a test left to the host was grading a different arm. **And it
  records `standInPID`, never a literal** (2026-09-29, backlog B106):
  4242, or 4243 when the test binary is 4242. Every such test binds its
  port in the test process, so a probe left to the host names that
  process as the holder, and where it IS the recorded pid the check
  answers "bound by our own bridge": a freshly booted macOS CI runner
  handed out pid 4242, and `TestPortCheck_DeadPIDStillFails` and a
  `TestChosenPortIsExcusedOnlyByTheRecordedBridgeSeenListening` row, which
  left the probe to the host, failed that way (a no-lsof row would have on
  Linux; main as pid 4242 in a pid namespace fails all three). Left to the
  host, the dead-pid test was also blind on Windows, whose table rules the
  stand-in out: a `checkPort` that ignored liveness still FAILed its port.
  `TestStandInTestsHoldWhenTheStandInIsThisProcess` finds every test that
  reaches `standInPID` in the source, runs them in a child whose stand-in
  is its own pid, the collision on every run, and refuses a
  `writePIDFile` given a pid literal. **A pid a scripted lsof lists as
  ANOTHER process dodges the stand-in too** (`otherThanStandIn`): that
  child can be pid 1305, and with the stand-in at 1305 the fixed "other"
  pid named the recorded bridge, failing the lsof-account tests
  (CodeRabbit on #1115).

- **…and a bridge no probe can read is ruled out by the port's OTHER
  holders, and the uid arm answers only for a listener no readable
  process holds** (#1030). Row L6, #1029's open shape: a bridge granted
  `cap_net_bind_service` runs with dumpable=0, so as its own user
  `/proc/<pid>/fd` is root's and EACCES, and nothing rules it out. With
  it live on its old ports and its config edited to a port ANOTHER
  process of the same user holds, the uid arm answered ok for that
  process's listener (`… lsof lists pid 579 listening on this port`),
  `bridge doctor --config` exited 0 and the restart could not bind, in
  both dido images. **The census**: where the recorded pid's descriptors
  do not read in full, `procSighting` walks every `/proc/<pid>/fd` this
  user CAN read (`socketHolders`), and every socket listening on the
  port held by one of them, other than the pid, rules the pid out
  (`heldByOthers`, now `listenersNotOf`'s first half), so #1029's arm
  FAILs it. An inode names one socket, and a listener of the pid's own is
  held by it alone, which nothing here reads, so it would have no holder. **The one shape a census cannot
  see is a socket the pid SHARES with a readable process, and the bridge
  shares none**: every listener is its own `net.Listen` (close-on-exec),
  it consumes no inherited fd (`internal/supervision` only READS
  `LISTEN_FDS`), hands none on, and `pidfd_getfd` on a dumpable=0
  process needs CAP_SYS_PTRACE. Windows' listener table rules out on the
  same terms, since a duplicated socket keeps the binder's pid. **Adding
  socket activation or a listener handoff to the bridge breaks this
  premise; revisit the census in the same change.** A listener with no
  readable holder (another user's process, root's, io_uring, another pid
  namespace) left the pid possible here, as a table that did not read
  still does; the next bullet rules it out by the uid that created it.
  Root in a container without CAP_SYS_PTRACE lists every fd directory
  and reads no other uid's link, so the holders change nothing for it
  (R6 stays a warn).
  **The uid arm** (`hiddenListenerOf`) now needs both marks of that
  bridge: the listener was created by this uid, AND no process this user
  can read holds it. One a readable process holds is that process's. The
  census could not rule out where another listener has no readable holder
  (L6m: another user's `[::1]:7788` beside the same-user holder), and
  there the old arm said ok. It was a warn after this change, exit 0, and
  FAILs since the next bullet's. **Both halves are
  needed, and the census is the one that fixes L6**: with it disabled
  (NC1) L6 reads warn, exit 0, from the refined arm alone. **Every
  pid-numbered `/proc` read first checks `/proc/self` against `getpid`**
  (`procOfAnotherPIDNamespace`, proctest's rule): under another pid
  namespace's `/proc`, `<pid>` is some other process, and the recorded
  bridge can be a "holder" under another number (a Gemini consult called
  the guard necessary). **The uid arm deliberately skips that guard**: it
  reads no pid, and a `/proc` whose socket tables read at all is this pid
  namespace's or an ancestor's (`/proc/net` is `self/net`), which omits no
  process of this one. Measured on dido: under `nsenter -m` into a
  container both tables are unreadable, and under `unshare --pid --fork`
  the ancestor `/proc` lists every host process. CodeRabbit proposed the
  guard there, and it would only turn L2 into a warn. **The walk is cheap**: 1.1 to 1.8 ms as a user
  over dido's 290 processes, 11 to 15 ms as root without
  CAP_SYS_PTRACE. **A test stands the bridge in without setcap**:
  `prctl(PR_SET_DUMPABLE, 0)` in a re-run of the test binary gives the
  same root-owned 0500 fd directory (measured), so CI runs L2 and L6 on
  the real kernel (`hidden_bridge_linux_test.go`). **A holder this user
  cannot read** (L7, another user's; root's daemon on the NUC is this
  shape) still warned, exit 0, until the next bullet.

- **…and a listener another UID created is not the bridge's, so a
  holder this user cannot read FAILs too** (#1032). Row L7, left by
  #1030: the capability-bound bridge live on its old ports, its config
  edited to a port held by a process of ANOTHER user, or by root's
  daemon (L7z, the NUC's likelier shape). No readable process holds that
  listener, so nothing ruled the bridge out: `bridge doctor --config`
  warned, exit 0, `bridge init --force` saved the port, and the restart
  could not bind (dido, both images, and as root: R7). **The census's
  second half** (`listenersNotOf`): a listener no readable process holds
  is another's when the uid that created it, the tables' uid column, is
  not the recorded pid's fsuid, the FOURTH value of `Uid:` in
  `/proc/<pid>/status` (`pidFSUID`, read behind the `/proc/self` guard,
  NC4). That file reads where the pid's descriptors do not (dumpable=0,
  to its own user and to container root). **The fsuid, not the real or
  effective uid**: a socket is stamped with its creator's fsuid
  (measured: root that called `setfsuid(1234)` and listened shows
  `Uid: 0 0 0 1234` and row uid 1234), and only `fchown` re-stamps one.
  **The premise is the bridge's, checked in the binary, not the
  source**: no setuid-family call, `AllThreadsSyscall`,
  `ParseUnixRights` or `net.FileListener` is linked into the linux build
  (`go tool nm`), nothing fchowns a socket, and every thread carries the
  leader's creds (one distinct `Uid:` line across 16 to 20 threads).
  **Adding a privilege drop, `setfsuid`, socket activation or a listener
  handoff breaks it; revisit the census in the same change.** **No
  special case for the overflow uid**: both files render a uid through
  the reader's user namespace, so DIFFERENT values always name different
  uids, 65534 included, and only EQUAL ones are ambiguous (two unmapped
  uids both render 65534), which `createdByAnother` never counts as
  another (measured in two user namespaces on dido; treating 65534 as
  unknown turned two correct rulings-out into warns, NC7). A readable
  holder is named ahead of a creator uid, since it is what an operator
  stops. **hidepid=1 or 2 hides a dumpable=0 process's status from its
  own user too**, so L7 stays a warn there (measured, with a hidepid=0
  remount as the control). A pid file naming a recycled pid of another
  user now FAILs a port whose listeners that uid did not create, the
  trust #1029 already gave a recycled pid of the same user (P7a, P7b).
  A hidden holder of the bridge's OWN uid (row L6h) was left open here;
  the next bullet closes it wherever that holder runs in another
  cgroup. The L7 kernel test takes root (it runs the holder,
  the bridge and doctor as two other uids), so it runs on dido and skips
  in CI, where the fixture tests carry the rule.

- **…and a listener created in a cgroup that does not nest with the
  bridge's is not the bridge's either, so a hidden holder of its OWN uid
  FAILs too** (#1033). Row L6h, left by #1032: the capability-bound bridge
  live on its old ports, its config edited to a port held by ANOTHER
  hidden process of the same uid (a second capability-bound binary of the
  service user in its own unit, one of its processes in another group,
  one in a container, or one hand-started from another login). No
  readable process holds that listener and it carries the bridge's uid,
  so the uid arm read ok, `bridge doctor --config` exited 0, `bridge init
  --force` saved the port, and the restart could not bind (dido's host,
  the real binary in systemd units, a container and two SSH sessions,
  with lsof and without). **The census's third accounting**
  (`cgroupsNotOf`, after the readable holders and the creator uids): the
  kernel's socket diagnostics (`NETLINK_SOCK_DIAG`, what `ss --cgroup`
  reads, `listenerCgroups`) give the cgroup each listener was created
  in, to any user and for root's sockets too, as the id of that cgroup's
  directory on the cgroup2 mount (its inode: 60 of 60 on dido, equal to
  `name_to_handle_at`'s id), and `/proc/<pid>/cgroup`, readable where a
  dumpable=0 process's descriptors are not, the one the bridge runs in.
  **A socket keeps the cgroup it was created in**: the kernel commit
  that added the attribute (6e3a401fc8af, Linux 5.8) says so, and a
  holder root moved to another unit still reported its first. So a
  listener created in a cgroup that neither is the bridge's, contains
  it, nor sits below it is another process's, **given the premise**: the
  bridge creates its listeners in the cgroup it runs in and never moves.
  Nothing in it writes `cgroup.procs` or asks systemd to move it, and
  **systemd moves no running service** (systemd 259: a `Slice=` edit plus
  daemon-reload left the process in place until a restart); root does,
  and a bridge moved to another cgroup after it listened FAILs its own
  port (row CM, the hint naming both cgroups), while one moved into a
  child of its own nests and stays ok (CMd). **Adding socket activation
  (systemd creates the socket in the `.socket` unit's cgroup), a
  listener handoff, threaded cgroups or a self-move breaks it; revisit
  the census in the same change.** **Nesting counts nothing**, in either
  direction: 5.8 to 5.14 (and 5.10.y before 5.10.226) stamp every new
  socket with the ROOT cgroup once net_cls or net_prio v1 tagging is in
  use (8520e224f547), and threads in threaded cgroups or a delegated
  subtree put a process's own sockets below its cgroup. **Nor does
  anything this process cannot see**: no attribute (before 5.8, or no
  `CONFIG_SOCK_CGROUP_DATA`), hidepid, a v1-only host, a path outside the
  mount (`/../x`, how a container renders a host cgroup; `unifiedCgroup`
  refuses it BEFORE it is joined onto the mount point, which would climb
  out of it, NC10), a deleted cgroup, one past the walk's budget of 4,096
  directories. So in a container, where every process shares its one
  cgroup, nothing changes. The kernel ends a dump it refuses with
  `NLMSG_DONE` carrying a negative errno, not with `NLMSG_ERROR`
  (captured), and the parser reads that status; its tests run on every
  platform against datagrams captured from a real kernel in a container's
  own network namespace, so no host address is in them. **Still open: a
  hidden holder in the bridge's OWN cgroup** (row C6s: a second process
  the bridge's unit starts, one started from the same login session, any
  process of the bridge's container). The runbook's answer is the moved
  port's own line: the running bridge is still on the old port, so
  anything but `free` on the new one is another process. The root-only
  kernel test makes two cgroups under its own, so it runs on dido's host
  and skips in CI and in containers (read-only cgroupfs); the kernel-fact
  tests (`listenerCgroups` against a real listener, a dumpable=0
  child's `/proc/<pid>/cgroup`) run as any user wherever a cgroup2 mount
  holds the test's cgroup (measured in both dido images, as uid 1000 and
  as root).

- **…and on macOS a live recorded bridge that lsof lists listening
  elsewhere is ruled out by that LISTING, never by its uid** (#1034). Row
  ML4, left open by #1029: nothing on macOS set `ruledOut`, so a bridge
  live on its old ports, its config edited to a port another process holds
  (1Password's, this user's; or root's Tailscale extension, row ML7),
  warned, `bridge doctor --config` exited 0, and the restart could not
  bind. After lsof's clean miss the probe now asks lsof for the recorded
  pid's own TCP listeners (`lsof -nP -a -p <pid> -iTCP -sTCP:LISTEN -F n`,
  `portowner_darwin.go`), the macOS twin of Linux's `/proc` read, through
  the same seam (`procOwnerFunc`, which takes the context now, since this
  look runs lsof) and the same merge (`procSecondOpinion`). Listeners, none
  on the port, rule the pid out (`ownListenersSighting`): XNU's check
  (`proc_security_policy`) is made per process, not per descriptor, so a
  pid lsof lists at all is one whose descriptors it read. One on the port
  is a match. Nothing listed says nothing, which is what M1 (a recorded
  pid of root's, to a user), a recorded pid with no listener (M2, a
  recycled pid), a bridge of another user and a blinded lsof all look
  like, so they stay warns. **Not the pid's uid** (`kern.proc.pid`), which
  the #1029 bullet proposed: lsof as uid 501 read all 614 processes of
  effective uid 501 and none of the other 211 (a later census: all 91
  app-sandboxed and all 107 hardened-runtime ones), but an lsof a sandbox
  denies `process-info-pidinfo` or `-pidfdinfo` (an App-Sandbox terminal's,
  an agent's) exits 1 with no output and nothing on stderr, byte for byte
  a clean miss, and the uid rule then FAILed a sandboxed doctor's own
  bridge on its own port, "stop the process that holds the port" about the
  bridge's own listener (NC3b; `TestABlindedLsofRulesNoBridgeOut` pins it).
  **Not `netstat -anv`** either: run by a Go process, from a shell or as a
  launchd job, it printed no TCP row at all, so #1028's note that it names
  every listener's pid without root holds only where a shell runs it.
  **The `-a` is load-bearing**: without it lsof ORs `-p` with `-iTCP` and
  prints every process's listeners beside all of the pid's files, which
  the reader refuses by its pid check (NC2), so a missing `-a` costs the
  ruling-out and never invents one; any name the reader cannot parse voids
  the whole listing for the same reason.
  `TestPortCheckFailsAPortTheLiveBridgeListensBeside` records a child that
  listens and FAILs on all three platforms; the tests that record the `go
  test` parent, which holds no listener, still warn on macOS
  (`parentListens` skips a run whose parent does listen).

- **`configuredPort` asks what an address NAMES; `splitHostPort` asks what
  can be DIALED, and they differ on exactly port 0.** `config.validatePort`
  accepts 0 (the OS-picks-an-ephemeral-port mode every `:0` fixture uses),
  but `splitHostPort` folds it in with a parse failure and both Deps
  assemblies seed the DEFAULTS — so such an install was graded on 7788/7789,
  ports it does not use and which are usually free, and a re-init aborted
  when something else held 7789. `checkPort` has always had the honest
  answer (`"no port set"`, warn, non-blocking); it never received it. Every
  spelling of zero, because `validatePort` runs `Atoi` and `"00"` is as
  legal as `"0"`. `autoStartProbeTarget` is the same question for
  `spawnNowOrWarn` — extracted because the other branch starts a real
  detached process, so the behaviour otherwise has no test at all.

- **A doctor hint is read by an OPERATOR, so no string in `internal/doctor`
  names a `Deps` field** (2026-09-28). Three hints were notes for whoever
  calls the package, and each reached operators, measured with the real
  binary: `pass Deps.port-apiPort` under "no port set" for every config or
  init flag naming `:0` (the answer the bullet above says `checkPort` "has
  always had"), `pass Deps.DataDir so doctor can inspect cert state` on
  EVERY `bridge doctor` run before `bridge init`, and `pass Deps.ConfigDir
  …` on one with no home directory. Each is a sentence about the install
  now (`portZeroHint`, `noConfigDirHint`), severities unchanged there; the
  tls-cert one, `noDataDirHint`, went with its warn on 2026-09-29, when
  the pre-init report learned to grade init's data dir (the doctor bullet
  on it, below). `TestNoStringInThisPackageNamesADepsField` walks the package's
  string LITERALS by AST, so the docblocks that discuss the fields are not
  read, and a hint built from pieces (`"pass Deps."+name+"Port"`) is caught
  by its first; a floor of files and literals keeps a sweep that read
  nothing from passing. A caller's mistake (a zero `Deps`) and an operator's
  choice (`:0`) arrive at the same branch, and only the operator reads the
  report.

- **`bridge init` decides every refusal BEFORE it writes `bridge.yaml`, and
  keeps what an install already has: its TLS pair and a public install's
  admin ACCOUNT** (#1038). A public `--force` re-init over a public install
  saved the config, kept the cert, and then exited 1: `MintInitial` refuses
  a store that holds an account, and initCmd asked the store only after
  `Save`. So every public re-init that rewrote the config reported a
  failure about an install it had already changed, and never reached the
  service install or the footer (#1027's row C). **Re-running init is not
  a request to rotate**, for the password as for the cert; `bridge admin
  reset-password` is. So
  `keepOrMintAdminCredentials` keeps the account and SAYS so, in a box that
  names it, and mints only into a store with no account (no file, or an
  empty one). **Don't mint over a store that does not load** (that destroys
  what the file still holds, and serve refuses the same file), and **don't
  keep silently**: an operator expecting the "shown ONCE" box goes looking
  for a password that was never made. The store (`openInitAdminAuth`) and
  the TLS pair (`LoadOrGenerateWithOptions`) are both READ before `Save`,
  and each refusal says the config was NOT changed. The preflight's
  tls-cert FAIL covers a broken pair only when it runs. A new check init
  can refuse on goes before `Save` too; after it, only the mint's own write
  and the service install may fail. **Which pair is kept, and what else a
  rewrite keeps, is the next bullet's**: this one said "the data dir's
  only" until 2026-09-27, an open defect #1038 measured and left.
  `TestInitPublicReinitKeepsTheAdminCredentials`
  compares every file byte for byte and verifies the first run's password,
  because an exit-code test also passes a "fix" that rotates. The other
  defect found there: `box()` cuts a line longer than its 51-column body in
  the middle, and the "shown ONCE" box printed `The plai... is not stored
  anywhere.` on every public install
  (`TestAdminCredentialBoxesAreNotTruncated`).

- **A rewrite replaces the SETTINGS, never the INSTALL.** `bridge init
  --force` (or "Overwrite? y") keeps `dataDir`, `tlsCertPath` and
  `tlsKeyPath` always, a loopback install's `customEndpoints` on a loopback
  rewrite, and `libraryRoots` when the run names no `--library` (#1040),
  and `libraryName` when it names no `--name` (#1041), and `listenAddress`
  and `adminAddress` when the run names neither address flag and the
  posture does not change (backlog B125). It
  built `bridge.yaml` from `baseConfig` and the flags and kept nothing.
  Measured with the real binary: a config naming its pair outside the data
  dir lost it, init minted a new one there and printed it as "Stable across
  restarts", and `bridge serve` then presented `13:BE:B2:…` to devices that
  had pinned `04:AF:CB:…` (read off the socket); a config naming another
  `dataDir` was pointed back at `<dir>/data`, stranding its `tokens.json`; a
  config with a misspelt key lost its pair the same way; `customEndpoints`
  vanished, and iOS replaces its alternates with `/v1/health`'s on every
  fetch (`BridgeEndpointSelector.update`), so every device lost that route;
  a public rewrite without `--library` wrote `libraryRoots: []`. **The rule
  is who could give a value back.** The pin and the tokens only a re-pair
  restores, so they are kept always. The endpoints and roots, which init
  never asks for, are kept where the run writes nothing in their place: a
  `--public` rewrite writes the domain's endpoint (a posture change starts
  from the new posture's, since the old list names the other posture's
  addresses), and a `--library` replaces the roots. The preflight grades
  only a root the run names: a kept public root may be a mount that is not
  up, `checkLibraryRoots` FAILs a missing one, and public init must not
  need the mount (init's public-mode note). **So is the name, where the
  run names none** (#1041), in either posture, since a name names no
  address. The hostname a run without `--name` took is only init's guess
  for a first install: measured, `My Library` became `Macbook.local` in the
  file, in `/v1/health` (unauthenticated) and in every new pairing QR's
  `name=`, the name a newly paired phone takes, and the interactive prompt
  offered the hostname as its default over the install's own. The prompt
  offers the install's name now, `--name` replaces it, and the kept list
  shows it only where a first install would have taken another, so the
  rewrite of an install named for its host prints nothing. **A config
  giving no name, or a blank one, keeps none** and takes the hostname, as
  before: `Load` serves it `DefaultLibraryName` (a blank one only since
  #1042: before it, `Load` served a blank name as written; blank means blank
  to `config.TrimLibraryName`), a fallback nobody chose,
  and keeping that listed it as kept from a config that never held it (a
  Gemini consult caught the first draft doing so). Don't default the name
  in `readPriorInstall` the way it defaults `dataDir`, whose `Load` default
  is init's own. The rest (features and cadences) is the documented
  overwrite. The ports are kept when the run names neither address flag
  and the posture does not change, read from the file and defaulted as
  `Load` defaults them (the admin address only on a loopback install). A
  posture change writes the new posture's ports. Naming either flag writes
  that address and the posture's address for the one it does not name, so
  a run that names only `--listen-address` still resets the admin address.
  A script that re-runs `bridge init --yes --force` expecting `:7788`
  keeps the old port. `printKept` lists an address only when a first
  install of this posture would have written another, so a default-port
  rewrite says nothing. **A kept endpoint
  that names the port the rewrite moves the API off is kept, and named in
  a warning** (`warnKeptEndpointsOnAMovedPort`, 2026-09-29, backlog B61):
  measured, an install on 127.0.0.1:X listing `https://nas…:X`, rewritten
  onto :Z, kept it without a word, and the restarted bridge's `/v1/health`
  advertised it beside its :Z addresses, an alternate every device tries
  and fails over past. Since B125 an unflagged same-posture rewrite no
  longer moves the port, so the warning fires when a flag or a posture
  change does. The test's move onto `:7788` is an explicit
  `--listen-address`. An endpoint names a port by its own or by its
  scheme's (443, 80); `:0` names none. **Never drop or rewrite it**: the
  endpoint is the operator's word for what reaches the bridge, its port may
  be a router's or a proxy's (the endpoint-synthesis rule under The wire
  contract is the same fact), and a forward from it is right again once
  pointed at the new port. **Read what is kept from the FILE
  (`readPriorInstall`), never through `config.Load`**: Load applies
  `BRIDGE_*`, so keeping its values writes the caller's environment into the
  YAML (`writeAutoInitConfig`'s rule), and its unknown-key refusal would cost
  a misspelt-key config (#1027's row C) the pair it names. init LOADS the
  pair the saved config names through `resolveCertPaths`, which serve now
  calls too, so the fingerprint it prints is the one serve presents; over a
  config that does not load, the preflight grades that pair as well.
  **Refused before anything is written**: a config this user cannot read;
  one that does not parse, from which nothing a rewrite keeps can be read
  (**never "proceed when init's data dir holds a pair"**, #1040's first
  version: that pair being there does not make it the one the install
  serves; moving the file aside is the remedy, and init then keeps a pair it
  finds there as on a first install); one naming half a
  TLS pair (**never "keep the paths only when both are set"**, a bot's fix
  on #1040: dropping the half a config names serves the data dir's pair or
  mints one, the same pin break); and one setting
  `demo.enabled` or `deployment.managed*`, postures init never writes, whose
  rewrite dropped the demo's pinned token from every shipped app or handed a
  tenant's withheld controls to its console. A fresh start is moving
  `bridge.yaml` aside, never `--force`.
  **So is the output block, whole, in either posture, a change of posture
  included** (2026-10-22, backlog B505). It says what is beside the
  bridge on this box, MPD and the player's DAC, and init asks for none of
  it, so nothing in a run replaces it; a rewrite dropped it, `output.player`
  and its binding with it. Only the unit gives the binding's serial back
  (bind reads it from sysfs and never prints it), MPD's password comes
  from MPD's own configuration, the loopback route's port is where MPD's
  queued URLs point, and `output.mpd: off` is the operator's opt-out, which
  the default would turn back on. `--force`'s help names the block beside
  the data dir and the pair. `priorOutput` reads it from the bytes the run
  read, on its own and a setting at a time: as a field of
  `priorInstallFile` a type error in it fails that struct's whole decode,
  and `refuseRewrite` refuses the rewrite about a data dir and a pair it
  could read. **Every setting it keeps holds the value Load gave it**
  (round 2, Codex). `resolveOutput` resolves the block's merge keys as
  gopkg.in/yaml.v3 v3.0.1's decoder does (`decoder.mappingStruct` and
  `decoder.merge`): the block's own keys first, then each merged block's,
  in order, where nothing set that setting, a merged block's own merge
  after its keys, a key written as an alias read through it. Each setting
  is then decoded once. Round 1 decoded the block's pairs in the file's
  order and kept each value that was not zero. So a merge written after a
  key overwrote it: `mpd: off` was saved `on`, and the player's serial
  binding was replaced by a merged port binding. And an explicit empty or
  null value let a merged one through. What makes Load refuse the whole
  block costs that much alone: a key written twice in one block, two merge
  keys in one block, a merge key naming no block. A key written beside a
  merge hides the merged value even when the written one is refused. A
  setting that does not read, or that `config.OutputConfig.Problem`
  refuses, is one the config did not load with either: it is not kept, and
  a warning after the save names it by key, line and the check's reason,
  **never its value**. `Problem` returns an `*config.OutputProblem` whose
  reason carries none, and `outputRefusal` builds the warning line from its
  parts (`warnOutputNotKept`). This is round 2's fix, after Codex and the
  Claude API: round 1's warning quoted a refused `output.mpd`, so MPD's
  password pasted onto that line reached stderr. A player block never
  fails, and one this release cannot read is kept as written. A key output
  does not take goes, as every unknown key does. `printKept` lists each
  kept setting, the password as "set (not shown)" and the player by
  whether it is enabled and its binding's kind. **Don't drop the output
  block, and don't read it as a field of `priorInstallFile`.** **Don't
  decode the block's pairs in the file's order: resolve its merge keys as
  the decoder does, and don't print a refused setting's value.**
  (`TestInitRewriteKeepsTheOutputBlock`,
  `TestInitRewriteKeepsAPlayerBlockThatDoesNotReadAsWritten`,
  `TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo`,
  `TestInitRewriteKeepsWhatTheOutputBlockLoadedWith`,
  `TestInitRewriteResolvesMergesBesideASettingItCannotKeep`,
  `TestPriorOutputResolvesMergeKeysAsLoadDoes`.)
  `TestInitRewriteKeepsTheTLSPairItsConfigNames` and its siblings run the
  real initCmd twice over one `--dir` and assert the exit code, the saved
  config and the fingerprint at the paths `resolveCertPaths` finds.

- **A flag the run would not write is never dropped without a word, and
  a posture flag stops no first install** (2026-09-29, backlog B61).
  `--domain`, `--email` and `--admin-tls-proxy` describe a public install
  and were ignored without `--public`, silently: measured, a loopback first
  install given all three exited 0 and saved none, and a `--yes --force`
  rewrite of a PUBLIC install given `--domain` and `--admin-tls-proxy` but
  not `--public` exited 0 with a loopback config, the endpoint every paired
  device dials dropped. `warnIgnoredPostureFlags` decides, after the
  overwrite decision and before the preflight: **a rewrite of a PUBLIC
  install is refused, exit 2, the config untouched** (the flag says the
  operator meant public, and the rewrite would make that install loopback,
  dropping the endpoint); **a first install, and a rewrite of a loopback
  install, warn and go on** (each writes a working loopback install and
  loses nothing; refusing the loopback rewrite, the first draft, would fail
  a script that rewrites with `--yes --force` on every run, passing these
  flags, on its second run, where its first only warned); **a run that
  keeps the config says nothing more** (every flag goes unused, which
  "keeping it" says, and an idempotent `bridge init --yes` re-run must go
  on working). **Don't refuse a run over one that loses nothing by it**:
  a refusal is for the rewrite that would cost the devices their route.
  `--email` with `--public --admin-tls-proxy` is unused too (the bridge
  then runs no ACME client) and warns. Every line names the flags, never
  their values: a `--domain` can carry a password, which `--public`
  refuses without echoing (B54).

The largest package in the repo — 52 production files, ~19k lines, `main.go`
alone 4,698 — and until 2026-09-06 it had **no section here**. Its invariants
were distributed by subject into the sections above (the `bgWriters` join under
Scanner, `SetPostScanHook` under Job pools, the cadence rearm under Config),
which is defensible for a rule about the scanner and useless for a rule about
the wiring itself. The LOUPE run that swept it found five confirmed defects and
four stale claims; the prior-art check explains why, at 20/12/2/6 `cmd/bridge`
mentions across the four `ops/audit-*.md` files.

- **"Always construct, never stop" removes a gate wherever the enclosing `if`
  WAS the gate.** PR #781 converted `if analysisActive {` / `if upscaleActive {`
  into bare blocks so both pools are always built — which is what makes the
  flags hot — and converted every READER to a live predicate in the same commit.
  The WRITE paths had no predicate to convert, so they silently lost theirs: the
  analysis sweeper ran on the default config (`analysis.enabled` is **false**),
  forking a decode per track and pushing a whole-library `indexed_at` delta to
  every paired device 90 s after every boot, while `/v1/analysis/*` 404'd; and
  `POST /v1/upscale` + `DELETE /v1/upscale/variants` gated on an adapter that is
  now never nil, so any bearer-token holder could enqueue sox jobs on a bridge
  advertising `upscaleEnabled: false`. **When a construction guard becomes
  unconditional, enumerate what that guard was gating — the nil-ness of a
  handle is a gate, and it stops being one.** **Every pass stopped short of
  the console's batch** (2026-09-28): #852 restored the two /v1 handlers above,
  #878 (the 2026-09-09 LOUPE) restored `POST /v1/upscale/batch`, and
  `POST /api/upscale/batch` still checked only `BatchCoordinator == nil`, a
  coordinator runServe builds on every bridge. Measured on main with the real
  `serve` and upscale off (the default): 202, `enqueuedCount: 2`, two
  renditions written, while `/v1/health` said `upscaleEnabled: false`; any
  loopback process or public-mode session could do it. The submit now reads
  `admin.Deps.UpscaleActive` first, which runServe wires to `upscaleActiveFn`,
  the closure `WithUpscale` hands /v1 (`TestConsoleBatchGateIsTheV1UpscaleGate`
  requires the same identifier; a nil gate reads as off), and the optimize kind
  reads `OptimizeActive` too, as the projection endpoint does: /v1 refused that
  kind with the CarPlay switch off and the console accepted it. Both read it
  through `Server.optimizeActive` since 2026-09-28, the predicate the player's
  variant summary serves, so "Generate CarPlay" is disabled where this refuses
  (the Admin console section). Both come before
  the scope, and `TestEveryBatchSubmitReadsTheUpscaleGateFirst` sweeps every
  `BatchCoordinator.Submit*` caller by AST. **The console's delete stays open on
  purpose**, as do cancel, list and the failure retry: the owner's call, so an
  operator who switched upscaling off can still reclaim the disk, where
  `DELETE /v1/upscale/variants` refuses. None of them starts sox work.
  **The READS refuse on the store alone, and that is the contract** (backlog
  B108, 2026-09-29): `/v1/download?variant=`, `/v1/waveform`, `/v1/spectrum`
  and `GET /v1/upscale/batches` answer with their feature off as with it on
  (the list on a demo bridge too), and the manifest's analysis fields have no
  gate; only its `variants` are stripped. A switch stops new work, drops the
  work queued for it (backlog B155, the next bullet), and withdraws nothing
  made: a client drops a rendition or a curve it gets a 404
  for, and a delta sync never carries the stripped `variants` (the switch
  bumps no `indexed_at`), so a phone that listed a rendition keeps asking for
  it. PROTOCOL.md said 404 (and 503 for the list) until then, and the owner
  changed the spec to match the code. **Don't gate these reads on the live predicate**:
  `internal/api/feature_off_reads_test.go` pins each route over stubs and
  `TestServeWithItsFeaturesOffServesWhatItMadeBefore` the wiring, through a
  real serve.

- **…and a switch-off also stops the work QUEUED for it: each pool asks its
  live gate when a worker TAKES a job, and drops a job the gate refuses**
  (2026-10-15, backlog B155, the owner's decision). The gates refused only
  new work, and serve never stops a pool, so a queue ran to its end after a
  switch-off, up to 5,000 jobs a lane, while B113's console hid it. Measured
  with the real serve on the dev Mac, one worker, 40 FLACs: after
  `PATCH {"analysisEnabled": false}` answered `live`, 34 more analyses
  landed in 120 s (five-minute files), each a `track_analysis` write and an
  `indexed_at` delta to every paired device; after `upscaleEnabled: false`,
  37 more renders (one-minute files, about 300 MB of sidecars) in under a
  minute, and the batch read `completed 40/40`. With the fix, one more of
  each: the job already running. The analysis pool takes
  `analyze.WithLiveGate(analysisActiveFn)`, the gate /v1 reads; the
  transcode pool `SetLiveGate(kindGates.admits)`, on the one
  `renditionGates` runServe builds (the closures POST /v1/upscale reads per
  kind, which the stale-rendition heal reads too). The gate is asked once
  per job, after the stop check and before anything runs or shows on the
  worker grid. A refused job is `outcomeDropped`: not run, its dedup claim
  given back in finishJob's one critical section, counted as neither done
  nor failed (`PoolStats.Dropped`, which no wire or console DTO carries),
  and, in the transcode pool, announced (`SetOnJobDropped` →
  `Coordinator.OnJobDropped`), because a batch waits on every job it
  enqueued and sat `running` forever without the event. A batch with
  dropped files ends `cancelled` (`batchState.settle`, from the three job
  callbacks and both queue-full truncations, and the same error from the
  post-loop drain check), counting only work that ran, its `error`
  naming how many files were not rendered and that a resubmit renders them
  (measured: 36 of 36, the 4 that ran counted covered). The sweepers offer
  their dropped tracks again at their next pass with the feature on
  (measured: 36 of 36 analysed after an Analyze-now nudge). Switching a
  feature on nudges no sweep, so without one that pass is the sweep's tick
  (`scanIntervalSec`, six hours by default) or the next scan's nudge.
  **A job already RUNNING when its gate closes
  finishes**, by decision: one job per worker, 3.5-3.8 s for an analysis
  and 5.5-5.7 s for a 192/24 sox render of a five-minute FLAC on the dev
  Mac (a DSD render runs minutes), and cancelling one needs a per-job
  cancel and a new outcome on every error exit. A run of drops logs ONE
  line with its count, kept by the one `dropburst.Tracker` both pools use
  (`internal/dropburst`, guarded by the pool's own mutex): a run ends once
  no accepted job is left undecided (the queue's length cannot stand in,
  because a worker holding a job it has not decided would then log a second
  line) or when a job runs, and the drop that ends it writes the line
  before its count: `Tracker.Drop` takes a critical section of its own
  ahead of finishJob's, the order every exit keeps. **Don't fold Drop into
  finishJob's section** (a review suggestion on #1247, declined): one
  section writes the line after its unlock, so the count is visible before
  the line, and a test reading the log once the pool was idle failed 6 of
  20 runs that way. **The transcode gate is
  the rendition FAMILY's, never the enqueuer's** (`renditionGates.admits`:
  the kind's gate, an empty kind being the batch's upscale, AND the
  DSD-render gate for a DSD source): the pool dedups on (source, variant),
  so a phone's request for a rendition a sweep or a batch queued rides that
  job, and a gate only one enqueuer passed would drop the phone's request
  with it. Residual, for that reason: switching pre-generation alone
  (`autoOptimize.enabled`) off stops the next sweep, not the up to
  `maxPerSweep` jobs the last one queued, which are the CarPlay family's.
  **Don't drop a job at Enqueue, release a dropped job outside finishJob's
  critical section, count a drop as done or failed, ask the gate before the
  stop check (a job the stop took is shutdown's), key the transcode gate on
  `Background` or `BatchID`, or end a burst on the queue's length.**
  `TestAQueuedAnalysisDoesNotRunOnceTheGateCloses`,
  `TestAQueuedRenderDoesNotRunOnceItsGateCloses` and
  `TestABatchWhoseQueuedFilesAreDroppedEndsCancelled` lead the pools' and
  the batch's tests (`pool_live_gate_test.go`, `batch_live_gate_test.go`),
  `TestAJobStillBeingDecidedKeepsItsBurstOpen` pins the burst's end,
  `TestAQueuedRenderRunsOnlyWhileItsFamilysGateIsOpen` the family table and
  `TestThePoolsTakeJobsUnderTheV1Gates` the wiring; thirteen controls each
  reddened exactly the tests predicted (ops/engineering-log.md). The two
  pools' tests share their scenarios through `internal/pooltest`, a
  test-only package each pool adapts its own pool to, keeping its own tests.

- **…and a value runServe DECIDES from the config while it builds Deps is a
  boot snapshot, however live its reader is** (2026-09-28).
  `admin.Deps.ProjectedSize` and `AvailableDiskSpace` were function literals
  called in place that answered nil unless `upscale.enabled` was true at
  that moment, and the projection handler read nil as "feature off".
  Measured with the real `serve`, flipping `upscaleEnabled` through
  `PATCH /api/settings`, which answered `live` both times: `GET
  /api/library/browse-projection` answered 503 `upscale-disabled` after the
  switch went on and 200 after it went off, while `/v1/health` followed it,
  and with no sox on PATH it projected while health said off. The same nil
  made `GET /api/upscale/variants-dir` report `freeBytes: 0`, "0 B free" on
  the Library roots page, on every bridge booted with upscaling off. The two
  sat directly above `OptimizeEligible`, whose own comment records this fix
  for `optimizeEnabled`. Both helpers are now wired on every bridge, and the
  handler refuses on `s.upscaleActive()`
  (`Deps.UpscaleActive`, the batch's gate and /v1's, which
  `TestConsoleBatchGateIsTheV1UpscaleGate` pins) before the target read, the
  walk and the disk probe. **A function literal called where a Deps field or
  a `With*` option is written decides on a WIRING fact (a nil handle), never
  on the config**: `TestNoDependencyIsDecidedFromTheConfigAtConstruction`
  sweeps runServe for one that reads `cfg`, `cfgHolder`, `liveCfg()` or a
  live predicate outside the closure it returns. **A `live` row in
  `ops/settings-apply-semantics.md` is a claim about every consumer, and
  `TestMatrixDocMatchesWhatTheHandlerReports` checks only the REPORT**: it
  passed throughout. The consumers are checked by a boot test that flips the
  field through the PATCH and asks each one
  (`TestServeProjectionFollowsTheLiveUpscaleGate`, which answers serve's sox
  probe itself, `serveOpts.soxProbe`, so the switch-on leg means something
  on a host without sox, and checks health agrees before it compares; it
  put a stand-in sox first on PATH, POSIX only, until B105, under Build,
  CI, and test discipline).

- **…and a LIVE reader of the flag alone splits the gate too: every consumer
  that answers "is upscaling on" reads `upscaleActiveFn`, the flag AND a
  usable sox** (2026-09-28). Four read `upscale.enabled` live and without the
  sox half: the console's tile (`Deps.UpscaleStats` / `UpscaleBusy`, and the
  Settings chip that takes its verdict from it), `/v1/upscale/stats`'
  `enabled` (whose PROTOCOL.md row says "matching `/v1/health.upscaleEnabled`"),
  the auto-optimize sweeper and its Jobs card, and `Deps.OptimizeActive`. On
  a bridge without sox, health said off and they said on, and the sweeper
  WORKED on it: its decodability check reads a failed probe as "can decode",
  so it queued every eligible track, each job failed with a WARN and struck
  its file, and the third strike suppresses a file from pre-generation for 30
  days. Measured with the real binary, six hi-res tracks, a 20 s cadence: 18
  jobs, 18 WARNs, all six suppressed within 40 s; restarted WITH sox, every
  sweep still queued nothing (`remaining: 0`, rendered "all caught up") until
  `POST /api/upscale/failures/retry`. A toolchain fault recorded as a fact
  about the files. **The CarPlay kind reads one closure**,
  `carPlayOptimizeActiveFn` (the upscale gate AND the optimize switch):
  `WithCarPlayOptimize`, `Deps.OptimizeActive`
  (`TestConsoleCarPlayGateIsTheV1CarPlayGate`, the only pin possible, since
  both of its readers ask `UpscaleActive` first) and, with the pre-generation
  flag, the sweeper. **A card that reports a switch beside a gate says why
  they differ**: the auto-optimize card's `enabled` is the switches, `active`
  the gate, and `degradedReason: "sox_missing"` the difference, rendered as a
  "degraded" badge, a hint of its own that clears when the probe (30 s TTL)
  finds sox, and "not run" where a refused sweep's `disabled` would read
  "turned off". No restart is needed and none is advised. The settings PATCH
  gives `optimizeEnabled` and `autoOptimizeEnabled` the sox reason
  `upscaleEnabled` had. `TestServeWithoutSoxReportsUpscalingOffOnEverySurface`
  boots the real serve on a PATH with no sox and asks every surface; it and
  the report test were red on the old code, and seven controls each turn red
  only the assertions of the surface they revert. The transcode pool struck
  a file for ANY runner error, a missing sox included, which left a job
  queued inside the probe's TTL after sox disappears; this said "Still open"
  until the next change the same day made a tool the host lacks strike
  nothing and v48 expire the suppressions it had written (the TRANSCODE
  bullet under **Job pools**).

- **…and a surface that reports the gate beside its tool reads both from
  ONE probe: a cache over the shared one is a second clock** (2026-09-28,
  backlog B45). The console's two stats endpoints kept a 30 s cache of the
  sox precheck on top of `soxToolchainCache`, and the /v1 pair's two
  adapters each kept one over a `transcode.PrecheckSox` of their own, so
  `enabled` (the gate, on the shared probe) and `soxAvailable` (another
  cache) answered probes up to 30 s apart. Measured with the real binary,
  the two caches put 20 s out of phase and sox then taken off the PATH:
  `enabled: false` beside `soxAvailable: true` for 14 s on both console
  endpoints; after the fix they moved on the same poll. All three read
  `soxCache.precheck` per snapshot now (the console's `soxAvailability`, the
  adapters' `soxPrecheck`). The per-poll fork CodeRabbit flagged on #110 is
  the shared cache's to cap: a warm read is a mutex and a clock read, and
  the SSE tick already read that cache through the gate.
  `TestServeReadsSoxThroughTheSharedProbe` refuses a sox probe anywhere in
  cmd/bridge outside the shared probe and the CLI's once-per-run preflights
  (`soxProbeCallers`), and requires every precheck wiring to be
  `soxCache.precheck`. **Don't give a surface a cache of its own to save a
  probe: give the shared one to it.** Serve's boot line about a feature
  without sox said "— disabling", which reads as a demotion a restart
  undoes; it says "stays off until sox is installed (no restart needed)".

- **Every subcommand tail resolves its config through `loadCLIConfig`.** The
  `--config` flag defaults to the EMPTY string, so `config.Load(*configPath)`
  resolves nothing and the command dies with `read config ""` on any host where
  the operator did not pass `--config` — while the flag's own help promises the
  `./bridge.yaml`-then-platform fallback only `loadCLIConfig` implements. Two
  shared tails missed the migration and took six commands with them, including
  `bridge token revoke`, the documented orphaned-token recovery path.
  `TestNoSubcommandTailBypassesLoadCLIConfig` now sweeps the package against a
  four-entry allowlist of sites holding an already-resolved concrete path.

- **A GC or reaper whose "in use" set comes back EMPTY must refuse, not
  proceed.** `runArtworkGC` treated an empty referenced set as "everything is an
  orphan" and would unlink the whole cache — permanently for the scanner-written
  `local-<sha256>-500.jpg` covers, which the mtime skip gate stops it from
  regenerating. Its own docblock records this shipping once via a wrong DB path;
  that fix corrected the path and left the shape. Reachable today by a `--config`
  naming another install, or a run between a root flip's `WipeFilesystemTracks`
  and the rescan. An empty set with an EMPTY cache is still a clean exit — check
  the directory, not just the set.

- **`LiveHost` must accept the routing key's spelling.** `upnp_track_routing.server_udn`
  holds `StableServerKey` (lowercased); the SSDP cache is keyed on the raw
  advertised UDN and nothing folds it. An upstream whose UDN carries any
  uppercase character walked, routed and reached the phone — then 503'd
  `upnp_server_offline` on EVERY byte fetch, across `/v1/download`,
  `/dlna/file/{trackID}` and the web player. Exact `Get` first, folded scan as
  fallback; the cache holds single digits of entries, and the fallback measures
  **411 ns / 912 B at five upstreams**, which is noise against proxying a FLAC.
  **Don't memoise it in package-level state** — `runServe` is re-entered
  in-process by the launcher menu, so the memo outlives the cache it describes.

- **An operator-facing promise is a contract with an expiry date.** The uninstall
  prompt told operators the bridge "has no code path that can delete `--library`
  files (read-only by design)" — true when written, false since delete-as-trash
  landed. Scope such a promise to the COMMAND, not the product. Guard it by
  driving the real function (`actUninstall` with a buffer), never by scanning the
  source: this package's commentary names what it discusses, so a text scan
  reports its own docblock.

- **One `bridge serve` per data dir, decided before its first write: runServe
  takes an exclusive kernel lock on `<dataDir>/server.lock`
  (`lockServeDataDir`, over `fsutil.TryLock`) right after the read-only config
  checks, ahead of the TLS mint, the token store, `server.pid`, the
  migrations, the updater's boot marker and every sweeper** (2026-10-02,
  backlog B208). A second serve of a live data dir ran all its wiring and
  failed only at the bind. Measured with two runServes on one data dir: its
  batch coordinator's `RecoverInterruptedBatches` marked the live bridge's
  running batch `interrupted` for good, its exit removed the live bridge's
  `server.pid` (so `bridge doctor` then FAILed the live bridge's own ports)
  and rewrote `tokens.json`, and a second serve on ports of its own (`--addr`,
  another config naming the data dir) never failed at all: two bridges served
  one database. Now it exits 1 at once, naming the data dir and the pid
  `server.pid` records, having written nothing in the data dir. **The lock
  needs the data dir, which comes from the config**, so the one write that
  can come before it is `--init-if-missing`'s seed config, made only where
  no config file exists, which is never the config a live bridge loaded;
  building the seed in memory to lock first would copy `Load`'s env and
  path resolution (declined on #1140). **Only a lock another serve
  HOLDS refuses**: any other failure (a filesystem that keeps no locks answers
  ENOLCK or EOPNOTSUPP; a lock file this user cannot open) prints one line and
  serves without the check, since a lock that cannot be taken must not stop a
  bridge from starting. **A kernel lock, not a lock FILE**: the kernel drops it
  with the process however it exits, a SIGKILL included (measured on macOS,
  Linux and Windows), so the stale-lockfile objection the next bullet records
  is not about it. The file stays: never unlink a held lock file, or a later
  serve locks a new file at the same path beside the old holder. **flock and
  LockFileEx, never fcntl**: the lock belongs to the OPEN FILE, so a second
  runServe in the same process (the launcher menu, every serve test) is
  refused as well, where fcntl's per-process record locks would let it
  through. **Released by runServe's own defer, after every teardown registered
  below it** (the writer joins, the store close, the pid file's removal), and
  never left to the `*os.File`'s finalizer, which closes a file nothing
  references, and so releases its lock, at whatever collection finds it while
  the serve still serves (`TestServeFreesTheDataDirWhenItReturns` runs with
  automatic collection off for that reason). On Windows the release unlocks
  before it closes (a close's release may lag), and the locked range refuses
  other handles' reads, so nothing reads `server.lock`. **The lock file opens
  through the data dir's `os.Root`**: `Precreate`'s O_EXCL refuses a link at
  that path and a plain open then follows it, so a serve run as root would
  create or lock whatever file a planted link names, as root; through the
  root a link out of the data dir is an error (warned, not a refusal).
  **Not the listeners
  first**: binding before the wiring would refuse nothing on ports of its own.
  And **`server.pid` is removed only while it names this process**
  (`removeServerPIDFile`), for a serve that ran without the lock. Tests:
  `TestASecondServeOfALiveDataDirChangesNothing` (both shapes, red on main on
  every check), `TestServeStartsWhereTheDataDirCannotBeLocked`,
  `TestServeFreesTheDataDirWhenItReturns`,
  `TestRemoveServerPIDFileKeepsAnotherProcesssRecord`,
  `TestServeLockFileKeepsTheInstallOwnerAsRoot` (as root, on dido: a lock
  file `sudo bridge serve` left root's would lock the service user out of
  the check), `TestServeLockFileThatLinksOutOfTheDataDirIsNotFollowed`, and
  fsutil's `TestTryLockRefusesASecondOpenOfTheFile` and
  `TestALockDiesWithTheProcessThatHeldIt`.

- **A lock file is opened through `fsutil.OpenLockFile`, which asks again
  when its create loses a race** (2026-10-14, backlog B185). On macOS 27 an
  openat(2) with O_CREAT that loses a concurrent create of the same name
  answers ENOENT rather than opening the file the winner made: 596 of 900
  opens by three concurrent openers on 27.0.1, with or without O_NOFOLLOW,
  while open(2) on the whole path never did, and every retry opened the
  file. `lockServeDataDir` took that ENOENT for a data dir that cannot be
  locked, so of two serves started at once on a fresh data dir the second
  went on unlocked beside the first; the store's migration lock lost the
  same race. **Don't open a lock file with a bare
  `os.Root.OpenFile(name, O_CREATE)`** (`TestOpenLockFileOpensForEveryConcurrentOpener`,
  `TestServesLockingAFreshDataDirAtOnceTakeItOnce`, both red on macOS 27.0.1
  before the retry).

- **`bridge restore` and `bridge manifest clear-missing` hold the data-dir
  lock for the whole run.** Both mutate the store from a second process,
  where `Store.mu` does not reach and `busy_timeout` is a retry rather than
  a serializer. Each takes `lockServeDataDir` (the kernel lock `bridge
  serve` holds, the bullet above) before it reads the store or writes, and
  releases it only after the last write, so a serve started during the
  confirmation prompt, or one whose admin address is ephemeral, is refused:
  the lock does not depend on the admin port. A `*serveDataDirHeldError` is
  the refusal, and the message is the lock's own, which names the pid
  `server.pid` records when it records one. Any other error from the lock
  means it could not be taken (a filesystem that keeps no locks, a
  permissions error, a directory where the lock file goes); those commands
  then keep the admin-port probe, both times, and its message. The lock
  is the guarantee between these commands and a serve. The probe stays as
  a second guard against a serve that continued after a lock error other
  than contention and therefore holds none: when the lock was taken and
  the admin port is fixed, both probes still run and a bridge that answers
  is refused with the probe's message
  (`TestARestoreOverAnUnlockedServeRefusesWithTheProbe`). An ephemeral
  admin port cannot be probed; the lock alone decides there
  (`TestAFreeLockLetsRestoreProceedOnAnEphemeralAdminPort`). A killed
  holder does not block the next restore: the kernel drops the lock with
  the process (`TestAKilledDataDirHolderDoesNotBlockRestore`, and
  `TestALockDiesWithTheProcessThatHeldIt`). This bullet said until
  2026-10-12 that the two commands only probed the admin port, and until
  2026-10-02 that an interprocess lock was declined because a stale lockfile
  after an unclean exit blocks `restore` at exactly the moment an operator
  needs `restore`: true of a lock FILE, and not of this kernel lock.

- **A snapshot captures the console account and the harvest state, and
  restore brings the account back signed out** (2026-10-13, backlog
  B200). The bundle was `bridge.db`, `tokens.json`, the TLS pair and
  `bridge.yaml`. A public console's `adminauth.json` (username, bcrypt
  hash, `passwordChangedAt`) lived only in the data dir, so a restored
  bridge had no account and the operator set the password again.
  `atlas-harvest.json` is the other file of that shape: the harvest
  token and the result cursor, last submit, booklet check and pending
  covers (`atlasharvest.State`). Harvested rows are already in the
  manifest database. Without the file the restored bridge still serves
  them, and the harvest waits until the app provisions a credential
  again and the cursor starts over. The file is small, secret-grade
  like the token store, and is not re-derived, so the snapshot copies
  it byte for byte. Both files are optional: a missing source is
  skipped (`os.ErrNotExist`), an older snapshot whose `Files` list
  omits one restores exactly as it did, and a loopback install with no
  account is unchanged. Schema stays 1.
  **Sessions are not restored.** `load` keeps every session the file
  still holds and treats a copied `sessionsRevokedAt` as already seen,
  so a byte copy of a signed-in console signs the restored host in.
  Restore rewrites the file once, through `adminauth.SignedOutFile`:
  the account stays, `sessions` is omitted, and the marker moves with
  `nextSignOut` (now, or one nanosecond past the stored marker when the
  clock does not). That is the store's own sign-out. A file with no
  account is copied unchanged. A file that does not parse fails the
  restore before ANY file is written, so a live account is not replaced
  and a missing one is not created. **Sign the account out before the
  loop, never at its turn in it**: the manifest lists it after
  `bridge.db`, the token store, the TLS pair and `bridge.yaml`, and a
  check at its turn left a half-restored install. Login tickets stay
  out of the bundle. Both files are written at `0600` by the same
  `writeFrom` the other copies use (`fsutil.KeepOwner`, one chown).
  **`writeFrom` streams an `io.Reader`: never read a copy's source
  whole** — Restore copies `bridge.db` through it, and a whole read held
  the database in memory. The CLI, the serve ticker and the console
  snapshot all name them through `buildBackupSources` and
  `buildRestoreTargets`.
  (`TestRestoreBringsTheConsoleAccountBackSignedOut`,
  `TestRestoreOfASnapshotWithoutTheConsoleAccountLeavesItAlone`,
  `TestRestoreRoundTripsTheHarvestState`,
  `TestSnapshotSkipsAMissingConsoleAccountAndHarvestState`,
  `TestRestoreRefusesAConsoleAccountThatDoesNotParse`,
  `TestRestoreRefusesAnUnparseableConsoleAccountBeforeWritingAnyFile`,
  `TestSignedOutFileKeepsTheAccountAndDropsSessions`,
  `TestBuildBackupSourcesNamesTheConsoleAccountAndHarvestState`,
  `TestSnapshotAndRestoreAsRootKeepTheConsoleAccountAndHarvestState`,
  `TestCopyFileStreamsRatherThanReadingTheFileWhole`.)

- **A snapshot captures the player's identity, and restore gives the box
  the snapshot's: read by the player's rule before any file is written,
  and written by the identity file's own writer** (2026-10-22, backlog
  B505). `<dataDir>/player-uuid` is the player's identity to a controller
  (PR 5a makes it the OpenHome device's UDN), and the bundle did not hold
  it, so a restored box kept the identity it had, or had none and made a
  new one: every controller saw a new player. `Sources.PlayerUUID` and
  `Targets.PlayerUUID` name it, through `buildBackupSources` and
  `buildRestoreTargets`. `Snapshot` copies it as stored, 0600, whatever it
  holds (a backup that refused a damaged identity would stop every
  backup), and skips a box with none. **The snapshot's identity wins over
  the box's**: a restore makes the box the bridge the snapshot was taken
  from, its database, tokens, TLS pair and config included, and that
  bridge's player is the device its controllers know by that UUID. A
  snapshot without the file, taken before B505 or of a box whose player
  had none, leaves the box's identity as it is and makes none. `Restore`
  reads the copy before its loop, by the player's own rule
  (`config.ReadPlayerUUIDFile`: one lowercase UUID, white space after it
  and nothing else, 128 bytes at most), for the console account's
  reason: the manifest lists the file last, and a check at its turn would
  come after every other file was replaced. A copy that does not hold one
  UUID fails the restore before any file is written; the refusal names
  the copy and says to remove it from the snapshot, which restores the
  rest and leaves the box's identity as it is (a copy the snapshot lists
  and no longer holds is skipped). The refusal tests restore the database
  too and watch its -wal and -shm, and the CLI's a track added after the
  snapshot: a check moved after the database step, the first file a
  restore writes, passed tests that restored no database (round 2,
  Codex). The UUID is written by `config.WritePlayerUUIDFile`, the writer
  `EnsurePlayerUUID` uses: the UUID and a newline, 0600 whatever the
  copy's mode, one `KeepOwner`. Schema stays 1. **Don't restore
  `player-uuid` as bytes, and don't let a copy that does not hold one UUID
  reach the loop.**
  (`TestBackupAndRestoreGiveThePlayerTheIdentityItHad`,
  `TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo`,
  `TestSnapshotCarriesThePlayerUUIDAndRestoreWritesItAsTheIdentityFile`,
  `TestSnapshotSkipsAPlayerUUIDTheBoxDoesNotHave`,
  `TestRestoreOfASnapshotWithoutThePlayerUUIDLeavesTheIdentityAlone`,
  `TestRestoreRefusesAPlayerUUIDThatIsNotOneUUIDBeforeWritingAnyFile`,
  `TestSnapshotAndRestoreAsRootKeepThePlayerUUID`,
  `TestBuildBackupSourcesNamesThePlayerUUID`.)

- **`probeBridge` cannot answer for an ephemeral admin port, and must say so.**
  It fails closed on anything but connection-refused, which is right; but
  `adminAddress: …:0` names no port to dial, so the default produced "a bridge
  is answering on 127.0.0.1:0" about an address where nothing can. Refuse with
  the true reason. **PARSE the port** — a text compare against `"0"` accepts
  `"00"`, and `validatePort` runs `Atoi`, so every spelling of zero is legal.

- **The positional-scope class covers `--path`, not just `--filter`.** It was
  closed twice without being swept: #856 fixed `enrichment retry`, #882 fixed the
  four `--filter` commands, and `bridge duplicates` / `bridge enrichment misses`
  still parsed with `--path` empty — where an empty path scope emits a query with
  no `WHERE` clause at all, so a full-table answer comes back presented as the
  scoped one. Read-only, which is exactly why it survived both passes. Grep for
  the PATTERN (a flag whose empty value means everything), not the symptom.

- **A `stopFn` that signals is not a join.** `internal/integrity`'s watchers
  closed a channel and returned, while `runServe` defers that stop ahead of
  `Store.Close()` — an ordering that means nothing unless the stop waits. Both
  loops now join, grace-bounded. **And the work has to be cancellable for the
  wait to mean anything**: the adapters passed `context.Background()`, so the
  wait would have delayed `Store.Close()` behind work that was never going to
  stop.

- **A cancel has to reach the process that WRITES, and `exec.CommandContext`
  reaches only its direct child.** The standalone macOS Tailscale app's CLI
  helper at `/usr/local/bin/tailscale` is a shell script that runs the app
  WITHOUT `exec`, so the kill landed on `/bin/sh` and the app, an orphan now,
  wrote `<dataDir>/tls` after `runServe` had returned. On a Mac running
  tailscaled, `TestServeWiresResolvedConfigPathIntoAdminAndBackups` failed 6
  of 116 runs in one afternoon (`TempDir RemoveAll cleanup: unlinkat
  …/data/tls: directory not empty`). CI has no tailscaled and never saw it.
  Every CLI call in `internal/tailscale` runs in its own process group now,
  and `stopTreeOnCancel` kills the group. **Neither the group kill nor the
  join is enough alone.** On macOS a child forked while the signal is
  delivered can miss it (45 of 4,830 cancels in a probe), and it holds the
  output pipes, so `Wait` returns only once it has exited (8 of 8 escapes),
  and the auto-pilot joined on `bgWriters` with it. **Never set `Cmd.WaitDelay` there**: it unblocks
  `Wait` by closing those pipes, which abandons exactly the process the join
  exists to outlast. `TestServeLeavesNoTailscaleCLIRunning` drives a
  wrapper-shaped fake through the real exec path and pins the kill;
  `TestServeWaitsForAnInFlightTailscaleMint` holds a mint that ignores its
  context and pins the join. Neither sees the other's defect. (#997) The
  first, like `TestCancelStopsTheWholeCLIProcessTree`, asks whether the CLI
  has EXITED (`proctest.Exited`), not whether kill(pid, 0) still finds it,
  which a zombie is (#1024, under Build, CI, and test discipline).

- **A cancelled pass is not a failed one, and a join makes the difference
  visible.** `detectAndMint`'s context is cancelled by shutdown, by
  `Disable()` and by an admin client leaving mid-"Re-mint now" (RefreshNow
  runs on the request's context). `passCancelled` returns the snapshot it
  found and logs and publishes nothing, because the Detect error branch
  UNLOADS the LE cert and the MagicDNS suffix: a cancel read as a failure left
  every `*.ts.net` client on the self-signed cert until the next good pass,
  up to a day later. A DEADLINE is still a failure. **Only an ERROR the
  cancel caused is quiet**: a call that COMPLETED reports a fact (exec
  answers success only when the process finished on its own), and the pass
  applies it. Re-checking the context after a success was proposed in
  review and declined, because it would discard a completed "Re-mint now";
  `TestACompletedTailscaleCallIsAppliedAfterACancel` pins it. Joining a
  writer makes its cancelled exit path run before shutdown completes, so
  check what that path reports and what it changes. (#997)

- **…and the backup ticker applies the rule by asking the ERROR too,
  because one error can carry both** (#998). A shutdown during the startup
  snapshot (a `VACUUM INTO`, long on a big library) printed `backup
  (startup): snapshot failed: vacuum manifest db: context canceled`, and one
  during the prune printed two more lines about a pass that failed at
  nothing. `ctxerr.WithoutCancellation(ctx, err)` (`internal/ctxerr`, so
  every loop can ask it; it was `withoutCancellation` in `cmd/bridge` until
  the follow-up below) returns err with ctx's cancellation taken out, and
  the ticker reports what is left. Both of its
  conditions are load-bearing: ctx is CANCELLED (a deadline is a failure),
  and the error IS that cancellation (a snapshot's file copies do not watch
  ctx, so an I/O error that lands during a cancelled pass is a failure). An
  `errors.Join` is filtered child by child: `PruneContext` and its orphan
  sweep keep going past a directory they cannot remove or read, then join
  those failures with `ctx.Err()` when a later cancel stops them, so
  `errors.Is` on the whole error would silence the failures with the cancel.
  A join under a `%w` WRAPPER is reported whole, cancellation text
  included, since the wrapper cannot be rebuilt around what is left without
  losing its context (Gemini's suggestion returned the filtered inner error
  and compared errors with `==`, a runtime panic on an uncomparable type).
  A stopped snapshot leaves nothing to report: `Snapshot` removes its
  partial directory, destination file included
  (`TestSnapshotStoppedMidVacuumLeavesNothing` cancels INSIDE the running
  VACUUM), so the backups directory is as the pass found it. The run state
  keeps `sweepFinished(nil)`, the recorder's "no new counts" for a failed
  pass and a stopped one alike, because `running` must clear. **This did
  not close the class**: a survey the same day found about 33 more log sites
  that report a shutdown cancel as a failure (scanner batch writes,
  enricher, updater poll, harvest `tick_error`, fingerprint and smart-mix
  sweeps, integrity watchers, UPnP ingest, tsnet). Each subsystem it named
  has had its change since: the fingerprint and smart-mix sweeps (#999),
  the scanner's writes (#1000), the enricher (#1001), the updater (#1002),
  the harvest tick (#1003), the integrity watchers (#1004), tsnet's start
  and status query, UPnP ingest and the premium-cover record (#1005), and
  the tsnet goroutine's binds and HTTP/3 serves (#1009, the bullet after
  next). The rule #999–#1005 follow is the next bullet. A site is
  covered only once its own change has landed, so check the code
  before assuming one outside those is. (Until #1009 this still named
  only #999's two, through the six changes that followed it.)

- **Every loop `runServe` starts reports a shutdown as a STOP, and a log
  site in one asks `ctxerr.WithoutCancellation` with the context its PASS
  runs on** (#999, #1000–#1005). The scanner, enricher, updater, harvest
  client, integrity watchers, fingerprint and smart-mix sweeps, tsnet and
  UPnP ingest all do. The PASS's context, because a timeout derived from it
  is a failure: a harvest request that runs out of its own time while the
  loop is live is reported (`TestATickWhoseRequestTimesOutStillReportsIt`),
  and for the updater's "Check now" the pass IS the request. Sites that
  check only `ctx.Err() == nil` (the scanner's lookups, the enricher's
  searches) are the older, looser form and were left: each is a read the
  next pass redoes. That looser form is RIGHT only where the error cannot
  carry the cancellation: a tsnet node the shutdown closed under
  `ListenTLS` fails with the node's own error, so `tsnetListen` asks the
  context alone. Where the stop leaves its OWN mark on the error, ask the
  error and not the context: both discovery clients' M-SEARCH sends drop
  `net.ErrClosed`, which only each client's `Stop`'s close produces
  (`discovery.SendFailureLog`, the advertiser's NOTIFY burst likewise), and
  still report a genuine failure that lands during the stop (2026-09-28,
  under DLNA, UPnP and discovery). **A stopped pass reports no failure the stop caused, and
  records no verdict, count or status for the work the stop
  interrupted.** A `ctxerr` site still reports any other failure, even
  one that lands during the shutdown (#998's second condition). The
  enricher's fetches absorb their own errors, so a cancelled portrait
  search used to reach `markSkipped`, which counted `no_mb_match` and
  logged `enrichment skipped` for a verdict it never wrote. It now stops
  at `stampEnriched` / `markSkipped`, where every path converges, never at
  the five absorb sites (a sixth would be the one that forgets), and the
  row stays `enriched_at = 0`. The updater's `LastError` and the console's
  UPnP "Rescan now" keep the last result that ANSWERED, and a premium
  cover whose version record a shutdown stopped stays PENDING, so the next
  pass records it. What a pass DID before the stop is still summarised,
  marked `cancelled` (the integrity sweeps; an orphan walk that did not
  finish is `tick cut short`, never `tick complete`). A shutdown during
  `runServe`'s STARTUP is a stop too: the first-run upscale seed, the one
  startup step that runs on the serve context and exits 1, exits 0 when
  the shutdown lands in it. **The scanner's final flush DROPS its batch on
  a cancel, deliberately**: the skip gate compares each file with its
  STORED row, which the dropped write never touched, so the next scan
  redoes exactly what was dropped. Three traps. A cancellation error from
  an AUTOCOMMIT statement does not prove the write did not land (modernc
  answers `ctx.Err()` whenever its interrupt fired, a committed statement
  included). database/sql can answer `sql: statement is closed` in place
  of the cancellation (1 in 3,000 measured), which is reported rather than
  matched by its text. And a context cancelled BEFORE the call can pass a
  test without reaching the site at all: the seed's `GetUpscaleTarget`
  failed on it first and the seed was skipped, so the test was green on
  the unfixed code. Cancel INSIDE the call the test names
  (`internal/sqlitetest`'s collation over the column a write moves, or a
  fake upstream that cancels and holds), assert that the cancel happened
  there (`mustHaveReachedGitHub`), and give every stopped test a twin
  failing on a LIVE context: without one, the site's "never log" control
  cannot bite, which happened twice here. Joining the tsnet start
  goroutine was #997's class, not this rule's; #1009 did it (next
  bullet).

- **A goroutine serve starts is stopped by a context the TEARDOWN
  cancels, and joined before anything it uses is closed** (#1009).
  runServe ran the embedded tsnet node's start (up to five minutes,
  interactive auth included), its HTTP/3 binds and its HTTPS listen on
  one goroutine it never joined, on serve's context, and its deferred
  teardown closed the node, which upstream forbids "before or
  concurrently with Start". **serve's context is not the teardown's**:
  runServe's own cancel is its FIRST defer, so it runs LAST, and on an
  error exit (the admin console cannot bind, say) the whole teardown runs
  with serve's context live. A start waiting for auth went on waiting,
  and a listen that landed after the close was published and served on a
  node that was gone (CodeRabbit on #1005). `tsnetFront`
  (tsnet_bringup.go) runs the goroutine on a context of its own, and its
  `stop` (the shutdown branch's since #1019, deferred for every other
  exit) cancels it, refuses publication and takes what was
  published in ONE critical section, drains that, waits for the goroutine
  and every HTTP/3 Serve it started (drains and wait share one grace;
  running out costs a line), and only then closes the node. **A server is
  served only once it is published**: starting each HTTP/3 server as it
  bound let a shutdown mid-bind close the node under one nothing could
  see, which then reported `h3 serve tsnet` with quic's `transport
  closed`. The wrapper's half: a `Close` that lands during `Start` marks
  the server closed and cancels that start WITHOUT waiting (upstream's
  own start takes no context), and the start closes the node it built
  instead of publishing it; after Close, or on a context already done,
  Start builds nothing. **A drain is a wait too, and stop's is bounded**:
  quic-go runs `ServeHTTP` inside the wait `http3.Server.Shutdown` makes
  past its deadline (it calls `Close`, which waits for every connection's
  handling), so an HTTP/3 handler that ignores its context (a read from a
  hung mount) held an unbounded drain, and the exit, for as long as it
  blocked (CodeRabbit on #1009). `stop` is therefore the ONLY drainer of
  the tailnet side: the shutdown branch's early drain of the tailnet
  HTTP/3 servers was the same wait, ahead of stop's. The LAN HTTP/3 drains
  had the same shape (next bullet). **Hold the window, not its
  aftermath**: a first draft of the listener test released the listen
  after runServe had returned, and PASSED on the unfixed code, because
  runServe's final cancel had run by then and #1005's post-check caught
  it. The window is between the node's close and that cancel, and the
  test holds serve there on its `tsnet close:` print. The publication
  gate has no seam a boot test can hold, so it is driven directly.

- **An HTTP/3 drain is bounded, drained ONCE, and its socket closed a
  moment AFTER the grace, never at it** (#1010). Both LAN HTTP/3 drains,
  the shutdown branch's (beside HTTPS) and the defer every other exit
  takes, waited for `http3.Server.Shutdown` with no bound, so a LAN
  HTTP/3 handler that ignored its context held serve's exit for as long
  as it blocked: the previous bullet's defect, on the LAN. `lanHTTP3`
  (lan_http3.go) owns the server, its socket and its one drain: `stop`
  waits out the grace and `http3ForceCloseAllowance` (1 s) past it,
  prints a line if the drain is still running, and closes the socket
  under it. **Once, because a second drain is a second wait**: the
  `Close` that `Shutdown` calls past its deadline holds the server's
  mutex while it waits for the handlers, so the defer's second
  `Shutdown`, after a bounded branch, blocked on that mutex for as long
  as the first. The comment it replaced called the two calls idempotent,
  which held only for a drain that finished. **The allowance, because
  `Close` writes each connection's CONNECTION_CLOSE BEFORE it waits for
  the handlers**: a socket closed the moment the grace ran out lost the
  client's close 20 of 20 times on macOS and on Linux, whether the
  connection was idle, streaming or held, and the client learned of it
  only from its idle timeout (quic-go's default is 30 s). The writes land
  within 4 ms of the deadline. #1009's `stop` closed the tailnet node at
  exactly that moment, so it takes the same allowance (`drainedWithin`).
  **Bound a wait by what the thing you close is still doing, not by the
  deadline alone.** `stop` closes the socket itself, not the drain's
  goroutine when it returns: a held handler would keep the port bound,
  and the launcher menu's next start would fall back to HTTP/2.
  `Shutdown`'s error is printed only once it has returned, so nothing
  reaches stderr after runServe has.   The boot tests hold a LAN request
  through `serveOpts.wrapAPIHandler`, and hand serve a TCP listener and
  a UDP socket on one port (`holdLoopbackTCPAndUDP`), because serve
  prints no UDP address.

- **An event stream ends when shutdown starts, and an ordinary request
  keeps the grace** (2026-10-08, backlog B216). `http.Server.Shutdown`
  closes idle connections and does not cancel a handler.
  `GET /v1/events` and `GET /v1/pairing/{id}/events` select on the
  request context and on the serve context the stop cancels before it
  drains (`Server.EndEventStreamsWhen`). Since sync phase 2 every
  foreground phone holds one of the first, so every stop ran the whole
  5 s grace and logged `http shutdown: context deadline exceeded`.
  HTTP/3 did the same: quic-go's `Shutdown` stops new streams and does
  not cancel one already running, and the drain logged
  `lan h3 shutdown: context deadline exceeded`. Measured on a real
  `serve` with both protocols held: before, the HTTP/3 handler returned
  at 5002 ms while the client was still reading at 8 s, the HTTP/2
  streams were still open at 8 s, and both deadline lines printed.
  After the streams selected on the serve context, each ended with a nil
  read error in the same millisecond the stop ran, and neither deadline
  line printed. The first wiring also parented every API request on
  that context, and with that parent `serve` returned 11 ms later. That
  parent cancelled a manifest page, a favorites write and an artwork
  read the moment the stop began. The streams select on the context
  themselves, set once before the listeners accept. Ordinary requests
  keep the context net/http gives them and finish inside the grace
  (`TestAnOrdinaryRequestInFlightAtShutdownCompletes`: 200, and the
  request context is still live). With the parent gone, `serve` returns
  in about a second, inside the grace: the HTTP/2 GOAWAY wait on a
  connection the parent used to abort. The handler returns the 200 it already
  flushed. The phone reads that as a finished stream and reconnects. A
  404 is the demo fallback and is not this path. **A download is left
  alone**: `ServeContent` does not read the request context, so an
  in-flight file still occupies the grace, which is what the grace is
  for. The console already parented its own server
  (`admin.Server.Serve`'s `BaseContext`), which is what ends
  `GET /api/events`, and that parent was left as it was. `Connection:
  keep-alive` is sent only when `ProtoMajor < 2`
  (`setEventStreamHeaders`): HTTP/3 forbids that field name, and a
  client that enforces it dropped the stream before a shutdown could be
  measured. Removing the serve-context select turns
  `TestEventStreamsEndWhenServeShutsDown` red again (both deadline
  lines, streams still open at 8 s). Putting `BaseContext` or
  `ConnContext` back on a production API server turns
  `TestAnOrdinaryRequestInFlightAtShutdownCompletes` red, because the
  request context is cancelled, and
  `TestAPIServersLeaveOrdinaryRequestsTheirOwnContext` red with it.

- **On a shutdown the LAN and the tailnet drain TOGETHER, under one
  grace** (#1019). The shutdown branch drained the LAN servers and
  returned, and only then did the deferred `tsnetFront.stop` begin on
  the tailnet's. With an HTTP/3 request held on each side, SIGINT to
  exit measured 12.0 s. With a client on each side keeping its
  connection open past GOAWAY it measured 10.0 s, and the tailnet served
  a new request one second after SIGINT. **`docker stop` sends SIGKILL
  after 10 s**, and tsnet is how the image joins a tailnet, so both cases
  were killed mid-drain, before the node's close, the writer join and
  `Store.Close`. The branch now starts `stop` beside the LAN drains on
  the SAME context: 6.0 s and 5.0 s. **A teardown called from two places
  stops once.** The defer stays for the error exits, so `stop` runs under
  a `sync.Once`, as `lanHTTP3.stop` does. Without it the deferred call
  drained again, on the mutex of an HTTP/3 server still in Close (a
  second grace and a second line), and closed the node a second time.
  `stop` takes the drain's context, and the defer builds one when it
  RUNS, never when it is registered. quic-go's own client closes an idle
  connection on GOAWAY, so the 10 s case needs a client that does not.
  `TestServeShutdownDrainsTheTailnetBesideTheLAN` pins the ORDER, not a
  duration: it holds the LAN drain's give-up line and requires the
  tailnet's HTTPS listener to be closed already, which the old order
  cannot do on any host. The error exits still drain one after another.

- **A snapshot a shutdown cancels STOPS, and `VACUUM INTO` went on past its
  cancel in three places** (2026-09-29, backlog B63). modernc stops a
  running statement with `sqlite3_interrupt`, which SQLite reads only
  between the steps of a statement that is running. On CI's Windows leg a
  serve test's startup snapshot still held `data/backups/<stamp>/bridge.db`
  after `runServe` returned: the `bgWriters` join had given up on it, as
  it must (grace-bounded), and the TempDir cleanup met the open file.
  **The commit reads no interrupt**, and VACUUM INTO builds its output in
  a page cache the size of its source connection's (2 MB by default), so
  what is still cached at the end is written by the commit: a database
  under 2 MB was copied whole there. Of 3,818 cancels that landed while
  the statement ran (an 820 KB database, dev Mac), 2,053 finished the
  copy anyway, and on nomos under disk contention the commit spent 2.3 s
  writing a 316 KB copy after its cancel, which the driver then answered
  as `context.Canceled` and the snapshot threw away. `cache_size(-64)` on
  the snapshot's source connection (`snapshotSourceQuery`) makes the copy
  write as it goes and leaves the commit 64 KB (220 of 3,722; a 118 MB
  snapshot took 326 to 698 ms whatever the cache). **A busy handler
  sleeps through it**: `busy_timeout(5000)` answered a cancel 300 ms into
  a lock wait 5 s later, with `database is locked`. The source takes
  `busy_timeout(100)` and `retryWhileBusy` waits out a longer lock between
  attempts, on the context, for `snapshotBusyPatience` (5 s) in all:
  0.11 s now. **A cancel before the statement starts is cleared by it**
  (`sqlite3Step` resets the flag when no other statement is active, and
  the driver checks the context before it arms the interrupt): 2 of
  4,000 random cancels, each a whole copy. The INTO target is a Go SQL
  function (`vacuumTarget`), which SQLite evaluates once the statement
  runs and before `OP_Vacuum` copies anything, and which refuses a
  cancelled snapshot. What is left of the statement is a cancel in the
  commit's last 64 KB, which keeps the output file open until the
  statement returns (the next bullet). **Don't raise the source's cache,
  don't give the snapshot SQLite's busy wait back, and don't Ping before
  the VACUUM** (a Ping is a statement that waits on the lock too, and the
  first attempt is the probe). Any statement that must stop on a cancel
  meets the same three.
  `TestASnapshotCancelledAsItsVacuumStartsCopiesNothing`,
  `TestASnapshotWaitingOnALockedSourceStopsForItsCancel` (beside its
  control, `TestASnapshotStillWaitsOutABriefLock`) and
  `TestASnapshotWritesItsCopyWhileItCopies` were each red on the old code,
  and each turns red alone when its fix is taken out.

- **Shutdown waits for the backup ticker to close its snapshot file, past
  the shared writer grace** (2026-10-09, backlog B309). The ticker is
  joined on `bgWriters`, and that join gives up after `shutdownGrace`
  (5 s). A startup snapshot is `VACUUM INTO` on the serve context. A
  cancel in the commit's last 64 KB finishes the copy, and the connection
  holds `data/backups/<stamp>/bridge.db` until the statement returns and
  `Close` runs. B216 shortened the HTTP drain from a full grace (an event
  stream held `Shutdown`) to about a second, so the overlap in which that
  commit could finish before serve returned shrank with it. On the Windows
  leg of #1166 (run 37847678515) `TestServeBakesHealthEndpointsIntoThePairingQR`
  and `TestServeProjectionFollowsTheLiveUpscaleGate` then failed TempDir
  cleanup: `unlinkat …\data\backups\2026-10-08T21-43-21Z\bridge.db: The
  process cannot access the file because it is being used by another
  process`. The same evening that leg passed twice on main. macOS deletes
  an open file, so the TempDir cleanup there says nothing.
  `backupShutdownWait` (45 s) is a defer registered after the shared grace
  wait, so on the way out it runs first. **It cancels `scanCtx` itself
  before it waits** (backlog B311): on a stop the parent cancel has already
  cancelled the ticker, but on an early error return (the admin port in
  use) nothing has, and `defer scanCancel()` runs only after this defer.
  Without that cancel every early exit sat out the whole 45 s (bridge
  #1166's CI, run 37865927261, a port collision in
  `TestServeRedeemsThePairingLinksCode`). **Don't drop it.** The wait
  returns when the goroutine does, which is after `db.Close`. A wait that expires logs `shutdown: backup snapshot
  did not close its files within …` and teardown continues; the other
  writers keep the 5 s grace. `vacuumInto` also closes its connection
  before it removes a partial destination: the deferred `Close` used to
  run after `os.Remove`, and on Windows that remove fails while the handle
  is open. `TestAShutdownWaitsForTheStartupSnapshotToCloseItsFile` parks
  the startup VACUUM (`backuptest.WriteSource` before serve,
  `sqlitetest.ArmUntil`), requires this process still has the snapshot
  `bridge.db` open (device and inode of each `/dev/fd` name on macOS,
  `/proc/self/fd` on Linux, an exclusive open on Windows; names only,
  because `os.ReadDir` of `/dev/fd` fails the call with `fstatat`'s
  EBADF when a descriptor closes between the listing and its stat),
  cancels, and requires serve still running
  25 s later — past the drain, the shared grace and the store close's 5 s
  busy wait. With the wait removed, the same test exited 0 in 5.16 s
  while that file was open and logged `background writers did not drain
  within grace`.

- **Anything reading Go source in a test must normalize CRLF first.** No
  `.gitattributes` pins `eol`, so a Windows checkout has CRLF and every
  `\n`-literal scan finds nothing. One such guard failed loudly on the Windows
  leg; its sibling would have passed VACUOUSLY, which is worse. This rule was
  already written under **Build, CI, and test discipline** and was still tripped
  by a session that had read it — the platform leg is what closes that gap.

- **`bridge init`'s preflight grades the install that is THERE, and
  `ensureDoctorClean` prints warns.** init is re-run far more often than it is
  run (service reinstall, config rewrite, data dir moved to a new host) and it
  built `doctor.Deps` from its prompts alone — so the cert checks graded
  `<cfgDir>/data/server.{crt,key}` rather than `cfg.TLSCertPath`, and
  `tls-cert-sans` skipped itself, a nil `CertSANs` being a silent ok. Both are
  wired from a config readable at the TARGET path (`withExistingInstallDeps`,
  #963's name for it, which this bullet gave as `withExistingInstallCertDeps`
  until 2026-09-27); a first install keeps the skip, because a
  narrower want-set than `bridge serve` builds would be a comparison presented
  as authoritative that nobody made. Grading the PRE-init state is the point,
  not a compromise: the cert on disk is the pair a rewrite keeps, and a
  loopback rewrite of a loopback install keeps `customEndpoints`, so there
  the old want-set IS the new one. **This bullet said every rewrite kept
  them until 2026-09-27, and none did** (the bullet on what a rewrite keeps):
  a `--public` rewrite writes the domain's and a posture change starts over,
  so for those two the pre-init grade is about the list being replaced, and
  `bridge doctor` after the run grades the saved one. **The warn printing
  is what makes any of it reach an operator** — every verdict these two checks
  give about that state is warn-level by design (neither is a reason to refuse
  to initialise), and `ensureDoctorClean` printed only on a FAIL, so a check
  wired into the preflight would compute a finding and discard it. Warn-lines
  only, in `printReport`'s layout, nothing at all on a clean host.

- **`bridge doctor` finds its config through `resolveConfigPath` (explicit,
  then `./bridge.yaml`, then the platform dir) and loads only a path it
  FOUND.** A missing config is not an error there, because doctor runs before
  `bridge init`, which is why it is not `loadCLIConfig`. Until #985 it tried
  the explicit path or the platform path and nothing between, so in the image
  (WORKDIR `/data`, config `/data/bridge.yaml`) a plain `docker exec … bridge
  doctor` graded an install with NO config. With no data dir there was no
  `server.pid`, and both port checks FAILed against the bridge's own
  listeners. With no env overrides, `audio-toolchain` read "not enabled"
  beside `BRIDGE_UPSCALE_ENABLED=true`. **`config-dir` grades the absolute
  directory of the config it RESOLVED**: init, `config.Save` and a relative
  `dataDir` all write beside the config the bridge reads, and the check
  CREATES what it is handed, so grading the platform dir beside a
  `./bridge.yaml` vouches for (and leaves behind) a directory nothing uses.
  With nothing found it is the platform dir, where init writes, and it is
  EMPTY when that cannot be resolved, never the working directory. **The
  launcher's doctor row passes the platform path explicitly**: it is offered
  only before that install exists, beside the Setup wizard that writes it.
  **Images up to v0.2.0 still need `--config /data/bridge.yaml`**, which is
  why the docs keep passing it. On those images the Dockerfile and
  docs/docker.md blamed the FAIL on "`bridge serve` writes no PID file" for
  seven weeks after #639 made it write one, and a re-check without the flag
  would have CONFIRMED that, since it failed identically for another reason:
  **measure an old image's doctor with `--config`, or you are measuring its
  config lookup.** **The `config-file` line names the config the report
  graded, and one that is THERE but will not load FAILS.** Until #985 the
  load error was dropped without a word, so a typo'd key read "all clear",
  exit 0, where `bridge status` exits 2 on the same file. The local-first
  lookup made that worse: a broken `./bridge.yaml` shadows a good platform
  config (CodeRabbit's finding). The FAIL is what makes the runbook's
  "validate a config edit BEFORE restarting" step true. **A `--config` that
  names a file that is NOT THERE fails too** ("does not exist"), because an
  operator who names a file asserts it exists; that is loadCLIConfig's rule
  for every other subcommand. `config-dir` then reports "not checked" rather
  than `MkdirAll` the named file's directory, which it always did, so a
  typo'd path created that directory and called it ok. With no flag, a
  missing config is ok and the line names where doctor looked. **`resolveConfigPath` folds EVERY stat
  error into "not found"**, which is right for its fallback walk and wrong
  for a named path, so doctor re-stats a named path to learn why. One this
  user cannot reach or READ only WARNS, because that is a fact about the
  doctor run (the public-mode layout, as with the cert key), and the port
  checks then say "not checked" rather than grade the defaults (#1022), as
  config-dir does rather than probe as this user (#1023).
  The launcher's pre-setup row names the platform path through
  `buildDoctorDepsFor(path, true)`, never `--config`, so its absence stays
  "none found", ok, and a config there this user cannot read still leaves
  config-dir and the ports graded, as Setup's preflight grades them
  (#1023). `bridge init`'s preflight leaves the lookup nil, so
  config-file reports itself skipped and does not block the re-init that
  replaces a broken config. Since #1027 the port checks do not block it
  either: they recognise the bridge being replaced by the pid file in the
  data dir init writes, by attribution only (the "…over a config that is
  there and does not load" bullet). This bullet said a broken existing
  config "cannot block" that re-init until 2026-09-25, when the port
  checks still could: with the config unloadable the preflight graded
  7788 / 7789 with no pid file, and a bridge still live on them FAILed
  both. (#984, #985)

- **…and where it finds no config, tls-cert grades the pair in the data
  dir `bridge init` writes, as the port checks grade the ports it writes**
  (2026-09-29, backlog B61). config-file says "none found; the checks
  below use defaults", and tls-cert graded nothing there: it warned "no
  data dir set" on EVERY pre-init run (`16 ok, 1 warn`, the warn its),
  while init, run next, keeps a pair it finds in `<config dir>/data`
  (`initDataDirFor`) and its preflight FAILs a broken one. So a cert left
  without its key there read "all clear.", exit 0, and init then refused
  on `[FAIL] tls-cert partial state` (measured with the real binary);
  now doctor FAILs it too, and a fresh host reads `absent (init will
  mint)`, `17 ok, 0 warn`. The launcher's row does the same over a config
  it finds and cannot read, as Setup's preflight grades that dir over one
  (#1023's rule for the row;
  `TestMenuDoctorPreviewsSetupOverAnInstallThisUserCannotRead` compares
  the two lines). **A config that was named or found and not graded
  leaves no data dir**: tls-cert answers ok "not checked" and why (not
  readable, not there, does not load), #1022's rule, where it warned too;
  a run over one reports one warn fewer. `bridge init`'s preflight and the
  console always hand a data dir and are unchanged.

- **The image's `lsof` package is load-bearing — don't drop it to slim the
  image.** Alpine's own `/usr/bin/lsof` is busybox's applet, which ignores
  `-iTCP:<port> -sTCP:LISTEN -t` and lists every open file, and
  `isPIDListeningOnPort` searches that output for the pidfile's PID — so doctor
  would credit ANY occupied port to a running bridge (measured: a root-held
  `:8080` read `bound by our own bridge (pid 1)`, where the package reports a
  warn). (#984)

- **The HEALTHCHECK's connect-and-close is silenced at the LISTENER, never by
  making the probe speak TLS.** net/http logs every failed handshake and
  `bridge health` closes before any ClientHello, so each probe was `http: TLS
  handshake error from 127.0.0.1:…: EOF`: 2,880 lines a day, the M-SEARCH
  class. `handshakelog.Wrap` takes the RAW listener and returns the server's
  `ErrorLog`. For a peer on this host only (loopback, or source ==
  destination: `bridge health` dials a specific bound IP as-is, so it arrives
  FROM that IP), the listener records whether the peer's first answer was
  EOF, and the logger drops a line only on that evidence. **The text cannot identify the probe**:
  a client that sends a whole ClientHello (1,483 bytes from Go's) and then
  vanishes gets the byte-identical `: EOF`, and that one is a real failure.
  **Nor can a verifying probe replace it** (`InsecureSkipVerify` was refused in
  #485): checked against the cert on disk, it reports a live bridge DEAD
  whenever the served cert is expired, not yet valid, or rotated ahead of the
  restart, and logs `remote error: tls: bad certificate` per probe in each
  case, so the flood returns exactly when someone is reading the log. Kept
  lines go to `log.Print`, where a nil `ErrorLog` sends them;
  `TestEveryOtherHandshakeFailureIsLoggedAsBefore` compares against a
  nil-`ErrorLog` server rather than restating net/http's wording. Never wrap a
  listener that yields `*tls.Conn` (tsnet's `ListenTLS` does): http.Server
  asserts that type to find the handshake, ALPN and `r.TLS`. The console's
  public-mode TLS branch takes the same pair, since the launcher's
  `probeAdminRunning` and `waitForListen` hit it the same way, and **a new TLS
  listener takes it too.** (#986)

- **…and the servers' error log keeps no client address** (#1055). net/http
  prints the peer's address in a failed handshake, a recovered panic and the
  HTTP/2 connection errors, and the privacy page promises no client IPs for
  the phone-facing API: a phone with a stale pin, a cancelled endpoint probe
  and a scanner each left one in the journal. `handshakelog.Wrap`'s logger
  redacts every line it keeps (`RedactPeers`, anchored on the words net/http
  prints before a PEER and on the `->` of a socket error, the next bullet,
  so a listen address in an accept error stays), and
  AFTER the silent-probe check, which recognises the probe by that address.
  The tailnet server, whose listener yields `*tls.Conn`, takes
  `handshakelog.ErrorLog()`. `TestEveryServeHTTPServerRedactsPeerAddresses`
  requires every `http.Server` in `cmd/bridge` to get its `ErrorLog` from
  handshakelog, and an assigned one before the server is first served. **A test that finds a log line by its peer address passes
  vacuously once the address is redacted**: the probe tests find the
  filtered server's line by the placeholder as well.

- **…and a kept line's REASON names the peer again, after the `->` of a
  socket error, which is redacted too** (2026-09-29, backlog B172). A
  handshake that times out or is reset, the commonest failure a phone
  produces, logs net's `*net.OpError` as its reason, and that names both
  ends of the socket: `http: TLS handshake error from <client address>:
  read tcp 127.0.0.1:7788->127.0.0.1:51786: i/o timeout`. Measured on
  macOS, Linux and Windows 11 over a stalled client, a reset before the
  ClientHello and after the ServerHello, a reset right after the
  ClientHello (the server's WRITE fails: `broken pipe` on macOS,
  `connection reset by peer` on Linux, `wsasend` on Windows) and the
  HTTP/2 preface of a reset h2 connection (the phone's own shape; logged
  on Linux and macOS, while Windows' h2 server counts `WSAECONNRESET` as a
  closed connection and logs it only verbosely): every one was
  `<local>-><peer>`, for a read and a write alike, since the OpError's
  Source is the local end and its Addr the remote, and gVisor's gonet (the
  tailnet listener's sockets) builds it the same way. So the tailnet
  server's `ErrorLog()`, which runs the same function, leaked it too.
  `RedactPeers` now also takes the address after `->`, and **every other
  place the line repeats an address a word or an arrow named as a peer**
  (one walk of the line, never inside a longer address): net's accept ignores a
  failed getsockname (fd_unix.go, fd_windows.go), and a socket error with
  no local end names the peer alone, `read tcp <peer>: …`, where only the
  words earlier in the line say what it is. **A lone address can also be
  the LOCAL end**: gVisor's socket has no remote address left after a
  reset, nothing names it, and it stays. **Don't redact every lone
  address after `read tcp`**, which labels the tailnet node's own address
  `<client address>`. **And an IPv6 zone runs to the `]` before the port
  whatever it holds**: Windows names a link-local zone after the adapter,
  spaces and parentheses included (`[fe80::…%Wi-Fi 4]:51195`, measured),
  and #1055's `\[[^\]\s]+\]` matched no such address even after "from ",
  so the whole line kept the peer. The part before `%` must be an IPv6
  literal, so a bracket in a panic value is not taken for one (a looser
  bracket swallowed `[x] happened at [y]:7`). The listen address before
  the arrow stays, and `isSilentProbe` still reads the raw line (its
  `: EOF` carries no arrow). **A leak check reads every address in the
  output, never only the place the fix put the redaction**:
  `TestEveryOtherHandshakeFailureIsLoggedAsBefore` drove the stalled shape
  since #986 and passed through #1055, because it looked for the peer only
  right after "from ". It now requires every address the wrapped server
  logged to be the server's own (a loopback client shares its IP, so the
  port tells them apart). `TestAnHTTP2PrefaceErrorKeepsNoClientAddress`
  (skipped on Windows, which logs nothing there) and
  `TestRedactPeersInASocketError`, whose rows are built from `net.OpError`
  and `net.TCPAddr` values so none holds a shape net never prints. The
  other `http.Server`s bypass handshakelog knowingly: DLNA's nil
  `ErrorLog` names a renderer in a panic line (the privacy page's
  disclosed DLNA exception), and the console's plain-HTTP modes see only
  loopback peers (a proxy's, in public mode).

- **`RedactPeers` walks the line once** (2026-10-12, backlog B246). The
  previous form scanned the whole line once per distinct address a word
  or an arrow had named. 40,000 distinct peers took 5.7 s. The addresses
  are collected, then one walk copies each named span and replaces a
  later repeat of the same text only where it stands alone (the previous
  byte does not continue an address, and the next byte is not a digit),
  which is what keeps `10.0.0.2:5000` from eating `10.0.0.2:50001` or
  `110.0.0.2:5000`. **The finder is a direct walk of the same pattern as
  `peerAddr`, not `FindAll`**: under the race detector the regexp
  interpreter spent 1.3 s of a 1.3 s call on those 40,000 matches, so a
  one-second bound could not hold while `FindAll` ran. `peerAddr` stays
  the spec; `peerAddressSpans` must return its group 2.
  **The zone is `%[^\n]*?` and ends at the first `]` that `:` and a
  digit follow, so a zone may contain `]`.** `from [0%]]:0` is one
  address. Stopping at the first `]` misses it: `FuzzPeerAddressSpansMatchThePattern`
  found that in under a second, and the seed is committed.
  `nextZoneClose` is one backward pass per line — each byte's next
  `]:digit`, or -1 when a newline comes first — and the zone reads its
  end from that table in O(1). Scanning forward from every open ` from [0%` walks the
  tail once per prefix (40,000 of them, 2.77 s). A newline ends the
  reachable close, so `[ab%\n]:1` is not an address. Without a `%` the
  `]` is still the next byte.
  `TestPeerAddressSpansMatchThePattern` is that lockstep.
  `TestRedactPeersOfThousandsOfPeersFinishesAtOnce` is the bound (under
  1 s, every peer replaced, idempotent).
  `TestALineOfUnclosedZonePrefixesFinishesAtOnce` is the unclosed-zone
  bound. The app twin is B239's linear parse.

## Scanner, deletion passes, and manifest writes

- **Shutdown joins every background writer**, and the wait must be written
  INLINE in the defer, never routed through a variable assigned later in
  `runServe` — the first tracked goroutine starts ~1200 lines before the end, so
  an early return leaves it nil and lets a live writer race `Store.Close()`.
  Both this wait and the watcher's are grace-BOUNDED; a wedged writer degrades
  to a log line, never a hung exit. A writer includes a goroutine whose CHILD
  PROCESS writes files: the Tailscale auto-pilot's `tailscale cert` was the one
  unjoined writer in `runServe` until #997 (the `cmd/bridge` section below).
