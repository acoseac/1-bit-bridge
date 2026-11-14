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
  tenant's withheld controls to its console. That refusal names the
  controls this build knows, a fixed set, and each other entry of
  `deployment.managedControls` by its place in the list
  (`deployment.managedControls[1]: not a control this build knows`), never
  by what it holds: an entry that names no control can hold anything, and
  the refusal joined the list as written, a password pasted onto the line
  with it (B505's round 4, Codex; `managedControlsSaid`,
  `TestInitRefusalNamesAManagedControlItDoesNotKnowByItsPlace`). A fresh
  start is moving `bridge.yaml` aside, never `--force`.
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
  could read. **Every setting it keeps holds the value Load gave it, and a
  file Load's decoder takes whole gives up Load's own block** (rounds 2 to
  4). `priorOutput` decodes the unchanged bytes with Load's own decode,
  `config.DecodeFile` (the same decoder set the same way, `KnownFields`,
  and since backlog B528 a whole number written as a fraction refused, as
  a word there is), into `config.Config`, before Load's defaults, its
  BRIDGE_* overrides, its paths and its checks, and keeps that decode's
  output block (`outputAsLoadDecodes`; the orchestrator's decision on
  round 4). So nothing the decoder reads or counts differs from Load's.
  Round 1 decoded the block's pairs in the
  file's order and kept each value that was not zero: a merge written after
  a key overwrote it (`mpd: off` was saved `on`, a serial binding replaced
  by a merged port binding), and an explicit empty or null value let a
  merged one through (Codex). Round 2's `resolveOutput` resolved the merge
  keys itself, as a copy of the decoder's rules, and the copy disagreed
  with the decoder three ways (Codex and the Claude API on round 2): it
  compared a key written as `!!binary` by its base64 text, so `{!!binary
  cGFzc3dvcmQ=: "", <<: {password: old-pw}}`, an empty password to Load,
  was saved with `old-pw`; it followed 64 merged blocks, so a password
  listed after 63 empty ones was dropped from a config that loads; and it
  dropped a setting a merged block names twice through an alias key, where
  the decoder takes the first. Round 3 decoded the block on its own with
  the decoder (`outputAsLoaded`, an Unmarshaler of yaml.v3's older form,
  whose callback decodes with the decoder at hand and returns the errors of
  that value alone), which read every value as Load does but not what
  yaml.v3 counts across the document: it refuses a document whose alias
  expansions pass 99 in 100 decodes, so a block whose merges expand aliases
  tenfold a level, beside 1,000 library roots, which Load takes, was refused
  there, fell to the certainty recovery, met the merge key and kept
  nothing, its password with it (Codex on round 3). Of the block Load
  decodes, every setting is kept but those
  `config.OutputConfig.WithoutRefused` refuses (Validate's refusals, of
  which `Problem` is the first), each named by the line of the block's own
  key that writes it, or as merged when none does; the lines come from the
  node the decoder hands the output field, read without decoding a value
  (`outputNode`). Only a file the decoder refuses somewhere, one Load
  refuses, goes further, and of it the rewrite keeps only what is certain:
  the block as the decoder decodes it on its own, when it does
  (`outputAsLoaded`; what the decoder counts outside the block can only make
  it refuse more, never read a value otherwise); else, when every key is
  text with no tag (`plainKey`), each setting a key names once, decoded on
  its own, unless refused (`outputRecovered`). A block or a setting that
  writes a whole number as a fraction decodes there as its whole part, so
  each is also asked `config.CheckWholeNumbers`, Load's check, and does not
  decode when it says no (B528: until then Load took a file holding
  `loopbackPort: 7795.5`, and the rewrite kept the 7795 it read); a key
  written more than once
  and a key output does not take are left out and named by their lines (an
  unknown key went without a word until round 3). A merge key, an alias or
  a tag among the keys, or a block that is not a mapping, keeps nothing,
  and the warning says so: which setting a value is for is the decoder's to
  say, and the decoder refused the block. So does a block the file writes
  twice, once encoded (`outputKeyLines`; written twice as it is,
  `refuseRewrite` refuses the rewrite). Every line names the setting or the
  block and a line, **never a value**: `OutputProblem`'s reason carries
  none, a key output does not take is named by its line alone
  (`warnOutputNotKept`), and round 1's warning, which quoted a refused
  `output.mpd`, put MPD's password pasted onto that line on stderr (round
  2). The refusal of a config that does not decode prints yaml.v3's error
  without the file's text (`config.ValueFreeYAMLError`, round 3). A player
  block never fails, and one this release cannot read is kept as written.
  `printKept` lists each kept setting, the password as "set (not shown)"
  and the player by whether it is enabled and its binding's kind. **Don't
  drop the output block, and don't read it as a field of
  `priorInstallFile`.** **Don't read the output block apart from the file
  when the whole file decodes: keep Load's own decode, and of a file the
  decoder refuses keep only what is certain; don't print a refused
  setting's value, or a managed control this build does not know.**
  (`TestInitRewriteKeepsTheOutputBlock`,
  `TestInitRewriteKeepsAPlayerBlockThatDoesNotReadAsWritten`,
  `TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo`,
  `TestInitRewriteKeepsWhatTheOutputBlockLoadedWith`,
  `TestInitRewriteResolvesMergesBesideASettingItCannotKeep`,
  `TestPriorOutputResolvesMergeKeysAsLoadDoes`, whose whole-config shapes
  put 1,000 library roots, 200 custom endpoints and 100 upstream servers
  beside the block,
  `TestInitRefusalOfAConfigThatDoesNotDecodeQuotesNothingOfIt`,
  `TestInitRefusalNamesAManagedControlItDoesNotKnowByItsPlace`, and
  `TestPriorOutputOfAFileTheDecoderRefusesElsewhere`, which puts a
  misspelt top-level key beside a merged block the decoder takes alone,
  kept as Load decodes it once the typo is put right, and beside Codex's
  alias-heavy block, of which nothing is kept: since round 4 gave every
  file that decodes Load's own block, its fixtures are the only ones that
  reach that reading (the Claude API on round 4; the test, round 5); and,
  since B528, a fraction in `scanIntervalSec` beside a block kept whole,
  and the fraction rows of the rewrite tables, a `loopbackPort: 7795.5`
  not kept and a fraction in a merged block, which keeps nothing.)
  **So are the enrich block's two base URLs, each on its own, in either
  posture** (2026-11-08, backlog B761): each names the mirror the operator
  set up, with any credential it carries, and init asks for neither. A base
  Load takes that is not the default (`config.DefaultMusicBrainzBaseURL` and
  `DefaultCoverArtBaseURL`, held to the enricher's own by
  `TestEnrichBaseDefaultsAgree`) is kept as Load keeps it, trimmed and with
  no trailing slash; one written empty, or as the default, is written as no
  key, as a first install writes it. `priorEnrich` reads each from the
  file's bytes on its own, never through Load, so no variable's value
  reaches the file, and a base beside a sibling or a switch that does not
  read is still kept. Load's own check, called through config
  (`config.NormalizeEnrichBaseURL`), decides what is refused: a base it
  refuses, or one that does not read as text, is not kept, and the run
  names it by its line with a fixed reason, never its value (the check's
  own words name the value's scheme and host). `warnEnrichNotKept` says
  what each kind costs (a switch not kept is on now; for a base not kept
  the default is used now), and the block's own line names both kinds.
  `printKept` lists a kept base whole only where it carries no user
  information, query or fragment, each of which can carry a credential
  (B54, B69), and as "set (not shown)" where it does. **Don't drop an
  enrich base Load takes, keep one it refuses, or print a kept base's user
  information, query or fragment.** (`TestARewriteKeepsTheEnrichmentBases`,
  whose credential rows put a credential in one part of a base alone, so
  that each part is shown or not on its own, and
  `TestARewriteOverTheDefaultsWritesWhatAFirstInstallWrites`, a rewrite
  over the defaults writing the bytes the first init wrote.)
  **So is the update check's switch written off** (2026-11-09, backlog
  B761's PR 1), as the enrichment switches are: `priorUpdate` reads
  `update.checkEnabled` from the file's bytes on its own (`readPriorSwitch`,
  which `priorEnrich` reads its two switches with), so no variable's value
  reaches the file; a false is kept as false and a true written as no key,
  as a first install writes it; one that does not read, or a block that is
  no block, keeps nothing and is named by its line, never its value, and
  `warnUpdateNotKept` says update checks are on now and how to turn them
  off; `printKept` lists the switch kept. **Don't drop an opt-out written
  off: the enrichment switches, or the update check's.**
  (`TestARewriteKeepsTheUpdateCheckSwitchWrittenOff`.)
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
  checks and the data dir's preparation, ahead of the TLS mint, the token
  store, `server.pid`, the migrations, the updater's boot marker and every
  sweeper** (2026-10-02, backlog B208). A second serve of a live data dir ran all its wiring and
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
  bridge from starting. Since B606 (2026-11-02, the next bullet) that is a lock
  that cannot be taken on a data dir that is there: a data dir serve cannot
  prepare refuses before the lock is tried, the lock makes nothing, and the
  line says which kind of answer the lock met (B537 item 1). **A kernel lock, not a lock FILE**: the kernel drops it
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

- **…and before that lock serve refuses a data dir it cannot prepare: `prepareServeDataDir` makes it (`fsutil.MkdirAll`, whose walk as root gives what it makes its parent's owner) and then requires a directory there by `os.Stat`, and its `*dataDirUnusableError` names the data dir and the entry in its way (a regular file, a link to one, a link to nothing, at the data dir or above it), exit 1 before the player's state, the TLS mint, the token store, `server.pid` and the migrations; a link that leads to a directory, and a data dir not made yet below one, are taken; and `lockServeDataDir`, which makes nothing, still serves where no lock can be taken on a data dir that is there, saying the kind: a `*dataDirUnlockedError` marked `noLocks` where `fsutil.KeepsNoLocks` reads TryLock's answer, said without claiming the filesystem keeps no locks, which ENOLCK does not show, and every other failure, the lock file's open included, as before** (2026-11-02, backlog B606, the owner's decision of 2026-10-31; B537 item 1). **Don't fold the preparation back into the lock, refuse a data dir that is there over a lock that cannot be taken (a lock file this user cannot open, a read-only filesystem, a directory at `server.lock`), over a link that leads to a directory, or over what cannot be written there (the player's preflight and the doctor say that), or word the no-locks line as proof that the filesystem keeps none.**

  *The defect.* `runServe` went on after every `lockServeDataDir` error that
  was not contention, and the lock made the data dir first, so MkdirAll's
  errors were among them: the EEXIST of a data dir where a link to nothing
  stands, at the data dir or above it, and the ENOTDIR of one that is, or is
  below, a regular file. Serve then started without its lock, skipped
  `AdoptPlayerState` and `CheckPlayerStart`, started no player, and went on
  to its first writes. Measured on the branch's seams (127d1b90, which change
  no behaviour), with the config keeping the TLS pair outside the data dir,
  for each of five shapes (a regular file, a link to nothing and a link to a
  regular file at the data dir; an ancestor that is a regular file or a link
  to nothing): serve printed "could not lock …", minted the TLS pair, and
  exited 1 only at the token store ("open token store: stat token store: …
  not a directory", the file shapes) or at the manifest store ("open manifest
  store: mkdir store dir: … file exists", the link shapes, after the pid
  file's warning). The commands that take the lock beside a serve did no
  better over a data dir that is a regular file: restore and reset-player
  said the lock could not be taken and to fix its permissions or use storage
  that supports locking; clear-missing kept the admin probe, passed its
  prompt and failed at the store; the DAC bind exited 0 having written the
  binding into bridge.yaml.

  *The decision* (the owner, 2026-10-31): refuse. Serve refuses to start,
  with an error naming the path, when it cannot prepare its data dir; a lock
  that cannot be taken on a directory that is there goes on as before, since
  serve must never fail to start over a lock.

  *The preparation.* `prepareServeDataDir` makes the data dir with
  `fsutil.MkdirAll` (as root each directory it makes takes the owner of the
  directory it is made in, `mkdirAllAsRoot`), then asks `os.Stat` again and
  requires a directory: MkdirAll answers nil for a directory there, and one
  replaced since is refused all the same (`prepareServeDataDirWith` takes a
  test's MkdirAll). Its `*dataDirUnusableError` names the data dir and,
  from what `os.Lstat` and `os.Stat` find at the entry MkdirAll's
  `*fs.PathError` names (`whyNotMade`, `notADirectory`), the entry in the
  way: "X is a regular file, not a directory", "X leads to T, a regular
  file, not a directory", "X is a symbolic link to nothing", MkdirAll's error
  beside it; any other failure (a parent this user cannot write) is "it could
  not be made: …". `errors.Is` reaches MkdirAll's error (`fs.ErrExist`,
  `syscall.ENOTDIR`), and the kinds are `fsutil.EntryKind`'s. Where an
  ENOTDIR's entry cannot say so, an MkdirAll naming the entry its own make
  failed at, below the one in the way, which no Lstat reaches, the refusal
  walks up the data dir's path past every entry Lstat cannot reach to the
  first entry that is there, and names it when it is no directory
  (`blockingEntry`, round 2, Argon): a directory there is no blocker and
  names nothing. Since B659 (2026-11-02) both are fsutil's,
  `fsutil.NotADirectory` and `fsutil.BlockingEntry`, the one walk bridge
  doctor's data-dir and player-storage lines read too (the B659 entry, after
  the B603 one), and whether an entry os.Lstat calls no directory leads to
  one is asked of os.Stat, as os.MkdirAll asks it (a Windows junction to a
  directory). Go 1.27's `os.MkdirAll` names the entry in the way itself,
  an ancestor that is a link to a regular file included (measured on
  legion0: for `A/sub` and `A/sub/data` with `A` a link to a regular file it
  answers "mkdir A: not a directory"), so the walk guards another MkdirAll,
  and `TestThePreparationNamesTheEntryInTheWayBelowWhichMkdirAllFailed`
  pins it through the seam. runServe asks it right after the read-only
  checks, before it initialises any of its runtime state (the one write
  above it is `--init-if-missing`'s seed config, `writeAutoInitConfig`,
  which can make the config's directories and write bridge.yaml, and only
  where no config is there), and exits 1, saying "serve: the data directory
  D cannot be used: WHY; bridge serve starts only on a data directory it can
  make or use: move what stands in its way, or set dataDir in bridge.yaml to
  a directory". A link that leads to a directory is taken,
  the data dir's own entry or an ancestor's (Linux and NAS installs keep one
  there, and refusing it would break their upgrades), and so is a data dir
  not made yet below such a link, which MkdirAll makes through it; as root
  over a data dir that is there, a directory or a link to one, MkdirAll's
  walk returns at its first stat and neither makes nor chowns anything
  (`TestPreparingAnExistingDataDirectoryAsRootMakesAndGivesNothing`, which
  covers the preparation alone, on a directory that can be statted: the lock
  and serve's later writes on a read-only filesystem are not claimed). What
  can be written there is not asked: a lock file that cannot be opened in a
  directory that is there is the lock's fallback, and the player's preflight
  (`backup.AdoptPlayerState`, `backup.CheckPlayerStart`) and bridge doctor
  say whether the data dir can be written. The four commands that take the
  lock beside a serve prepare it first too (the restore bullet below).

  *The order* (round 2, Codex). The player's state preflight
  (`playerStatePreflight`: AdoptPlayerState, then CheckPlayerStart, under
  the data dir's lock alone) runs on every boot, the player on or off, after
  the preparation and the lock. The refusal test ran with the player off,
  where nothing showed the preflight's order, so it would have passed with
  the preflight moved ahead of the preparation. It now runs a stand-in in
  the preflight's place (`serveOpts.playerStatePreflight`) that counts its
  calls and requires none, and
  `TestServeRunsThePlayersStatePreflightOnceItsDataDirIsPreparedAndLocked`
  requires its one call to find the data dir made and its lock held. With
  the preparation moved after the preflight (round 2's negative control),
  both go red.

  *The lock's answers* (B537 item 1). `lockServeDataDir` makes nothing now:
  a data dir that is not there is a lock file that cannot be opened. It
  answers a `*serveDataDirHeldError` for contention (`fsutil.ErrLocked`:
  EWOULDBLOCK and EAGAIN on unix, ERROR_LOCK_VIOLATION on Windows), and a
  `*dataDirUnlockedError` for anything else, marked `noLocks` where
  `fsutil.KeepsNoLocks` reads TryLock's answer (ENOLCK, EOPNOTSUPP, ENOTSUP,
  `errors.ErrUnsupported`, which ENOSYS matches; Windows' ERROR_NOT_SUPPORTED
  and ERROR_INVALID_FUNCTION) and never for the lock file's open. Serve
  serves on after either and says which: "serve: no lock was granted on L
  (ERR): another process may still hold one, and serve continues without
  exclusive use of the data dir" (round 2's shorter line, Codex; round 1's
  named the NFS mount and the kernel out of lock records), worded so
  because ENOLCK is no proof that the filesystem keeps none (Linux answers
  it when it cannot allocate a lock record, on a filesystem that keeps locks
  and while another open holds one: the config lock's `noLockToGive` asks
  again for that reason); and
  "serve: could not lock L (ERR); serving without the check that no other
  bridge serve runs on this data dir" for a permission, I/O or descriptor
  error and an open that failed, as before. The kinds are told apart in the
  line alone: what serve does after each is what it did. The migration's
  ladder lock sorts its answers the same way (scanner.md's ladder bullet).

  Pinned by `TestServeRefusesADataDirectoryItCannotPrepare`,
  `TestServeRunsThePlayersStatePreflightOnceItsDataDirIsPreparedAndLocked`,
  `TestPreparingTheDataDirectoryRefusesWithItsPathAndItsCause`,
  `TestThePreparationAsksAgainWhatStandsThereOnceItIsMade`,
  `TestThePreparationNamesTheEntryInTheWayBelowWhichMkdirAllFailed`,
  `TestPreparingAnExistingDataDirectoryAsRootMakesAndGivesNothing`,
  `TestServeStartsOnADataDirectoryALinkLeadsTo`,
  `TestServeStartsWhereTheDataDirCannotBeLocked`,
  `TestServeStartsWhereTheLockFileCannotBeOpened`,
  `TestPreparingTheDataDirectoryAsRootGivesEachDirectoryItMakesItsParentsOwner`,
  `TestServeSaysWhichKindOfLockItCouldNotTake`,
  `TestServeSaysNoLockWasGivenWhereTheKernelHadNoneToGive`,
  `TestTheDataDirsLockSortsEachAnswerIntoItsKind` and fsutil's
  `TestKeepsNoLocksReadsOnlyTheNoLocksAnswers`; the record is in
  `ops/engineering-log.md`, 2026-11-02, B606.

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

- **`bridge restore`, `bridge reset-player` and `bridge manifest clear-missing` hold the data-dir lock for the whole run; restore and reset refuse if the lock cannot be taken, and reset repeats both of restore's admin-port checks around its confirmation; each of them, and `bridge output dac bind`, prepares the data dir first as serve does and refuses one it cannot prepare, before it writes anything (`holdDataDirForMutation`, B606), clear-missing and the bind keeping the admin-port probe where only the lock cannot be taken.**
  Restore and clear-missing mutate the store from a second process,
  where `Store.mu` does not reach and `busy_timeout` is a retry rather than
  a serializer. Each takes `lockServeDataDir` (the kernel lock `bridge
  serve` holds, the bullet above) before it reads the store or writes, and
  releases it only after the last write, so a serve started during the
  confirmation prompt, or one whose admin address is ephemeral, is refused:
  the lock does not depend on the admin port. A `*serveDataDirHeldError` is
  the refusal, and the message is the lock's own, which names the pid
  `server.pid` records when it records one. Any other error from the lock
  means it could not be taken (a filesystem that keeps no locks, a
  permissions error, a directory where the lock file goes). Since B470 5a0,
  restore refuses that case too: its reservation merge requires an exclusive
  lock, not a best-effort probe (`TestRestoreRequiresAnExclusiveDataDirLock`).
  `clear-missing` still falls back to the admin-port probe and its message. The lock
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

  Since B606 (2026-11-02) each of them, and `bridge output dac bind`, first
  prepares the data dir as serve does (`holdDataDirForMutation` runs
  `prepareServeDataDir` before the lock) and refuses one it cannot prepare,
  before its prompt and before anything is written, naming the data dir and
  what stands in its way ("restore: the data directory D cannot be used: D is
  a regular file, not a directory (…)", then "Move what stands in its way, or
  set dataDir in bridge.yaml to a directory, then re-run this command.").
  Over a data dir that is a regular file, restore and reset-player had said
  the lock could not be taken and to fix its permissions or use storage that
  supports locking, clear-missing had kept the admin probe, passed its prompt
  and failed at the store, and the bind had exited 0 having written the
  binding into bridge.yaml. A data dir not made yet is still made, so a
  restore onto a fresh box restores. Their policies for a lock that cannot
  be taken on a data dir that is there are unchanged: restore and
  reset-player refuse, clear-missing and the bind keep the admin probe
  (`TestEveryCommandThatMutatesTheDataDirRefusesOneItCannotPrepare`,
  `TestEveryCommandThatMutatesTheDataDirKeepsItsLockPolicy`).

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
  the snapshot's: read by the player's rule before any snapshot data is written,
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
  (`config.ReadPlayerUUIDFile`: a file, opened without waiting, so a
  named pipe, a directory or a device in its place fails the restore at
  once, before any snapshot data is written, B525; one lowercase UUID, white space
  after it and nothing else, 128 bytes at most), for the console account's
  reason: the manifest lists the file last, and a check at its turn would
  come after every other file was replaced. A copy that does not hold one
  UUID fails the restore before any snapshot data is written; the refusal names
  the copy and says to remove it from the snapshot, which restores the
  rest and leaves the box's identity as it is (a copy the snapshot lists
  and no longer holds is skipped). The refusal tests restore the database
  too and watch its -wal and -shm, and the CLI's a track added after the
  snapshot: a check moved after the database step, the first file a
  restore writes, passed tests that restored no database (round 2,
  Codex). The UUID is written by `config.WritePlayerUUIDFile`, the writer
  `EnsurePlayerUUID` uses: the UUID and a newline, 0600 whatever the
  copy's mode, one `KeepOwner`. B470 5a0 writes schema 2: older restores refuse it,
  while the new restore also reads schema 1. Before this UUID and every
  other identity file, restore durably merges and raises both reservation
  marks; the reservation is never copied by the file loop. A valid captured UUID must match the live reservation's UUID;
  a mismatched pair refuses, with ordinary reset retaining the floor before
  the next mint. The UUID-only upgrade takes the jumped adoption (B524);
  reservations refused as data get the player reset command advice;
  cancellation and file-access errors refuse backup without it,
  and a captured UUID without a floor refuses with discard advice while
  the live UUID remains, or retry advice if it vanished during capture.
  A lone local discard seal is completed before restore preflight; that
  local marker write imports no snapshot data.
  An identity replacement also durably removes the destination queue before
  writing the UUID; restoring the same UUID keeps its queue. The floor survives identity replacement, so a non-file live UUID may be replaced without reading it. No snapshot UUID leaves the live reservation
  alone too. The full arithmetic, assumptions, exhaustion remedies, crash
  order and release coupling are in History-rules-player.md under the
  restored-identity rule. Two boxes running one identity at once are
  unsupported; use the newest snapshot when moving to another box. **Don't restore
  `player-uuid` as bytes, and don't let a copy that does not hold one UUID
  reach the loop.**
  (`TestBackupAndRestoreGiveThePlayerTheIdentityItHad`,
  `TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo`,
  `TestSnapshotCarriesThePlayerUUIDAndRestoreWritesItAsTheIdentityFile`,
  `TestSnapshotSkipsAPlayerUUIDTheBoxDoesNotHave`,
  `TestRestoreOfASnapshotWithoutThePlayerUUIDLeavesTheIdentityAlone`,
  `TestRestoreRefusesAPlayerUUIDThatIsNotOneUUIDBeforeWritingAnyFile`,
  `TestRestoreRefusesANamedPipeForThePlayerUUIDAtOnce`,
  `TestRestoreOverANamedPipeAtTheBoxsPlayerUUIDDoesNotWait`,
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
  start-up database step on the serve context, exits 0 when the shutdown
  lands in it, whether its error is the cancellation or the
  `sql.ErrTxDone` its commit answers once database/sql's own goroutine has
  rolled the transaction back (B662, the next bullet;
  `disarmEarlyRollback`, B613's, is the other start-up step on that
  context that can exit 1, and says its stop itself; this said "the one
  startup step that runs on the serve context and exits 1" until then).
  **The scanner's final flush DROPS its batch on
  a cancel, deliberately**: the skip gate compares each file with its
  STORED row, which the dropped write never touched, so the next scan
  redoes exactly what was dropped. Three traps. A cancellation error from
  an AUTOCOMMIT statement does not prove the write did not land (modernc
  answers `ctx.Err()` whenever its interrupt fired, a committed statement
  included). database/sql can answer `sql: statement is closed` in place
  of the cancellation (1 in 3,000 measured), which is reported rather than
  matched by its text, and so can `sql.ErrTxDone`, which a loop reports
  too, as `ctxerr.WithoutCancellation` does, and a start-up step takes
  for its stop beside a cancelled stop context (B662). And a context
  cancelled BEFORE the call can pass a
  test without reaching the site at all: the seed's `GetUpscaleTarget`
  failed on it first and the seed was skipped, so the test was green on
  the unfixed code. Cancel INSIDE the call the test names
  (`internal/sqlitetest`'s collation over the column a write moves, or a
  fake upstream that cancels and holds), assert that the cancel happened
  there (`mustHaveReachedGitHub`), and give every stopped test a twin
  failing on a LIVE context: without one, the site's "never log" control
  cannot bite, which happened twice here. Joining the tsnet start
  goroutine was #997's class, not this rule's; #1009 did it (the bullet
  after next).

- **A stop that lands in a start-up step on serve's context exits 0 only
  on that stop's own artifacts: serve's stop context cancelled and an
  error made of the cancellation and `sql.ErrTxDone` alone, the error a
  commit answers once database/sql's own goroutine has rolled the
  transaction back (`stoppedBeforeServing`)** (2026-11-06, backlog B662).
  macOS run 41480956932 failed `TestServeRunsThePlayersWatcherWhileThePlayerIsOn`
  with `serve exit code 1; stderr=seed upscale target: sql: transaction
  has already been committed or rolled back`: the test stops serve once
  its hotplug watcher has read once, and the watcher starts before the
  seed, so the stop can land while serve seeds the upscale target on its
  fresh database. #1005 had taken the seed's `context canceled` for a stop
  (the bullet above) and nothing else. The seed is
  `SetUpscaleTarget(ctx, …)`: `BeginTx`, two upserts and `Commit`, all on
  serve's own context. `BeginTx` gives the transaction a child of it and
  starts `Tx.awaitDone`, which rolls the transaction back as soon as that
  child is done, marking it done before anything else. A statement on the
  cancelled context answers `ctx.Err()` (database/sql's `grabConn` asks the
  context first, and modernc answers `ctx.Err()` for a statement its
  interrupt goroutine saw cancelled, one that completed included), which
  is #1005's case. A cancel that lands after the last statement's driver
  call has returned, with the rollback's mark before the seed reaches
  `tx.Commit()`, leaves `Commit` to find its context done and the
  transaction done, and it answers `sql.ErrTxDone` without calling the
  driver's Commit (go1.27.3's `sql.go`, `Commit`). A statement whose
  `grabConn` passed its context check just before the cancel and found the
  transaction done just after the rollback's mark answers it too. Neither
  carries the cancellation, so the seed exited 1. Not a transaction used
  twice, and not a statement on another context: every call of the seed
  takes serve's own `ctx`, unshadowed from runServe's first line to the
  seed. **Clean shutdown is a serve-level outcome**: `stoppedBeforeServing`
  (serve_startup_stop.go) is the seed's one check, the cancellation's case
  and the ended transaction's together, so the two cannot drift. It asks
  `errors.Is(stopCtx.Err(), context.Canceled)`, never any `ctx.Err()`, so a
  deadline stays a failure as `ctxerr.WithoutCancellation` rules, and
  `stopArtifactsOnly`, which walks the error as `errors.Is` does: the
  cancellation as `ctxerr.WithoutCancellation` reads it, or `sql.ErrTxDone`,
  wrapped or joined, and nothing else beside them, so a disk's refusal that
  arrives after the stop still fails, and so does a wrapper of nothing.
  **The store still returns the error**: the console's batch submit calls
  `SetUpscaleTarget` too and takes nil for a committed update. **And
  `ctxerr.WithoutCancellation` is unchanged**: it keeps reporting a
  deadline and an unrelated failure during a shutdown, and the
  `sql.ErrTxDone` a loop's commit meets after a stop stays the measured
  residual its doc names (one reported line). The transaction code is
  unchanged; its test seam (`Store.SetUpscaleTargetBeforeCommitForTest`,
  `serveOpts.upscaleTargetBeforeCommit`) is nil in production. The audit
  of every start-up database call and transaction `runServe` reaches is
  in `ops/engineering-log.md` (2026-11-06, B662): the seed is the one on
  serve's context; the store's open, its migrations, the scanner's
  `last_full_scan` read and the batch coordinator's recovery run on
  `context.Background()`, where a stop cannot reach them, and
  `GetUpscaleTarget`'s stop only skips the seed. The tests hold the seed
  between its last statement and its commit, stop serve, wait at the
  driver for database/sql's rollback (`sqlitetest.WatchRollbacks`, the
  bullet on the store-level hold under **Build, CI, and test
  discipline**), and only then let it commit:
  `TestAServeStoppedAtItsUpscaleSeedsCommitExitsCleanly` (red without the
  fix, exit 1 with exactly the CI's stderr), its twins that stay failures
  (`TestAServeWhoseUpscaleSeedFindsItsTransactionEndedStillFails`, the
  same `sql.ErrTxDone` with the stop context live;
  `TestAServePastItsDeadlineAtItsUpscaleSeedsCommitStillFails`;
  `TestAServeWhoseUpscaleSeedFailsAsItIsStoppedStillFails`, an unrelated
  error after the stop) and `TestAStopEndsAStartUpStepOnlyByItsOwnArtifacts`,
  the predicate's table. **Don't take `sql.ErrTxDone` for a stop on a
  live context or past a deadline, take any `ctx.Err()` for the stop, let
  an error beside the stop's artifacts pass, make the store swallow the
  error, or teach `ctxerr.WithoutCancellation` the rule.**

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
  **The one goroutine the stop leaves behind** (2026-11-06, backlog
  B660): the install's confirmation (`serveProbation`,
  serve_probation.go) runs on `scanCtx` and is joined through
  `bgWriters`, but each of its readiness rounds runs on a goroutine of
  its own, because one of its answers is `GET /v1/health` asked in
  process through the API handler, and `ServeHTTP` takes no cancel (the
  health handler's endpoint walk is a singleflight that ignores its
  caller's context). A round still out at its bound is a no, no second
  round starts beside it, and the stop does not wait for it: a handler
  that never returns would otherwise hold serve's exit, this bullet's
  defect. What such a round reads after the teardown fails as a read of
  a closed store does, and its answer is dropped.
  `TestAHealthCallThatNeverReturnsIsNotReadyAndDoesNotHoldTheStop` holds
  the call until after serve has exited 0.

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

- **Serve runs the sample-format header pass at once and after every
  successful scan, on a `postScanNudges` channel and `scanCtx`, joined through
  `bgWriters`** (2026-10-23, backlog B513). `runSampleFormatBackfill` calls
  `manifestStore.BackfillSampleFormats` with `apiSrv.Resolver().ResolveChecked`
  (hot on a root change), once at once and once per nudge, until `scanCtx`
  ends. On a first start the pass at once is the one that runs to its end and
  completes the backfill, which is when `/v1/health` advertises
  `sampleFormat` (round 3; scanner.md). `logSampleFormatPass` writes one
  Info line of counts when a pass completed the backfill (once per library)
  or read a header, nothing when it read none (every pass once the backfill
  is done), and a Warn through `ctxerr.WithoutCancellation` for a failure
  that is not the shutdown; `serveOpts.sampleFormatPassed` lets a boot test
  wait on a pass. **Don't give it a `SetPostScanHook` of its own** (the hook
  REPLACES, and the sweepers would lose theirs), **don't drop the pass at
  once** (a startup scan that ends before runServe registers the hook sends
  no nudge, and the rows, and the health key with them, would wait for the
  next scan, six hours by default), **and don't log a path** (counts only;
  the privacy page's rule). (`TestServeGivesARowIndexedBeforeSampleFormatItsKind`
  boots the real serve over a store rewound to before the column and waits
  for a pass that gained a kind and completed the backfill,
  `TestTheSampleFormatPassRunsAtBootAndAfterEveryScan`,
  `TestServeHandsTheSampleFormatPassThePostScanNudge`.)

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
  requires every `http.Server` in the module to get its `ErrorLog` from
  handshakelog: in its literal, through a variable every assignment of
  which is a call into handshakelog, or by Wrap's assignment before the
  server is first served. It swept `cmd/bridge` alone until B604's second
  round (2026-10-31), when the DLNA MediaServer's server had no `ErrorLog`
  and the console's had none on plain HTTP, so a handler's recovered panic
  named the client it was serving (Codex on the integrated branch); it now
  also requires a server in each package that builds one (`cmd/bridge`,
  `internal/admin`, `internal/dlna`, `internal/player/openhome`). **A test that finds a log line by its peer address passes
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

- **Serve starts its player from the boot snapshot on a goroutine of its own, defers its stop after the watcher's stop and the store's close so the engine and the backend are joined, bounded, before either closes, and shares the data dir's lock with it (`dataDirHold`) while anything it started runs: a state write that outlives the stop, and a Run a panic ended, are the exceptions to the join** (B470 PR 4b3). `planServePlayer` decides once, from the boot snapshot (the player.md section "Player inside bridge serve"); a refusal adds no goroutine and no file, and the hotplug watcher's dormant service is as it was (on Linux; none off Linux). The order on every exit, a stop (exit 0) and a restart request (exit 75) alike: `bridgePlayer.stop` cancels the supervisor, which stops the engine's `Run` and waits for it at most `playerEngineStopBound` (`engine.ReleaseTimeout` + 5 s), then `Backend.Wait` at most `playerBackendWaitBound` (5 s), says what was left ("the device may still be held until the process exits" for an unconfirmed release or a call held in the kernel), waits for the library index's build ahead of the first lookup, which the player begins with the OpenHome surface and stops at once when the surface does not start, at most `playerWarmStopBound` (2 s; B470 PR 5a4, `openhome.md`), and closes its feed subscription; then the watcher's stop (its service on a context its stop alone cancels); then the store's close; then the lock. `stop` waits for the supervisor at most the sum of the player's bounds and a second: these two, the OpenHome surface's and its discovery's since B470 PR 5a2a, and since PR 5a4's warm the library index's build's (`servePlayerBounds`). **The one exception to the join**: `Run` can outlive its bound in a state write that does not return (its flush is synchronous and fsynced, and its shutdown waits for a reservation write and the queue writer), and a timed-out wait is an abandonment. The stop logs it and returns; the player keeps its share of the lock (`dataDirHold`), letting go only once its supervisor and `Run` have both returned. Production `main` exits when `runServe` returns (`os.Exit(run(…))`; `run` → `serveCmd` → `runServe`), so the writer ends with the process and the kernel releases the lock with it, with no instant between an unlock and the exit; a serve started again in the same process (the launcher menu, the tests) is refused the lock until the write ends. A backend call held in the kernel holds the DAC, not the data dir, and takes no share. A write of the player's DSD state does (B762, 2026-11-07): the backend counts each write it starts (`recordDSD`) and the silent probe's goroutine, which may start one, among its writes (`playback.Backend.DSDWrites`), and `settle` lets go of the player's share only once those have returned too, saying once that a write past the stop's bound keeps it ("a write of the player's DSD state did not finish within the stop's bound: the player keeps its share of the data directory's lock until it ends"); the supervisor's asks end with it (red before: the share was let go once `Run` returned, an evidence write held). Since round 2 (2026-11-07, backlog B780) `settle` closes those writes once `Run` has returned (`Backend.CloseDSDWrites`, in the step that reads those under way, under the lock that counts them and the backend's goroutines) and waits on the channel it returns, so a write that a goroutine the stop abandoned asks for later, a session's open the kernel held past the stop recording its driver_accepted once it lands, is never made, and is said once (red before: written after the gate). And the stop says each kind of goroutine it left past its bound for what it holds, from one reading (`Backend.Running`): the line above whenever a write runs, and the device line whenever any other goroutine of the backend's does, both where both run (red before: a write and a control read held in the kernel were said as the lock alone). **The second exception** (round 2, Codex): a panic on the goroutine running `Run` skips Run's shutdown, which is what waits for those writes, so the player keeps its share for good (`playerRun.panicked`, `settle`, logged), and in this process the data dir stays locked; production's exit releases it. Release of the DAC is attempted separately (B523): for a Run a panic ended, the supervisor's shutdown has the backend release every session it holds (`releaseAll`, `Backend.ReleaseAll`) within `playerBackendWaitBound`, the bound its `Backend.Wait` shares, before that wait, and logs that the device is free, or a Warn that it may still be held. The earlier watch line says only that the backend is asked to let go; it cannot promise the bounded attempt will succeed (B523 round 2). Serve also hands the player the holder's `Changed` and `livePlayerSettings`, the watcher's own reading of the player block, which its output reports follow (player.md). **Don't defer the player's stop before the watcher's or the store's, don't wait for the engine or the backend without a bound, promise successful device release before its outcome, release the data dir's lock while the player's engine, or a write of its DSD state (B762), may still write there or before those writes are closed to any asked later, or let the stop's line for a write left running (the lock kept) stand in for its line for any other goroutine left (the device held).** `TestServeStopReleasesThePlayersDACWithinItsBounds` (the release confirmed, the DAC closed, and the journal's order: the player shut down before the watcher stopped), `TestServeRestartReleasesThePlayersDACWithinItsBounds`, `TestServeStopWithAPlayerCallHeldInTheKernel`, `TestServeStopWithThePlayersStorageStalled`, `TestServeStopsThePlayerBeforeItsWatcherAndItsStore`, `TestAPanicOnTheEnginesGoroutineKeepsItsShareOfTheLock`, `TestAPanicOnTheEnginesGoroutineLetsGoOfTheDAC`, `TestServeKeepsTheDataDirLockWhileTheProbesEvidenceIsWritten` (B762), `TestAStopBesideAHeldWriteAndAHeldReadingSaysBoth` (its round 2) and `TestOnlyServeAndTheHarnessBuildThePlayer` (bridge doctor and the console's doctor build no player) pin it.

- **Serve installs the console's view of its player (`newConsolePlayer`, `admin.Deps.PlayerOutput`) beside GET /v1/outputs' view, over the same boot plan, `bridgePlayer` and `bridgeOutput`, the boot snapshot, whether it holds the data dir's lock and its player-state preflight, so the page's pending changes are `planServePlayer`'s answer on the live config** (B601, 2026-10-31). `runServe` builds it right after `apiSrv.WithBridgeOutput(playerView)`, from `playerPlan`, `player`, `playerView`, `cfg`, `opts.playerGOOS`, `lockErr == nil`, `playerStateErr`, `opts.playerTrees` (the zero value is `hostTrees`) and the manifest store, and hands it to the console as `Deps.PlayerOutput`. Its status reads the boot plan, `bridgeOutput.Facts` (the element's facts: one engine snapshot and the bounded DAC read), the engine's snapshot after `ready` (the transport, the occurrence heard and its position), the hotplug feed's latest resolution of the boot binding, and the live config the console hands it; the next start's decision is `planServePlayer` on that config (or the boot's player-state refusal again, which only a start can find gone), and the pending list is its difference from the boot plan. The track heard is named through `bridgePlayer.res` (`library.Resolver.StoredPath`, then the store's row), looked up once an occurrence within 500 ms. A DAC listing is one reading of the box at a time (`consolePlayer.read`: 3 s, its slot let go only when the reading returns, so a read stuck in the kernel holds the next back rather than doubling it), bracketed by a reading of sysfs's identities before /proc as `readPlayerDACObserved` is (a card whose unit or devnum moved between the two is set aside as transient, and so is one whose devnum either reading could not read, saying so; a transient card's /proc id and name, which the list shows, are redacted of both readings' serials and ports, as a USB row's are of its own), and refused whole when it is not whole (`CardsComplete`, `Partial`); each USB card's playback endpoint gets a token (128 random bits, 10 minutes, the newest 64 kept) for the unit as that reading read it, its devnum included, and the binding each kind builds there (`binding.For`), and a choice reads the box again, in the write's config turn just before the save, and requires the same unit (`binding.SameUnit`) at the same devnum building the same binding (`hotplug.SameBinding`). A transport row plays only while the player's output is open (`outputOpen`: the live config withholding nothing, GET /v1/outputs' element not unavailable, the engine's output report available): the engine runs on after the player is turned off, another DAC is chosen or its DAC is unplugged, and plays nothing then (Codex on the PR). **Never let the view write the config, start, dial or probe MPD, hand a serial out but as its label, read `eng` or `res` before `ready`, read the box but through its one bounded, bracketed reading at a time, or call a transport playable while the output is withheld or gone.** `TestTheConsoleShowsTheRunningPlayer`, `TestTheConsoleSaysNothingPlaysOnceTheDACIsUnplugged`, `TestTheConsoleTurnsAPlayerOnForTheNextStart`, `TestTheConsoleOffersNoPlayerOffLinux`, `TestTheConsoleListsEachPlaybackEndpointOfTheBox`, `TestAUnitSwappedWhileTheBoxIsReadIsNotOffered`, `TestAUnitWhoseEnumerationCannotBeReadIsOfferedNoToken` and `TestTheTokensAreBounded` pin it.

## Scanner, deletion passes, and manifest writes

- **Shutdown joins every background writer**, and the wait must be written
  INLINE in the defer, never routed through a variable assigned later in
  `runServe` — the first tracked goroutine starts ~1200 lines before the end, so
  an early return leaves it nil and lets a live writer race `Store.Close()`.
  Both this wait and the watcher's are grace-BOUNDED; a wedged writer degrades
  to a log line, never a hung exit. A writer includes a goroutine whose CHILD
  PROCESS writes files: the Tailscale auto-pilot's `tailscale cert` was the one
  unjoined writer in `runServe` until #997 (the `cmd/bridge` section below).

### B544 — retire the unchecked UUID minter (2026-10-26)

- **`backup.EnsurePlayerUUID` is the sole UUID minter; the retired config minter must not return.** Its reservation writer checks the medium before UUID publication; keep minting tests in backup's tests to avoid config's import cycle, including v4 shape, persistence, non-file kinds and a refusing medium leaving no final UUID on either attempt (B544).

The symbol census found only tests calling `config.EnsurePlayerUUID`, so
it was retired; serve and bind already use the checked backup minter. Keep
`config.WritePlayerUUIDFile` for that minter and restore. Round 14 above
records why the reservation writer checks the medium before publishing the
UUID. Backup's external tests hold v4-shape, persistence, permission and
malformed-identity coverage, avoiding config's import cycle.
`TestPlayerUUIDRefusingProbeLeavesNoIdentity` checks probe counts and no
final UUID on both attempts; publishing the UUID before the reservation
makes both attempts fail. `TestPlayerUUIDMinterRefusesNonFilesAtOnce`
covers a named pipe, a link to one, a link to a character device and a
directory: within five seconds, an error other than `fs.ErrNotExist`, no
identity, and the same node and mode. A plain-open negative control fails
both FIFO rows; the restored test passes all four under `-race`. The
B544/B520 entry in `ops/engineering-log.md` records both controls.

## Shared Source discovery lifecycle (B470 PR 5a2b)

Serve constructs its sole socket-free SSDP subsystem before starting either
owner. `servePlayerWiring.SSDP` and `dlnaWiring.SSDP` carry the same instance.
No device means no SSDP sockets or workers; DLNA off does not gate Source.
The Source's eligible interfaces are independent of the MediaServer's picker.
Its initial incarnation zero shares a listener when the interface overlaps.
The boot snapshot still gates the surface; live player-disable only withholds
output. Failed/refused/disabled players publish no Source.

The full lifecycle closure is deferred after the UPnP upstream stop, so it
runs first under LIFO: MediaServer withdrawal and HTTP stop must precede the
resolver teardown its routed file handler depends on. A player-only early
fallback refuses/stops the player if that closure was never registered.
The full closure refuses Source first, withdraws the MediaServer
and Source, then calls shared Close, all with the same absolute five-second
SSDP deadline. Cancellation of serve's parent ends normal sends but leaves
byebye possible. HTTP receives separate deadlines afterward; the player's
surface stops before its engine/backend, before the watcher/store/lock. The
player's outer stop adds DiscoveryStopBound independently of Limits.StopBound.
Publication of a new surface and refusal serialize under the player lock;
no late start can escape a stop that found no surface yet.

The instance `serveOpts.ssdpNetwork` seam reaches the real subsystem, while
`dlnaNetwork` supplies synthetic pick/gather/inventory and a held HTTP listener.
Defaults retain production selection and host transport. The new serve tests
use in-memory HTTP, fake SSDP, an empty hotplug observation and the real engine;
no audio tree read or PCM open is needed. Tests count memberships and packets
for off/Source-only/MediaServer-only/both/disjoint/failed/refused configurations,
strict capability failure beside MediaServer, repeat serve generations with
stable identity, and park withdrawal to observe both 503 and a live engine.

### Round 2: teardown order and non-vacuous observers

`TestServeStopsDLNABeforeItsUPnPResolver` records the MediaServer's actual
byebye write, its HTTP listener's close, and the upstream stop boundary through
an instance seam. Before the fix it recorded UPnP first. Moving the full
teardown to DLNA's slot makes the order byebye, HTTP close, UPnP stop, while
retaining refusal, both withdrawals, shared Close, DLNA HTTP stop and player
stop inside the closure. The early fallback runs only if the full closure did
not run.

The 50 ms blocked-write case in
`TestServeDiscoverySharesOneShortShutdownDeadline` checks bounded release,
not equality of budgets: four fresh 50 ms budgets fit under its 500 ms bar.
Its `same-absolute-deadline` case now records SetWriteDeadline's value at
every byebye write, requires both Source and MediaServer, and compares those
absolute deadlines. Fresh per-owner contexts fail that assertion.
`TestServeDiscoveryRestartsWithStableIdentity` waits for alive packets,
requires a nonempty Source identity in each generation and checks every
byebye USN. Replacing Source's NT or corrupting byebye USNs fails it.
`TestServeSharesDiscoveryAcrossItsEligibleDevices` checks the start channel
and bind history after serve stops; eligible cases prove a real bind is seen.
A control injects real late surfaces through the upstream-stop seam for the
disabled/refused/failed cases: all three fail the settled-state assertions,
where the old immediate nonblocking check observed nothing.
## Live backup reads across rename (B470 PR 5a0, Round 9)
- **A live backup retries Windows sharing/access failures while opening files replaced by rename, with atomicwrite’s bounded, cancellable backoff; persistent errors still refuse it.** **Only a reservation refused as data gets backup’s player reset advice; cancellation, an expired context and file-access errors never do. Exhausted sharing/access retries say the file was busy and to retry the backup.** `Snapshot` reads a reservation while the engine may refill it asynchronously (`ahead` → `reserveWrite` → `writeReservation` → `WriteBytesDurable`). The writer closes its staged descriptor before rename; neither it nor `readLimited` leaks a reservation handle. A rename or a Windows scan-on-close handle can nevertheless conflict with the reader's open. Closing the test's engine before backup would remove the live-backup case, not fix it. Production currently refuses the player on Windows because directory fsync is unavailable; the Windows fixture supplies that barrier, but backup also reads files other live writers replace there.
  `atomicwrite.ReadWithRetryCtx` shares `RenameWithRetry`'s five-attempt schedule (0, 50, 100, 200, 400 ms; 750 ms total sleeping). Only Windows `ERROR_SHARING_VIOLATION` and `ERROR_ACCESS_DENIED`, including wrapped errors, retry. Other platforms and other errors return at once. The context interrupts backoff; exhausted errors keep their cause. Snapshot uses it for the live reservation, the freshly renamed UUID copy it rereads to stamp that reservation, and source stats/opens, including player UUID, tokens, config and account files. The copy still streams and commits once; no queue is backed up. A persistent unreadable reservation refuses backup; a captured UUID without a floor requires retry.
  `TestSnapshotRetriesAHandleHeldAcrossRename` uses the existing rename seam to hold a real Windows staged-file handle that permits rename and denies read. It verifies the unprotected open returns the sharing violation, then releases the handle only when the snapshot's context is consulted for backoff: no timer can release it before the failed read. Reservation, source UUID, captured UUID and tokens cases require captured bytes and both floors. A read error on the captured UUID refuses backup after retries rather than silently omitting the mark; a malformed UUID still keeps the existing no-mark policy. `TestSnapshotRefusesAReservationStillHeldOrCancelled` keeps the handle through exhaustion or cancellation and requires the failed snapshot to be removed. `TestReadRetryWindowsErrors` checks both Windows codes; `TestReadRetryBoundsAndCancellation` checks the shared schedule and cancellation on every platform; `TestReadRetryKeepsPermanentErrors` keeps absence and malformed data immediate.
### Round 10 — reset advice requires invalid reservation data
A shutdown cancellation in the Windows backoff used to reach the same advice
as malformed reservation data. An exhausted sharing retry also recommended
permanently giving up the identity, though neither established damaged data.
`ReadWithRetryCtx` now wraps `ErrReadBusy` and the last Windows error on
exhaustion; cancellation/deadline errors stay plain. `Snapshot` names
`player-ids.json` and the cause without reset advice for cancellation,
deadline expiry or a filesystem `PathError`. Busy errors also say to retry
the backup. A reservation read successfully but refused as data (malformed,
unsupported version, invalid UUID or excessive size) retains the reset advice.
A captured UUID without a floor now refuses too; all these errors remove the partial snapshot. `ERROR_ACCESS_DENIED` remains retryable: replacement or
delete-pending files can return it during the swap, with the same bounded
schedule as sharing violations.
`TestSnapshotRefusesAReservationStillHeldOrCancelled` now checks held,
cancelled and expired cases for no reset advice, the file and cause, and
busy/retry wording on exhaustion. `TestUnreadableReservationAdviceResetsIdentityAndQueue`
requires no reset for a backup's file-access error and retains the complete
advice for malformed data and an unsupported version. Restore's existing
refusal policy is unchanged. `TestReadRetryWindowsErrors` requires the busy
marker on exhaustion of either code and its absence on cancellation/expiry.
Windows tests are cross-compiled here; their native execution remains a
Windows-runner check, distinct from portable race-test results.

## Backup source opens (B525 round 2, 2026-10-24)

- **Every backup source copied by `copyFileCtx` opens through
  `fsutil.OpenAsFile` inside the sharing-violation retry: a named pipe is
  refused at once, naming the file without its value, and the partial
  snapshot is removed. Don't replace this with a plain open; keep missing
  optional sources skipped.** The live capture of `player-uuid` did not
  use `ReadPlayerUUIDFile`: it copied bytes first and parsed the snapshot
  copy afterward. Round 1 guarded only the parser's open. The copy now
  refuses a FIFO at the live identity, tokens, certificate, key, YAML,
  console account or Atlas state through the same open inside
  `atomicwrite.ReadWithRetryCtx`. The opened descriptor decides its kind;
  the earlier stat remains only the optional-source skip. The existing
  snapshot cleanup removes the partial directory on refusal. Restore uses
  the same helper for ordinary snapshot files and still round-trips them.
  `TestSnapshotRefusesALivePlayerUUIDPipeAtOnce` and
  `TestSnapshotRefusesALiveTokensPipeAtOnce` pin prompt refusal, the
  `*fs.PathError` around `*fsutil.NotAFileError`, the exact value-free
  message, the unchanged source pipe and the absence of a snapshot directory.
  With the plain open restored both tests fail after five seconds; the
  fixture then releases and joins the blocked snapshot.

## bridge.yaml's lock in the CLI and serve (backlog B506, 2026-10-24)

- **A command that takes both the data dir's lock and bridge.yaml's takes
  the data dir's first (`holdDataDirForMutation`, `lockServeDataDir`) and
  bridge.yaml's inside it: bind, restore (in `backup.RestoreWithOptions`,
  before its first write, to its end, bridge.yaml written through the
  turn's `SaveBytes`) and serve; library add and remove (`config.Edit`),
  init (`saveInitConfig`) and serve's seed (`writeAutoInitConfig`) take
  bridge.yaml's alone, each deciding on what its turn found
  (`FileLock.LoadFound`, `Found`); a restore compares the config with the
  bytes `bridge restore` read before its lock and its prompt, and with what
  its turn found, before its first write, refusing a change, or a name a
  link repointed since (`config.ErrNameMoved`), with nothing restored, and a
  refusal of either at bridge.yaml's own write names what it restored (a
  replaced identity's queue as let go only when the restore removed one)
  and says running it again finishes it; serve and init keep the config's
  name as written (`fsutil.AbsAsWritten`), serve's boot read resolves it
  before each read, reads the file it resolved, and resolves it again after
  (`loadServeConfigFile`, `config.LoadFileAt`), and tells its holder that
  file, or no file where no resolution could follow the name, its console
  then refusing every save until a restart, and init and the seed refuse a
  link to no file before anything is written; a save the turn refuses
  before a rename (`config.ErrChangedInTurn`) writes nothing, the seed then
  saying it wrote nothing and serve loading what appeared, init and bind
  saying the file changed, bind a failed read of its own comparison as that
  read's failure, and a turn whose name now opens another file than it
  locked (`config.ErrNameMoved`, at the lock after its read, at a rename
  after its comparisons, or at a check after its read) writes nothing, every
  CLI writer saying to run it again** (2026-10-24, backlog B506, five
  rounds; the lock itself is config-lifecycle.md's).
  Serve fixes the order: it holds the data dir's
  lock for its whole life (`dataDirHold`) and takes bridge.yaml's for each
  console save, so the data dir's is necessarily the outer one, and bind and
  restore keep it: `bridge output dac bind` takes bridge.yaml's after
  `holdDataDirForMutation` and before its read under the locks, and gives it
  back after its `SaveIf`, before it makes the player's UUID, which is not
  the config; `bridge restore` holds the data dir's for its whole run (the
  prompt included) and `RestoreWithOptions` takes bridge.yaml's, when the
  snapshot holds the file and the caller maps it, before the player's
  reservation and identity and the copies, holding it until it returns, so a
  config another writer holds refuses the restore with nothing restored
  (`TestARestoreRefusedTheLockRestoresNothing`: a track added after the
  snapshot is still there). Since the second round the snapshot's
  bridge.yaml is written in that turn, through `FileLock.SaveBytes`, staged
  where the config's name resolves and renamed in only over the file the
  turn found: `copyFile` staged it in the directory `filepath.Dir` cleaned
  the name to, and renamed it over whatever an edit had put there meanwhile
  (`TestARestoreLeavesAConfigChangedInItsTurn`, red before: the snapshot's
  copy over the edit).

  **A refused restore restores nothing, a late one says what it did, and
  running it again finishes it** (the third round: the Claude review's
  Medium, and Codex's review of the brief). `bridge restore` reads the
  config before it takes the data dir's lock and asks for confirmation,
  and works out every target from that read (its data dir, its TLS pair);
  a library add, an init or an edit can change the config while it asks,
  since bridge.yaml's lock is not held at the prompt, and the second round's
  restore compared only with what its own turn found, after that change, and
  restored into the data dir the config no longer named. Now `restoreCmd`
  hands the restore the bytes it read (`loadCLIConfigFile`,
  `RestoreOptions.ConfigAsRead`), the restore takes bridge.yaml's turn
  whenever it has them (a snapshot without bridge.yaml included), and once it
  has read the snapshot, before its first write (the player's reservation),
  `configStillAsRead` asks whether the turn found exactly those bytes and
  whether the file, read again, is still the one the turn found: a config
  changed since either refuses the restore with nothing restored ("… is not
  the config this restore read when it worked out where each file goes:
  another writer changed it since …, so nothing was restored: run the
  restore again, to restore into what the config names now"). A writer that
  takes no turns can still change the file while the files before
  bridge.yaml are copied; the refusal at bridge.yaml's own write then names
  the files restored before it, the player's identity with its reservation
  among them (and the queue it let go, which until the fourth round it named
  whether or not the restore had let one go: below), and those not
  restored, and says that running the restore again finishes it, putting
  every file the snapshot holds back, bridge.yaml over that change
  (`partialRestoreError`); it said "nothing was written" until then. The copies keep their order. Red before:
  `TestARestoreRefusedBeforeItsFirstWriteRestoresNothing` (an edit landed
  just before the first write: every file before bridge.yaml restored, the
  reservation, the identity and the queue included, each live file holding
  bytes the snapshot's differ from), `TestARestoreRefusesAConfigItsTargetsDidNotComeFrom`
  (the config changed since its caller read it, with a snapshot that carries
  bridge.yaml and one that does not: restored), the late refusal's words in
  `TestARestoreLeavesAConfigChangedInItsTurn` (which then runs the restore
  again and finds every target as the snapshot has it), and in `cmd/bridge`
  `TestARestoreRefusesAConfigChangedWhileItAsked` (its data dir, and its TLS
  pair, changed at the prompt: the database replaced).

  Nothing takes the data dir's lock while it holds bridge.yaml's. The data dir's lock is a try that never waits and
  bridge.yaml's waits at most `config.LockWait`, so no order could block
  forever; the fixed one keeps it so if the data dir's ever waits, and keeps
  a command from holding bridge.yaml's, and every writer behind it, while it
  is refused the data dir. `TestTheDataDirLockComesBeforeTheConfigLock`:
  with bridge.yaml's lock held by another open, bind and restore are found
  holding the data dir once they wait for bridge.yaml's (the lock's own word
  for it, `config.LockWaitedForTest`), and go through once it is free; with
  both held, each is refused at the data dir at once ("already running on
  the data dir"), never after waiting out bridge.yaml's. The check asks the
  data dir's lock once, never in a poll: that lock is a try, so a poll
  holding it at the instant the command asked for it would refuse the
  command. Red on main: bind and restore returned at once, exit 0, never
  waiting. The first round asked halfway through the 5 s wait, a fixed
  sample a slow runner could miss in either direction; the second round's
  control (bind's two locks swapped) went red 10 runs in 10.

  **library add and remove's offline change is one turn of bridge.yaml's
  lock** (`config.Edit`): the config is read again under it, after the
  admin probe that read it first, and every decision is made on that copy,
  already configured or not, the root list `ValidateRoots` checks, the 1→N
  wipe, the last root, the basename guard and the collapse; the library
  index's wipe or prune runs in the turn, and a save that fails after a wipe
  retargets the carry as before (`retargetAbandonedCarry`,
  `RetargetFirstIndexedCarry`), the turn's own refusal just before the
  rename (a writer that takes no turns, landing after the turn's read) among
  those failures. Every message and exit code is as it was;
  a lock another writer holds past the wait is "library add: another bridge
  command or the running bridge is changing …", exit 1.
  `libraryEnv.beforeLock` is the window between the first read and the turn.
  `TestLibraryChangesKeepASettingsSaveThatLandsAfterTheirRead` (a console
  save through a holder in that window is kept, for add and for remove; red
  on main, `libraryName "Test Library"`), and
  `TestBindKeepsALibraryAddThatLandsInItsWindow` (a library add started
  while bind holds the lock, between its read and its save, waits for bind's
  turn and adds its root to the file bind wrote; red on main, bind refused
  by its byte comparison and the binding gone). Since the second round that
  test holds bind's turn until the add is waiting for the lock, which the
  lock says (`config.LockWaitedForTest`), and fails at once if the add
  finishes first; the first round's 300 ms passed whether or not the add had
  reached the lock.

  **init's turn is its final check and save, never the run**
  (`saveInitConfig`): the lock, `configChangedSinceRead` (which reads the
  file again), then `cfg.Save` with the turn. A run asks its questions
  between its read and that turn, so it decides from its read and refuses a
  file that changed, as it did before, rather than hold every other writer
  off while it waits on the operator; no writer that takes the lock can land
  between the check and the save. `TestTheChangedConfigCheckIsTheStepBeforeSave`
  pins it by the syntax tree (initCmd calls `saveInitConfig` once and
  neither the check nor `cfg.Save` itself; in `saveInitConfig` the lock
  comes before the check and the check directly before the save), and
  `TestInitRefusesAConfigThatChangedWhileItRan` the refusal. The save then
  compares the file once more just before its rename (the turn's own
  comparison, the second round), and init answers its refusal with the
  check's own words ("the config at … changed while this init ran …; this
  run did not write it", `sayConfigChangedWhileInitRan`), exit 1:
  `TestInitsSaveLeavesAConfigChangedWhileItSaves`, red before, the edit
  written over. Since the third round the check compares the run's first
  read with what the turn found (`FileLock.Found`), the turn's own bytes,
  rather than reading the file once more; init names the config file as
  written (`fsutil.AbsAsWritten` of `--dir`, the file the OS opens for it,
  where `filepath.Abs` cleaned a `..` after a link away), keeps the data dir
  `filepath.Join` of the cleaned directory as before, makes the directory as
  written, and refuses a config that is a link to no file before it writes
  anything (`config.CheckName`): until then it minted its TLS pair, made its
  data dir and saved its config over the link
  (`TestInitRefusesALinkThatLeadsNowhereBeforeItWritesAnything`). A config
  dir that does not exist yet, below a linked ancestor, is made where the OS
  puts it (`TestInitMakesANestedConfigDirUnderALinkedAncestor`).

  **serve --init-if-missing writes its seed only while the config is still
  missing in its turn** (`writeAutoInitConfig`, which answers whether it
  wrote; the boot's "wrote a default" line follows that answer), so a config
  another writer saved after serve looked is kept and loaded.
  `TestTheSeedKeepsAConfigWrittenBeforeItsTurn`. Its save asks once more
  just before the rename (the second round): a config a writer that takes
  no turns put there meanwhile is kept too, the seed answers that it wrote
  nothing, and serve loads that config
  (`TestServesSeedLeavesAConfigThatAppearsWhileItSaves`, red before: the
  seed renamed its default over it and said it wrote). Since the third round
  the seed decides on the turn's own answer (`FileLock.Found`: a turn that
  found no file), not a `Stat` beside it, makes the config's directory as
  written, and a name that is a link to no file is refused by its turn, so
  the seed neither makes the file the link names nor puts one in its place
  (`TestServesSeedRefusesALinkThatLeadsNowhere`, red before: it wrote its
  default over the link). Serve keeps its config's name as written
  (`fsutil.AbsAsWritten`), for its load, its seed, its console's saves and
  its backups: it cleaned the name with `filepath.Abs`, so for
  `cfg/link/../bridge.yaml` it loaded, locked and saved `cfg/bridge.yaml`
  while the OS, and every CLI writer, opened `real/bridge.yaml`
  (`TestServeOpensTheConfigItsNameOpens`, red before: it saved the decoy at
  the cleaned name); the base of the config's relative paths is still the
  one `config.Load` takes from the name, so its data dir does not move.

  **bind answers the turn's refusal as its own** ("changed after bind read
  it, so the binding was not written: run bind again"): the turn's
  comparison, against the file as the turn read it, runs before bind's
  `SaveIf` comparison, against bind's own read in the turn, and the two
  differ only by the instant each read
  (`TestOutputDACBindNeverWritesOverAnUpdateAfterItsRead`). Since the third
  round bind decides on the config its turn found (`FileLock.LoadFound`) and
  its `SaveIf` compares the file with those bytes (`Found`), before each
  attempt at the rename, reading as the turn's comparison reads
  (`config.ReadFile`, asked again while Windows refuses the read for a
  moment), and a read that fails for good is reported as that read's
  failure ("save config: read … again before replacing it: …"), never as a
  config that changed (`TestBindsOwnComparisonAsksAgainWhileTheConfigIsBusy`,
  `TestBindsOwnComparisonSaysAReadFailedForWhatItIs`, red before: "changed
  after bind read it" for both).

  **The fourth round** (the same day) took the reviews of the third; the
  turn's half of it is config-lifecycle.md's, where a turn resolves the
  config's name again once it holds the lock, before each rename attempt
  and at its check, and refuses a name that opens another file than it
  locked (`config.ErrNameMoved`, a `*config.NameMovedError` saying "… now
  opens another file than the one this writer locked …, so nothing was
  written: run it again"). Every CLI writer refuses it with those words and
  none tries again on the file the name opens now: library add, waiting
  for the lock beside the file its name opened while the link at the name
  was repointed to a twin holding the same bytes, wrote that file, wiped the
  library index for its second root and said it had added the root; now it
  refuses with nothing written and the index untouched
  (`TestLibraryAddRefusesANameRepointedWhileItWaited`, red before: exit 0,
  the file written, `bridge.db` made by the wipe). A restore takes a moved
  name as it takes a changed config (`configMovedOn`): before its first
  write a `configChangedError` with nothing restored, at bridge.yaml's own
  write a `partialRestoreError` naming what it restored
  (`TestARestoreRefusesANameRepointedBeforeItsFirstWrite`, red before: every
  file restored and the snapshot's bridge.yaml written into the file the
  name no longer opened; `TestARestoreSaysWhatItRestoredBeforeANameRepointedAtItsConfig`,
  red before: the snapshot's copy written there). The late refusal names a
  replaced identity's queue as let go only when the restore removed one
  (`discardReplacedPlayerQueue` answers whether it did): a restore of the
  identity the box runs keeps its queue, and one over a box with no queue
  removes none, and both said "and its queue let go" (Codex's Low on the
  third round; `TestALateRefusalDoesNotSayAKeptQueueWent` and
  `TestALateRefusalClaimsNoQueueWhereThereWasNone`, red before, each with
  the edit landing just before bridge.yaml's write, after the files before
  it were restored; `TestALateRefusalSaysAReplacedIdentitysQueueWent` the
  guard). Serve's boot read (`loadServeConfigFile`, `config.LoadFileAt`)
  resolves the config's name just before it reads and just after, reads
  again when the two differ, up to three reads, and refuses to start on a
  name that moved across each (`config.ErrNameMoving`, naming the name and
  saying to start again once the link is settled); its holder is told that
  file, resolved at the read (`RecordFile(target, raw)`, which resolves
  nothing itself): until then `RecordFile` resolved the name a thousand
  lines of boot after the read, so a link repointed in between, to a twin
  holding the same bytes, had the holder record a file serve never read,
  and the first console save went into it (the Claude review's Low;
  `TestServeRecordsTheFileItsBootReadOpened`, red before: 200, the twin
  written). `serveOpts.afterConfigRead` is the window between the read and
  the rest of the boot.

  **The fifth round** (the same day) took the reviews of the fourth; the
  turn's half is config-lifecycle.md's, where the name is now resolved after
  every read a decision rests on, last, and the window no turn closes is
  stated once. Through the CLI writers: bind's own comparison in its
  `SaveIf`, during which a link at the config's name was repointed to a
  twin holding the same bytes, let the binding be written into the file the
  name no longer opened, exit 0; now the name is resolved after that
  comparison and bind refuses, saying it now opens another file and to run
  it again (`TestBindRefusesANameRepointedDuringItsOwnComparison`). Library
  add, whose turn's own read the repoint fell inside, wiped the library
  index for its second root before its save refused; now the turn refuses
  before its writer's change (`TestLibraryAddRefusesANameRepointedDuringItsTurnsRead`,
  red before: `bridge.db` made by the wipe). A restore's check, during
  whose read the repoint fell, let every file before bridge.yaml be
  restored; now it refuses with nothing restored
  (`TestARestoreRefusesANameRepointedDuringItsCheck`). Serve's boot read
  reads the file it resolved, never the name again, and a name no
  resolution could follow at any of its three reads boots serve on the last
  read through the name with no file: its console refuses every save before
  its turn, saying the bridge could not resolve the file the name opens
  when it started and to restart once the name resolves, and goes on
  refusing once it does (`TestServeRefusesConsoleSavesWhenItsBootCouldNotResolveTheConfig`,
  red before: 200, and the file written). A `bridge library add` beside
  such a serve hands its change to the console, and is refused the same
  way; a CLI writer that takes its own turn is not.

  **Don't take the data dir's lock while holding bridge.yaml's, take
  bridge.yaml's across init's prompts or a restore's prompt, decide a
  library change on the read before the turn, restore into targets worked
  out from a config that changed since it was read, or through a name
  repointed since, say nothing was written after a restore wrote files, or
  that a queue went that the restore kept, clean a config's name in an
  entry, resolve serve's config file anywhere but at its boot read, boot a
  holder on a file its boot read was not of, retry a writer on the file a
  moved name opens, or write a restored bridge.yaml past its turn.**

## Player floor and reset (B524 round 5, 2026-10-24)

**The player floor is monotonic across identities; reset keeps it byte-identical. Only `--discard-floor` drops it after BOTH local seals, replacing a damaged seal entry, with a fresh seal synced beside it before the entry moves, only once `player-initialized` holds a durable seal; its one-shot exemption opens after the floor rename and directory sync, and any floor, UUID or queue evidence consumes it before completion returns. A seal entry that is not a regular file is still a seal: completion keeps it unopened and records the seal in `player-initialized` beside it, only discard replaces it, and no advice says to move a seal record aside; a discard stopped at the seals says to run it again. A damaged initialized marker stays present with its exemption spent. Only a start's preflight warns about it, once per serve boot or bind, and only beside a readable, unexhausted floor naming the UUID: move it aside, or keep it when its sealed bit is the only intact seal; never discard that floor. Beside a lost floor nothing warns, and the refusal says not to move it. Player-file errors turn the player off with a reason, never stop serve; seal-repair errors block neither backup nor identity-free restore, only a busy one is deferred, and a failed one's advice names its cause, claiming a recurrence only for a persistent one. Serve boot, start, bind, reset, backup and restore adopt UUID-only upgrades at 2^31 plus a random value below 2^24 on BOTH counters; the bound is counter space, including skipped reservations, never issuance count. Restore adopts first, from its own random source, before taking max(snapshot, live) plus its jump; a snapshot listing no floor and holding none counts as 2^31 + 2^24 from an older writer and is refused from this one, and one holding a floor it does not list is refused as a damaged manifest. Start and backup require a matching UUID/floor; exhausted minting changes neither. Never repair a live floor from a snapshot. Two live boxes, older cross-box snapshots, pre-upgrade counter space reaching 2^31 and loss of both seals remain unsupported.**

Ordinary reset keeps the box's numeric upper bounds across all identities,
including identities displaced by successful or interrupted restores. It needs
no UUID history or pending transaction flag: after a durable queue unlink, an
old UUID can run empty; after the UUID unlink, the checked wrapper mints a new
UUID and restamps by max. Reset reads the floor strictly, with Lstat deciding
absence, and never reads the UUID it removes. A directory at the UUID or queue
path refuses; malformed UUID bytes, inaccessible UUID bytes and dangling UUID
links can be unlinked safely while the floor remains intact. The command keeps
its exclusive lock, refuses the lock fallback, probes admin before and after
confirmation, and lives beside backup and restore, not under output player.

`player-initialized` and `player-floor-discarded.json` are version-1 local
markers with one bounded, nonblocking reader. Existing entries, including
unparsable bytes and non-files, count conservatively as initialized or sealed.
Only a well-formed newer version requires a newer bridge. Ordinary lifecycle
operations preserve their bytes except when completing a lone seal or consuming
a sealed exemption on allocation evidence or the first mint after discard. Discard records `sealed:true` in the initialized
marker as independent evidence, after the seal file is durable and before any
queue, UUID or floor is removed. It replaces an unparsable or non-file marker
with a fresh one, staged and synced beside it before the old entry is renamed
aside (B539), a damaged seal entry only once `player-initialized` holds a
durable seal (round 11). A damaged initialization marker beside a matching floor is kept as evidence;
its start preflight warns to move it aside, or to keep it when its sealed bit is the only intact seal, without discarding the floor (round 10). A missing floor beside a UUID, queue or initialization marker is
loss, except the UUID-only upgrade and the unconsumed post-discard exemption
described below.
A queue without UUID refuses minting, because an unstamped v1 queue must not
appear under a new device. Existing valid floors gain the initialization marker
before a UUID is published. Pristine initialization commits a v2 zero floor,
then marker, then UUID. The UUID writer fsyncs the parent after rename.

`--discard-floor` durably seals every future incoming identity restore, unlinks
queue then UUID with directory barriers after BOTH seals are durable, renames even a non-file floor aside
with a random suffix, then fsyncs the directory. Rerunning completes any
interruption. After the queue and UUID unlinks, the floor rename and its directory sync, discard records
`sealed:true,minted:false` in the initialization marker. This permits one fresh
floor. The next mint durably records `sealed:true,minted:true` after the floor
and before UUID publication. Ordinary reset preserves that consumed state;
losing the floor afterwards refuses start. A retry of an interrupted mint with
a durable floor reestablishes the marker barrier before publishing a UUID. A sealed box still restores ordinary
data from a snapshot copy without UUID and floor; an identity-bearing restore
refuses before restoring data and explains that remedy; completing a lone local
seal and adopting a UUID-only destination may precede snapshot writes. The seal never leaves
through backup. Ordinary reset does not free exhausted counter range.

**A snapshot captures UUID then floor through bounded Windows sharing retry, refuses a captured UUID without a readable floor, and records `playerFloorRequired` in its manifest. Schema stays 2; local initialization and discard markers never travel in backups. Restore refuses that writer's UUID without its listed floor, refuses a floor the snapshot holds but its manifest does not list as a damaged manifest, and bounds an older writer's floorless snapshot at 2^31 + 2^24, completes a lone local seal, checks the seal, adopts a UUID-only destination from restore's own random source (the adoption's draw, then the jump's), then checks the live floor and both exhaustion bounds before restoring any data, then writes max(snapshot, live) plus the jump, ensures the initialization marker, durably clears a replaced identity's queue and writes the UUID through its own durable writer.**

The engine independently refuses unreadable or corrupt reservation data instead
of moving it aside and starting at zero. Queue-corruption recovery is separate.
A non-file is refused without waiting and names moving it aside, rather than
sending the operator to an ordinary reset that also refuses the floor. Restore
preserves the existing max-and-jump and queue ordering, without history hooks.
A listed snapshot floor still must parse and name its captured identity; older
unmarked snapshot compatibility remains subject to the documented migration
margin. The exhausted-destination remedy is a snapshot copy without UUID and
floor, preserving the box's identity, or explicit discard with its permanent
seal. Local files cannot detect full data-directory rollback or erasure.

Tests cover X at 2e9 → restore Y → reset → old X or Y, including an interrupted
UUID commit followed by a refused start, reset and a real engine restart, every discard crash point,
marker barrier failures, initialization order, byte-identical ordinary reset,
seal and lost-floor preflight with no writes, backup without a floor, and both
serve and bind through the shared wrapper. Negative controls and gate outputs
are in the round-five scratch report. The design replaces the history model
after four rounds failed to converge: remembering names was unnecessary while
preserving their shared numeric bound supplies the same-box proof directly.

### Round 6: pairing, recovery and the upgrade boundary

Before any mint write, the floor must leave room for the engine's skipped
4,096-value block and its first value in BOTH counters (`Reservation.Exhausted`,
using `startCounter`). Exhaustion advises discard and leaves the floor byte
identical, with no UUID written. A local seal may be completed or consumed before that preflight; a UUID-only
restore destination is adopted before its live-bound check. Restore's stricter bound additionally
reserves its largest jump and 2^24 values of running room.

A current UUID is a regular directory entry by Lstat, never a symlink, and the
floor must name exactly it. A successful mint restamps the floor before writing
the UUID; restore likewise writes its named floor before UUID. An interrupted
restore can leave mismatched names: start and backup refuse, and ordinary reset
keeps both numeric bounds while allowing the next mint to restamp them. A live
version-1 reservation has no UUID and cannot establish this pairing: ordinary
reset retains its counters and the next mint writes version 2. Historical
version-1 snapshot floors remain accepted as migration inputs; restore names
the new live floor before publishing that snapshot's identity.

Backup captures UUID first, then reads the floor through the bounded sharing
retry and requires the same name. A concurrent discard and mint cannot publish
an old UUID with the new identity's zero floor. A missing floor names discard
and its seal consequence while Lstat still finds the UUID; only a UUID that
vanished between captures receives retry advice. Restore's floor-loss error
also explains the snapshot-copy remedy and discard's permanent consequence.

A UUID-only upgrade is a valid regular UUID with no floor, queue, initialization
marker or seal. Serve boot, start, bind, reset, backup and restore adopt it without sealing.
Start, bind and backup retain the UUID; reset adopts its floor before removing
the UUID. Both counters start at 2^31 plus a random value below 2^24, followed
by the durable initialization marker. A UUID-only restore destination has an
unknown history, so restore adopts it first, then takes the larger of that
new floor and the snapshot bound before adding its smaller jump. Serve performs this adoption under server.lock before TLS
and live backups, even when playback is disabled, without minting on a pristine
box. Backup takes that lock for adoption and rechecks the captured UUID;
if adoption is busy, stop the service and run the backup again. It also takes
the lock for seal completion: a busy completion is logged and left to the next
lifecycle operation, and any other failure is logged with its error and the
advice to restore write access, since it recurs at the next serve boot or
start (round 10); backup still checks the captured pair.
A main-era engine can hand out token 4097 without saving a queue, so losing ONLY
its floor looks identical. The adoption bound is pre-upgrade counter space below
2^31, including skipped reservations; a count of values handed out cannot bound
that space. Reaching the upper half before floor loss remains unsupported.
Additional lifecycle evidence makes an absent floor a loss, except immediately
after a completed explicit discard and before its first mint.

### Round 7: one-shot exemption and completing a lone seal

A seal file without the initialized marker's seal bit is completed by the next
lifecycle operation, after verifying the first record's durability. Start, reset,
restore and serve boot hold the lifecycle lock; backup takes it for this repair,
so it cannot overwrite a concurrently minted bit. Completion preserves a consumed
bit and marks it consumed when UUID, queue or floor evidence already exists.
Malformed initialized records beside a valid seal stay untouched for conservative
floor-loss checks; completion verifies the seal and returns. With a readable
floor naming the UUID they start, with a warning to move the marker aside.
Their exemption is spent; later floor loss still refuses. Newer records are never rewritten. Restore may complete the local seal before refusing an
identity import, but writes no snapshot data. Ordinary floor and exhaustion
refusals without an incomplete seal remain free of writes.

The real-engine regression starts from main's initial zero reservation without
markers, reads token 4097 without creating a queue, stops, deletes only the floor,
adopts, and starts again above the jumped floor. Another runs discard, mint,
engine allocation, ordinary reset and floor-only loss: the next start refuses.
Barrier regressions pin floor → consumed marker → UUID and the first seal's
barrier before the second record. The disabled-player boot and live-backup tests
cover the upgrade while server.lock is held.

The discard evidence is duplicated in the seal and initialized marker rather
than added to reservation JSON. Every floor writer, including the engine,
leaves these independent files untouched. After both seals are established, losing, truncating or replacing any
ONE file cannot reopen identity imports, before a fresh start or after engine
reservation writes. The smallest deletion set that CAN reopen imports is both
`player-floor-discarded.json` and `player-initialized`, after a fresh identity has
a readable floor (or immediately after discard before a new identity exists).
This two-file loss, full storage rollback, two live holders of one UUID, and
non-latest cross-box snapshots are documented limits, not guaranteed recovery.
The new single-file-loss matrix, second-seal barrier test and recovery tests
pin the supported boundary. Both markers remain local and excluded from backup.


### Round 8: counter-space bound and player-only failures

A main-era start skips a 4,096-value block and reserves the next block, even
without edits or a saved queue. After 4,097 starts it has published only 4,097
tokens, yet the highest is 33,558,529 (8192 × 4096 + 4097). The old adoption
jump from zero could fall below those values for every draw. Adoption now
sets BOTH counters to 2^31 + r, with 0 <= r < 2^24. It covers pre-upgrade
counter space strictly below 2^31, including skipped and reserved values;
it cannot establish an absent floor's actual history. At the largest draw,
2,130,706,432 values remain before uint32 exhaustion, comfortably past the
engine's required 4,096 skipped values plus its first value. Restore still
uses max(snapshot, live) + 2^24 + r, after adopting a UUID-only destination
as described in round 9; such a destination has no known bound beforehand.
`TestAdoptionBoundsReservedSpaceAcross4097Restarts` runs the real engine for
every start, deletes only the floor, adopts, and checks both first values.

Completion consumes a valid sealed marker lacking minted whenever ANY floor,
UUID or queue entry exists, even if the other seal file was lost. It does so
before returning, including reset and backup; backup locks before mutating.
This covers old discard markers whose writers never recorded minted. A fresh
discard with no evidence keeps its one-shot exemption. Explicit discard writes
sealed:true,minted:false only after queue and UUID unlink barriers, the floor
rename and its directory sync; a crash before that write is conservative.
`TestDiscardExemptionWaitsForFloorRenameBarrier` pins this final barrier.

A present invalid initialization marker beside a valid seal is not a lone
seal. Completion verifies the first seal's barrier and leaves the damaged
marker untouched. With a matching readable floor, start accepts it and warns
to move it aside; a later floor loss refuses. Serve records other adoption,
completion or preflight errors as a player-off plan and keeps booting, even
with playback disabled. Backup logs a failed local repair and still applies
its strict UUID/floor capture checks. Restore retains a completion error only
for an identity import, so an identity-free snapshot still restores ordinary
data. Neither path copies local markers. `TestDamagedSealedMarkerKeepsBackupAndIdentityFreeRestore`,
`TestSealedExemptionConsumedByEveryEvidenceAndLifecycle`,
`TestCompletionErrorDoesNotBlockBackupOrIdentityFreeRestore`,
`TestPlayerFileErrorsDoNotStopServeBeforeTLS` and
`TestServeKeepsLibraryWithDamagedSealedPlayerMarker` pin these boundaries.


### Round 9: restore adopts before its jump; damaged markers preserve service

A UUID-only destination may have run a main-era engine whose sole floor was lost.
Restore calls the shared adoption path under its existing exclusive lock before
reading the live bound. Adoption writes both counters at 2^31 + r, then the
initialization marker, with the same durability order as start. Only then does
restore add 2^24 + r to max(snapshot, adopted live). The regression runs 8,193
real engine starts without markers or queue edits, captures the first 8,192
reservation, deletes only the floor, and restores before upgraded service boot.
Both first values exceed every token published before restore.

A damaged initialized marker beside a floor naming the current UUID is accepted
by every lifecycle operation. It stays present and grants no missing-floor
exemption. The warning names player-initialized and advises moving it aside;
start re-ensures it, and completion beside a valid seal records sealed and minted.
A later floor loss refuses. Missing or unreadable floors retain discard advice.
Backup's busy-stop-and-retry advice applies to adoption alone; busy or failed seal
completion is logged and left to the next lifecycle operation (round 10 below: only
a busy completion is deferred).

### Round 10: floorless snapshots, restore's draws, busy completion and one warning

A snapshot whose manifest lists no floor beside its player UUID (the unmarked
case of `snapshotReservationFloor`) cannot bound what that identity handed out.
This bridge never writes one with a readable UUID: backup refuses a missing
floor unless the box is in the UUID-only state, which it adopts first and then
captures, and every manifest it writes records `playerFloorRequired`. Restore
refuses such a snapshot of this bridge's making, whose floor was stripped, with
the snapshot-copy remedy before any write. One from an older writer, whose
manifest has no such record (`bridgeVersion` is the build's stamped version and
cannot tell the two apart), takes the adoption ceiling, 2^31 + 2^24, as its
bound. One helper, `floorlessSnapshotBound`, applies both, right after
`snapshotReservationFloor` and only when nothing is listed; `marked` stays
false, so the missing-mark warning still fires, and a listed floor that is
missing or corrupt keeps its refusal. Restore then writes max(ceiling, live)
+ 2^24 + r. The 2^24 term covers a source that upgraded and adopted after its
snapshot: adopted at the largest draw and started once, its durable floor is
2,164,273,151 (2^31 + 2^24 + 12,287, as measured), below the restored
2^31 + 2^25. Codex's 8,193-start main-era
sequence now restores at exactly 2^31 + 2^25 with draw zero and
2^31 + 2^25 + 2^24 - 1 with the largest; it restored at 2^24 before.

Restore adopts a UUID-only destination with its own random source
(`RestoreOptions.Random`, crypto/rand unless a test injects one), the
adoption's three bytes before the jump's: six there, three elsewhere. Every
other adopter keeps crypto/rand. The adoption draws before it writes, so a
source that fails at once leaves the UUID-only state untouched; one that
fails after the adoption's draw leaves the adopted floor durable and no
snapshot data, and a retry jumps from that floor.

Only a busy server.lock defers a backup's seal completion, which backup logs as
deferred to the next lifecycle operation. Any other failure, such as a data
directory backup cannot write, recurs at the next serve boot or start: backup
logs it as a failure, with its error and the advice to restore write access to
the data directory, or a marker refusal's own remedy. Busy adoption still
refuses the backup and says to stop the service and run it again.

Marker reads log nothing. `CheckPlayerStart` is the one place that warns, once
per call: serve boot's preflight, once per boot, covering the player that boot
starts, and bind's. The minter and every other lifecycle operation preflight
silently, and no process state hides a later boot's warning. The move-aside
warning needs a readable floor that names the current UUID and is not
exhausted, beside a present, invalid marker. When that marker reads
`sealed:true` and no valid `player-floor-discarded.json` exists, its sealed bit
is the only intact seal: moved aside, the next start would write an unsealed
marker and reopen identity restores, so the warning names the seal and says to
keep it. The player runs as it is; `bridge reset-player --discard-floor`
rewrites both records at the cost of the identity and its queue. Beside a lost
floor nothing warns, and every floor-loss refusal with the marker present says
`do not move player-initialized aside`.

`TestThisBridgeNeverCapturesAFloorlessIdentity`, `TestThisBridgesSnapshotWithoutItsFloorIsRefused`,
`TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling`, `TestLegacyCeilingCoversASourceThatAdoptedAfterItsSnapshot`,
`TestLegacyCeilingYieldsToEachHigherLiveBound`, `TestLegacyFloorlessSnapshotOntoAUUIDOnlyBox`,
`TestALegacySnapshotsListedFloorStillMustRead`, `TestRestoreAdoptsWithItsOwnRandomnessFirst`,
`TestRestoreAdoptionDrawFailureWritesNothing`, `TestRestoreAdoptionDrawThenFailureKeepsTheAdoptedFloor`,
`TestBusySealCompletionIsLeftToTheNextLifecycleOperation`, `TestFailedSealCompletionKeepsItsErrorAndNamesWriteAccess`,
`TestBusyAdoptionStillAsksToStopTheService`, `TestFloorLossRefusalsNeverAdviseMovingTheMarker`,
`TestMoveAsideWarningOncePerStart`, `TestMoveAsideAdviceNeedsTheFloorToNameTheUUID`,
`TestADamagedMarkerHoldingTheOnlySealIsKept`, `TestServeBootRefusesALostFloorWithoutTheMoveAsideWarning`,
`TestServeWarnsOnceEachBootBesideADamagedMarker` and `TestBindWarnsOnceBesideADamagedMarker` pin these boundaries.

### Round 11: an unlisted floor, a non-file seal and completion advice by cause

Every writer that captures the floor lists it beside the UUID, those from
before the `playerFloorRequired` record included (main since 5a0, and round
9's), so a snapshot that holds `player-ids.json` and does not list it has a
damaged file list. On round 10's code one bad byte in that entry
(`player-ids.json` read as `player-ids.jsoo`) sent such a snapshot down the
older writer's path, and its intact floor gave way to the adoption ceiling.
Codex's sequence, run through `bridge restore`: a box adopted at 2^31 starts
4,097 times, is captured by such a writer, has that entry corrupted and is
restored onto a pristine box at draw zero; the floor came out at
2,181,038,080, and the restored player's first startup token, 2,181,042,177,
was the source's last. When the manifest does not list the floor, restore now
Lstats `<snapshot>/player-ids.json`, never opening or following it
(`unlistedFloorError`). An entry of any kind refuses before anything
snapshot-derived is written, the adoption included, with the snapshot-copy
remedy or a newer snapshot of this box, never just another one. Only
ErrNotExist is a genuine absence, which `floorlessSnapshotBound` then treats
as round 10 does; any other Lstat error refuses with itself. Restore's own seal
completion still runs first: it adds local seal evidence only.

The manifest's word for a file's absence is taken in three other places, none
of which can weaken the floor. An unlisted `player-uuid` makes the restore
identity-free: the box keeps its own identity and floor byte for byte, and no
identity the snapshot held comes back. An unlisted console account, library,
token store, TLS pair, config or harvest file is not restored and holds no
part of the floor. Listing, pruning and the restore preflight read manifests
for display, retention and schema only.

A seal entry that is not a regular file, a directory, a symbolic link or a
named pipe, is still a seal, as `playerRestoresSealed` already counted it.
`ensurePlayerMarker` refused one with "is not a regular file: move it aside",
which start, a backup's completion and every adoption printed, and following
that advice dropped the box's only seal record. It now keeps such an entry
under either marker's name, never opening or following it (a sibling's
barrier), so completion goes on to record the seal in `player-initialized`
beside it: twice recorded. Only discard replaces it. Beside such an entry a
damaged marker reading `sealed:true` gets the move-aside advice, since start
seals the marker again there; beside a missing seal file, or one whose bytes do
not read, the advice still says to keep the marker. Round 10's reason, that
the next start would write an unsealed marker, holds beside a missing seal
file; beside one whose bytes do not read, start seals the marker again too, and
the advice keeps the intact record either way. No move-aside or remove text in
`internal/backup` or `cmd/bridge` can now be printed for
`player-floor-discarded.json`, or for `player-initialized` holding the only
seal: the move-aside warning is given only where the marker is not the only
seal record, and the other texts concern `player-uuid`, `player-queue.json`,
`player-ids.json` or a copy of a snapshot.

Discard repaired a damaged seal entry by renaming it aside and then writing a
fresh one (since B539 the fresh one is written first), and it repaired the
seal file first: with `player-initialized` gone, a crash between that rename
and the replacement left no seal record. With the seal entry damaged, discard
now seals `player-initialized` first, so each record is renamed aside only
while the other holds a durable seal.

A failed seal completion's advice names its cause and claims no more than it
supports. Each step tags its error where it happens with what it was reading or
writing and the access that needed (`accessError`): an Lstat and a barrier
need the data directory, a marker's read needs that file, a marker's write
needs write access to the data directory, and opening server.lock needs that
file. A permission error names that path and access, and says serve boot and
start fail on it too when run as this user, since a CLI backup need not run as
serve does; a read-only data directory says so; a marker refusal keeps its own
remedy; any other error, an interrupted or refused lock or an I/O error, keeps
only its error. `fsutil.TryLock` maps flock's EWOULDBLOCK, the same Errno as
EAGAIN on every unix it builds for, and Windows' `ERROR_LOCK_VIOLATION` to
`ErrLocked`, nothing else, and retries EINTR inside; the mapping is a pure
function now, `lockedErr`, pinned per platform.

Round 7's test that required a non-regular lone seal to refuse startup is
replaced: `TestANonFileLoneSealIsCompletedAroundIt` completes the seal around
it, spent on a box minted before and open as the one-shot exemption on a box
with no allocation evidence. Three older tests that made their completion
error from a directory seal now use one a newer bridge wrote, and restore's
own completion may write `player-initialized` beside a dangling seal before
its refusal. `TestRestoreRefusesAFloorItsManifestDoesNotList`,
`TestRestoreRefusesAnUnlistedFloorEntryWithoutFollowingIt`,
`TestAnUnlistedFloorTheSnapshotDoesNotHoldIsStillAbsent`,
`TestADamagedManifestRefusesBeforeAdoptingTheDestination`,
`TestARefusedDamagedManifestOnlyCompletesTheLocalSeal`,
`TestRestoreKeepsTheBoxIdentityWhenItsUUIDEntryIsDamaged`,
`TestAnUnlistedFloorRefusesUnlessItIsGenuinelyAbsent`,
`TestANonFileSealIsKeptAndRecordedTwice`, `TestANonFileSealIsCompletedByABackup`,
`TestAFIFOSealIsKeptWithoutOpeningIt`, `TestMoveAsideAdviceCountsANonFileSealAsASeal`,
`TestDiscardRecordsASecondSealBeforeRenamingADamagedOne`,
`TestDiscardFloorRepairsANonFileSealFromTheStateItself`,
`TestSealCompletionAdviceClaimsNothingForATransientError`,
`TestSealCompletionAdviceNamesTheReadItLacked`,
`TestBusySealCompletionAcrossProcesses` and `TestLockedErrMapsOnlyAHeldLock`
pin these boundaries.

### Round 12: one remedy for a directory failure, a file's own access for a file

Round 11's advice named the access a step needed, and three kinds of failure
named one the user already had. An Lstat, which needs the directory's search
access, said read; the marker write's directory sync, in a directory the user
may write and search but not read, said write; and the lock file's open, for
reading and writing, said write access to `server.lock`, also where the file
was missing and the open had failed creating it. A remedy is advice the
operator follows, not a diagnosis, so every failure at the data directory
itself now takes one remedy that always suffices (`directoryAccess`): on unix,
restore read, write and search (execute) access to the data directory and each
directory above it, or clear an immutable flag or ACL on it; on Windows,
restore full control of the data directory. These failures are an entry's
Lstat (in `markerEntry`, `playerEntryNotAFile`, `playerAllocationEvidence` and
the lock file's check), the barrier, which opens the directory to sync it and
creates nothing, and a durable write's temporary file, rename and directory
sync. `ensurePlayerMarker` now takes the entry's kind from the one Lstat that
read the marker (`markerEntry`), so it has no Lstat of its own.

A file's own open names the file (`fileAccess`). A marker's open or read
needs read access to it; an EPERM rather than an EACCES adds "or clear an
immutable flag or ACL on it", which read access alone does not undo. The lock
file is opened for reading and writing and created when missing, so a
permission failure there is told apart by an Lstat through the data
directory's root, which follows nothing outside it (`lockFileAccess`). A
regular file needs read and write access to it, with the same EPERM addition.
No entry, a link the root resolves to nothing, a root that will not open and
an Lstat that fails take the directory remedy: the open failed creating the
file, or never reached it. Any other entry keeps the lock file's write remedy.
The claim that serve boot and start fail on it too holds when they run as this
user, as before.

On the owner's Mac a backup's own lock open needs to read the data directory
(`os.OpenRoot`), so in a 0300 directory it fails there, with the directory
remedy, before completion reaches the seal's barrier; the barrier case drives
the completion itself. The identity-free restore of a snapshot whose
`player-uuid` entry is damaged is now pinned to its one outcome: it completes,
and the box's identity and floor stay byte for byte. Round 10's failed-write
case now asserts the directory remedy.
`TestALockFileThisUserCannotReadNamesReadAndWriteAccess`,
`TestALockFileTheOpenCouldNotCreateNamesTheDirectory`,
`TestAnUnreadableDataDirectoryFailsAtTheSealBarrier`,
`TestAnUnreadableDataDirectoryFailsAtTheMarkerWrite`,
`TestADataDirectoryThisUserCannotSearchFailsAtTheFirstLstat`,
`TestEveryDirectoryStepNamesTheDirectory`,
`TestAMarkerOpenRefusedAsNotPermittedNamesTheFlag`,
`TestAnImmutableLockFileNamesTheFlag`, `TestAnyOtherLockEntryKeepsItsAdvice`,
`TestTheDirectoryRemedyPerPlatform` and
`TestRestoreKeepsTheBoxIdentityWhenItsUUIDEntryIsDamaged` pin these
boundaries.

### Round 13: Windows, one union for a named entry, and busy only for a held lock

PR #1346's first CI run, at 032934fe, was B524's first on Windows, and eleven
cases failed there; nomos, a Windows 11 host on Go 1.27.3, failed the same
eleven at 9a21f8d2. None was a fault of what Windows production does.

- Four tests, seven of the eleven with their subtests, built a fixture with
  the durable writer, which Windows production refuses: the player's minter
  writes its floor durably (`engine.WriteReservation`) and its identity writer
  (`config.WritePlayerUUIDFile`) ends in a directory probe, and with no
  directory barrier both return `atomicwrite.ErrNoDirectoryBarrier`.
  `TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo` seeds its
  identities with plain writes on Windows, and
  `TestSnapshotRefusesAReservationStillHeldOrCancelled` seeds its UUID with
  one, since a backup reads the identity there all the same; the historical
  restores keep the writer of 71af3f26, which returned once its rename landed.
  What each asserts is unchanged, and Windows proceeds where they assert it: a
  player-uuid that is not one UUID is refused before any write
  (`snapshotPlayerUUID`), an identity-free restore writes no player state, and
  a backup's floor read retries a held file.
- `TestAnUnlistedFloorRefusesUnlessItIsGenuinelyAbsent` used ENOTDIR as an
  Lstat error that says neither absent nor present. On Windows
  `syscall.ENOTDIR` is ERROR_PATH_NOT_FOUND, which reads as no entry; it uses
  ERROR_SHARING_VIOLATION there.
- `TestBackupNeverPairsAcrossDiscard` and `TestBackupRetryOnlyWhenUUIDVanished`
  changed the live identity from the snapshot copy's rename, while the copy
  still held the live player-uuid open without delete sharing. Windows
  refused the removal, the hook returned that error, and the rename's retry
  ran again on a temporary file the first attempt had already renamed: "cannot
  find the file specified". The change now runs from a seam
  (`afterIdentityCopy`) once the copy has closed its source, exactly once, and
  the pairing assertions stand. Production retries a rename after its
  temporary file was consumed only where a rename lands and still reports
  failure, as a network share whose reply is lost could; that is not changed
  here.

A permission failure of a step that names an entry, a marker's read or durable
write or the open or lock of `server.lock`, now takes one union rather than a
classification by entry (`entryAccess`, `entryRemedy`). Round 12's
classification still missed an immutable or append-only flag on the marker a
rename replaces, a link chain to a file that refuses reading, a dangling link
into a directory that refuses writing, and a Windows ACL that denies reading
the lock file. On unix the union is the access the step needed on the entry
(read for a marker's read, read and write for `server.lock`), the directory's
remedy, any immutable or append-only flag or ACL on the entry and on what it
links to cleared or a link at it replaced with a regular file, and in a sticky
directory the entry owned or the sticky bit cleared. On Windows it is full
control of the data directory and of the entry, its read-only attribute
cleared or a link at it replaced with a regular file, and any program holding
it open closed. A failure at the data directory alone, an Lstat or the
barrier, keeps the directory's remedy, which now says immutable or append-only
flag. The lock file's per-entry classification and the EPERM branch are gone.
A `server.lock` that is a link leading out of the data directory is no
permission failure: the data directory's root refuses it unfollowed
(`path escapes from parent`), and a seal completion logs it, or an adoption
refuses the backup with it, with that error alone, as in round 12; round 12's
review had taken it for followed.

A backup's adoption takes `server.lock` through the seal completion's open and
lock seam (`openSealLock`, `tryLock`), and only a lock another open holds
(`fsutil.ErrLocked`) makes it busy, with its advice to stop the service; any
other failure to open or take the lock refuses the backup with its own error,
and the lock file's union for a permission error (`adoptionFailed`). The seal
completion's lock refused for permission carries the same union.

On nomos the SSH session holds an administrator's token with SeBackupPrivilege
and SeRestorePrivilege enabled, and Go opens a file with
FILE_FLAG_BACKUP_SEMANTICS, which then reads through any deny entry: the
deny-read case drops both privileges for its duration.
`TestOnlyAHeldLockMakesAdoptionBusy`,
`TestALockRefusedForPermissionNamesTheLockFile`,
`TestTheEntryRemedyPerPlatform`,
`TestALockFileLinkNamesTheEntryAndWhatItLinksTo`,
`TestAProtectedMarkerNamesTheMarker`,
`TestALockFileLinkLeadingOutIsNeverFollowed` and
`TestALockFileDeniedReadingNamesTheEntry` pin these boundaries, beside round
12's permission cases, which now assert the union.

### Round 14: a TMPDIR on tmpfs, and no identity left by a refused start

PR #1346's CI run on Ubuntu 26.04, where `/tmp` is tmpfs, failed
`TestARestoredIdentityHandsOutValuesPastItsSnapshot` at its seed write.
Round 5 made `config.WritePlayerUUIDFile` end with `atomicwrite.ProbeDurable`,
which refuses a root on storage that does not outlive a reboot
(`atomicwrite.ErrVolatile`), and the write came before the test's skip.

On any host, a test-only `init` that makes the medium check answer volatile
for a whole test binary stands in for that TMPDIR. Run that way at 51d2900b,
`cmd/bridge` failed 35 tests and `internal/player/openhome` one; main at
9e7b2675 failed 27 in `cmd/bridge` and none in openhome, where the test
skipped. Nine discovery and OpenHome serve tests that wait for a player that
cannot start were left out of both runs; with a short deadline they fail alike
on both. Twelve tests are B524's to fix: ten failed on B524 alone, and two
more failed on both but, on B524, at their own identity write. Each writes the
player's identity, floor or marker in its temporary directory, through a
fixture or the bind, serve start or restore it drives, and B524's writers
refuse that storage. Each now answers the medium check as a disk does while it
runs (`persistentMedium` in `cmd/bridge`, the seam inline in openhome). The
other 24 fail on main too: they need the engine running, and the engine
refuses volatile storage there as well. A TMPDIR on disk (the workflow's
PR #1349) is their fix, not this round's.

`TestARestoredIdentityHandsOutValuesPastItsSnapshot` had in fact been skipping
on every host since it reached B524 with main's merge at 032934fe. It seeded a
bare `player-uuid`, and since round 5 the engine refuses an identity whose
floor is missing ("player floor is missing"). It now mints its source as
serve's start does (`backup.EnsurePlayerUUID`: the floor, the marker, then the
UUID). It fails, rather than skips, on any refusal except storage the engine
cannot keep IDs on (`ErrBarrierFailed`, `ErrMountsUnknown`,
`ErrMediumUnknown`).

In production, a refused probe leaves no identity behind.
`config.WritePlayerUUIDFile` does rename the file into place before it probes.
But serve's start and bind mint through `backup.EnsurePlayerUUID`, which
writes the floor and the marker through the strict writer first, and that
writer refuses volatile storage before it creates anything; the UUID comes
last. A start on an identity that is already there probes it at its marker
(`ensurePlayerMarker`), and the engine probes again as it starts.
`config.EnsurePlayerUUID` does leave its file when its probe refuses, and a
second call returns it unprobed, but nothing in production calls it.
`TestAMintRefusedForVolatileStorageLeavesNoIdentity` and
`TestServeOnVolatileStorageRefusesEveryStartAndKeepsNoIdentity` pin this.

### B539: a fresh seal synced before a damaged one moves (2026-10-28)

B539 came from round 10: a sealed, minted box loses `player-initialized`, its
seal file becomes an entry that is not a regular file, serve refused with
"player-floor-discarded.json is not a regular file: move it aside", and
following that let the next start write `{"version":1}` and an identity
restore succeed. Round 11 had removed that refusal, so on main at 2c7be326
that sequence ends sealed. What still ended unsealed after a discard was the
discard's own replacement of a damaged record: `sealPlayerMarker` renamed the
entry aside and only then wrote the fresh record, so a failure of that write,
or a crash during it, left the seal's name empty, and the discard said nothing
but the error. Round 11's order kept `player-initialized` sealed meanwhile,
but the next start spent it, and once it was lost too the start after wrote
`{"version":1}` and an identity restore was accepted.

`sealPlayerMarker` now replaces every damaged record through
`atomicwrite.ReplaceDurable`: after every refusal the strict writer makes, the
fresh record is staged beside the entry and fsynced; then the entry is renamed
aside to a unique name and the staged file into its place, with no write and
no sync between those two renames; a refused second rename puts the entry back
and syncs the directory again, and one that cannot says where the entry is
kept; once both renames have landed the chain is synced. No failure of a
single step leaves the name empty. Between the two renames it is empty, since
no rename puts a file in a directory's place in one step, and round 11's order
has made the other record's seal durable before then; losing that record as
well is the loss of both seals, which stays unsupported. A discard that stops
at its seal records says it discarded nothing and to run it again once the
cause is fixed; a record a newer bridge wrote keeps its own remedy.

Refusing a non-regular seal at start, with `--discard-floor` as its advice,
was considered and not taken: round 11 keeps it as a seal at every check, and
a refusal would turn the player off on a sealed box, its only remedy giving up
the player's identity and queue.

`TestTheSealWalkEndsSealedFollowingTheBridgesOwnAdvice` walks B539's four
steps through `bridge reset-player`, serve's boot and `bridge restore`,
following every piece of advice the bridge gives about the seal; with round
10's refusal put back it fails as B539 described.
`TestAFailedHealKeepsTheSealEntryThroughTheNextStarts`,
`TestEveryCrashPointOfTheSealsHealLeavesTheBoxSealed`,
`TestAFailedStepOfTheSealsHealKeepsTheEntryItFound` and
`TestANonRegularSealIsASealAtEveryCheckUntilDiscardHealsIt` pin the rest, and
the `ReplaceDurable` tests in `internal/atomicwrite/replace_test.go` its
contract. `TestDiscardRecordsASecondSealBeforeRenamingADamagedOne` refuses
every rename into the seal's name, so it now drives the double failure, and
passes unchanged.

## B603 — bridge doctor's player-storage line (2026-10-30)

- **`bridge doctor`'s `player-storage` line is the strict writer's own medium verdict on the data dir (`atomicwrite.InspectMedium`), one source for the CLI and the console's doctor (`doctorPlayerStorage` in `buildDoctorDepsFor`), placed just before the output checklist; a data dir that is there and is no directory, a regular file, a link to one or a link to nothing (`atomicwrite.ErrRootNotADirectory`, never `fs.ErrNotExist`), and one below a link to nothing, a regular file or a link to one, is a fail saying bridge serve will not start, which serve refuses since B606, judged by the data-dir line's walk in its words (`dataDirOnItsWay`)** (2026-10-30, backlog B603; round 2, 2026-10-31; B606, 2026-11-02; B659, 2026-11-02). **Don't give the line a medium check of its own, word a good medium as the player starting (serve's other gates still run), show a line for a player the config leaves off, call a data dir "not there yet" where the nearest entry of its path that is there is no directory (serve's MkdirAll stops at it), take a data dir that is no directory for storage, judge the path's shapes by another walk than the data-dir line's, or let an inspection or walk error that names no entry in the way read as ok, or as a fail.**

The orchestrator's amendment 8 to B603: one shared verdict for the writer and
the doctor, its outcomes defined for a disabled player, an absent data
directory and an unsupported platform, and a good medium never read as "the
player will start". `playerStorageCheck` builds the line from the loaded
config and the platform. A player the config leaves off (`output.player.enabled`
not true, the block read) has no line, on Linux or off it. Off Linux a player
the config asks for is "not checked: the bridge's own player runs on Linux
alone", ok, and nothing is inspected. On Linux a player the config's own
refusal turns off is not checked, a warn naming `PlayerEnabled`'s reason, which
names no value (B505). Then `atomicwrite.InspectMedium(cfg.DataDir)`, the
inspection whose verdict the player's strict writer reads as it starts.

An `fs.ErrNotExist` sends the line up the data directory's path
(`dataDirNotThere`; round 2, CodeRabbit on #1368). The inspection resolves
every link in the path, so a link to nothing answers ENOENT as a directory not
made yet does, while serve's `MkdirAll` stops at such a link with EEXIST, as
this user and as root, whose walk is fsutil's own (`mkdirAllAsRoot`)
(`TestServeCannotMakeADataDirectoryThroughALinkToNothing` measures both) and
serve then went on without its lock and started no player (since B606 it
refuses to start); until round 2 every
ENOENT read ok, "serve makes it". From the data directory up, the nearest entry
of its path that is there (`os.Lstat`, the kernel resolving the entries above
it) is judged: a symbolic link there whose `os.Stat` answers ENOENT is a fail,
"bridge serve will not start: <the link> is a symbolic link to nothing, and
serve cannot make <dataDir> through it" ("the player will not start: …"
until B606), hint "point the link at a directory, or set
dataDir in bridge.yaml to one", naming the entry of the data directory's own
path (the head of a chain that ends in nothing) and finding a broken link
reached through a live one where it sits; anything else there (a directory, a
live link to one) is "not checked: the data directory … does not exist yet",
ok, since serve's preparation (`prepareServeDataDir`) makes the components
below it. Every other error,
of the inspection or of the walk, is not checked, a warn, never ok
(`dataDirNotChecked`), its summary naming the data directory beside the error,
since `filepath.EvalSymlinks` answers an entry on the way that is no directory
with a bare ENOTDIR that names none: a permission error advises the service's
user, who may read what this user cannot; any other (a link cycle, which
EvalSymlinks answers as too many links, or a non-directory on the way) advises
mending what the error names on the way, or setting dataDir to a directory
serve can make or use. (A non-directory on the way is no longer one of these
since B659: the walk finds it and fails it, the paragraph on B659 below.)
This said until B606 that serve itself was unchanged,
going on after a lock error that is not contention, and that telling "cannot
make the data directory" from "cannot lock it" there was a follow-up: that
was B606, and serve now refuses a data directory it cannot prepare (the B606
bullet under The CLI and the serve wiring).

*B606 (2026-11-02): a data directory that is there and is no directory.* The
inspection checked the type of no root: a data directory that is a regular
file, or a link to one, resolved, and the mount it lies on was inspected as a
directory's would be, so the line read ok ("… is on storage in which the check
finds no volatile layer"), and a data directory that is a link to nothing
answered EvalSymlinks's ENOENT, the absence of a data directory not made yet,
which the walk above then caught. `InspectMedium` now checks the resolved
root's type before it reads the mount, as the strict writer's `checkPath`
does, and refuses a root that is there and is no directory with a
`*atomicwrite.RootNotADirectoryError` (matching `ErrRootNotADirectory`, and
never `fs.ErrNotExist`): a regular file or any other entry but a directory
(`Kind`, `fsutil.EntryKind`'s), a link that leads to one (`Link`, `Target`),
and a link at the root whose links lead to nothing (the root's own entry a
link, its resolution ENOENT). The line fails each (`dataDirNotADirectory`),
saying bridge serve will not start, which serve, refusing a data directory
it cannot prepare, makes true: "bridge serve will not start: the data
directory D is a regular file, not a directory", hint "move it aside, or set
dataDir in bridge.yaml to a directory serve can make or use"; "… the data
directory D leads to T, a regular file, not a directory" and "bridge serve
will not start: D is a symbolic link to nothing, and serve cannot make D
through it", hint "point the link at a directory, or set dataDir in
bridge.yaml to one". A data directory below a link to nothing is still not
there to the inspection (its own entry cannot be reached), and the walk
fails it as before, now saying bridge serve will not start too. An
inspection or permission error stays a warn, never a fail: the service's
user may read what this one cannot. Both doctors carry the line word for
word: `TestADataDirectoryThatIsNoDirectoryFailsThePlayersStorageLine` (the
real inspector, which never answers a link to nothing there as
`fs.ErrNotExist`) and
`TestBothDoctorsJudgeADataDirectoryThatIsNoDirectoryAsServeDoes` (bridge
doctor's run, exit 1, and the console's, over a regular file; the warn below
a directory this user cannot search), with atomicwrite's
`TestInspectMediumRefusesARootThatIsNotADirectory`.

*B659 (2026-11-02): one walk with the data-dir line.* A data directory below
a regular file, or below a link to one, was still a warn here, "not checked:
the data directory D cannot be inspected: not a directory": the inspection's
`filepath.EvalSymlinks` answers it with a bare ENOTDIR that names no entry,
while serve refuses it (MkdirAll's ENOTDIR at the entry). Every path answer
of the inspection (`ErrRootNotADirectory`, `fs.ErrNotExist`, the bare
ENOTDIR) now sends the line to the walk up the data directory's path that
bridge doctor's data-dir line takes and serve's refusal reads
(`dataDirOnItsWay` over `fsutil.BlockingEntry`, the entry B659 below), and
the line says what the data-dir line says, word for word: such an ancestor is
a fail, "bridge serve will not start: A is a regular file, not a directory,
and serve cannot make D below it" (hint "move it aside, …"), or "… A leads to
T, a regular file, not a directory, and serve cannot make D through it" (hint
"point the link at a directory, …"); the owner's decision, since serve refuses
it. The shapes at the data directory and a link to nothing above it keep
their words (`dataDirNotADirectory` and the walk's own fail are gone into
`dataDirBlocked`). A walk that finds nothing in the way of what the
inspection could not reach (the path changed in between) is not checked, a
warn naming the inspection's error, never "not there yet"
(`TestThePlayersStorageLineTrustsNoWalkThatContradictsItsInspection`, and its
unix half `TestThePlayersStorageLineTrustsNoWalkThatFindsNothingInTheWay`,
since on Windows `syscall.ENOTDIR` is ERROR_PATH_NOT_FOUND, an absence). An inspection or
walk error that names no entry in the way stays a warn: the link cycle and the
directory this user cannot search of `TestNoErrorOfThePlayersStorageLineReadsAsOK`,
whose ancestor-that-is-a-regular-file case went with this change.
`TestThePlayersStorageLineSaysWhatTheDataDirLineSays` (unix, the player on, a
stand-in Linux box, the real inspector) requires the two lines to fail every
shape serve refuses alike.

A refusal is a fail, "the
player will not start: " and the writer's own refusal, whose hint puts the data
directory on persistent storage, or for `ErrOverlayBind` on the overlay's own
mount or a filesystem that is not an overlay; and storage the check takes is
ok, "<dataDir> is on <storage>; serve checks the rest as it starts the player",
the storage "Volumio's data partition (mmcblk0p3, ext4)" where the check names
it and "storage in which the check finds no volatile layer" otherwise. The
directories' barriers, the DAC's binding and serve's other gates still decide
at the start, so no ok line says the player will start. The console's doctor
(`adminDoctorRunner`) builds the same Deps (`buildDoctorDeps`), so it carries
the line word for word; `bridge init`'s preflight sets no `PlayerStorage`. Run
appends the line after its own checks and before the output checklist, which
stays last. The two seams, `playerStorageGOOS` and `inspectPlayerStorage`, are
package vars only tests change, never in parallel.

Pinned by `TestThePlayersStorageLineSaysWhatTheWritersCheckFound`,
`TestBridgeDoctorNamesThePlayersStorage`,
`TestTheConsolesDoctorNamesThePlayersStorageAsBridgeDoctorDoes`,
`TestRunPrintsThePlayersStorageBeforeTheChecklist`, and since round 2
`TestADataDirectoryALinkToNothingStandsInIsNoneServeMakes`,
`TestServeCannotMakeADataDirectoryThroughALinkToNothing` and
`TestNoErrorOfThePlayersStorageLineReadsAsOK` (unix, the real inspector). The
writer's side is `ops/History-rules-config-lifecycle.md`, B603.

## B659 — bridge doctor's data-dir line (2026-11-02)

- **`bridge doctor`'s `data-dir` line says, for every config that loads, whether serve's preparation (`prepareServeDataDir`) can make or use the data dir, from what stands on its path by Lstat and Stat with no write, one source for the CLI and the console's doctor (`doctorDataDir` in `buildDoctorDepsFor`, `doctor.Deps.DataDirPreparation`), placed right after config-dir: each shape serve refuses (a regular file, a link to one, a link to nothing, at the data dir or above it) is a fail saying bridge serve will not start, naming the entry in the way and its fix; a directory, a link to one, and a data dir not made yet below either are ok, unless the record beside the config names that data dir and it holds neither its database nor its lock file, or since B741 not the identity the record holds, or the record or the marker does not read, a fail in serve's refusal's words (B651, scanner.md; a record or a marker this user cannot read a warn, not checked); a walk that an error other than an entry's absence stops (a permission error, a link cycle) is not checked, a warn; a config found or named that does not load is not checked, a warn, in config-file's terms (`doctor.ConfigFile.NotGradedReason`); a run that finds no config, and init's preflight, have no line; serve's refusal and both doctor lines read one walk, `fsutil.NotADirectory` and `fsutil.BlockingEntry`, which asks os.Stat, as os.MkdirAll does, whether an entry leads to a directory** (2026-11-02, backlog B659; an ancestor that is a regular file a fail by the owner's decision; Codex's check on the brief). **Don't give the line a write or a walk of its own, judge a data dir from a config that did not load, fail a permission or inspection error, or ask whether the data dir can be written (serve's preparation does not).**

*The gap* (B606's follow-up). bridge doctor had no line on the data
directory as such. The player-storage line, which since B606 fails a data
directory serve refuses, exists only for a config that asks for the player on
Linux, so with the player off a data directory serve will not start on went
unreported: tls-cert finds no pair there, "absent (init will mint)". Measured
on main (18acfb52) through `doctorCmd`, the player off and the data directory
below a regular file: "13 ok, 7 warn, 0 fail" and "all clear.", while serve
exits 1 on it. `bridge init`'s preflight and the console read the doctor, so
an operator who set `dataDir` to a file, or whose NAS link dangles at boot,
saw a passing report and a serve that exits 1 (under systemd, a restart loop
with serve's refusal in the journal).

*The line.* `dataDirCheck` (`cmd/bridge/doctor_data_dir.go`), which
`buildDoctorDepsFor` wires as `doctor.Deps.DataDirPreparation` and
`doctor.Run` places right after config-dir, so bridge doctor's report, the
console's (`adminDoctorRunner`) and the launcher's row carry it word for
word. It walks up the data directory's path (`fsutil.BlockingEntry`) to the
first entry that is there and judges it (`fsutil.NotADirectory`), by Lstat
and Stat, writing nothing:

- Each shape serve's preparation refuses is a fail, "bridge serve will not
  start: …" (`dataDirBlocked`): at the data directory, "the data directory D
  is a regular file, not a directory", "the data directory D leads to T, a
  regular file, not a directory" and "D is a symbolic link to nothing, and
  serve cannot make D through it"; above it, "A is a regular file, not a
  directory, and serve cannot make D below it", "A leads to T, a regular
  file, not a directory, and serve cannot make D through it" and "A is a
  symbolic link to nothing, and serve cannot make D through it". A link's fix
  is "point the link at a directory, or set dataDir in bridge.yaml to one",
  any other entry's "move it aside, or set dataDir in bridge.yaml to a
  directory serve can make or use". An ancestor that is a regular file is a
  fail by the owner's decision, as serve refuses it; a link to a regular file
  above the data directory is one of the shapes too (Codex's check).
- A directory there, or a link to one, is ok, "D is a directory", and a data
  directory not made yet below one is ok, "D does not exist yet; bridge serve
  makes it".
- An Lstat on the way, or a Stat of the entry there, that fails for another
  reason than an entry's absence is not checked, a warn naming the data
  directory and the error, with the player-storage line's hints
  (`dataDirNotChecked`): a permission error the service user's, any other (a
  link cycle, a link through a regular file) the path's. Serve refuses such a
  path too, its MkdirAll failing, but with no shape it names, and the
  service's user may read what this one cannot: "the same verdict as serve"
  covers the path's shapes alone (Codex's check).
- A config the lookup found or was named and could not load names no data
  directory the report can trust, so the line is "not checked", a warn
  (Codex's check), never a verdict on a guess: "not checked: the config that
  sets the data directory does not load", "… is not readable by this user"
  or "the named config does not exist", config-file's own reason
  (`doctor.ConfigFile.NotGradedReason`), hint "config-file above says why;
  the data directory is checked once the config loads". A run that finds no
  config, before bridge init, has no line: no config is there for serve to
  refuse. `bridge init`'s preflight sets none: init makes the data directory
  itself (`mkdir data dir`, exit 1) before it writes the config.

Whether the data directory can be written is not asked, as serve's
preparation does not ask it: the lock's fallback and the player's preflight
say that.

*One walk.* Serve's `notADirectory` and `blockingEntry` moved to
`internal/fsutil` (Argon's idea on B606; Codex's check: reuse them before
writing a new walk): `fsutil.NotADirectory` says what stands at an entry
(`fsutil.EntryInTheWay`: the entry, whether it is a link, where its links
lead, what it is), and tells an entry it cannot read (its error) from a
directory (nil, nil); `fsutil.BlockingEntry` answers the nearest entry that
is there, what stands there and the walk's error apart. Serve's
`whyNotMade` and `prepareServeDataDirWith` read them and say what they said
for each shape (`TestPreparingTheDataDirectoryRefusesWithItsPathAndItsCause`
and `TestThePreparationNamesTheEntryInTheWayBelowWhichMkdirAllFailed` pass
unchanged). One reading changed: whether an entry os.Lstat calls no directory
leads to one is asked of os.Stat, as os.MkdirAll asks it, so a Windows
junction to a directory, which os.Lstat reads as an irregular file since Go
1.23 (a name surrogate gets no ModeDir), is no entry in the way, and a
junction whose target is gone is in the way, named "an irregular file" as
serve named it; the data-dir line would otherwise fail a junctioned data
directory serve takes (`TestNotADirectoryTakesAJunctionToADirectory`, Windows
only, compiled here). The player-storage line judges every path answer of
its inspection by the same walk (`dataDirOnItsWay`), so the two lines say one
thing of a data directory serve refuses (the B659 paragraph under B603
above). `internal/fsutil/` is a guarded prefix: qualificationVersion 27
(`ops/b470-native-qualification.md`; 26 on its branch, raised at the merge
over B621's 26); nothing in the player calls the new functions.

Pinned by `TestBothDoctorsFailEveryDataDirectoryServeRefuses` (serve's own
table, `unusableDataDirs`, with serve's preparation refusing each and no
player-storage line beside it), `TestTheDataDirLineSaysWhatStandsInTheWayAndItsFix`,
`TestBothDoctorsTakeADataDirectoryServeCanPrepare`,
`TestTheDataDirLineIsNotCheckedWhereTheConfigCannotBeLoaded`,
`TestTheDataDirLineIsAWarnWhereThePathCannotBeInspected` (unix),
`TestThePlayersStorageLineSaysWhatTheDataDirLineSays` (unix), doctor's
`TestRunPrintsTheDataDirLineAfterTheConfigDir` and
`TestNotGradedReasonSaysWhyAConfigWasNotGraded`, and fsutil's
`TestEntryInTheWaySaysWhatStandsThere`,
`TestNotADirectoryNamesAnEntryThatIsNoDirectory`,
`TestBlockingEntryFindsTheEntryInThePathsWay` and
`TestTheWalkUpAPathSaysWhatItCannotRead` (unix); the record is in
`ops/engineering-log.md`, 2026-11-02, B659.

## B676 — bridge doctor's player-dac line (2026-11-03)

- **`bridge doctor`'s `player-dac` line says, on Linux for a config that asks for the player, whether the process it checks may open the player's DAC node for playback, one source for the CLI and the console's doctor (`doctorPlayerDAC`, `doctor.Deps.PlayerDAC`, placed right after player-storage): the config's binding resolved by the player's own machinery (`playback.BindingLocator` over the box's trees, one bounded reading at a time), the endpoint's own node (`alsapcm.Endpoint.Path`) lstat'ed for its group (named by its gid, `audio` only where the group database names none) and asked with the raw `faccessat2` (R_OK|W_OK, AT_EACCESS), which opens nothing; refused is a fail by its cause (the user not in the group and no ACL: usermod only for a resolved user name (otherwise add the account to the group by name), then the restart of `user@<uid>.service` on systemd, or a new login and bridge start without systemd; in it, in a process older than the membership: the restart; refused despite the group: the mode, the ACL or a security module); allowed is ok, never a word on the DAC being free; bridge doctor checks its own credentials and warns where its login has the group and the running bridge (`server.pid`, `/proc/<pid>/status`'s Groups) does not; the console's doctor checks the serving process, and says when the config names another DAC than its player started with; ENOSYS or EPERM from the check, a DAC not connected or ambiguous, a missing node, and, for `bridge doctor`, root or another user than the running bridge's are not checked, a warn (the console's doctor in a serving process that runs as root reports the access that process has)** (2026-11-03, backlog B676; Codex's and Argon's notes on the brief). **Don't fall back to the mode bits (`unix.Faccessat`), open or hold the node, take the control node for the playback one, call an absent DAC a permission failure, word ok as the player playing, or ask the host's `/dev/snd` from a test (`hostDACNodes`, which the suite's TestMain refuses and records).**

*The gap.* On Linux the player opens its DAC's node under `/dev/snd`, which
the audio group owns. A desktop login opens it through logind's seat ACL; the
bridge runs as a systemd user service kept alive by lingering, often with
nobody at the seat, so it has neither the ACL nor, until its user joins the
group and its user manager restarts, the group. Nothing said so before a
play: the doctor's player checklist is MPD's alone (`output_checklist.go`),
and the player's open refused the node with `permission_denied` in the log
alone.

*The line.* `playerDACCheck` (`cmd/bridge/doctor_player_dac.go`), which
`buildDoctorDepsWith` wires as `doctor.Deps.PlayerDAC` for every config that
loads and `doctor.Run` places right after the player's storage line, before
the output checklist. No line off Linux, nor for a config whose player is off
or refused (the storage line says why). A player with no DAC bound is not
checked, saying how to bind one. Otherwise:

- The binding is resolved as the player's backend resolves it at every open,
  `playback.BindingLocator` over the box's `/proc` and `/sys` (hostTrees), on
  one reading within 3 s, on a goroutine of its own and one at a time (a
  slot per host, as the console's readings take one): a reading the kernel
  holds is abandoned to return on its own and its slot let go then, and the
  next line says an earlier reading has not returned. Every state but
  available (`device_missing`, `device_ambiguous`, …) is "the bound DAC is
  not connected now", a warn, never a permission failure (Codex).
- The node is the endpoint's own, `/dev/snd/pcmC<card>D<device>p`, the one
  `alsapcm` opens O_RDWR; the control node is not an equivalent test
  (Codex). lstat, its link not followed as the open's, gives its group and
  whether it is a character device: a node not there is not connected, and
  one that is no character device is not checked.
- The access is the raw `faccessat2(AT_FDCWD, node, R_OK|W_OK, AT_EACCESS)`:
  the effective uid and gid and the supplementary groups, and the node's
  ACL, judged as an open judges them, and nothing opened, so no stream the
  player or another process holds is disturbed. Never `unix.Faccessat`,
  which on ENOSYS or EPERM falls back to the mode bits and so ignores an
  ACL; ENOSYS (before Linux 5.8) and EPERM (a seccomp filter, say) are not
  checked, never ok and never a fail (Codex).
- The group is named by its gid from the group database, `audio` only where
  it names none (Argon).
- Refused (EACCES) is a fail by its cause: the account not in the group
  (os/user's GroupIds, the group database's, never the process's) and no ACL
  letting it, whose fix is the installer's two lines (`sudo usermod -aG
  <group> <user>`, then `sudo systemctl restart user@<uid>.service`, or a
  reboot, sudo written only where the box has it, and what the restart
  costs). The restart is printed only where `/run/systemd/system` is a
  directory, as in the installer, never on the presence of systemctl or
  loginctl alone. Without systemd, log in again (or reboot) and start the
  bridge from that new login. If LookupId fails, keep the uid in the
  diagnosis and the manager unit, but withhold usermod and ask to add the
  account to the group by its user name. Neither changes the verdict;
  the account in it, the process not, a process started before the
  user joined, whose fix is the restart; the process in it and still
  refused, the node's mode, its ACL or a security module (AppArmor,
  SELinux), whose fix is to read them.
- Allowed is ok, saying it was asked with faccessat2, which opens nothing,
  and that whether the DAC is free, and plays, the player finds as it opens
  it (Argon): never a word that playback will work.

*Bridge doctor and the console.* The console's doctor (`adminDoctorRunnerFor`,
on `servingDACHost`) runs in the serving process, whose credentials are the
bridge's, so its probe is the service's own (Codex). Bridge doctor checks its
own credentials (`cliDACHost`), and beside them reads the running bridge:
the pid its pid file holds, taken only for a process whose command line is a
bridge serve, its effective uid and gid and its Groups line from
`/proc/<pid>/status`, through hostTrees' `/proc`. It warns where its own
access comes through the node's group and the running bridge is not in it,
the case a fresh SSH login after the usermod shows while the lingering
service still fails (Argon); and where its own login is the one older than
the membership while the running bridge has the group, it warns rather than
fail, since the player would open the node. Run as root, which may open any
node, or as another user than the running bridge's, it is not checked. The
saved settings and the boot binding: both doctors grade the config's binding,
what the next start opens, as every other line grades the config; the
console's line says when the running player started with another, which it
opens until the restart (Codex P2: the console tells them apart, as
`serve_player_console.go`'s `pending` does).

*Tests.* The judgement runs on a fake box (`dacBox`), each seam a test's: the
cases the brief names (access by an ACL, stale process groups, an absent or
an ambiguous DAC, a missing node, an unsupported probe, denial despite
membership) and the rest above. The real calls run on Linux over files of
the test's own (`TestAccessForPlaybackAsksTheKernelAsAnOpenWould`: read and
write allowed, read alone, write alone and neither EACCES, a missing file
ENOENT, lstat a link's own). The wiring runs the real locator over the GO
bar's trees to card 5's node through both doctors, and the console's default
host over the suite's empty hostTrees. `hostDACNodes`, the one name for the
host's nodes, refuses and records in the suite's TestMain, which fails the
binary on a record, as it does on a PCM open. The record is in
`ops/engineering-log.md`, 2026-11-03, B676.

*Round 2 tests.* `TestDACAdviceKeepsAnUnresolvedUID` injects a failed user
lookup, retains the kernel credentials and checks both doctors' FAIL line,
no usermod, and the numeric manager unit. `TestDACAdviceWithoutSystemd`
checks joining, stale process groups, and a CLI login whose groups are newer
than the running bridge's, preserving FAIL/WARN and asking for a new login
without a systemd command or manager claim. These cases bind no sockets.

## B744 — serve's mDNS responder and the host's converted name (2026-11-07)

- **Serve's mDNS lifecycle says, when its responder fails to start, that the `.local` endpoint only the bridge's own responder answers will not resolve while mDNS is down (`advertise.OwnResponderEndpoint`: the host's name in its IDNA ASCII form, which /v1/health, the pairing QR and the doctor keep listing from the setting), and hands the responder the host-name probe the endpoints and the certificate read (`advertise.ReportedHostname`, `mdns.Config.HostnameSource`)** (2026-11-07, backlog B744). **Don't keep a state of the responder's failure, change what is published over it, or let the responder read the host's name but through that probe.**

*The failure's line.* Since B739's second round a host name that is not
ASCII is advertised in its IDNA ASCII form (`xn--f1ai0a.local` for `Жук`),
which no responder on the LAN publishes but the bridge's own, so its
`.local` endpoint is listed only while `mdns.enabled` is on
(`advertise.Params.MDNS`). That is the setting, not the responder: one that
fails to start leaves the endpoint listed in /v1/health, the pairing QR and
the doctor's `pairing-url`, and nothing answers it. The owner asked for a
diagnostic and no new state: `mdnsLifecycle.Set` prints, after "mDNS
advertise failed (non-fatal)", that the advertised
`https://<name>.local:<port>` will not resolve while mDNS is down, that only
the bridge's own responder answers it, and that /v1/health and the pairing
QR keep listing it while `mdns.enabled` is on. `advertise.OwnResponderEndpoint`
is that endpoint, or "" where none is published that the bridge alone
answers (an ASCII name, which the system's responder answers; no name; a
container's generated one): one helper with `Endpoints`' own
(`localEndpoint`), read through the same host-name probe at the call,
keeping nothing. What is published is unchanged. `mdnsLifecycle.start` is a
per-instance seam (nil is `bridgemdns.Advertise`), through which
`TestAFailedResponderSaysTheConvertedLocalNameWillNotResolve` forces the
failure: the line for `Жук`, and the failure's line alone for an ASCII name
and for a name with no valid form.

*The responder's name.* The responder took its SRV target and `host=` from
`os.Hostname` (`mdns.Config.advertisedHost`), while /v1/health's `.local`
endpoint and the certificate read the name through `advertise`'s host-name
probe, which `SetHostProbeForTest` moves. In production both are
`os.Hostname`; under a test's probe they differed, and no test asked the
responder: `TestServeOnAHostWhoseNameIsNotASCII`'s mDNS-on row pinned it to
loopback and asked nothing. Asked (below), the started responder left
`xn--f1ai0a.local` unanswered and answered the name the system reports
(checked at the red commit). `mdns.Config.HostnameSource`, read at each
rebuild where `Hostname` is empty (nil is `os.Hostname`), is the probe the
lifecycle hands it (`advertise.ReportedHostname`), so the responder
publishes, and answers for, the name the bridge advertises. It is read only
in a rebuild, under the lock Close takes, so nothing reads the probe once
serve stops.

*The question.* `mdns_query_test.go`'s querier asks the started responder
as a LAN client asks: an A question for the `.local` name /v1/health lists,
from a UDP socket bound to one selected interface's IPv4 address, sent to
224.0.0.251:5353 out of that interface (`SetMulticastInterface`) with
multicast loopback on, so the responder, pinned to the same interface
(`pick`), hears it on this host. hashicorp/mdns answers every question by
unicast to the address that asked (`Server.sendResponse`), which the socket
reads, so neither side depends on the host's default multicast route (a
Docker bridge, a VPN). The interface is chosen before the responder starts
(`newMDNSQuerier`): up, able to multicast, no loopback or point-to-point
one, holding an IPv4 address, on which a socket joined to the group hears a
question the querier sends there (a probe with a nonce, within 2 s). Where
none qualifies, and on Windows as internal/mdns's live tests, the row pins
loopback as before and records a skipped subtest, "the responder answers
the name it advertises", saying why. Once the responder has started, no
answer carrying the interface's address within 10 s (the question asked
again every half second) is a failure, never a skip, and says what came
back instead. An answer is taken by its address because another responder
on the host may answer for the same name: internal/mdns's live test
publishes `xn--f1ai0a.local` on loopback, and on Linux a socket joined to
the group on one interface also takes the group's datagrams arriving on
another where a socket joined (`IP_MULTICAST_ALL`, which hashicorp/mdns
leaves on). A probe measured it both ways on legion0, and that test's own
question takes its answer by its address too (ee885b74). On legion0 the
responder is pinned to enp4s0 and answered at once.

## B745 — the player's DSD state in a snapshot, and a restore where no directory can be synced (2026-11-07)

- **A snapshot carries the player's DSD state (`player-dsd.json`) as its bytes beside the player's identity, and a restore writes both durably, ahead of the snapshot's other files; where no directory can be synced (Windows), a restore of a snapshot holding either refuses before anything is written, naming each of the player's files it holds and the way through, removing them from a copy of the snapshot, which then restores the rest while the box keeps its own** (2026-11-07, backlog B745; the owner's decision to refuse the DSD state as the identity is refused, kept). **Don't restore the DSD state or the identity where its write cannot be made durable, refuse them without naming the way through, or write anything before that refusal.**

*Why a bullet here.* The DSD state's backup and restore rules are in
`player.md`'s DSD-state bullet (B728 B3), which loads with the player's
packages. `internal/backup`, which carries the file, loads `cli-serve.md`
and `config-lifecycle.md`, and neither named it, so a session changing the
backup met no rule for its newest file.

*The refusal.* Both of the player's files are written through the strict
writer (`atomicwrite.WriteBytesDurable`), which refuses where the platform
has no directory barrier (`ErrNoDirectoryBarrier`: Windows, where the
player does not run). A Linux box's snapshot holds both once its player has
run, so its restore on Windows was refused, and the identity's refusal
("restore player reservation: atomicwrite: this platform cannot fsync a
directory, so no write here is made durable: unsupported operation") named
no way through; the DSD state's own refusal did (`restorePlayerDSD`). The
owner decided in B728 that the DSD state is refused as the identity is, not
skipped, and that stands. `RestoreWithOptions` now asks once, after it has
read both files from the snapshot and compared the config, before the first
write (`atomicwrite.HasDirectoryBarrier`, which reads the barrier the
strict writer reads, so a test's `SetSyncDirForTest(nil)` moves both), and
refuses through `playerFilesNeedABarrier`: wrapping `ErrNoDirectoryBarrier`,
naming every one of those files the snapshot holds (`player-uuid`, with
`player-ids.json` where the manifest lists it, and `player-dsd.json`),
saying nothing was restored, and giving the way through, removing them from
a copy of the snapshot, which then restores the rest while this box keeps
its own. `restorePlayerDSD`'s refusal stays behind it.

*Pinned natively.*
`TestARestoreWhereNoDirectoryCanBeSyncedRefusesThePlayersFilesNamingTheWay`
takes a snapshot where directories sync and restores it with the barrier
taken away (the existing seam, so it runs on every OS), for the identity,
the DSD state and both: the refusal matches `ErrNoDirectoryBarrier`, says
nothing was restored and names each file in the way through, nothing is in
the box, and a copy of the snapshot without those files restores the rest
and writes neither of the player's files. With the check removed (control
C745-1) the identity's rows go red, and with the DSD state's writer made
non-durable as well (C745-2) every row does, with the DSD state's own
refusal tests. The record is in `ops/engineering-log.md`, 2026-11-07, B745.
## B727 — every command asks the data dir's record, and serve says its refusal once (2026-11-07)

- **Every command but serve that makes, opens or writes the data dir asks the check serve makes of the record beside its config, right after its config loads (`refuseDataDirAway`: the record beside the file the config's name opens, as bridge doctor reads it, and `checkServeDataDirRecord`), and refuses a data dir the record names that holds neither `bridge.db` nor `server.lock` (and since B741 every other cause of the check's, the next bullet), exit 1, in serve's first sentence under its own name, saying that it makes and writes nothing there, to mount the volume and run it again, and how to go on from a data dir moved on purpose (set `dataDir` to where it is now, which the record does not name) or removed on purpose (remove the record): scan, the render commands, analyze, variants move, artwork, enrichment misses and retry, duplicates, library add and remove, pair, token rotate, expire and revoke, admin reset-password, sign-out-everywhere and login-link, cert rotate, backup, tsnet auth, status and logout, update but under `--check`, init's rewrite before its preflight, and, through `holdDataDirForMutation` before it prepares and locks, restore, reset-player, manifest clear-missing and the DAC bind; and serve says its own refusal once per state: a note beside the record (`.bridge.yaml.datadir.refused`), written only into the directory the record was read from (`atomicwrite.WriteBytesInExisting`), names the data dir refused, its cause and the identity its record held (B741), and when it was said, a start says the refusal where the note names another data dir, cause or identity, or none, or is a day old, and always to a terminal, and the first start after it that gets past the data dir's preparation and its lock removes the note and says at Info how the refusal ended: that the data dir is back where the note names the data dir its config names and that data dir held before the preparation the identity the refusal was of, or, for a record that held none, its database or its lock file (`dataDirBack`), and otherwise that the refusal no longer applies** (2026-11-07, backlog B727 items 2 and 5, and its round 2). **Don't let a command make, open or write the data dir before the check, ask it of a read (doctor, status, logs, token list, cert info, update --check, init keeping the config), ask it in `manifest.OpenStore`, tell the operator of a data dir moved on purpose to remove the record, keep the note in the data dir or write it through a writer that makes its directory, let the note's write decide what serve refuses or how it exits, say how a refusal ended before the data dir is prepared and its lock taken, or say a data dir is back that the note does not name or that held neither file, or since B741 not the refused identity, before the preparation.** `TestEveryCommandThatWritesTheDataDirRefusesItWhileItsVolumeIsAway` (tsnet auth and status run whole, their tsnet server stood in for: `tsnetCommandServer`), `TestTheDiagnosticsStayAvailableWhileTheDataDirsVolumeIsAway`, `TestACommandOverAnInstallWithNoRecordMakesItsDataDirAsBefore`, `TestUpdateAsksTheRecordBeforeItAsksForARelease`, `TestServeSaysItsRefusalOncePerState`, `TestServeSaysTheRefusalOfADataDirItsNoteDoesNotName`, `TestServeSaysItsRefusalToATerminalAtEveryStart` (Linux), `TestAStartThatCannotRemoveTheNoteSaysSo`, `TestServeSaysItsRefusalAtEveryStartWhereItCannotKeepTheNote`, `TestTheRefusalsNoteMakesNoDirectory`, `TestTheEndOfARefusalIsSaidAsItIs` and `TestTheEndOfARefusalCallsBackOnlyTheDataDirItRefused` pin it.

*The gap* (B727's second item). B651 gave serve the refusal and left every
other command as it was. Over an install whose record names its data dir,
on a volume whose mount point is bare, each of 28 commands run through
`run`'s dispatch on main (49071fbf) went on, and 22 made a `bridge.db` or a
`server.lock` under the bare mount point: `manifest.OpenStore` makes the
data dir and an empty database (scan, upscale, optimize, render, analyze,
variants move, artwork, enrichment misses and retry, duplicates, library add
and remove), and a command that takes the data dir's lock its lock file
(restore, reset-player, manifest clear-missing, the DAC bind); pair made
`tokens.json`, admin reset-password `adminauth.json`, cert rotate and init's
rewrite a TLS pair, backup a snapshot. Either file has the next serve take
the bare mount point for the data dir it served from, and start on an empty
database with a new TLS pair, which unpairs every device: B651's own defect,
reached through another command.

*The check* is the command's, right after its config loads and before
anything that makes, opens or writes the data dir: `refuseDataDirAway` asks
`checkServeDataDirRecord` of the record beside the file the config's name
opens (`dataDirRecordPath`, as bridge doctor reads it: a command has no boot
read whose file serve keeps to), so the doctor, serve and every command
answer one state alike. Never in `manifest.OpenStore`, which knows a
database path and not the config whose record names the data dir, and which
the doctor's reads and serve after its own check open too. No record is no
check: a fresh install, init's first run and an install no serve has run on
are refused nothing, and a record of another data dir (one moved on purpose
and set where it is now) refuses nothing either. Init has three paths kept
apart: a first install has no config whose record it could ask, a run that
keeps the config writes nothing in the data dir and asks nothing, and a
rewrite (`--force`, or yes at the prompt) asks before its preflight, which
would grade a certificate it cannot see there. `bridge update` asks it but
under `--check`, which reads, and before it asks GitHub for a release, since
its install writes its marker in the data dir. The reads ask nothing:
doctor, whose data-dir line fails the state in serve's words, status, logs,
token list, cert info and update --check. The refusal is serve's first
sentence under the command's own name, then that the command makes and
writes nothing there, to mount the volume and run it again, and how to go on
(`commandRefusal`).

*A data dir moved on purpose.* B651 said to remove the record for a data dir
removed or moved on purpose. Over a moved one that has the next command make
an empty data dir at the old path, and the next serve start there. The
refusal, the doctor's fix and serve's now say: moved, set `dataDir` in
bridge.yaml to where it is now (the record names the old place, so nothing
is refused, and the next serve records the new one); removed, remove the
record (`onPurpose`).

*Once per state* (B727's fifth item). The service manager starts serve again
until the volume is mounted (systemd's unit every 2 s, launchd, the SCM's
recovery, a container's restart policy), and each start printed the whole
refusal: three starts on main, three refusals said, nothing kept. Each start
is a new process, so the state lives on disk: a note beside the record,
`.bridge.yaml.datadir.refused` in the config's directory, never in the data
dir, which is the bare mount point, holding the data dir refused as its
config gives it and when the refusal was said (`sayServeRefusal`). A start
says the refusal where the note names another data dir or none, or was
written a day or more ago (the variant watcher's latch's rule, so a journal
read later has a line of the day), and always to a terminal, a start by hand
(`writesToATerminal`); then it writes the note. The first start after it
that gets past the data dir's preparation and its lock removes the note
and says at Info how the refusal ended (`endServeRefusal`; round 2, below).
A note that cannot be written leaves the next start to say the refusal again
(`TestServeSaysItsRefusalAtEveryStartWhereItCannotKeepTheNote`); what serve
refuses, and its exit, are the same either way. The one write whose failure
quiets a refusal is the removal: a note the start that found its data dir
cannot remove keeps the next refusal quiet until a day after the last was
said, which that start says at Warn (`TestAStartThatCannotRemoveTheNoteSaysSo`).
The record is in `ops/engineering-log.md`, 2026-11-07, B727.

*Round 2* (the review of #1431, 2026-11-07), each point confirmed red first.

*The note makes no directory.* `sayServeRefusal` wrote the note through
`atomicwrite.WriteBytes`, which makes the file's directory first: a config
directory inside the data dir, on the volume that went after the record was
read, was made again for the note, and with it the data dir being refused,
in the bare mount point (`TestTheRefusalsNoteMakesNoDirectory`, red with
`bridge/config/.bridge.yaml.datadir.refused` made there). The note goes
through `atomicwrite.WriteBytesInExisting` now (config-lifecycle.md), which
makes no directory and fails as its staging's create does, writing nothing;
a note it cannot write leaves the next start to say the refusal again, as
before.

*How a refusal ended, said after the lock and only as it is.*
`endServeRefusal` ran at the record's check and said the data dir "holds
its database or its lock file again" wherever the check passed: with
`dataDir` set to another directory, with the record removed and the volume
still away (serve then makes the data dir anew), and before the preparation
and the lock, either of which can still refuse the start (another serve
holding the lock, a regular file where the data dir goes), whose note it
had removed. It runs once the data dir is prepared and its lock taken, or
serve goes on without it, and says the data dir is back only where the note
names the data dir this start's config names and that data dir held its
database or its lock file before the preparation (`dataDirBack`, read before
the preparation and the lock make the data dir and the lock file); and
otherwise that the refusal no longer applies, naming both data dirs. The
record is not asked there: the line is about what the data dir holds, which
no record changes. A note naming another data dir than the config's, one a
start that could not remove it left, ends in the second line.
`TestTheEndOfARefusalIsSaidAsItIs` (five starts after a refusal) and
`TestTheEndOfARefusalCallsBackOnlyTheDataDirItRefused` (the two conditions).

*The reads, pinned.* bridge doctor, `--fix` or not, logs (a log of the
test's own at `packaging.DefaultLogPath`'s path under a home of the test's)
and update --check (its release check asked of a server of the test's own,
`updateReleases`, so that it asks nothing of GitHub) are rows of
`TestTheDiagnosticsStayAvailableWhileTheDataDirsVolumeIsAway`: each refuses
nothing, exits as it would, and makes nothing under the bare mount point.
The doctor reaches no store open in the refused state, so its open needs no
gate on the data-dir line: every open asks first whether `bridge.db` is
there, and the refusal is of a data dir that holds none. (B741 made this
false: a data dir it refuses can hold a database, and the doctor's store
probes now open nothing in a data dir the record refuses: B741's entry.)

*tsnet auth and status as they exit.* The table's tsnet row called the gate
the two share and mapped its error to exit 1 itself, so it could not see
either command's own mapping: with either exiting 0 past its gate's
refusal, the row stayed green. Its rows run `bridge tsnet auth` and `bridge
tsnet status` whole, through `run`'s dispatch, with the tsnet server each
builds past its gate stood in for (`tsnetCommandServer`, `newTsnetServer`
in production), so a gate that let one through brings no node up on the
tailnet, makes no state dir in the data dir, and fails its row. The refusal
names the subcommand, since bare `bridge tsnet` is no command to run again.
The record is in `ops/engineering-log.md`, 2026-11-07, B727's round 2.

## B741 — the data dir's record gains an identity (2026-11-08)

- **Serve keeps the data dir's identity, a random id minted once, in a marker in the data dir (`data-dir-id.json`, readable by its group and others) and in the record beside the config: the marker written durably once the database is open and before the record gains the identity (staged, its bytes synced, renamed, synced where it is through a handle of its own, opened for reading as a file off Windows (`fsutil.OpenAsFile`, which refuses a named pipe at once) and for writing on Windows, whose `FlushFileBuffers` needs it, so that there alone a marker serve may not write is never taken up, and its directory synced: `markerDurability`, each step a seam on the serve instance), the identity the one the marker or the record holds, never a second one minted, a marker a start before left taken up only once synced where it is, and only serve writing the marker; a marker or a record serve cannot write never refuses the start, which goes on with B651's protection alone and says so at Warn, and no write of the record drops or replaces the identity it holds of its data dir or follows a marker that does not read or cannot be inspected at the write: each leaves the record as it was. The check, still keyed on the record naming the data dir, refuses one holding `server.lock` or `bridge.db` and no marker while the record holds an identity (gap 2), one holding another identity, a marker that does not read, and a record that does not read (a link to nothing among them, `errLinkToNothing`, an entry a read missed and its lookup then found read again once first, and looked up once more where that read misses too, `readAgain`), its own state and never no record, each through `dataDirGoneError` with a cause and words of its own beside today's recovery advice, in serve, both doctors and every command `refuseDataDirAway` guards, a record or a marker this user cannot read being bridge doctor's warn, not checked, and a command's refusal saying to run it as the user the service runs as (serve's, to make it readable by that user), and no doctor probe, the player checklist's proof of the library's paths among them, opening the database of a data dir the record refuses (`doctorStoreGate`, asked at each call: `errStoreOfARefusedDataDir`, or `errStoreNotChecked` where this user cannot read the record or the marker; a stray database in the bare mount point, which a store open migrates); the refusal note is keyed on the data dir, the cause and the identity, and the end of a refusal calls back only a data dir holding the identity it was of. A record written before B741 holds none and is taken up at its first start, so the `rm bridge.db*` reset still starts and gap 2 closes from the first marker on; what stays open is named by backlog B813** (2026-11-08, backlog B741; Codex's, the Claude API's and Argon's review of its design, the two reviews of its brief, Codex's and the Claude API's of #1441's first round, and the Claude API's of its second). **Don't mint an identity where the marker or the record holds one, let the record gain one before the marker holds it durably (one a start before left included), drop a step of the marker's durability from what its test records, refuse a start over a marker or a record it could not write, write the marker anywhere but serve's start, take a record or a marker that does not read for none or replace it unread, take a link to nothing for no record, write the record past a marker that does not read or so that it drops or replaces the identity it holds of its data dir, refuse a data dir the record does not name (dataDir set to a new directory on purpose), refuse a record written before B741 over its lock file or its database, let a doctor probe open the database of a data dir the record refuses, open the marker for its sync for writing off Windows or by an open that waits on a named pipe, answer a read of the record that missed an entry its lookup then finds without reading it again, name what stays open but as backlog B813, tell a command run by another user than the service's only to make readable what the service's user may read already, key the refusal note on the data dir alone, or call back a data dir that does not hold the refused identity.** `TestServeKeepsAnIdentityInTheDataDirAndItsRecord`, `TestServeRefusesWhatTheBareMountPointHoldsOnceItsRecordHoldsAnIdentity`, `TestServeRefusesADataDirHoldingAnotherDataDirsIdentity`, `TestServeRefusesARecordThatDoesNotRead`, `TestServeRefusesARecordThatIsALinkToNothing`, `TestServeRefusesAnIdentityThatDoesNotRead`, `TestServeRefusesWhatItCannotRead`, `TestServeTakesUpTheRecordOfAServeBeforeIt`, `TestServeTakesTheIdentityItsMarkerHolds`, `TestAStartWhoseMarkerCannotBeWrittenGoesOnWithoutAnIdentity`, `TestAStartWhoseRecordCannotBeWrittenLeavesItsMarkerToTheNext`, `TestTheMarkerHoldsTheIdentityBeforeTheRecordDoes` (each step of the marker's durability in its order), `TestAMarkerThisUserMayNotWriteIsTakenUp`, `TestTheMarkersSyncWaitsForNoNamedPipe`, `TestAReadOnlyMarkerIsNeverTakenUpOnWindows` (Windows), `TestAMarkerWhoseBarrierFailedIsTakenUpOnceDurable`, `TestADataDirWhoseIdentityDoesNotReadKeepsItsRecordAsItWas`, `TestARecordKeepsItsIdentityWhereTheMarkerCannotBeReadAtItsWrite`, `TestNoWriteOfTheRecordDropsOrReplacesItsIdentity`, `TestTheMarkerIsReadableByWhoeverReadsTheDatabase`, `TestACommandThatOpensTheStoreMakesNoMarker`, `TestARestoreKeepsTheDataDirsIdentity`, `TestADatabaseRemovedOnPurposeKeepsItsDataDirsIdentity`, `TestADataDirMovedOnPurposeKeepsItsIdentity`, `TestADataDirSetToANewDirectoryOnPurposeStarts`, `TestTheCheckDecidesByTheIdentity`, `TestTheRecordReadsOnlyAsOne`, `TestARecordReadThatRacesAWriteReadsAgain`, `TestTheMarkerReadsOnlyAsOne`, `TestEachRefusalSaysWhatItIsAndHowToGoOn`, `TestTheDoctorsChecklistOpensNoStoreInARefusedDataDir`, `TestTheRefusalNoteIsOfOneState`, `TestServeSaysItsRefusalAgainWhenItsCauseChanges` and `TestTheEndOfARefusalCallsBackOnlyTheDataDirHoldingItsIdentity` pin it.

*The gaps.* B651's record named the data dir serve served from, and serve
took the data dir at that path for it wherever it held its database or its
lock file. Three gaps were left. (1) Backlog B813, which stays open. (2)
`server.lock` was evidence too, and the lock is made before the database
opens: a start on the bare mount point before any record was written that
failed between the two left one, as did a database a command made there
before B727, and every start after the volume went took it for the data dir
(`checkServeDataDirRecord` asked an Lstat of each). (3) `fsutil.SamePath`
probes how the folder holding a path compares names now, so with the volume
away a record and a config that differ in case alone read as two data dirs,
and the refusal was skipped (scanner.md's B741 entry).

*The identity.* The record and a marker in the data dir, `data-dir-id.json`,
hold one random id, 16 bytes in lower-case hex (`mintDataDirIdentity`).
Serve writes it once its database is open and its lock file is there
(`recordServeDataDir`): the marker first, staged beside its place, fsynced,
renamed with Windows' retries and no barrier of the rename's own, then
synced where it is, its own bytes and metadata through a handle of its own
and then the data dir, that barrier's failure the write's (`writeDataDirMarker`,
the updater marker's shape; a directory fsync the filesystem cannot make,
EINVAL, ENOTSUP or EOPNOTSUPP, taken as made, and none asked on Windows,
where the marker's own sync, `FlushFileBuffers`, commits its rename to
NTFS's journal: round 1 said the journal kept the rename, which it does only
at its own flush, round 2 below); the record gains the identity only after.
The order is load-bearing: a record holding an identity over a data dir whose
marker a crash took is refused at every start, which a service manager then
loops on; a marker whose record never gained it is taken up by the next start.
The identity is the marker's where the data dir holds one, else the record's
own where it names this data dir, else a new one: never a second one minted
beside one held (until B741 the write rebuilt the record fresh and rewrote it
on any difference, which would drop or remint it). A marker serve cannot write
leaves the record without an identity, said at Warn, so the next start is
judged as before B741 and starts; a record it cannot write leaves the marker
for the next start to take up. Only serve writes the marker: a command that
opens the store makes the data dir and a database, and its data dir gains a
marker at the next serve. Backup does not carry it, so a restore keeps the
marker of the data dir it restores into.

*The check*, still keyed on the record naming the data dir, so that `dataDir`
set on purpose to a new directory, empty or not made yet, is refused nothing
and gains an identity of its own. Of a data dir the record names: a marker of
the record's identity is the data dir served from, whatever else it holds (its
database removed on purpose, its lock file too); a marker of another identity
is refused (`otherIdentity`), and so is one that does not read
(`identityUnreadable`); with no marker, neither file is B651's refusal
(`dataDirNotThere`), and either file under a record holding an identity is
refused (`noIdentity`, gap 2), under a record holding none taken up. A record
that does not read is refused whatever the data dir (`recordUnreadable`): until
B741 it named nothing, and a fresh data dir in the bare mount point followed
from a torn or unreadable record exactly as from none. An error other than an
entry's absence on the way to what the data dir holds is still no refusal:
serve's preparation, its lock and its store answer it.

*A record written before B741* holds no identity, and its first start takes
it up, so the supported reset (`rm bridge.db*`, the lock file kept) still
starts. Gap 2 is closed from the first marker on; what stays open is backlog
B813.

*The words.* Every cause goes through `dataDirGoneError`, its `cause`, so that
serve, bridge doctor and the console's doctor, which share `dataDirCheck`, and
every command `refuseDataDirAway` guards (`dataDirAway` recognises the type,
whatever the cause) refuse each alike, nothing written. Each first sentence
names the data dir as its config gives it and says what it is; only B651's
says it holds neither file, which the others would make false; each ends in
today's recovery advice (`recovery`: set `dataDir` to where a data dir moved
on purpose is now; remove the record for one removed on purpose, or for the
data dir to serve from now), a marker that does not read moved aside first,
and a record or a marker this user may not read made readable first, by
serve's account; a command and bridge doctor, which may run as another user
than the service, are told to run as the user the service runs as, and to
make it readable by that user if it is not (round 2). bridge doctor says that
is not checked, a warn, as B659 says of a data dir it may not inspect.

*The doctor's store probes.* The codec probe behind the ALAC and DSD lines,
sidecar-paths, variants-index and name-collisions, and since round 2 the
player checklist's proof of the library's paths, open the data dir's
`bridge.db` wherever it is there, and a store open migrates what it opens.
B727 left them ungated, since the data dir it refused held no database; a
data dir B741 refuses can hold one, and both doctors migrated a stray 0-byte
`bridge.db` in the bare mount point (red: a whole schema written where the
data-dir line said bridge serve will not start). Where the record refuses the
data dir, whatever the cause (`dataDirAway`, asked as each doctor builds its
report), each probe opens nothing and answers `errStoreOfARefusedDataDir`, so
its line says the store was not read; a database at this version, which no
open migrates, was read and left as it was before, and is not opened now
either.

*The note and the end of a refusal.* The note beside the record keeps the
cause and the identity beside the data dir, so a refusal of the same data dir
for another cause, or under another identity, is said again; a note written
before B741 holds neither and is said again once. `dataDirBack` calls a data
dir back only where it holds, before the preparation, the identity the refusal
was of (`dataDirEvidence`, read before the preparation and the lock, which
make no marker), said in a line of its own, `holds its identity again`; a
refusal whose record held none keeps B727's rule and line. A lock file in the
bare mount point, with the record removed and the volume still away, ends the
refusal as no longer applying, never as the data dir back.

*Not taken.* A mount check: B651's reasons stand. Refusing a start over a
marker or a record serve could not write: the brief's rule that such a write
never refuses a healthy start. Adopting a record that does not read from a
marker that does: the marker at the path may be another volume's. The record
in the data dir, or the identity in bridge.yaml: B651's reasons stand.

*Red first, and the controls.* The tests through serve's real start, both
doctors and bridge scan were written as files and committed red at main's
code (each fails as predicted but B651's two refusals and the case-sensitive
sibling, which pass as controls); the seams (`dataDirRecording`) and the unit
tests came with the fix. Fifteen negative controls, each predicted by name,
each turned its tests red, every file restored. The record is in
`ops/engineering-log.md`, 2026-11-08, B741.

*Round 2: Codex's and the Claude API's reviews of #1441.* Every finding
was real but one.

The player checklist that `bridge doctor` prints on a box with
`output.mpd: on` proves the library's paths to MPD from the indexed
tracks, which it asks for whether MPD answers or not, and its `Tracks`
opened the data dir's database with no regard to the record: a stray
0-byte `bridge.db` in the bare mount point was migrated while the data-dir
line said serve would not start there, and the identity tests, which set
MPD off, never reached it (Codex P1). The probes and the checklist ask one
gate, `doctorStoreGate`, at each call, which answers
`errStoreOfARefusedDataDir`, or `errStoreNotChecked` where this user cannot
read the record or the marker: the probes' words had said serve refuses
a healthy data dir that a group member could not read.

A record that is a link to nothing, as a record kept on the volume with a
link to it beside the config leaves it with the volume away, read as no
record: `fsutil.OpenAsFile` answers the open of such a link with
`ENOENT`, and serve took the data dir it found, made it in the bare mount
point and renamed a fresh record over the link (Codex P2). The record's
absence is its entry's (`os.Lstat`), and a link to nothing is
`errLinkToNothing`, which is no `fs.ErrNotExist`.

The marker's write. A marker that does not read, or cannot be inspected,
at the record's write had serve write the record naming its data dir with
no identity: dataDir set on purpose to a data dir with a torn marker then
started once and was refused at every start after, and a marker an EIO
hid at the write dropped the record's identity, so that the record read as
one written before B741 (backlog B813; the Claude API, Low 1). Such a
marker leaves the record as it was,
and so does every write that would drop or replace the identity the
record holds of its data dir (a marker gone since the check that cannot be
written again, one of another identity): the next start is judged as this
one was. The marker's durability was its writer's alone, so a test saw the
order of the marker and the record and no sync: dropping both left it
green (Codex's test gap, confirmed on a copy of 9527b2a0). Its steps are
seams on the serve instance (`markerDurability`: the staged bytes' sync,
the rename, the marker's own sync in place and its directory's), recorded
in order before the record's write; a failure after the rename leaves the
marker in place and the record without its identity, and the next start
takes the marker up only once its own sync in place has answered. On
Windows `fsutil.SyncParentDir` does nothing, so the rename reached the disk
at NTFS's own flush, and a record on another volume could reach its disk
first (the Claude API, Low 2): the marker's own sync in place opens it for
writing and syncs it, `FlushFileBuffers`, which commits its metadata to the
journal. And the marker was the owner's alone, CreateTemp's 0600 (Low 3):
it is 0644, since it holds no secret. The record stays 0600, as B651 wrote
it: a command run as another user that cannot read it is refused, saying to
run it as the service's user.

Not a defect: the Claude API asked whether a restore keeps the marker, and
whether the read-only commands open a store. A restore writes a snapshot's
files in place and no snapshot carries the marker
(`TestARestoreKeepsTheDataDirsIdentity`, green before and after); `token
list`, `status`, `cert info` and `logs` open none, and every other store
open in cmd/bridge is behind `refuseDataDirAway`: the doctor's checklist
was the one read that opened one. The case-folding finding is scanner.md's
gap-3 entry. Each fix's tests were red first at 9527b2a0, and sixteen
negative controls, each predicted by name, turned their tests red. The
record is in `ops/engineering-log.md`, 2026-11-08, B741 round 2.

*Round 3: the Claude API's review of round 2, Codex's of its PR body, and
the two reviews of round 3's brief.* Three points, each red first but the
first, which is wording.

What this change leaves open is named in public records by its backlog id
alone, B813. Round 1 had written it out, here and in scanner.md's entry,
their long forms, the identity file's header, two test docs, the runbook
and the engineering log, and the refusal of a data dir holding no identity
explained it to the operator in an aside; that refusal says what to do and
no more (`TestEachRefusalSaysWhatItIsAndHowToGoOn` pins the advice, and no
aside in any cause's).

The marker's sync in place opened it for writing on every OS, where only
Windows needs that, its `FlushFileBuffers`: a marker serve may read and not
write (another user's, as where root owns the data dir and a member of its
group serves it, or an immutable one) was never synced, so its identity
was never taken up, every start writing the record without it (red:
`TestAMarkerThisUserMayNotWriteIsTakenUp`, a 0444 marker under no record
and under a record written before B741, its sync's open answering EACCES).
Off Windows the open is `fsutil.OpenAsFile`'s, for reading, which syncs,
and refuses a named pipe at once where a plain open for reading would wait
for a writer and hold serve's start (`TestTheMarkersSyncWaitsForNoNamedPipe`);
on Windows it stays for writing, so a read-only marker there, its attribute
or an ACL, is still never taken up
(`TestAReadOnlyMarkerIsNeverTakenUpOnWindows`, CI's Windows leg).

A read of the record that missed, and whose lookup then found an entry,
answered at once: a regular file with the miss's ENOENT, a link with
`errLinkToNothing`, though the record may have just been written there
(serve's first, beside bridge doctor or a command) or a link's target just
mounted, so the doctor or the command refused the data dir for a record
that does not read. The entry is read again, once (`readAgain`), and where
that read misses too, looked up once more: no record only where no entry
is left, `errLinkToNothing` only where a link still is (red:
`TestARecordReadThatRacesAWriteReadsAgain`, through steps a parameter of
`readDataDirRecordWith` takes, never a package variable parallel runs
race on). Five negative controls, each predicted by name, turned their
tests red. The record is in `ops/engineering-log.md`, 2026-11-08, B741
round 3.

## B749 — the embedded node's diagnostic-log uploads and its name (2026-11-09)

- **The embedded Tailscale node (tsnet mode) uploads no diagnostic logs unless `tailscale.uploadDiagnosticLogs` is true, keeps the name a registered node has, and a tailnet that requires the logs is said in the bridge's words, at a start and while the node runs** (2026-11-09, backlog B749 part 3, and its rounds 2 to 5): every path that builds a node (serve, `bridge tsnet auth` and `status`, each after its mode gate) builds it through `tsnet.NewServer`, which sets `TS_NO_LOGS_NO_SUPPORT=true` through envknob unless the key is true, over whatever the process wrote there, unread, on the caller's goroutine before anything starts the node (`applyUploadPolicy`), leaves the environment as it is where the key is true, reading what it holds as Tailscale does (a true holds the uploads off, a false holds nothing, one written over an inherited true included), and refuses an inherited value `strconv.ParseBool` refuses, naming the variable and not its value (`tsnet.ErrNoLogsEnvUnreadable`), and, with the key true and nothing set, a value written in the process since that it refuses, in words that blame the process and never its launch environment (`tsnet.ErrNoLogsEnvWrittenUnreadable`); once the policy has set the variable, every later `NewServer` sets it again before anything reads it, until the process exits, so a node a process builds after one it built without the key (the menu's "Start now", and bridge init's start, run serve in their own process) starts with the uploads off whatever its config says, and the setting applies at a process restart; the policy reads what the process inherited once, at its first application whatever the config says (`policyRecord`), and its status (`Server.Uploads`, read live) tells three reasons apart, each in words of its own (`Uploads.HeldOff`): the launch environment held the variable, this process turned the uploads off earlier (quit the bridge, its menu included, and launch it again, or restart its service), or something else in it did (a new launch may restore them), which a tailnet's refusal says and the console's tile shows on a line of its own (`UploadsHeldOff`) whatever the tailnet says; what stays open is backlog B830; a mode-refused command and `tsnet logout` leave the environment alone; the key is process-restart-bound (the console's `tailscaleUploadDiagnosticLogs`, class D in `ops/settings-apply-semantics.md`), and `compose.yaml` forwards its variable empty where it is unset, which the load takes for no override (`TestAnEmptyVariableLeavesTheFilesConsent`). The node's name is `tailscale.hostname`, else what the state says at each start, read read-only through upstream's keys and types, never its store constructor (`readNodeState`: the current profile, listed among the known profiles, its prefs carrying a node ID): a registered node keeps its persisted hostname, a node not registered (no file, an empty one, `{}`, an authentication that never finished) is `tsnet.DefaultHostname` (`1-bit-bridge`), and a state that does not read keeps the library name with one line naming no path. A start the tailnet refuses for the logs (upstream's "tailnet requires logging to be enabled", the one substring matched, pinned to the resolved module source) ends in `tsnet.LogsRequiredError`, kept as the wrapper's last start error under `lifecycleMu`, and the console's tile says it in LastError, from a running node's health too (`Server.LogsRequired`), naming `TS_NO_LOGS_NO_SUPPORT` where an inherited true holds the uploads off; `tsnet.Config.BeforeStart` (`tsnetBeforeStart`, nil in production) is the seam just before the upstream start the tests read the environment and the name through. **Don't set the variable before a path's mode gate, from a refused command or `logout`, or once anything has started the node; let an absent key or an inherited false turn the uploads on, or the bridge turn them on itself; hand the variable back, or unset it, in a process that has set it, or read it in a later `NewServer` before setting it again, read a true something else set as the launch environment's, or a false written over an inherited true as holding the uploads off, refuse a node without the key over a value written in the process, blame the launch environment for a value written in the process, set the variable before reading what the process inherited, give Compose's forwarding of the key's variable a default other than empty, or advise a restart of serve in its process, or a bare restart, for a change of the key; count a node registered by the state file's existence, open the state through upstream's store constructor, or write, create or lock it; derive a new node's name from the library, the host, its user or a path; match the refusal on more than that substring, or show upstream's tailscaled advice; or set the hook in production.** `TestTheUploadPolicyByKeyAndInheritedValue`, `TestEveryPathAppliesTheUploadPolicyBeforeTheUpstreamStart`, `TestTheCommandsThatBuildNoNodeLeaveTheEnvironmentAlone`, `TestTheNodesNameOnEveryStart`, `TestTheTileSaysWhatATailnetThatRequiresLogsNeeds`, `TestAServeRunAgainInTheSameProcessKeepsTheUploadsOff`, `TestTheTileSaysWhatHoldsTheUploadsOff`, `TestTheTileReadsWhatHoldsTheUploadsOffWhenItAsks`, and internal/tsnet's `TestTheNodeNameIsTheRegisteredOneOrTheDefault`, `TestOnceThisProcessTurnsTheUploadsOffTheyStayOff`, `TestTheStateReaderWritesNothing`, `TestARefusedStartSaysWhatTheTailnetRequires` and `TestTheLogsRequirementIsUpstreamsWords` pin it; `scripts/tsnet-uploads/evidence.py` is the evidence from a production binary, confined by port and not by address, an upgraded install included. → History-rules-cli-serve.md

*What uploaded.* tailscale.com v1.104.2's `tsnet.Server.Start` runs
`startLogger` outside a test binary: it writes `tailscaled.log.conf` and a
filch buffer in the state dir and builds a logtail logger whose transport
(`logpolicy.NewLogtailTransport(logtail.DefaultHost, …)`) is a no-op only
while `envknob.NoLogsNoSupport()` reads `TS_NO_LOGS_NO_SUPPORT` true, live,
through `os.Getenv`. Network flow logs (`feature/netlog`, linked into the
bridge) run only while it is false, and `hostinfo.New` tells the
coordination server which it is. The bridge never set the variable, so every
tsnet-mode bridge uploaded its node's logs to `log.tailscale.com`; and it
named its node after the library, so a library rename renamed the node, its
MagicDNS name and its certificate's, which paired apps hold.

*The policy.* `tsnet.NewServer` applies it (`applyUploadPolicy`), so no path
that builds a node can skip it: serve's tsnet case, `bridge tsnet auth` and
`status` build through `newTsnetServer` after their mode gates, on the
caller's goroutine and before anything starts the node. With the key absent
or false it sets the variable true through `envknob.SetNoLogsNoSupport`,
upstream's own setter; with the key true it leaves the environment as
inherited, so an inherited true still holds the uploads off and an inherited
false alone never turns them on. An inherited value `strconv.ParseBool`
refuses is refused before any node is built (`ErrNoLogsEnvUnreadable`, the
variable named and its value not), where every reader of it in Tailscale's
code would end the process (`envknob.Bool`'s `log.Fatalf`); an empty value
is unset, as envknob reads it.
envknob's comment asks for `Setenv` before goroutines start
because of its registered knobs' caches; this variable is not one, and every
reader asks `os.Getenv`, so serve's earlier goroutines (the scanner, the
enricher), which run no Tailscale code, race nothing. What the policy found
(`Uploads{Allowed, EnvOff}`) is kept for the refusal's words. A
mode-refused command and `tsnet logout` build no node and leave the
environment alone. The key is the console's `tailscaleUploadDiagnosticLogs`,
class D in `ops/settings-apply-semantics.md`: the variable is the process's,
and upstream picks the node's log transport as the node starts. In Docker,
`compose.yaml` forwards the key's variable as
`${BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS:-}` (round 4), so a container
whose `.env` leaves it out gets it set and empty, which the load takes for
no override, as it takes every empty variable, and `bridge.yaml`'s value
stands; `TestAnEmptyVariableLeavesTheFilesConsent` pins that for a true and
a false (round 5, the Claude API's review of round 4).
A process can build a node again after one it built (round 2, from the
Claude API's question on #1445 whether anything starts the bridge again
with the variable this process set: nothing does; every child process serve
starts is a tool or `serve --validate-config`, which builds no node, and a
restart is the service manager's, from the unit's environment). The menu's
"Start now" runs serve in its own process and again after a stop, as bridge
init's start can before it. Round 2 handed the inherited value back to a
later node whose config allowed the uploads; round 3 took that out on Codex's
two P2s on #1445's round 2, one on the hand-back itself, the other that
upstream's own true, which the coordination server's `DisableLogTail` message
writes beside a `logtail.Disable` that lasts the process, was taken for the
policy's and undone. Once the policy has set the variable, every later
`NewServer` sets it again through envknob before it reads it, until the
process exits (round 4, the Claude API's review of round 3: round 3 only
refrained from unsetting it, so a false written in the process after the
policy's true let a later allowing node start with the uploads on, and a
value that does not read as a boolean made that node's `NewServer` refuse,
blaming the launch environment; the variable is set again only there, so
between such a write and the next `NewServer` it reads as the write left
it), so a later node whose config allows the uploads starts with them off,
and the setting applies at a process restart: quit the
bridge, its menu included, and launch it again, or restart its service. The
design consult chose this over tracking every node until its teardown, which
each early return of serve between the node's construction and its shutdown
defer would have had to release. Before the policy has set the variable, a
value written in the process since its launch is read as Tailscale reads it
(round 5, the Claude API's review of round 4 and the two reviews of round
5's brief): without the key the policy sets the variable over it unread,
once it has read and parsed what the process inherited, so an inherited
value it refuses still leaves the environment as it was; with the key a
true holds the uploads off, the launch environment's (`EnvOff`) only where
that held a true too and otherwise something else's (`OtherOff`), a false
lets the node upload, a false written over an inherited true included, and
a value that does not read as a boolean is refused before any node is built
(`ErrNoLogsEnvWrittenUnreadable`), in words that blame the process and never
its launch environment, since Tailscale's code would end the process on it.
Round 4 parsed that value before either branch, so a node without the key
was refused over it and a node with the key was refused in the launch
environment's words, and it took a false written over an inherited true for
the launch environment's hold. The policy reads what the process inherited
once, at its first application whatever the config says (`policyRecord`), and
its status (`Server.Uploads`, read live, so a `DisableLogTail` that comes
while a node runs shows) tells three reasons apart, all in `Off()`: the
launch environment held the variable (`EnvOff`: remove it, then launch
again); this process turned the uploads off earlier (`LatchedOff`: the
setting applies at the next launch, never at a restart of serve in its
process, which the menu's Start now and init's start make); something else in
it did (`OtherOff`: a new launch may restore them, with no promise that the
coordination server will not turn them off again). `Uploads.HeldOff` says
each in its own words, which a tailnet's refusal carries and the console's
tile shows on a line of its own (`admin.TailscaleStatus.UploadsHeldOff`),
for a node the tailnet accepted, whose badge, backend state and endpoints
stay as they are, as for one it refused; round 4 pins that the status is
read when it is asked for, on a node built before upstream's setter runs.
What stays open is backlog B830.
`ForgetUploadPolicyForTest` starts a test case as a process that has built
no node; a test that builds one without the helpers leaves the record to
the next (round 2's control R05b).

*The name.* An explicit `tailscale.hostname` wins. Otherwise each start
reads the state read-only (`readNodeState`): `<StateDir>/tailscaled.state`
opened through `fsutil.OpenAsFile` (a named pipe or a directory refused at
once) and read to 16 MiB at most, never through upstream's store
constructor, which writes `{}` over a missing file; the JSON object of state
keys upstream's file store keeps; the profile key under
`ipn.CurrentProfileStateKey`; that key listed among the profiles under
`ipn.KnownProfilesStateKey`, since upstream's start takes an unlisted one
for none (`profileManager.findProfileByKey`); and its prefs through
`ipn.PrefsFromBytes`, with a Persist whose attestation key takes any JSON and
keeps none of it (`ignoredAttestationKey`, the shape of upstream's own
fallback, a pointer, which encoding/json follows into the interface; a
non-pointer value there is an unmarshal error). Registered is a node ID in
those prefs, and the name is then their `Hostname`, which tsnet writes from
the name every start passes it. Not registered, so `tsnet.DefaultHostname`
(`1-bit-bridge`): no file; an empty one; `{}`; a machine key alone (an
abandoned authentication, or a first start that failed); a profile with no
node ID; one a logout deleted; one not listed. A state that is there and does
not read (it cannot be opened, it does not parse, a registered profile names
no hostname) is taken for neither: the start keeps the library name earlier
versions gave every node and says so in one Warn line naming no path.
`hasPersistedTsnetState`, the file's existence, is gone, and the
interactive-auth warning asks the reading too. After `bridge tsnet logout`
the state is gone and the next node is new. A backup does not carry the
state (`backup.Snapshot` copies the database, the tokens, the TLS pair,
bridge.yaml, the console account, the harvest state, and the player's
identity, its ID floor and its DSD state, never `<dataDir>/tailscale`, and a
restore writes only what the snapshot holds), so a restore leaves the node
on that box as it was, its name included, and on a box without the state
the node is new; the brief's "the restored profile's name" assumed
otherwise. Upstream's start takes the key the reading takes, on every OS:
tsnet passes `controlclient.LocalBackendStartKeyOSNeutral`, which
`ipnlocal.NewLocalBackend` turns into an empty OS for
`newProfileManagerWithGOOS`, so `readAutoStartKey` takes
`ipn.CurrentProfileStateKey` on Windows too. Until round 2 this said that
Windows starts from `ipn.ServerModeStartKey`, from `readAutoStartKey` read
without tsnet's flag (Codex on #1445). What stays open of the reading is
backlog B828 and B829.

*The refusal.* Upstream refuses a node with the variable set on a tailnet
carrying `nodecap.DataPlaneAuditLogs` (ipnlocal's local.go, about line
2048): `WantRunning` false, the local-log health warning ("The local log is
misconfigured: …" in `Status.Health`, once the tracker's built-in warnables
update), and `tsnet.Up`'s error, `tsnet.Up: backend: tailnet requires logging
to be enabled. Remove --no-logs-no-support from tailscaled command line.`, at
a start or while the node runs. The match is that substring alone
(`logsRequiredText`), pinned to the resolved module source by
`TestTheLogsRequirementIsUpstreamsWords`, which asks `go list -m -f
{{.Dir}} tailscale.com` (it reads go.mod and the module cache, so it works
with GOPROXY=off) and reads ipn/ipnlocal/local.go; a literal copied into a
test could not fail on a bump. A start whose error carries it ends in
`LogsRequiredError`, kept under `lifecycleMu` as the last start error
(`LastStartError`, cleared by a start that succeeds); `tsnetAdminStatus`
puts its words in LastError, since `Status` has no node to ask after a
refused start, and reads a running node's health through
`Server.LogsRequired`, beside the Stopped backend state the status reports.
The words: the tailnet requires Tailscale's logs and the node runs on it only
with their uploads on; set the key (Settings, Networking, or bridge.yaml) and
restart, after which the node uploads Tailscale's diagnostic logs; also
remove an inherited `TS_NO_LOGS_NO_SUPPORT=true` where it holds them off;
the bridge never turns them on itself. A fourth case covers uploads turned
off after the start with the key true and nothing inherited (the
coordination server's `DisableLogTail` debug message can).

*The seam.* `tsnet.Config.BeforeStart` runs on the upstream server just
before its Start, and an error from it ends the start there, the server
closed unstarted; nil in production, `tsnetBeforeStart` in cmd/bridge, so the
tests read the environment and the name each of the three paths hands
upstream and stop it before anything reaches the network. Serve is driven
through runServe itself: its node is built in runServe and started on the
tailnet goroutine runServe starts, so no narrower function does both.

*Evidence from a production binary.* No go test can see an upload:
`startLogger` returns at once under `testenv.InTest`. The
`scripts/tsnet-uploads/evidence.py` script runs a release-shaped binary
(CGO_ENABLED=0, the release tags, -trimpath), each run a throwaway install
made for it, in three arms: the key true; the key absent; the key true with
`TS_NO_LOGS_NO_SUPPORT=true` inherited. An unprivileged network namespace,
which would confine by address, is refused on legion0
(`kernel.apparmor_restrict_unprivileged_userns=1`: `unshare -rn` fails
writing uid_map), a user unit's `PrivateNetwork=yes` is not applied there
(the unit kept the host's network namespace), and a cgroup connect hook
needs CAP_BPF and CAP_NET_ADMIN. So the script confines the bridge by port,
not by address: Landlock (ABI 4 or later) lets it connect TCP sockets to the
proxy's port number alone, at any address, its network rule naming a port
and never an address, and seccomp refuses every IPv4 and IPv6 socket but
plain TCP (MPTCP refused, so Go falls back to TCP) and io_uring; HTTP and
HTTPS go through a proxy on 127.0.0.1 that records every request it receives
and refuses every one, and the verdict rests on that record. A connection to
the proxy's port number at another address is not prevented, and would not
be recorded. `TS_DEBUG_ALWAYS_USE_DERP=1` keeps the node from binding UDP,
and `GODEBUG=netdns=go` keeps lookups in Go's resolver. Before any run the
script checks the confinement inside and out (since round 2; round 1 took
any refusal): inside, another port with a listener, on 127.0.0.1 and on ::1,
is refused with Landlock's EACCES, every socket but plain TCP with the
filter's EPERM, and the proxy's port at ::1, where nothing listens, with the
kernel's ECONNREFUSED, never EACCES; outside, the same connections are made
and the UDP sockets created. A copy with Landlock not enforced, or with the
filter not installed, fails the check and runs no bridge. With
`--previous`, an earlier build, the script checks an upgraded install: the
earlier build's setup on a fresh install must ask for `log.tailscale.com`
and leave a non-empty `tailscaled.log*.txt` in the state dir, the earlier
build run again on a copy of that data dir must ask again (the positive
control on that state), and this build on the data dir itself, without the
key, must not ask in its whole window; every check of this build's runs is
over its runs alone. Round 2, on 2664276c with main's 71f707ab as the earlier
build, three rounds of 60 s windows: the key true, `CONNECT
log.tailscale.com:443` first at 2.23 to 2.30 s, two a window; the key absent,
its second start and the inherited true, none; the earlier build asked at
2.22 s and left 22,304 bytes of buffer, asked again at 0.11 s on the copy,
and this build on its data dir never asked in 60 s, leaving 92 bytes; the
coordination server's CONNECTs came in every run (25 to 27 a window); every
start of this build named the node `1-bit-bridge`. Round 1 (891f82d6) saw the
same for the three arms, and main's binary (9d59c8f6) asked for the log host
at 2.29 s, and at 0.08 s at its second start, named the node after the
library, and refused the key at load.

*Red first, and the controls.* Against main's behaviour (a copy of
9d59c8f6 with the new tests and a names-only shim: the seam, the names,
main's answers), 16 of the 21 new tests were red; the other five are the
seam's own test, the two pins of upstream, the clearing of the last start
error and the no-node commands' environment, which main meets by doing
nothing, and which controls turn red. 23 negative controls, each predicted
by name before it ran, were red where predicted (one, the library's name
restored, also on the unreadable state's line). Round 2's node built again
in one process was red first, with a names-only shim, on five rows of
internal/tsnet's later-node table and two of cmd/bridge's serve run twice;
its seven controls (R01 to R06 and R05b), each predicted, were red where
predicted but for the two the record names (R02 one row more; R05 green, and
its re-prediction R05b red). Round 3 rewrote both tests for the latch, as
`TestOnceThisProcessTurnsTheUploadsOffTheyStayOff` (each inherited value,
unset, false, true and 1, through no then yes, yes then no then yes, and no
then no, read through `strconv.ParseBool`, with controls and upstream's
setter before a node with the key, and before one without and one with it)
and `TestAServeRunAgainInTheSameProcessKeepsTheUploadsOff`, beside the tile's
`TestTheTileSaysWhatHoldsTheUploadsOff` and internal/admin's
`TestTheTailscaleTileShowsWhatHoldsTheUploadsOff`. On round 2's code, under
a names-only shim, they were red where a later allowing node was handed the
inherited value back (four rows of each matrix), on every reason and on the
words; seven controls (L01 to L07), each predicted, were red where predicted.
Round 4's latch row for a false written after the policy's true, flipped to
the uploads off, and its new row for a value written there that does not
read as a boolean were red on round 3's code. The tests read the variable
right after `NewServer` as well as at the upstream start, through
`strconv.ParseBool` wherever they read the upload state, exact text only
where they check the environment is preserved: a policy that writes `1`
where it writes `true`, which Tailscale reads alike, reddened 13 rows of
round 3's tests and none of round 4's. Three controls (M01 to M03), each
predicted, were red where predicted: the setter alone reverted reddens the
false row on its reads right after `NewServer` and at its upstream start,
and the unreadable row on its refusal, a status kept from the node's
construction reddens the live-read cases, and the setter placed after the
parse reddens the unreadable row alone. Round 5's rows were red on round 4's
code where they change what it did: a node without the key over an
unreadable value written in the process (refused), a node with the key over
one, inherited unset and true (the launch environment's refusal), and a
false written over an inherited true (held off by the launch environment);
its pins (a first node without the key with nothing inherited, `EnvOff`
false; a false written over an unset value; a 0 and a 1 written over an
inherited false) were green there. Nine controls (R01 to R07, N01, N02),
each predicted, came out as predicted: each of the policy's readings and
orders put back as round 4 had it, or made in a wrong order, reddens the
rows it changes, the refusal taken for the launch environment's or for
`OtherOff` reddens the two rows with the key, and a loader that reads an
empty variable as false reddens `TestAnEmptyVariableLeavesTheFilesConsent`,
while its check of an empty value alone dropped stays green, the parse
refusing the value. The record is in `ops/engineering-log.md`, 2026-11-09,
B749 part 3 and its rounds 2 to 5.

## B653 — each snapshot is verified, its verdicts kept with it and shown; the prune keeps what recovery needs (2026-11-09)

- **A snapshot records each captured file's size and SHA-256 in its manifest (`baselines`, the database's read once `VACUUM INTO` has written it) and publishes it with its verification record already `PENDING`; a verification (`backup.VerifySnapshot`) never replaces a baseline and records two verdicts, the database's (`databaseState`: its baseline; a regular file with no `-wal`, `-shm` or `-journal` beside it, of any kind, both asked before SQLite opens; then a read-only open of that same file on a connection of its own, `mode=ro&immutable=1` through modernc's read-only VFS over a filesystem that serves that one file, never `manifest.OpenStore`; `quick_check` answering exactly one `ok`, or with `--full` `integrity_check` and `foreign_key_check`; a `user_version` the ladder reaches, with the tables and columns `manifest.RequiredSchema` says that version holds; the bundle's entries unchanged after the close) and the other files' (`filesState`: there, a regular file, its size and SHA-256), each `PENDING`, `VERIFIED`, `FAILED` or `INCOMPLETE` with reason codes from `backup.Reasons`, which name no path; every reader takes `readVerdict`, which reads a manifest with neither, one written before verification, as `UNVERIFIED` (`no-baseline`; `--verify` checks its files' presence and records nothing), a `PENDING` by its owner (the kernel's lock on `.verification.lock` in the snapshot, which its capture takes right after it makes the folder, or a claim under the backups folder's lock, and holds until its verification ends, and which a dead process lets go): as one under way while the owner holds it, however old, and as `INCOMPLETE` (`interrupted`) at once once it is gone, and a `PENDING` with no owner lock to ask, written before owners were kept, by its age, past `PendingBound` `INCOMPLETE`; and a `VERIFIED` beside its veto, in the bundle or beside it in the backups folder, as `INCOMPLETE` (`unsynced`); and the record names the folder it was written for (`snapshot`), a reader reading one of another folder, a manifest copied there, as `UNVERIFIED` (`other-snapshot`)** (2026-11-09, backlog B653 PR 1; its second round for the owner, its third for the folder). **Don't open a snapshot's database through `manifest.OpenStore`, without `immutable=1` or beside a sidecar, replace a baseline with what a check found, read a stale `PENDING` as under way, or one whose owner is gone however young, say that `quick_check` checks an index against its table or a foreign key (only `--full` does), or take a record written for another folder for this one's.** `TestADamagedDatabasePageFails`, `TestACheckThatAnswersAnythingButOkFails`, `TestAFileWithOneByteChangedFailsTheFilesVerdict`, `TestABundleThatChangesWhileItIsCheckedFails`, `TestASidecarBesideTheDatabaseFailsBeforeSQLiteOpens`, `TestALinkInTheBundleFailsBeforeSQLiteOpens`, `TestVerifyingAWALHeaderDatabaseTwiceCreatesNoFile`, `TestTheDatabaseMustBeASchemaThisBridgeKnows`, `TestTheFullCheckEvaluatesTheExpressionIndexes`, `TestALegacyManifestReadsUnverified`, `TestAStalePendingReadsIncompleteAndAYoungOneSurvivesAPrune`, `TestADeadCaptureIsPrunedAndALiveOneIsKept` and `TestARecordCopiedIntoAnotherFolderReadsUnverified` pin it. → History-rules-cli-serve.md

- **…and every verification, `bridge backup --verify`, the startup pass's claim and the prune with its orphan sweep take one lock, `.backups.lock` in the backups folder where it resolves (`resolveSnapshotIn`, `resolveDir`: `.` in a snapshot, a relative path, a link to a snapshot and a backups folder reached through a link all name one folder, compared by `os.SameFile`; a snapshot outside the install's backups folder is refused, `ErrOutsideBackups`, and `--verify` loads the install's config to know it, exit 2), in every process (`fsutil.OpenLockFile`, `fsutil.TryLock`, taken behind this process's one holder of the folder, `folderTokens`): a verification from before it reads the manifest until its verdict is written and its prune has run, behind this process's one verification at a time, waiting for the lock within `AutoLockWait` (`VerifyCmdLockWait` for `--verify`) and then answering `ErrBackupsBusy`; the prune tried once, a round that finds it held skipped (`PruneResult.Skipped`, `SkippedFor`); and where the try is answered as no lock granted (`fsutil.KeepsNoLocks`, ENOLCK included), nothing protected runs: a verification answers `ErrBackupsUnlocked` and writes nothing, a prune and its sweep are skipped, a claim is refused; a verdict merges into the record under it (`mergeVerdict`: an `INCOMPLETE` replaces only a `PENDING` or an `INCOMPLETE`, and a database `VERIFIED` no completed check of a higher level) and is renamed into place through `atomicwrite.RenameWithRetryUnsyncedCtx`, which asks before every attempt whether the write was given up, behind a veto (`.verdict-unsynced`) wherever the record it replaces or the one it writes holds a `VERIFIED`, in the bundle or, where the bundle takes no file, beside it in the backups folder (`rootVetoPath`), kept where its owner cannot be given, and removed only once the folder's sync (`fsutil.SyncParentDir`) has answered, and only where a check that ended earns it (`earnsClear`: never for an `INCOMPLETE`, a stop's, a deadline's or a panic's; where what it found replaced both verdicts; or, where no veto stood before the verification began, over a `VERIFIED` carried over that it matched with one), each removal checking its write's and hold's give-up immediately before the call (`publishVerdict`'s `publication`, the unchanged record's after its veto lookup); these checks and calls have no atomic admission: a give-up between check and call still lets that operation run, and a removal or rename attempt already begun finishes after the give-up; removal of the last effective veto exposes what the completed check found and the write made durable, so a veto a failed write left stays over a `VERIFIED` a later verification carries over: a check of a snapshot whose record holds a `VERIFIED` puts its veto in place and syncs its folder before the check begins (`setAside`), and where none can be had durably checks nothing and writes nothing (`ErrRecheckRefused`), each veto creation attempt (bundle then root) and cleanup removal checking its step's and hold's give-up immediately before the call, including when its step registered after abandonment; a veto creation or removal that passed its check may finish after the give-up, and successful removal of the last effective veto restores the earlier check's record as it stood before the recheck, while another veto or a failed removal keeps the conservative reading, and where no directory can be synced at all (`fsutil.SyncsParentDir` false: Windows) runs its check without a prior veto and records no new verdict (`ErrRecheckNotRecorded`), but a completed FAILED component previously VERIFIED puts `putVeto` in place before read-back even beside an INCOMPLETE component; each attempt checks the step and hold before any filesystem call, and the cache stamp separately checks give-up; a successful veto with a skipped stamp is reported as vetoed with the console cache possibly stale until expiry or a folder change; an INCOMPLETE component disproves nothing; a veto there is not crash-durable, and if both attempts fail the error says it could not be vetoed; give-up before every location was tried instead says the veto could not be confirmed, and either skips its prune, with a later process still able to trust the stale VERIFIED; `abandonPending` writes nothing over a VERIFIED there; a recheck that recorded nothing, there or refused, is followed by no prune (`Result.PruneSkipped`, the reporters saying why); and any other write of a `VERIFIED` with no veto to be had writes nothing; its making and an ordinary write each move the backups folder's modification time (`stampChange`) while the step and hold permit the stamp; the recovery INCOMPLETE registered after refused publication admission stamps while its own fresh step permits it, with the hold still guarding every veto operation; a skipped stamp is a cache warning, never by itself a durability failure, and is reported separately from veto presence, and the console can retain its cached reading until expiry or a folder change; where no directory can be synced (Windows) the write is atomic and not known to be durable; every step of a verification that can wait on a filesystem (its paths' resolution, the lock's open and wait, every read of the manifest, the check, the write, the prune, the read-back) runs on one worker, which holds the slot, the lock and the snapshot's ownership until it ends; its caller answers within `StopWriteBound` of a stop, or of its own bound past its lock wait and its deadline, the abandoned worker keeping its hold until it ends and letting go at once a lock it takes after the stop, and every step under the lock, a verification's, the write of `INCOMPLETE` for one stopped before it began (`abandonPending`), a claim's (`backup.Claim`) and a prune's (`PruneContext`, on a goroutine of its own, its caller's wait bounded as a verification's), bounded from the moment the lock was taken (`hold.go`, `stepOf`: the steps before the check and the check's own give-up half of `StopWriteBound` past the deadline, `VerifyDeadline`; each step after it as long after the later of the deadline and its own start; `abandonPending`'s and a claim's `StopWriteBound` from the lock; a prune's `VerifyDeadline` from it), and so is a holder's let-go of the snapshot's ownership, a step of the hold before the lock goes (`alsoLetGo`, whatever took it: a capture, a claim its caller did not take, a step that took it late), and of the lock itself (`releaseBound`), begun in the section that takes the mark, after which a give-up marks this process alone and writes no marker; a step past its bound given up, its own give-up ended (never by its caller's stop, after which its `INCOMPLETE` is still written), so the next rename or removal whose check observes the give-up is skipped, and a verdict's write is admitted only where its hold was not given up before it, the give-up's check and the step's registration one section of the hold's lock with no I/O in it (`publishOf`), its holder writing its `INCOMPLETE` in a step of its own where it is not: its holder answers `ErrCheckGivenUp` naming the step, runs no check and no prune, writes nothing but its `INCOMPLETE` (`deadline`, merged, so a completed verdict stays), drops what a step answers late, and keeps the slot, the lock and the ownership until every step it gave up has ended, each let go once, on a panic too (a check's recorded `INCOMPLETE`, `aborted`), `VerifyResult.Recorded` saying only that what it wrote landed; the folder is marked by an overrun alone, never at the lock nor for a wait that overran nothing (a stopped verification's worker that handed its `INCOMPLETE` to `abandonPending`, whose own hold marks once its own step overruns: its caller's give-up of that worker marks nothing from the hand-off on, its release included, while a step of its own that overruns marks as any does), with no filesystem asked (the key resolved at the lock, and each spelling the folder was reached by, made absolute against the working directory read once as the process started), and the mark stands until every hold is let go (two holds given up under one key each until its own end): meanwhile a verification, a claim or a prune behind it answers `ErrCheckStillRunning` at once, naming the snapshot, or the prune, and the step, in this process by its mark, asked before any filesystem, and in another by the marker beside the lock (`.check-still-running`), which counts only while the lock is held, another process on storage that takes no writes waiting its lock wait out (`ErrBackupsBusy`); the doctor asks this process's mark first and bounds its reads of the folder (`doctorBackupsBound`), one read of a folder out at a time; a shutdown does not wait for it, the process's end ending it (what stays open is backlog B653's)** (2026-11-09, backlog B653 PR 1, and its second to seventh rounds). **Don't write a verdict, prune or reap without the lock, go on where none is granted, take the lock in a folder a path's spelling names, check a snapshot whose record holds a `VERIFIED` before its veto is durable, write over a `VERIFIED`, or write one, without its veto, remove the veto before the sync has answered, let a check that did not end, or a lower level's `VERIFIED`, replace a completed verdict, run a verification's filesystem step on its caller's goroutine, let an abandoned step's hold go before it ends, run a step under the lock with no bound, let a hold given up go of the slot, the lock or the ownership before every step it gave up has ended, start a check, prune or verdict write after observing a give-up (the recovery `INCOMPLETE` and read-back still run), record what a step answers late, wait behind it, mark a hold at its lock or ask a filesystem to mark one or to read the mark, drop the mark before the holds are let go, clear a veto that stood before the verification over a `VERIFIED` it carries, or any veto for an `INCOMPLETE`, omit the give-up check immediately before each rename attempt or veto removal, promise atomic admission of those calls, admit a verdict's write once its hold was given up or check that give-up apart from the step's registration, ask a removal only its write's step whether it was given up, let a recheck attempt a veto creation or cleanup removal without checking both its step and hold immediately beforehand, give a write its caller's stop for its give-up, recheck a `VERIFIED` behind a veto where no directory can be synced or write `INCOMPLETE` over one there, prune after a recheck that recorded nothing, let an ownership or the lock go unbounded or after the lock, write a marker once the lock's let-go has begun, mark a wait that overran nothing, ask the working directory at a key's use, or leave a read of the folder behind for every doctor run.** `TestAPrunerRacingAVerificationSkipsItsRound`, `TestTwoVerificationsOfOneSnapshotWriteOneAtATime`, `TestVerifyBesideTheTickersVerificationOfOneSnapshot`, `TestAQuickCheckNeverUndoesAFullOne`, `TestAStoppedCheckNeverReplacesACompletedVerdict`, `TestASyncFailureAfterTheVerdictsRenameReadsAsNotVerified`, `TestAnUnsyncedVerdictReadsAsNotVerifiedEverywhere`, `TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails`, `TestAVerifiedRecordLeftForAFailedOneIsVetoedWhereItsBundleTakesNoFile`, `TestALeftVerifiedThatCannotBeRecordedReadsAsNotVerifiedEverywhere`, `TestAVerdictWrittenByAnotherCallerShowsOnTheJobsCardAtTheNextPoll`, `TestACancelledVerificationWritesIncomplete`, `TestAStopWithABlockedSyncReturnsWithinItsBound`, `TestATickerStoppedWhileItsVerdictsSyncHangsReturnsWithinItsBound`, `TestAStopWhileTheManifestReadHangsReturnsWithinItsBoundAndKeepsTheLock`, `TestAStopWhileTheLocksOpenHangsReturnsWithinItsBoundAndTakesNoLockLate`, `TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting`, `TestAClaimStoppedWhileItsLockOpenHangsReturnsWithinItsBound`, `TestATickerStoppedWhileAVerificationsStepHangsReturnsWithinItsBound`, `TestNoLockNoProtectedWork`, `TestBridgeBackupVerifyTakesTheBackupsFoldersLockWhateverPathItIsGiven`, `TestBridgeBackupVerifyRefusesWhatIsNotInItsInstallsBackupsFolder`, `TestARecheckWithNoDurableVetoIsRefusedBeforeItsCheck`, `TestANewVerifiedIsNotWrittenWhereNoVetoCanBeHad`, `TestAFreshProcessReadsNoVerifiedAVerificationCouldNotRecord`, `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds`, `TestAVerificationWaitingItsTurnHearsOfACheckGivenUp`, `TestAPanicInAVerificationLetsGoOfWhatItHolds`, `TestACheckThatDidNotEndInAnotherProcessHoldsTheFolderUntilItEnds`, `TestEveryStepUnderTheLockIsBoundedAndMarked`, `TestASlowStepWithinItsBoundLeavesNoMark`, `TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries`, `TestARecheckGivenUpLeavesTheVetoItMade`, `TestARecheckWhereNoDirectoryCanBeSyncedChecksAndRecordsNothing`, `TestAGivenUpVerificationReadsItsRecordBack`, `TestTwoHoldsGivenUpUnderOneKeyEachStandsUntilItsOwnEnd`, `TestAStoppedVerificationsMarkStandsUntilTheWriteItStartedEnds`, `TestAStoppedVerificationMarksNothingUntilTheWriteItStartedOverruns`, `TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded`, `TestTheDoctorNamesAHoldGivenUpWhileTheBackupsFolderDoesNotAnswer`, `TestRenameWithRetryUnsyncedCtxRenamesNothingOnceItsContextEnds`, `TestAWriteGivenUpLandsNothingOnceItResumes`, `TestAnUnchangedRecordKeepsItsVetoOnceItsWriteIsGivenUp`, `TestARecheckThatDidNotEndLeavesTheVetoItMade`, `TestAVetoGoesOnlyWithACheckThatEndedAndEarnsIt`, `TestARecheckThatRecordedNothingIsFollowedByNoPrune`, `TestAStoppedVerificationRecordsNothingOverAVERIFIEDWhereNoDirectoryCanBeSynced`, `TestAPruneThatDoesNotEndIsGivenUpAndMarked`, `TestALockThatDoesNotLetGoIsMarkedUntilItDoes`, `TestAGiveUpOnceTheLocksLetGoBeganWritesNoMarker`, `TestAnOwnershipIsLetGoBeforeTheLock`, `TestASpellingIsKeyedAgainstTheDirectoryTheProcessStartedIn`, `TestAHoldsContextEndsWithIt`, `TestACallersGiveUpAfterAHandOffMarksNothing`, `TestTheDoctorKeepsOneReadOfABackupsFolderThatDoesNotAnswer`, `TestTheTickerAndTheDoctorNameAPruneGivenUp`, `TestTheVerdictsWriteIsAdmittedInOneSectionWithTheGiveUp`, `TestAVerdictsWriteIsAdmittedOnlyBeforeAGiveUp`, `TestNoVetoRemovalBeginsOnceItsHoldIsGivenUp`, `TestNoVetoRemovalBeginsAfterItsWriteIsGivenUp`, `TestAVetoSetAsideIsKeptOnceItsStepIsGivenUp`, `TestARecordNotReadBackIsSaidApartFromItsWrite`, `TestSetAsideAsksItsHoldBeforeEachVetoAttempt`, `TestSetAsideRemovalFinishesAfterGiveUp` and `TestRecoveryStepsKeepFreshSignalsAfterTheHoldIsGivenUp` pin it. → History-rules-cli-serve.md

- **…and every snapshot takes one path, `backup.SnapshotAndVerify` (the capture, `CaptureSnapshot`, which owns its snapshot from right after it makes the folder until its verification has ended, then `FinishCapture`: the verification and, under the same hold of the lock, the prune after it whatever the verdict), whose `Result` tells a failed capture from a kept bundle whose verification failed or did not finish: the ticker and `bridge backup` take it and say what was kept, `bridge backup` exiting 1 for a kept snapshot that is not a verified recovery point, and where a verdict was not recorded each (`--verify` too) says what the check found and, apart, what the record reads, of a snapshot not checked again the record an earlier check left, and of a verification given up how far its write went (`VerifyResult.Write`: "it wrote nothing" only where no write began, "its write did not finish" where one began and was given up) and what its record reads as read back, or that it could not be read back (`RecordRead`), and in every case a record not read back is said so, and why (`VerifyResult.ReadErr`: its read failed, or its read-back was given up), never printed and never stood in for by what the check found, how far the write went said from its outcome (`VerifyResult.Write`) apart from it, so a durable write whose read-back failed says it wrote its record and exits 1 (`ErrRecordNotReadBack`, joined to any other error, each keeping its identity), and the console's job logs one with its write's outcome, never as a verdict not recorded, and of a prune a recheck that recorded nothing skipped, that cause; the console's create takes the capture, answers once it is written, and runs `FinishCapture` as a job on the server's own context; with retention on the prune keeps the `keep` newest and beside them, beyond `keep` if it must, the newest snapshot whose database is `VERIFIED`, while none is the newest written before verification, the newest whose database is `FAILED` or `INCOMPLETE`, one at most, and every snapshot a reader reads `PENDING` (`keptByRecovery`), and removes none whose owner holds it as it is removed (`ownedNow`), and with it off nothing changes; the orphan sweep spares a capture under way; the startup pass is one rule (`decideStartupPass`): no snapshot where one from the last day has a database `VERIFIED`, nor where the newest from the last day owes its check (its database `PENDING` or `INCOMPLETE`), which it leaves to the live owner that holds it, or else claims (`backup.Claim`) and verifies in place, taking a snapshot after it where that check finds the database `FAILED`, whether or not its record could be written, or answers `ErrRecheckNotRecorded` with a vetoed read-back and a FAILED component in its own finding (`Found`), never from a leftover veto alone; otherwise a snapshot; so a restart loop keeps one snapshot, which the first start that lives verifies; where the backups folder grants no lock (`backup.ProbeBackupsLock`) the ticker takes none while the newest snapshot owes its check, nor while a check that did not end holds the folder (`backup.StuckCheckOf`), each read of the folder before a capture, its startup listing's too, bounded by the stop (`readBounded`), and `bridge doctor`'s `backup-lock` and `backup-check` lines say why; the check runs on a thread of its own at the lowest priority, its reads paced; `bridge doctor`'s `backup-newest` and `backup-verified` (their hints commands the CLI takes as worded, `--full` after the snapshot, the config given as `--config <bridge.yaml>`, the one the doctor read), `GET /api/backups` and the Jobs and Settings views read the manifests alone (`backup.ReadStatus`), never a verification or the live database, and name no path** (2026-11-09, backlog B653 PR 1, and its second to seventh rounds). **Don't prune the newest database-`VERIFIED` snapshot or one whose owner still owes it its check, keep a `PENDING` whose owner is gone by its age, decide the prune, the startup pass or the newest verified by the files' verdict, capture at a start beside a snapshot from the last day that owes its check, skip the prune after a kept snapshot for its verdict, verify a console snapshot on the request's context or leave it `PENDING` with no job to finish it, capture on a cadence where no lock is granted while the newest owes its check, or beside a check that did not end, decide the check in place on its record, read the folder before a capture on the ticker's own goroutine, advise a bridge backup command with no `--config`, say a verification given up recorded what its record does not read, say it wrote nothing of a write that began, print a record it did not read back or what a check found in its place, word a write from the error rather than its outcome, say a durable write whose read-back failed was not recorded, or let a reader run a verification.** `TestThePruneKeepsWhatRecoveryNeeds`, `TestACertificateThatDoesNotMatchItsBaselineStillAnchorsRecovery`, `TestADeadCaptureIsPrunedAndALiveOneIsKept`, `TestAQueuedVerificationKeepsItsCaptureFromThePrune`, `TestAClaimedSnapshotIsKeptFromThePrune`, `TestTheReaperSparesACaptureUnderWay`, `TestAClaimTakesOnlyWhatNoLiveOwnerHolds`, `TestRepeatedFailedVerificationsThroughTheTickerAndBridgeBackupStillPrune`, `TestRepeatedFailedVerificationsThroughTheConsolesCreateStillPrune`, `TestACancelledCreateRequestLeavesTheVerdictToCome`, `TestBridgeBackupVerifiesWhatItKeepsAndSaysSo`, `TestBridgeBackupVerifyRecordsAndSaysTheVerdict`, `TestTheStartupPassFollowsTheNewestRecentSnapshot`, `TestARestartLoopTakesOneSnapshotAndTheFirstBootThatSurvivesVerifiesIt`, `TestABackupsFolderGrantingNoLockGrowsByOneSnapshotAndTheDoctorSaysWhy`, `TestTheDoctorTheAPIAndThePrunerAgreeOnEveryState`, `TestTheDoctorsVerifyHintsAreCommandsTheCLITakes`, `TestBothDoctorsCarryTheBackupLines`, `TestTheReadsArePaced`, `TestTheVerificationRunsOnAThreadOfItsOwn`, `TestAStartupCheckThatFindsTheDatabaseFailedTakesASnapshotWhetherOrNotItsRecordIsWritten`, `TestATickerStoppedWhileItsStartupListingReadsANamedPipeReturnsWithinItsBound`, `TestATickerStoppedWhileItsProbeOfTheLockHangsReturnsWithinItsBound`, `TestTheTickerTakesNoSnapshotWhileACheckThatDidNotEndHoldsTheFolder`, `TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads`, `TestTheTickerSaysWhatAGivenUpVerificationsRecordReads`, `TestTheStartupPassSaysWhyARecheckThatRecordedNothingPrunedNothing`, `TestBridgeBackupVerifySaysTheWriteAndTheReadBackApart` and `TestTheTickerSaysTheWriteAndTheReadBackApart` pin it, and the console's `TestTheConsoleSaysARecordNotReadBackApartFromItsWrite`. → History-rules-cli-serve.md

*Why here.* `internal/backup` and the backup commands load `cli-serve.md`
(and `config-lifecycle.md`, at its budget), beside B745's bullet on the
player's files in a snapshot, and the doctor's backup lines are
`internal/doctor`'s, which loads this file too. The console's half is
`admin-console.md`'s B653 bullet, and the generated schema `scanner.md`'s.

*What main did.* A snapshot's manifest named its files and said nothing of
their bytes, and nothing read a snapshot back, so a copy damaged on its way
to the disk, or since, was found by the restore that needed it. The prune
kept the `keep` newest by time and deleted the rest whatever they held, so a
run of bad snapshots pruned the last good one; the startup snapshot was
skipped for any snapshot younger than a day.

*The database's check.* `mode=ro` alone, over a database whose header says
WAL, as the live store's does, makes a `-shm` and a `-wal` in the bundle
(measured 2026-11-08 with the module's SQLite, modernc.org/sqlite v1.61.0),
and a second verification would then refuse its own leftovers as sidecars.
`immutable=1` makes nothing. The
open goes through modernc's read-only VFS over a filesystem that serves the
file the verification already opened, under one name, every other name
answered as not there, so SQLite cannot open or create a journal, and the
bytes it reads are the ones the baseline was compared with, the same open
(`os.SameFile` against the Lstat before it). Every read, the hash's and
SQLite's, goes through one pacer that asks the check's context first, which
is how a stop reaches a statement SQLite is in. The functions the expression
indexes call are registered for every connection by internal/manifest, so
`--full`'s `integrity_check` evaluates them. A check that ends in an error
(a page SQLite cannot read) fails, and so does one that answers anything but
exactly one `ok`: a NULL in a NOT NULL column, written while the schema said
otherwise, is answered as a row (`NULL value in scan_state.v`) and no error
(measured 2026-11-09), and the damaged-page test alone, whose damage ends in
an error, passed a check that took any answer but an error (B653's control
A4). `quick_check` does not compare an index with its table, which is why
the public page says so and `--full` exists. The schema a `user_version` requires is generated from the ladder
(scanner.md's B653 bullet), never written by hand.

*One writer.* The lock is the kernel's on the open file (flock,
LockFileEx), so two opens in one process exclude each other as two
processes do, and a crash drops it; the file stays and means nothing
without the lock on it. Before the lock a verification takes this
process's one slot, so the bridge runs one verifier at a time and a second
waits its turn. The bridge's own verifications wait for the lock as long as
another process's may run (`AutoLockWait` is `VerifyDeadline`, 30 min), so a
fresh snapshot's check waits behind a `--verify --full` at a terminal
rather than leave its snapshot `PENDING`; `--verify` waits 15 s and says
the folder is busy. Where the try's answer is one `fsutil.KeepsNoLocks`
reads, nothing protected runs (the second round, below); the first round
went on without the lock there.
The merge keeps what a record says that a later check could only weaken: a
check that did not end replaces only a `PENDING` or an `INCOMPLETE`, a quick
`VERIFIED` never a completed full check, and a `FAILED` replaces anything.

*Publication.* Once the manifest's rename has landed every reader reads the
new record, so a folder sync that fails after it cannot be taken back by an
error: a `VERIFIED` read after that error would be a recovery point nobody
knows survives a crash. The veto is a separate entry whose presence alone
vetoes, put before the rename and removed after the sync, so whichever step
fails, the veto's own removal included, every reader (the doctor, the
console, the pruner, the startup skip) reads `INCOMPLETE` (`unsynced`); a
crash meanwhile leaves the old record or the new, with or without the veto,
and each reads `VERIFIED` only from a check that passed. Rejected: writing
`VERIFIED` only after a sync, since the rename is what publishes it and the
sync comes after; a second state rewritten once synced, which has the same
window at its own rename. Where no directory can be synced (Windows), the
write is atomic and not known to be durable: a crash can leave the previous
record, a `PENDING` among them, which reads `INCOMPLETE` once `PendingBound`
has passed, subject to round 12's no-barrier recheck residual when disqualification
cannot be written or survives no crash. The veto takes the manifest's owner (`fsutil.KeepOwner`, the job writers' rule
in `config-lifecycle.md`), so a `--verify` run as root leaves the service a
veto it can make and remove as its own, and one there already, of any
owner, is kept, its presence alone being the veto: a root-owned veto the
service could not open would have kept it from recording any `VERIFIED` of
that snapshot again (`TestJobWritersKeepTheInstallOwner` found it).

*A stop.* One bound, counted from the first wait that sees the stop and
shared by the check's return and the write, so a stopped verification
returns within `StopWriteBound` (2 s) of seeing it whatever it was doing, a
read a hung mount holds included; serve's wait for the backup ticker
(`backupShutdownWait`, 45 s) is far longer. A verification stopped before it
began, in the slot's queue or the lock's wait, writes `INCOMPLETE` over its
`PENDING` within the same bound. `PendingBound` is `VerifyDeadline` plus
`AutoLockWait` plus ten minutes; since the second round it reads only a
`PENDING` with no owner lock to ask, and one this bridge captures is read by
its owner. The first round's bound covered the check and the write alone,
and the second puts every step that can wait on a filesystem under it.

*The prune.* Every decision reads the database's verdict alone: a snapshot
whose files failed (a certificate rotated by hand into it, say) and whose
database is `VERIFIED` is still the recovery point. The newest verified one
stays however many failed ones come after it; while none is, the newest
written before verification stays, which is all an upgraded bridge has
until its first check passes; the newest that failed or did not finish
stays as evidence, one at most, so a run of failures neither fills the disk
nor erases what they found. The prune runs after every kept snapshot,
whatever its verdict: a bridge whose every check fails still prunes. Inside
`Finish` it runs under the verification's own hold of the lock, so no other
process's prune runs between the verdict and it.

*Readers.* `backup.ReadStatus` reads the manifests and nothing else: no
surface runs a verification or opens the live database to say what a
snapshot is. A snapshot is named by its folder's name, a time stamp, and no
line or body names a path. The record is in `ops/engineering-log.md`,
2026-11-09, B653.

*The second round* (Codex's and the Claude API's review of the first, and
both reviews of its brief). **A restart loop.** On an upgrade every
snapshot is legacy and none is `VERIFIED`, so the first round's startup
skip no longer held, and a boot that died before its verification ended
(an OOM kill, a panic loop under `Restart=always`) captured again at every
start, each capture a `PENDING` younger than `PendingBound` that every prune
kept: hundreds of database copies before the first prune could drop one. A
capture now owns its snapshot from right after it makes the folder until its
verification ends, through the kernel's lock on `.verification.lock` in it
(`owner.go`), which a dead process lets go; a reader reads a `PENDING` whose
owner is gone as `INCOMPLETE` at once, and the prune keeps a `PENDING` only
while a reader reads it so. The startup pass is one rule,
`decideStartupPass`: where the newest snapshot from the last day owes its
check and no live owner holds it, the boot claims it under the folder's lock
and verifies it in place, capturing only where that check finds the
database `FAILED`, so a restart loop keeps one snapshot and the first boot
that lives verifies it (a recent `FAILED` is no reason to skip: it is not a
recovery point). Rejected: one hold of the folder's lock across the capture
and its verification, which blocks every other process's check and prune for
a whole capture and still says nothing of the snapshots it does not hold;
and protection by age, which the queue's own wait outlasts (five captures
queued behind one 25-minute check passed `PendingBound` before their turn).
The lock file is the per-snapshot marker the review asked for: keyed by its
owner, released when its owner dies. Where a lock is carried out as a POSIX
record lock (a Linux NFS mount), a second open of a lock file in the
owning process takes the owner's lock again and its close lets it go, so
this process asks its own records first (`ownedHere`, `folderTokens`) and
never opens a lock file it holds.

**A `VERIFIED` left behind.** The first round's veto covered a record that
would hold a `VERIFIED`, and a verified snapshot whose check then failed
both verdicts, its write failing before the rename, kept a `VERIFIED` every
reader read. The veto now goes in place wherever either record holds one,
beside the bundle where the bundle takes no file, and stays where its owner
cannot be given; where neither can be made the write goes on, since landing
the `FAILED` is the better outcome, and only a write that fails then too
keeps the old record, `ErrRecordKept` saying so to its caller. The veto's
making moves the backups folder's modification time as the write does, so
the console's Jobs card, which caches what the folder holds by that time,
reads the folder again.

**The lock's name, and no lock.** `bridge backup --verify .` inside a
snapshot made `.backups.lock` in the snapshot, and a link to a snapshot in
the link's folder, so a verification and a prune could run side by side.
The snapshot and the backups folder are resolved now and compared by
`os.SameFile` (a spelled comparison refused every snapshot on macOS, whose
temporary folders are reached through `/var`), the install's config names
the folder, and a snapshot outside it is refused. Where the try is answered
as no lock granted nothing protected runs, since ENOLCK does not show that
no other process is checking or pruning: the verification answers
`ErrBackupsUnlocked`, the prune and its orphan sweep are skipped (the sweep
ran before the lock in the first round), and, so that a folder that never
grants a lock does not grow by a snapshot a pass, the ticker takes none
while the newest owes its check, and the doctor's `backup-lock` line says
why.

**The bound.** The first round's stop bound covered the check and the
write; the manifest's reads, the lock file's open and its wait ran on the
caller's goroutine, and a check its context could not reach was given up on
the caller's stop alone, never on the deadline. Every step now runs on one
worker that holds the slot, the lock and the ownership until it ends, so an
abandoned step keeps its hold and nothing protected runs beside it, in this
process or another; a lock taken after the stop is let go at once, and the
deadline frees a hung check. The record is in `ops/engineering-log.md`,
2026-11-09, B653 PR 1 round 2.

*The third round* (Codex's and the Claude API's review of the second, and
both reviews of its brief). **A check given up.** The second round gave a
check its context could not reach up past its deadline, and let go of the
slot, the folder's lock and the capture's ownership as its worker returned,
the check's goroutine still reading the bundle, and pruned after it: another
verification or a prune could run beside a check with the snapshot's files
open. The worker now answers its caller first and lets go of each hold,
once, only when the check's goroutine has ended; it runs no prune, records
the `INCOMPLETE` it gave up with and drops what the check answers later, and
marks the folder, so a verification, a claim or a prune behind it answers
`ErrCheckStillRunning` at once rather than wait (in this process by the mark,
in another by the marker it writes beside the lock, read only while the lock
is held and removed by the next holder), and the doctor says so
(`backup-check`). A shutdown does not wait for it: the process's end ends it.
A check that panics records `INCOMPLETE` (`aborted`), and a panic in the
worker's own steps answers an error; either lets go of all the same.
Rejected: deferring the prune until the check ends, which needs a holder for
it once its caller has gone; and ending the goroutine at the deadline, which
Go cannot do. What stays open here is backlog B653's.

**No `VERIFIED` survives a failed write.** Where no veto could be made, the
second round wrote over a `VERIFIED` all the same, or, for mixed verdicts,
returned before its rename with the database still read `VERIFIED` beside a
check that found it `FAILED`; and its veto, made just before the write and
synced best effort, left the time between the check and the write, and a
rename the folder's sync did not make durable, with nothing durable beside
the old record. A check of a snapshot whose record holds a `VERIFIED` now
puts the veto in place, and syncs its folder, before it begins (`setAside`),
and where that cannot be had checks nothing (`ErrRecheckRefused`, which the
CLI says apart from a check whose write failed, with the record an earlier
check left): a crash or a failed write at any point after leaves the old
record behind a durable veto. Any other write of a `VERIFIED` with no veto
writes nothing. The record names its folder, and a reader reads a record of
another folder as `UNVERIFIED` (`other-snapshot`); a record has no
generation of its own, since a crash that brings an old record back finds the
durable veto beside it. Rejected: removing the record before the check,
which leaves a bundle with no manifest for the orphan sweep to reap; and two
records, which moves the question to which of them a reader trusts.
`bridge backup --verify` and the ticker say what a check found and, apart,
what the record reads, and the startup check in place decides on what the
check found.

**The ticker's reads, and the doctor's hints.** The ticker read the backups
folder before a capture (its startup listing, the lock's probe) on its own
goroutine, so a hung mount, or a named pipe where a manifest goes, held its
stop past every bound: each read runs bounded by the stop now
(`readBounded`). The doctor's hints named no config, and their test put one
in front of each; they give `--config <bridge.yaml>`, the one the doctor
read, and the test runs each as worded. The record is in
`ops/engineering-log.md`, 2026-11-09, B653 PR 1 round 3.

*The fourth round* (Codex's and the Claude API's review of the third, and
both reviews of its brief). **Every hold bounded from its lock.** The third
round bounded the check alone: putting the veto in place (its folder's
sync), the verdict's write, the prune after it and the read-back ran under
the lock with no bound and no mark, and so did `abandonPending`'s write of
`INCOMPLETE`, so storage that answered reads and not writes held this
process's slot, the folder's lock and the capture's ownership while the
verification's caller lived (for the ticker, serve's whole life), other
verifications waiting their turn, prunes answering busy and the doctor
saying nothing; and a step that answered after its caller had been answered
went on to a fresh check and a prune. Each step under the lock runs on a
goroutine of its own now, bounded from the moment the lock was taken
(`hold.go`), and a step past its bound is given up: its holder answers, runs
no check and no prune, writes nothing but its `INCOMPLETE`, and keeps all it
holds until every step it gave up has ended; the folder is marked by the
overrun alone, with no filesystem asked, and the mark, which every reader
asks before any filesystem or bounds, stands until every hold is let go.
Rejected: a mark at the lock, which would refuse every healthy concurrent
run and show the doctor a stuck check for each; and a watchdog in the
caller alone, which cannot tell a slow step from a hung one, nor keep the
worker from its next step. Across processes the marker cannot land on
storage that takes no writes, and another process answers `ErrBackupsBusy`.
Run twenty times under the race detector, the step test then found two
holds of one folder given up at once, a stopped verification's worker and
the write of `INCOMPLETE` it started, whose marks shared a key: the later
overwrote the earlier and its end erased the key while the other hold
stood, and the worker's mark named its wait for the lock and went with the
worker while the write, within its own bound still, had none yet. A key
keeps every mark now, and the worker names the write and keeps its mark
until the write has ended, though its slot goes as it ends. Rejected:
keeping the slot until the write ends, which leaves this process's other
verifications waiting behind a hung write wherever the worker's caller
took its answer before giving it up.

**No veto cleared over a `VERIFIED` carried over.** The third round removed
every veto once a write landed, so a retry beside the veto a failed write
left (stopped before it began, past its deadline, given up, panicking, or a
quick `VERIFIED` over a full one) cleared it over the `VERIFIED` the merge
carried over, and the pruner, the doctor and the console took for a
recovery point a snapshot a completed check had found damaged. A veto goes
now only where the record is wholly what the check found, or where none
stood before the verification began. Each removal checks the give-up just
before its call; the seventh round below states the check/call window and
the late-removal exception for a recheck too. Rejected: never clearing
a veto beside a `VERIFIED` carried over, which keeps the veto a completed
quick check made over a full `VERIFIED` nobody doubted, so a routine recheck
would leave the snapshot read `INCOMPLETE` until the next full check.

**No recheck recorded where no directory can be synced.** On Windows
`fsutil.SyncParentDir` is a no-op that answers nil, and the third round took
a veto not known durable for a durable one. Where `fsutil.SyncsParentDir` is
false a recheck runs, says what it found and records no new verdict
(`ErrRecheckNotRecorded`). Round 12 supersedes the original no-veto rule:
a completed finding that disproves a VERIFIED component vetoes the record
before read-back, while an INCOMPLETE leaves it alone; see round 12 below. Rejected: refusing the check as well, which would
leave a Windows operator no way to learn of damage.

**What the record reads.** `VerifyResult.Recorded` was true for a give-up
whose merge kept a completed verdict and wrote nothing, and the CLI and the
ticker said "not recorded" of an `INCOMPLETE` that was written; `Recorded`
says now that what was written landed, and both say whether a give-up wrote
its record and what that record reads as read back. The round-3
fresh-process test's header claimed round 2 red on every reader; it says
now what round 2 did, case by case, and pins the refusal by the check the
process began. Codex's fourth finding on round 3, in code this PR does not
change, is backlog B826. The record is in `ops/engineering-log.md`,
2026-11-09, B653 PR 1 round 4.

*The fifth round* (Codex's and the Claude API's review of the fourth, and
both reviews of its brief). **Each rename attempt checks the give-up.** The fourth
round gave a step up and answered its caller, and the step's write went on:
once a stall inside its staging or before its rename ended, it renamed its
`VERIFIED` into place and removed the veto, so a caller told the
verification was given up, with nothing recorded, was followed by a record
every reader took for verified; and so did `abandonPending`'s `INCOMPLETE`.
Each step under a hold has a give-up of its own now, which only that step's
give-up ends (its bound, or its caller's abandon of the hold while it runs),
and a write asks it before every attempt at its rename and before it removes
a veto. The check and call are not atomic: an attempt that passed its check
can still run if the give-up arrives before the syscall, and a rename
attempt already begun finishes afterward. The reporters say a write given up did not
finish, never that it wrote nothing. Rejected: the caller's context as that
signal, which a plain stop ends too, after which the worker must still write
its `INCOMPLETE`.

**A veto only for a check that ended.** The fourth round let a recheck that
ended `INCOMPLETE` within its bound (past its deadline, stopped, or on a
panic) remove the veto it had made wherever none stood before, so the
`VERIFIED` its record carried over read verified though the recheck never
finished. A veto goes now only where every verdict the record carries over
was matched by a `VERIFIED` in what the check found (a quick check over a
full one, where no veto stood before), and never for an `INCOMPLETE`.

**No prune on a record a recheck could not keep.** Where no directory can be
synced, round 4 left a disproved `VERIFIED` readable, and the prune under
the same hold trusted it and could remove the older good snapshot. Round 12
also vetoes that record for later readers and prunes, subject to its stated
write-failure and crash residuals. A recheck that recorded nothing, there or refused for
want of a durable veto, is followed by no prune; the next snapshot's prune
runs as usual. `abandonPending` keeps the platform rule: no write over a
`VERIFIED` where no directory can be synced.

**Every holder and every step bounded.** The prune run on its own held the
lock with its removals unbounded; letting go of the lock and of the
snapshot's ownership had no bound; and an ownership a claim took late, or
that its caller never took, was let go on goroutines nothing waited for,
after the lock. The prune holds the folder as every holder does; the hold
lets go of the ownership before its lock, as a bounded step, and of the lock
within a bound too, its let-go begun in the section that takes the mark, so
a give-up after it writes no marker beside a lock no longer held. A stopped
verification's worker that handed its `INCOMPLETE` to `abandonPending` marks
nothing while it waits, and the write's own hold marks once its own step
overruns. Rejected: the fourth round's mark there, which named a wait that
had overrun nothing. Run twenty times under the race detector, the first
form of that quiet, which ended as the worker's release began, let the
caller's give-up, landing as the worker answered, mark the folder at the
worker's wait for the lock once in twenty; the quiet holds now for the
caller's give-up for the hold's whole life after its hand-off, and a step of
its release that overruns marks as before. A spelling's key is made
absolute against the working directory read once as the process starts, a
hold's context ends with it, and the doctor keeps one read of a folder out
at a time. Restore's lock on its source snapshot is backlog B826. The record
is in `ops/engineering-log.md`, 2026-11-09, B653 PR 1 round 5.

*The sixth round* (Codex's and the Claude API's review of the fifth, and
both reviews of its brief). **No write admitted after a give-up.** The
fifth round handed each step a fresh signal of its own, which only that
step's give-up ended, so a give-up between a verification's last check of
its hold and the registration of its verdict's write found no step of the
write's to end, and the write admitted then ran on a live signal: it
published the check that had ended, and cleared the veto, after its caller
was told the verification was given up. The check of the give-up and the
registration of the write's step are one section of the hold's lock now,
with no I/O in it: a give-up before it refuses the write, which never runs,
and the worker writes its `INCOMPLETE` in a step of its own; a give-up after
it ends the signal the write was registered with. Rejected: running the
write on a signal ended at its admission, which lands nothing, the
`INCOMPLETE` it must still write included.

**Each veto removal checks the give-up.** The fifth round asked the
write's signal once before its removals, and the unchanged record's branch
before its lookup of the veto, so a stall in the first removal, or in the
lookup, let the next removal run after the give-up. Every removal asks now,
immediately before it, the write's signal and its hold's give-up, since a
write begun after the hold was given up, the `INCOMPLETE`'s, has a signal
no earlier give-up ended. The check and removal are not one atomic
admission: a give-up between them still lets that removal run, and a
removal already begun finishes after the give-up. Only successful removal
of the last effective veto exposes what the completed check found and the
write made durable. Rejected: keeping the veto until that removal settles,
which would mean a removal no give-up can interrupt and a write that waits
on it past its bound. `setAside`, putting a veto in place before a recheck,
checks both its step's signal and its hold before each creation attempt
and cleanup removal, including if its step registered after abandonment.
A veto creation or removal that passed its check may finish after the
give-up. Successful removal of the last effective veto restores the earlier
check's record as it stood before the recheck. A second veto or failed
removal keeps the conservative reading; read-back reports its observation
at that time, not the eventual result of an in-flight call.

**The read-back said apart from the write.** The fifth round's read-back
answered only whether it read the record, and the CLI printed what the check
found in its place: a quick check that found `VERIFIED` over a record whose
full check had `FAILED`, its read-back failing, printed `VERIFIED` and
exited 0. The read-back says why it read nothing (its read's failure, or its
own give-up), the verification answers `ErrRecordNotReadBack` beside
whatever else it answers, each error keeping its identity, and the CLI, the
ticker and the console say what the write did from its outcome and, apart,
that the record could not be read back, or that its read-back was given up:
never what the check found as the record, and never that a durable write
failed or was not recorded. The record is in `ops/engineering-log.md`,
2026-11-09, B653 PR 1 round 6.


*The seventh round* (2026-11-10). The lock-free checks directly before each
removal, rename attempt and pre-recheck veto creation remain deliberate:
no new `h.mu` admission and no I/O under it. The check/call gap permits the
operation to run after a give-up, and each operation already begun when the
give-up lands finishes afterward. `setAside` asks the hold as well as its
step before both candidates and cleanup; ordinary `stepOf` still starts
`stepIncomplete`, `stepReadBack` and `abandonPending`'s INCOMPLETE after a
give-up with fresh signals. The admission pin injects abandonment after
lock acquisition but before the check, rejecting a check moved immediately
before `h.mu.Lock`, and observes an abandonment blocked on the mutex in the
registration case instead of assuming a 200 ms wait proved it tried.

The refused-recheck fixture parks bundle-veto removal after failed syncs.
It covers one veto (the late removal restores the earlier record) and two
(the root veto survives and the record still reads INCOMPLETE). The console
switches on every error's `Write`: only none/failed say not recorded;
not-durable says written, not known durable; unfinished says the write did
not finish before give-up. Every branch logs `write` and `recordRead`,
including shutdown errors. Its sync-failure case injects the directory
barrier with `SetSyncVerdictForTest`, observes FAILED already published,
and checks the log and subsequent console read. Validation is delegated to
the orchestrator; this edit-only round ran no builds, tests or docs checks.
The record is in `ops/engineering-log.md`, 2026-11-10, B653 PR 1 round 7.


*The eighth round* (2026-11-10). A caller that stops waiting cannot infer
that nothing was written. `verifyAndThen` returns `WriteUnknown` when its
outer expiration wins, or a recovered worker panic leaves no outcome.
`abandonPending` returns its completed INCOMPLETE publication's outcome to
both cancellation handoffs (slot queue and folder-lock wait); a missing
answer is unknown. These handoffs do not invent a record read-back. Every
consumer uses `Write`, including CLI and ticker errors with empty `Found`.
The console logs shutdown errors with `write` and `recordRead`, and calls an
unknown outcome unknown. Its deterministic outer-expiration case parks the
worker answer and asserts the expiration hook; a separate uncancelled case
lets the worker report its unfinished write. Both observe FAILED already
published before the directory sync stalls. Additional cases cancel
`ScanCtx` during the check and give up a staged write.

Pins: `TestAStopBeforeVerificationReportsItsIncompleteWrite`,
`TestTheConsoleReportsCancellationAndEveryWriteHandoff`, and
`TestTheReportersKeepWriteProgressWithoutAFoundVerdict`. The fixture and
cleanup invariants are in `History-rules-build-ci-tests.md`'s eighth-round
entry; the supporting record is in `ops/engineering-log.md`.


*The ninth round* (2026-11-10). An outer expiry remains `WriteUnknown`:
a record may have been written, or may still be: the verdict, or INCOMPLETE
once the stalled step ends. Rejected publication falls back to
`stepIncomplete` with a fresh signal, and a blocked lock attempt can still
hand off to `abandonPending`. Never infer `WriteNone` from the absence of an
admitted verdict write at expiry.

`TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting` now also
parks recovery before publication and independently parks the worker answer,
asserts the outer-expiry hook and Unknown while the record is PENDING, then
releases recovery and reads INCOMPLETE from disk before releasing the answer.
The original worker-answer case still proves the shorter check deadline.

The ticker suppresses an outer expiry's shutdown error when
`ctxerr.WithoutCancellation` leaves nothing and `Found` is empty: its verdict
is unknown. Round 11 below supplies the rule when a verdict is known.
A step given up, a record not read back, or a write failure accompanying
cancellation is still reported. The contrasting cases
are in `TestTheTickerSilencesOnlyCancellationWithoutAnotherFailure`.
`TestTheReportersSayWhenAnIncompletePublicationWasNeverReadBack` uses
`abandonPending`'s actual shape (INCOMPLETE Found, WriteRecorded, RecordRead
false, ReadErr nil): it says "its record was not read back", never "could
not be read back". No reporter behavior changes in this round.


*The tenth and eleventh rounds* (2026-11-10). The ticker decides shutdown
silence from the verdict. Round 10 joined a non-cancellation verdict to the
context error in one `verifyHeld` stop branch. Round 11 removes that join:
both stop branches return the stop's own error, preserving CLI and console
behavior and covering a stop that arrives between those branches.

With an error, the ticker is silent only when `ctxerr.WithoutCancellation`
leaves nothing AND `Found` is empty, passing or `CancelledOnly`. An empty
`Found` at outer expiry means the verdict is unknown, even if the worker
later publishes an aborted or expired verdict. A known non-cancellation
verdict is reported by the found-verdict branch with its write words; when
no other failure remains, the line ends with `res.VerifyErr`, never `<nil>`.
With no error, silence requires a stopped context and a `CancelledOnly`
read-back `Record`; passing records still use the normal success output.

`Verification.CancelledOnly` requires the database to be VERIFIED,
UNVERIFIED with only `ReasonNoDatabase`, or INCOMPLETE with only
`ReasonCancelled`; the files must be VERIFIED or INCOMPLETE with only
`ReasonCancelled`; at least one must be that cancelled INCOMPLETE. Anything
else is reported, including PENDING or any other UNVERIFIED beside a
cancelled verdict. Never decide silence from the database's state alone,
from the context ending alone, or from the error without its found verdict.

`TestTheTickerReportsAPanickedCheckAtShutdown` reproduces the overlap by
panicking in the real database check and cancelling through
`SetAfterCheckForTest`, requires bare `context.Canceled` (not merely
`errors.Is`) and database/files INCOMPLETE: aborted on stderr. Its second
case stops after the worker answer, exercising the no-error path.
`TestTheTickerReportsFoundVerdictsBesideCancellation` requires aborted,
deadline and FAILED found verdicts beside bare cancellation to reach stderr
with the write/read-back words and the stop's own error; cancelled, empty
and passing found verdicts stay silent.
`TestTheTickerSilencesOnlyCancelledRecordsAtShutdown` includes a cancelled
database beside failed or aborted files, cancellation-only contrasts,
missing and extra reasons, and round 11's database PENDING, no-baseline,
unknown-record or other-snapshot beside cancelled files, plus files PENDING
or UNVERIFIED beside a cancelled database. Each non-cancellation row requires
its verdict on stderr with no stdout.

The expiry error is built by `verifyExpiryError` in production and exposed
through `VerifyExpiryErrorForTest` for the outer-expiration reporter case.
A change in its wrapped causes must reach that case; its control adds a
non-cancellation cause to the constructor and requires silence to fail.
The supporting record, round-10 orchestrator validation and round-11
control handoff are in `ops/engineering-log.md`. Round 11's controls restore
only the shared join, only the old ticker error branch, and loosen each side
of `CancelledOnly` separately. No round-11 execution is claimed by the
edit-only worker.

## B653 PR 1 round 12: disproved records and precise verification reports

A no-barrier recheck (Windows, or `SetSyncsParentDirForTest(false)`) still
leaves the manifest byte-identical and answers `ErrRecheckNotRecorded`.
A completed component check disproves it when that component was VERIFIED
and is now FAILED, even beside an INCOMPLETE component (round 13).
Before read-back, `verifyHeld` runs `putVeto` in a bounded step and stamps the
backups root while step and hold permit it. Round 13 guards both attempts
and stamping, reporting a skipped stamp separately from veto presence. The existing bundle veto, or its root fallback, makes shared
`readVerdict` downgrade the old VERIFIED to INCOMPLETE: unsynced for List,
NewestVerified, doctor, console, startup and subsequent prunes. The Jobs
cache sees that stamp on its next poll. A files-only disproof also vetoes
the record: the veto is of the record, not just the prune's anchor choice.

This cannot promise crash durability where directories cannot be synced.
If neither veto location can be written, the error explicitly says the
disproved record could not be vetoed and a later process may still trust
its stale VERIFIED. The current verification skips its prune, as every
recheck that records no new verdict does. Skipping that prune is not a
persistent solution to failed disqualification, and the guide says so.
After a successful veto, `keptByRecovery` is unchanged: configured newest,
older trustworthy anchor, one evidence snapshot, and pending snapshots;
intermediate failures still go. An INCOMPLETE component itself disproves
nothing; a completed failure of its sibling can still require a veto.

The staged manifest uses `tmp.Chmod(0o600)` on its open descriptor before
sync and close. Its test replaces the exact staged pathname at a per-call
seam before that mode change, asserting the target's mode, not just bytes.
The stuck marker removes an existing entry (ignoring only not-exist), then
creates with O_EXCL. A nonempty directory that cannot be removed ends the
attempt; a racing replacement ends exclusive creation without following
it. A per-mark seam pins the removal/create window. The Unix staging race
is skipped on Windows, which does not permit replacing that open file;
marker symlinks are exercised where the Windows account can create them.
Since B871's round 5 the marker is published from a temporary file through
a link ("B871 round 5" below).

Schema table and column collection distinguish missing names from query,
Scan and iteration errors. `columnsOf` checks `rows.Err()` after the loop,
even when every required name arrived. The caller reports cancellation as
INCOMPLETE and other read failures as open-failed. The tests use a
per-connection SQL driver, injecting the three failures separately into
real database/sql machinery; table errors and cancellation are preservation
coverage. No production global seam is needed.

The doctor's fallback advice tests database FAILED first, then INCOMPLETE
in either component, then VERIFIED database with FAILED files, then
UNVERIFIED no-database with FAILED files, otherwise neutral advice. The
files-only no-database hint makes no recovery-point claim. Existing passing,
pending and legacy handling remains ahead of these fallback cases.

`VerifyResult.NotStarted` recognizes ErrBackupsUnlocked, ErrBackupsBusy and
ErrCheckStillRunning only with WriteNone. CLI, ticker and console report
that verification did not start, with the error's reason. Busy plus a
cancellation write still reports its actual outcome; error identity alone
cannot decide the wording. The console's production job calls
`reportBackupVerification`, so direct reporter cases use the same log site.
The CLI's no-barrier read-back lead is neutral ("its record reads now"),
since a failed veto may have left the old VERIFIED readable.

Regression cases and independent controls, not yet executed by the
edit-only worker, are recorded in the engineering log's round-12 entry.
The test teardown changes are in History-rules-build-ci-tests.md's round 12.


## B653 PR 1 round 13: component disproof, descriptor modes and guarded vetoes

`disproves` compares each component with its own prior state. FAILED over
VERIFIED is conclusive even while the other component is INCOMPLETE; the
latter component supplies no disproof of its own. The mixed database case
expires only the files phase's internal context, after real database damage
has been found, without stopping the verification caller (round 14), and
runs retention in a fresh process. The older genuinely
verified anchor must survive, and the intermediate failure must be removed.

The staged and vacuum permission changes use descriptors. `writeManifest` and
`writeFromHashed` chmod the still-open staged file before sync and close.
`vacuumInto` opens SQLite's output with no-follow semantics, then requires
a regular descriptor before chmod. Unix uses O_NOFOLLOW and O_NONBLOCK;
Windows opens the final reparse entry itself, with BACKUP_SEMANTICS so
directories, directory symlinks and junctions reach the non-regular refusal.
Only final-component symlink/reparse redirection of the permission change
is protected: replacement by another regular file or hard link, a parent
replaced by a link, and SQLite's own pathname VACUUM INTO write are outside
this guarantee. Root-run backup or restore over a tree an untrusted account
can rename is out of scope; no parent handle anchoring is added here.
The three per-call hooks replace the exact pathname before mode handling;
each regression asserts that the symlink target's MODE remains 0644.

`putVeto` is shared by the no-barrier disqualification (`vetoDisproved`)
and publication. Each bundle/root attempt checks the step and hold before
`writeVeto`, including its Lstat; there is no unguarded preflight lookup.
Only a failed bundle attempt reaches the root fallback, which checks again.
An already-existing veto or a created one is success, even when close or
ownership handling did not complete normally. Presence and cache stamping
are separate outcomes. Both give-ups are checked again before best-effort
sync and before the stamp; a skipped stamp says the record IS vetoed, with
the console's cached reading possibly retained until TTL expiry or a folder
change. A root veto creation itself changes the root's mtime. A bundle veto
does not. Both attempts failing is reported as "could not be vetoed";
give-up before all candidates were tried is "the veto could not be
confirmed", since an uninspected root may already hold one. Both callers
use the explicit unconfirmed flag, never an error-identity inference.
Either outcome skips the immediate prune.
These guards do not revoke a filesystem call that already passed its guard.
Recovery-INCOMPLETE admission and merging remain unchanged. Its round-13
post-rename stamp nevertheless checked the abandoned hold too, incorrectly
skipping recovery's stamp and turning its cache warning into a durability
failure; round 15 below separates those outcomes. Recovery that changes a
mixed record holding VERIFIED remains refused at the veto guard.

The direct caller tests inject step and hold give-up independently before
the first attempt, after a failed bundle, and after a created or existing
bundle or created root veto. They require exact lookup/create counts,
veto presence, an unsynced read-back and separate skipped-stamp wording.
They plant an old root stamp to require no change after bundle give-up.
The console regression exercises that resulting unstamped bundle state:
its list rejects VERIFIED immediately, its card retains the cached reading,
and forcing the cache's age beyond its TTL makes the card read the veto.
The cache test preserves an existing limitation, not a baseline-red claim.

`unsynced` is neutral evidence. A disproof, a successful verification whose
veto removal failed, an interrupted process, or a veto lookup error can
produce it. With no directory barrier, the doctor's package seam defaults
to fsutil.SyncsParentDir and, after FAILED-database advice, chooses
fresh-snapshot advice: the record
cannot be trusted or re-recorded here. Its tests cover both disproof and
successful checking with a leftover veto, and keep retry advice where a
barrier exists. The startup pass's in-place check takes a fresh snapshot
when ErrRecheckNotRecorded accompanies a FAILED component in its own
Found, including VERIFIED database / FAILED files. A leftover veto with
a passing check causes no capture across repeated starts, even with
retention off and veto removal persistently failing. A FAILED database
always triggers replacement whether or not recording succeeds. A subsequent
start skips the fresh verified database as before. No new wire fields or states are introduced.

## B653 PR 1 round 14: advice precedence, replacement evidence and unconfirmed vetoes

A FAILED database keeps the damaged-database advice even when the files
read INCOMPLETE (`unsynced`). The neutral no-barrier advice is selected
only afterward. `TestTheDoctorKeepsDatabaseDamageAdviceBesideUnsyncedFiles`
requires the damage warning, full-check command and advice to keep verified
snapshots, and rejects fresh-snapshot advice for this combination.

Startup's existing database-FAILED replacement remains unconditional on
recording. Only the additional `ErrRecheckNotRecorded` branch is narrowed:
its own `VerifyResult.Found` must contain a FAILED component, alongside
the existing vetoed-read-back requirement. The read-back
`Record` cannot supply that evidence, since a passing check whose initial
veto removal failed also reads unsynced. The real ticker tests run two
complete boot passes, waiting for the shared loop's next interval request
before cancelling. Retention is off so it cannot hide extra captures.
`TestStartupReplacesAVetoedSnapshotEvenWhenItsDatabasePasses` requires one
replacement across both starts; `TestStartupKeepsALeftoverVetoWithoutAddingSnapshots`
requires none with persistent veto-removal failure and VERIFIED findings.

`vetoPlacement.unconfirmed` is set only when give-up prevents an attempt
from starting. It is not inferred from ErrCheckGivenUp inside an attempt's
error. No filesystem call is added after give-up. Both `vetoDisproved` and
`publishVerdict` report "the veto could not be confirmed" when no presence
was established before that stop. Only two attempted, failed locations
justify "could not be vetoed". Neither permits a prune. The direct callers
and the no-barrier worker cover failed bundle/give-up with and without an
existing root veto; the latter still reads INCOMPLETE (`unsynced`). Exact
lookup and creation counts prove the root was not inspected by placement.
The direct callers also try and fail both locations with an error carrying
ErrCheckGivenUp, requiring definitive failure words despite that identity.

`SetFilesCheckIncompleteForTest(dir)` takes a resolved snapshot path, like
the other snapshot-specific seams. It expires a child context for that
snapshot's files check and gives its pacer that context, leaving the
VerifySnapshot caller live. Restore it only after callers and holds end.
The mixed fresh-process-prune fixture awaits the worker and requires
FAILED database / INCOMPLETE files with only ReasonDeadline, a vetoed
read-back and listing, the old verified anchor, and removal of the
intermediate failure. Restoring the either-INCOMPLETE early return in
`disproves` must break these assertions; cancelling the outer verification
is not a valid way to arrange the fixture.

Windows combines FILE_FLAG_BACKUP_SEMANTICS with
FILE_FLAG_OPEN_REPARSE_POINT when reopening the vacuum output. A directory,
directory symlink or junction reaches the descriptor's non-regular check
and says "vacuum output is not a regular file" rather than access denied.
The directory case needs no symlink privilege; the symlink case runs where
the OS permits creation, and the Windows junction fixture uses mklink /J.
All assert refusal and unchanged target mode.

The permission guarantee is limited to final-component symlink/reparse
redirection of staged and vacuum files' chmod. It excludes replacement by
another regular file or hard link, parent directories replaced with links,
and SQLite's own pathname-based VACUUM INTO write. Root-run backup or
restore over a tree an untrusted account can rename is out of scope. No
parent anchoring is added; the orchestrator owns the follow-up backlog item.
The existing deferred closes in both staged-file helpers already close
before removal, including on a chmod error; no leak fix is needed.

## B653 PR 1 round 15: recovery stamps and write durability

Round 13's post-rename cache-stamp guard checked `p.givenUp()`, including
the already-abandoned hold, so a recovery PENDING-to-INCOMPLETE write that
renamed and synced successfully returned `errCacheStampSkipped`.
`published.outcome` classified every written result with an error as
`WriteNotDurable`, making `Recorded` false even though this path never
entered `putVeto`.

Only the recovery publication registered after `publishOf` refuses admission
sets `publication.recovery`. Its post-rename stamp checks its own fresh
`p.given`; an ordinary publication still checks both step and hold.
The step is fresh because it is registered after abandonment, not because
recovery ignores later give-up: abandonment during recovery still ends that
step's signal. Veto creation, confirmation and removal always keep both
guards. Admission, rename guards and the bounded step's late-answer handling
are unchanged. `TestBothVetoCallersGuardEveryAttemptAndReportSkippedStamps`
continues to require no ordinary-publication stamp after hold-only give-up.

`published.outcome` treats a written record with only `errCacheStampSkipped`
as `WriteRecorded`, preserving the error for reporters. Wrappers and joins
of that warning alone have the same meaning; a joined sync or other failure
still means `WriteNotDurable`, an unanswered step still means
`WriteUnfinished`, and no warning turns an unwritten record into a write.
A skipped stamp is not a failed durability barrier. The console can still
retain its old reading until its cache expires or the backups folder changes,
which CLI and ticker retain as a separate warning.

Recovery over a mixed record remains deliberately limited: if either stored
or merged component is VERIFIED, `putVeto` must confirm a veto before the
record changes. The abandoned hold refuses that attempt before lookup,
even if a veto already exists. No recovery record is written, its outcome
is `WriteFailed` and `Recorded` false, and the error says the veto could not
be confirmed. The record and any existing veto stay as they were; read-back
still observes that veto. This is not a restoration of all recovery writes.

`TestRecoveryPublicationSeparatesItsCacheStampFromDurability` drives the
real worker through abandonment at the existing after-check seam and checks
a live recovery's planted root stamp, a fresh step given up at the existing
sync seam, sync failure alone and beside skipped stamping, and mixed records
with and without an existing veto. It requires no prune or post-abandonment
veto lookup and checks write outcome, error identity and read-back.
`TestACacheWarningNeverHidesAPublicationFailure` pins nested error mapping;
`TestTheReportersKeepARecordedRecoveryAndItsCacheWarning` pins both reporters.
The four earlier regression cases also reject a skipped-stamp warning for
a live recovery. Restoring only the post-rename guard to `p.givenUp()` must
fail all four on that new assertion even while the warning-only outcome
mapping remains fixed; with both production changes reverted, their original
recording/reporting assertions fail too. No new test seam is needed.

## B826 — a restore holds the lock of its snapshot's folder (2026-11-12)

- **…and a restore takes that lock in the folder that holds its snapshot, where the snapshot resolves, from before it touches its destination, a player seal it completes there included, until it returns (`backup.holdSource`, the data dir's lock before it, bridge.yaml's inside it; backlog B826): its admission asks this process's marks first by the spelling given, no filesystem asked (`backup.SourceStillHeld`), then resolves the snapshot, asks the resolved folder's marks, takes its lock within `RestoreOptions.LockWait` (bridge restore's `verifyCmdLockWait`, after the prompt, never across it), removes a marker a process that ended left there, and reads the manifest again under it (`ErrSnapshotGone`), all on a worker under a hold of the folder (`newRestoreHold`) that the restore waits for until its context ends or that wait and `watchdogSlack` pass, and `StopWriteBound` more; a worker given up keeps no lock it takes late, though an open of the lock file under way may still make that file (as root, with the folder's owner) and a stale marker's removal under way may still finish, and one given up while it holds the lock marks the folder as a restore's (`StuckCheck.Restore`), in this process and beside the lock, until it has let the lock go, its marker written only once that removal has ended (`hold.clearStale`, `markStuckAfter`); a context that ends as the hold is handed over still refuses the restore, and once its writes begin nothing stops it; its let-go is bounded (`hold.release`), the folder marked in this process past `releaseBound` until the unlock, the close and this process's holder of the folder are let go; every folder alike, a copy's included (the lock file made where missing), refused, nothing restored, while a check, a prune or a restore that did not end holds it (`ErrCheckStillRunning`, a `StillRunningError` naming which: the doctor, the ticker and every command name a restore's and say the bridge restore that runs it ends it), past the wait (`ErrBackupsBusy`), or where no lock can be had or made (`ErrBackupsUnlocked`: no locks kept, read-only media, a folder this user cannot write), in bridge backup --verify's words, exit 1; and bridge restore looks at the snapshot before its prompt, bounded alike (`backup.CheckRestoreSource`: the resolved folder's marker counted only while another open holds its lock); and the restore reads the snapshot where it resolves and names it as it was given it (bridge restore's argument, cleaned: `CleanSnapshotPath`), every path below it in a read's error spelled below that path (`spelledAsGiven`, its chain kept), at the read that made it and never in a destination's words, where it resolves following the path given only in a failed call's own words (the lock's file, a lookup of the snapshot; backlog B871), a refusal by a hold given up naming the snapshot so and the hold by its own name (`heldRefusal`)** (2026-11-12, backlog B826; Codex on B653 PR 1's third round; round 2, Codex on #1452 and on its brief; round 3, CI on its merge with main, Windows' 8.3 temporary folder). **Don't touch the destination before the source's lock or let the restore write once it has answered, take a missing lock file for a copy, resolve a spelling this process's marks name, hold the lock across the prompt, wait for the lock or read the snapshot before the prompt on the command's own goroutine, count a marker beside a lock nobody holds, let a stale marker's removal take a restore's marker away, drop a restore's mark before its unlock has ended, name a restore's hold as a check that a restart of the bridge ends, read the snapshot through the path it was given once it has resolved it, name the snapshot in a restore's words where it resolves but in a failed call's own words after the name given, respell a destination's words, or rebuild an error from its text to change its words.** `TestARestoreRefusesASnapshotAGivenUpCheckHolds`, `TestARestoreRefusedByTheSpellingItIsGivenResolvesNothing`, `TestARestoreRefusesASnapshotAnotherProcesssCheckHolds`, `TestARestoreWaitsForTheSnapshotsLockWithinItsWait`, `TestARestoreHoldsTheSnapshotsLockThroughItsCopies`, `TestARestoreLocksALiveFolderThatHoldsNoLockFileYet`, `TestAStandaloneCopyIsRestoredUnderItsOwnFoldersLock`, `TestARestoreThroughALinkTakesTheLockOfTheFolderItLeadsTo`, `TestARestoreTakesTheLockOfABackupsFolderReachedThroughALink`, `TestARestoreFromTheInstallsOwnBackupsFolderCompletes`, `TestARestoreClearsAMarkerItsHolderLeftBehind`, `TestASnapshotPrunedWhileTheRestoreAskedIsRefusedBeforeTheDestinationIsTouched`, `TestARestoreWhoseFolderCanHaveNoLockIsRefused`, `TestARestoreWhoseSourceDoesNotAnswerIsRefusedWithinItsBound`, `TestAGivenUpRestoreThatHoldsTheLockMarksTheFolderUntilItLetsGo` (Unix), `TestTheStaleMarkersRemovalNeverTakesAwayARestoresMarker`, `TestARestoresMarkStandsUntilItsUnlockEnds`, `TestAStuckMarkSaysWhatHoldsTheFolder`, doctor's `TestTheBackupCheckLineNamesWhatHoldsTheFolderAndWhatEndsIt`, `TestBridgeRestoreSaysWhyTheSnapshotsFolderRefusedIt`, `TestBridgeRestoreRefusesBeforeItsPromptWhileAHoldGivenUpHoldsTheSnapshot`, `TestBridgeRestoreRefusesASnapshotPrunedWhileItAsked`, `TestBridgeRestoreAnswersAStalledSnapshotWithinItsBoundBeforeItsPrompt` (Unix), `TestBridgeRestoreAsksTheMarkerAgainstTheLockBeforeItsPrompt`, `TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots`, `TestARestoreNamesTheSnapshotAsItWasGiven`, `TestARestoreReadsTheSnapshotItLocked`, `TestTwoSpellingsOfASnapshotsFolderShareItsLock`, `TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven`, `TestARestoreRefusedPastTheStopOfItsWaitNamesTheSnapshotAsItWasGiven`, `TestARestoreRefusedForAnotherRestoreNamesItselfAndThatRestore` (Unix), `TestTheRestoresWordsSpellTheSnapshotAsItWasGiven` and `TestBridgeRestoreNamesTheSnapshotAsItWasTypedBesideARestoreThatHoldsIt` pin it. → History-rules-cli-serve.md

The problem (Codex's fourth finding on B653 PR 1's third round, #1444, in
code that PR did not change): `restoreCmd` entered `RestoreWithOptions`
holding only the destination's data-dir lock. A restore of install A's
snapshot, named in A's backups folder, into a stopped install B went on
while A's `bridge serve` still held a check given up (`ErrCheckGivenUp`)
with that snapshot's files open, and nothing kept A's prune from removing
the snapshot under the copies. The prune already refused in that state
(`stuckBy`); the restore took no lock of its source's folder and read no
mark. Red on main through the real path: a verification of A's snapshot
given up past its deadline, its check parked where SQLite opens the
snapshot's database, and the restore of that snapshot into B answered nil
and wrote every one of B's files beside it.

**Where the lock is taken.** At the top of `RestoreWithOptions`, the one path
every restore takes, before `completePlayerSeal`, which can write the
destination's markers, and before anything else touches the destination;
held until the restore returns, through the manifest's read, the player's
files and every copy. Rejected: the lock in `restoreCmd` alone, which misses
every other caller, and a lock taken after the seal's completion, which would
already have changed B.

**Which folder, in verification's order.** This process's marks by the
spelling given first, which asks no filesystem (`SourceStillHeld`: the
folder is the parent of the spelling made absolute against the directory the
process started in, so `.` inside a snapshot names its folder); then the
snapshot resolved as a verification resolves it (`resolveSnapshotIn`), which
can hang on the very mount a check was given up on; then the resolved
folder's marks. Rejected: resolving first, which a spelling the marks name
would wait on, and, without resolving at all, taking a link elsewhere
(`/tmp/x` leading to `/dataA/backups/<snap>`) for a copy beside the link.

**A copy of a snapshot.** Copies stay restorable (the restore's own refusals
advise restoring from a copy), and nothing tells a copy from a live backups
folder for certain: the manifest records no folder (`verify_paths.go`), and a
live folder holds no lock file before its first verification, claim or prune
(a capture takes no backups lock), so a missing lock file is no mark of a
copy. The rule: every folder is locked alike, the lock file made where it is
missing (`fsutil.OpenLockFile`, which as root gives it the folder's owner, so
the service can still open it). Where no lock can be had (a filesystem that
keeps none) or made (read-only media, a folder this user cannot write), the
restore is refused, `ErrBackupsUnlocked` in a restore's words
(`sourceUnlockedError`, whose text is never the verification's), and bridge
restore says to copy the snapshot's folder to a local disk and restore from
the copy. Rejected: an unlocked restore wherever no lock file stands (it
takes a live folder before its first check for a copy); a copy mode on the
CLI (an operator's word that nothing checks, which removes the protection
exactly where the operator is wrong); a folder named `backups` taken for a
live one (a backups folder can be a link to a folder named otherwise, and a
copy can sit in a folder named so). Its cost: a `.backups.lock` left beside a
copy, and a copy on read-only media refused until copied.

**Stalls.** The lock's wait bounds its polling for contention alone: the
resolution, the lock file's open and the read of a marker beside a lock
another process holds are synchronous, and so, once the lock is held, are the
removal of a marker left there and the manifest's read; each can hang on a
mount that stopped answering. So the whole admission runs on a worker of its
own under a hold of the folder (`holdSource`, `newRestoreHold`), which the
restore waits for until its context ends or the lock's wait and
`watchdogSlack` have passed, then `StopWriteBound` more, as a verification's
caller does (`awaitSourceWorker`); past that it is refused, nothing restored,
and the hold is given up, its context ended. A worker given up keeps no lock
it takes late: a resolution that answers late takes none, and a try that
takes the lock late lets it go at once, `lockBackupsWith` asking the context
after the try (the hand-over, which a worker given up finds closed, lets go of
one taken just before). It may still touch the snapshot's folder, never the
destination: an open of the lock file under way as the restore gave up may
make that file afterwards, as root with the folder's owner, and a removal of
a stale marker under way may finish. Neither is atomic with the give-up, and
neither is made so.

A worker given up before it holds the lock marks nothing (the hold is
`markless` until then): it holds nothing a mark would say it holds, though an
open, a marker's read or a try under way keeps this process's holder of the
folder until it answers. One given up once it holds the lock marks the folder
as a restore's (round 2, Codex's P2 on #1452), naming the step it was in
(`hold.enter`: the stale marker's removal, or its read of the snapshot), in
this process and in the marker beside the lock (`StuckCheck.Restore`), until
the worker, its step answered, sees the give-up and lets the lock go, removing
the marker before it unlocks. Another restore, a prune and a verification
there are then refused at once in a restore's words. Red on round 1, where the
worker given up kept the lock with no mark, and each of them waited its lock
wait out and answered `ErrBackupsBusy`. The marker is written only once a
stale marker's removal under way has ended (Codex's finding 3 on the brief):
the give-up's check and the removal's registration are one section of the
hold's lock (`hold.clearStale`), and the marker's write waits for the removal
on a goroutine of its own (`markStuckAfter`), so the removal, once it answers,
takes the stale marker away and never the restore's, and a removal not yet
begun at the give-up is skipped. Nothing that refuses waits on the folder.

The destination is written on the restore's own goroutine alone, after the
worker has handed the lock over, so nothing is written there once the restore
has answered and bridge restore has let the data dir's lock go. Admission and
writing differ: a context that ends as the hold is handed over still refuses
the restore, the lock let go; once its writes begin the context no longer
stops it, since a restore stopped between its writes would leave the install
half restored. Its tenure is not bounded as a hold's steps are: a restore is a
command of its own, whose end, however it comes, lets the kernel's lock go.
Its let-go of the lock is bounded (`hold.release`, `releaseBound`): past it
the restore answers, and the folder stays marked in this process, naming the
restore, until the unlock, the close and this process's holder of the folder
are let go (`keepMarkUntil`, and `hold.drop` on a goroutine of its own); no
marker is written then, the lock being let go (Codex's finding 4 on the brief:
round 1's `letGoWithin` answered with the unlock still under way and no mark).

**A marker its holder left.** `lockBackupsWith` leaves a crashed checker's
`.check-still-running` to its caller; a verification, a claim and a prune
clear it once they hold the lock, and so does the restore, or another process
waiting for the lock would read a dead checker as still running for as long
as the restore runs.

**The prompt's window.** `restoreCmd` reads `manifest.json` before its
prompt (`LooksLikeSnapshotDir`), and a prune can remove the snapshot while
the prompt waits, or while the restore waits for a prune's lock. Under the
lock, before the seal's completion, the restore reads the manifest again and
refuses a snapshot no longer there (`ErrSnapshotGone`) or no longer one, the
destination untouched, its incomplete seal included; main completed the seal
first, wrote `player-initialized`, and only then failed on the read.
Rejected: holding the lock across the prompt, which holds the serving
bridge's verifications up for as long as the prompt waits (each waits
`AutoLockWait`, then leaves its snapshot unverified) and its prunes skipped.
Before its prompt bridge restore looks at the snapshot bounded as the
admission is (`CheckRestoreSource`; round 2, Codex's P2 on #1452: round 1's
first read, `LooksLikeSnapshotDir`, ran on the command's own goroutine, and a
fresh command sees none of a serving bridge's marks): this process's marks
first, then on a worker the resolution, the marker beside the lock of the
folder the snapshot resolves into, counted only while another open holds that
lock (`StuckCheckOf`; Codex's finding 1 on the brief: a marker beside a lock
nobody holds is one a process that ended left, which refuses nothing and
which the restore removes once it holds the lock), and the manifest. A folder
that does not answer is refused within that bound, in the admission's words,
before any prompt; the lock it tries, to ask its question, it lets go at once.

**Lock order.** The data dir's lock (`restoreCmd`), then the snapshot's
folder's, then bridge.yaml's (`config.LockFile`), let go the other way. No
path takes `server.lock` or bridge.yaml's lock inside a backups lock:
`owner.go`, `verify.go` and `finish.go` take neither, and a capture takes
`server.lock` (its seal completion, its adoption) before any backups lock.
The common self-restore, `bridge restore /dataA/backups/<snap>` into
`/dataA`, takes `/dataA/server.lock` and then `/dataA/backups/.backups.lock`,
this process's holder of the folder (`folderToken`) taken and given back at
the restore's end.

**The words.** Each kind in `verifyCmd`'s words and with its exit code, 1:
a check or a prune that did not end (restart the bridge that keeps those
snapshots, or stop the `bridge backup --verify` that runs it), another holder
past the wait (run it again once it ends), no lock to be had (copy the
snapshot to a local disk). A snapshot gone says it was there when the command
read it. A restore that did not end is named a restore wherever it is met
(round 2, Codex's finding 2 on the brief): its error is a `StillRunningError`
whose check says so (`StuckCheck.Restoring`, `What`, `HeldByRestore`), and the
doctor's `backup-check` line, the ticker's reason for taking no snapshot, the
words of a prune it skipped, bridge backup --verify and bridge restore each
name it, and say that the bridge restore command that runs it ends it, never a
restart of the bridge, which runs none. The sentinel's own words name a
verification, a prune or a restore. The field is `omitempty`: a marker written
before it holds none and reads as it always did, and an older bridge passes
over it in a restore's marker and names it its snapshot's verification, the
step's words saying what it was doing.

**The snapshot's name in the words** (round 3; CI on 75aedb53, the round's
merge with main). Round 1 read the snapshot where it resolves, in the folder
whose lock it holds, and from the lock on named it there too:
`RestoreWithOptions` replaced the path it was given with the resolved one
(`snapshotDir, m := src.snap, src.m`), so every message after it named the
resolved path. Windows gives CI's temporary folder in its 8.3 spelling
(`C:\Users\RUNNER~1\…`), which resolves to the long one, and
`TestRestoreRefusesAPlayerUUIDThatIsNotOneUUIDBeforeWritingAnyFile` (its ten
subtests) and `TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo`
failed every run: the refusal did not name the copy the test gave. A temporary
folder reached through a link fails the same way anywhere (macOS's `/var`; on
Linux with TMPDIR through a symlink, `TestRestoreRefusesANamedPipeForThePlayerUUIDAtOnce`
too). The rule: the resolved path stays for what it protects, the lock of the
folder that really holds the snapshot and every read under it; the words name
the snapshot as the restore was given it, bridge restore's argument cleaned
(`CleanSnapshotPath`), as before B826. The admission's refusals name the given
path themselves. A read's error names a path below the resolved snapshot, and
`spelledAsGiven` gives it words with each such path spelled below the given
one, as `filepath.Join` spells it, keeping the error itself for `errors.Is`
and `errors.As` (`givenSpelling.Unwrap`), so `sayRestoreRefusal`'s advice and
`HeldByRestore` read the chain as before; every error `RestoreWithOptions`
answered once admitted passed through it (`sourceHold.asGiven`, deferred),
until backlog B871 (item 13) respelled the snapshot's reads alone, each where
it is made, and no destination's words (its section below). Where the
snapshot resolves follows the path given only in a failed call's own words:
the lock's file, in the folder the snapshot resolves into, and a lookup of the
snapshot that failed, which names where the link led, whether it found the
snapshot gone or failed otherwise, at the restore's admission (`takeSource`)
and at bridge restore's look before its prompt (`checkSource`) alike: the last
two of these added by backlog B871's item 12, which this list left out. A refusal by a
hold given up names the snapshot requested by its path and the hold by its own
name (`heldRefusal`): its snapshot's folder name, as the marker beside the lock
holds it for every process (`StuckCheck.Snapshot`), which names no path.
Naming the holder's own spelling would need its path in the marker, so the
marker is unchanged. Rejected: reading through the given spelling again, since
a link repointed once the lock is held would hand the restore another snapshot
than the one locked (`TestARestoreReadsTheSnapshotItLocked`); and rebuilding an
error from its text, which drops the chain the CLI chooses its advice by.

The inventory, each site at this round's commit and what it names:

- Admission, `internal/backup/restore_source.go`: a hold given up in this
  process by the spelling given, :216 (`SourceStillHeld`), by the folder the
  snapshot resolves into, :322, another process's by its marker beside the
  lock it holds, :428, and the same before bridge restore's prompt, :405
  (`CheckRestoreSource`), each through `heldRefusal`: the snapshot as given
  and the hold by its own name (was: the hold alone). The lock held past the
  wait, :430: the snapshot as given (was: the resolved folder). No lock to be
  had there, :464 (`sourceUnlockedError`), and the lock's file failing
  otherwise, :438: the snapshot as given, then the lock file's call where it
  resolves (was: the resolved folder). The snapshot's lookup failing otherwise,
  :319: the snapshot as given, then the lookup's words (was: the lookup's
  words alone). The same lookup failing before bridge restore's prompt, :402
  at the merge (`checkSource`, `ErrNotASnapshot`'s words, then the lookup's),
  which this inventory left out: since backlog B871 (item 12) the snapshot as
  given, then the lookup's words, and a permission or an I/O failure there no
  `ErrNotASnapshot`. The manifest gone once the lock is held, :355, and not reading,
  :357, both respelled at :352, and before the prompt, :409: the manifest below
  the snapshot as given (was: where it resolves). Unchanged: the snapshot gone
  at its lookup, :317, or at the lock, :436 (`snapshotGone`: the snapshot as
  given, then the lookup's words); a schema not supported, :359 (no path, as on
  main); the restore stopped, :273, :325, :338 and :432 (`sourceStopped`), and
  its folder not answering past its bound, :265 and :393
  (`sourceExpiryError`): the snapshot as given.
- Once admitted, `internal/backup/backup.go`, each through :551
  (`src.asGiven`): a player-uuid that does not hold one UUID, :1692 (the CI
  failure), or does not read, :1695; a console account that does not read,
  :1669; a DSD state that does not read, :621; a snapshot file's stat, :703;
  bridge.yaml's read, :723; and a copy's source, :775: the file below the
  snapshot as given (was: where it resolves; a destination's path unchanged).
  `internal/backup/player_reservation.go`: a listed reservation that does not
  read, :104, and the check for an unlisted one, :133: likewise. Unchanged, no
  snapshot path: a console account that does not parse, :1673, or changed
  during the restore, :743; its write, :746 (the destination);
  `restoreMigrationHeld`, :819; `playerFilesNeedABarrier`, :922; and
  `player_reservation.go`'s :112, :129, :157 and :204, which name files by
  name alone.
- The given-up mark's words, unchanged: `internal/backup/stuck.go`:85
  (`What`), the doctor's backup-check line (`internal/doctor/backups.go`:66)
  and the marker (`internal/backup/hold.go`:241) name a restore's hold by the
  name it took at the lock (`restore_source.go`:332, its snapshot's folder name
  where it resolves).
- bridge restore, `cmd/bridge/backup.go`, unchanged: a path that holds no
  snapshot, :445, and the prompt, :486, name the argument cleaned;
  `sayRestoreRefusal`, :541 to :548, and :449 and :523 print the refusal's
  words above, the advice chosen through its chain.

Forty-nine sites: twenty whose words changed, twenty-nine that named the
snapshot as given already or name no path in it; and :402, which backlog B871
changed, fifty with it.

The record is in `ops/engineering-log.md`, 2026-11-12, B826.

## B871 — B826's Lows: the look before the prompt, a probe's release, and the words (2026-11-12)

- **…and bridge restore's look before its prompt (`backup.CheckRestoreSource`) asks its stop whichever answer its wait took, a look whose read answered refused as a plain stop (`backup.CheckStopped`), never `ErrSourceNotAnswering`, and the command asks its stop once more before the path that asks and `--yes` split, exit 1; refuses there a schema this bridge does not take, exit 1, and as a path that holds no snapshot (`ErrNotASnapshot`, exit 2) invalid input alone, a lookup or a manifest's read that fails otherwise keeping its cause, exit 1; a probe of a backups folder's lock (`backup.Prober`) lets go of the lock, the file and this process's holder of the folder on a goroutine of its own, and past `releaseBound` answers and marks the folder in this process alone, in the name of the caller that asked, until the release ends, `ProbeBackupsLock` answering so (`ErrCheckStillRunning`) and the ticker taking no snapshot behind it; a restore's hold is said by what holds it and what can be done; and the restore respells its source's words at each read of the snapshot, a quoted path and a root included, never a destination's** (2026-11-12, backlog B871, B826's Lows; round 2, 2026-11-13). **Don't take the look's answer without asking its stop, reach the prompt or the restore after a stop, refuse a newer schema as no snapshot or after the prompt, call a permission or I/O failure no snapshot, release a probe's lock unbounded on its caller's goroutine, answer a lock granted where the probe's own release holds the folder, write a marker for any release, name the doctor's or the ticker's probe as a restore's, tell an operator to stop a command that may have ended, or respell a destination's words.** `TestARestoreStoppedAsItsLookAnswersIsAPlainStop`, `TestBridgeRestoreStoppedDuringItsLookExitsBeforeItsPrompt`, `TestBridgeRestoreStoppedJustAfterItsLookExitsBeforeItsPrompt`, `TestTheLookBeforeThePromptRefusesASchemaThisBridgeDoesNotTake`, `TestBridgeRestoreRefusesANewerSchemaBeforeItsPrompt`, `TestTheLookBeforeThePromptKeepsTheCauseOfAReadThatFailed`, `TestBridgeRestoreSaysWhatItsLookCouldNotRead`, `TestAProbesReleasePastItsBoundIsMarkedInItsCallersName`, `TestBridgeRestoreNamesItsLooksStalledReleaseBeforeItsPrompt`, `TestTheDoctorAndTheTickerNameTheirOwnProbesStalledRelease`, `TestTheTickerTakesNoSnapshotWhileItsOwnProbesReleaseDoesNotEnd`, `TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots`, doctor's `TestTheBackupCheckLineNamesWhatHoldsTheFolderAndWhatEndsIt`, `TestTheRestoresWordsSpellAQuotedPathAndARootAsGiven`, `TestARestoreSpellsItsSourcesWordsAsGivenAndLeavesItsDestinationsAsTheyAre`, `TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven`, `TestARestoreNamesTheSnapshotAsItWasGiven` and `TestBridgeRestoreNamesTheSnapshotAsItWasTypedBesideARestoreThatHoldsIt` pin it. → History-rules-cli-serve.md

The Lows of B826's reviews (#1452: CodeRabbit, and round 3's Carbon, Claude
and Codex), one backlog item.

**The look's stop (item 1).** `awaitSourceWorker`'s select takes either case
that is ready, and the look's worker can answer as the stop comes, so the two
can be ready together and the select take the worker's. Before B871 the look
returned the worker's answer without asking its context, so a Ctrl-C during
its read led to "Type 'yes'", the read having answered within the stop's
grace. The look now asks `ctx.Err()` once its wait has answered, whichever
case took it: a worker that answered left nothing reading, so the refusal is
a plain stop (`CheckStopped`, its context's error in the chain, so
`errors.Is(err, context.Canceled)` holds), and only a worker the wait gave up
on carries `ErrSourceNotAnswering` (`sourceNotAnsweringError`, whose words are
`sourceExpiryError`'s as before), so a stop that left nothing behind is never
said as a folder that did not answer. bridge restore asks its stop once more
right after the look (its test seam `onRestoreChecked`), before it reads the
config, takes the data dir's lock or splits into the path that asks and
`--yes`, so a Ctrl-C that lands just after the look stops both alike.

**The stop exits 1.** The brief asked for the exit `cli-serve.md` gives a
cancelled command; `cli-serve.md` holds no such rule: the one it holds is
serve's (a stop that lands in a start-up step exits 0 on that stop's own
artifacts, `stoppedBeforeServing`). The restore's other stops
(`sourceStopped`, at its admission) and its prompt's "no" exit 1, and every
refusal of the restore exits 1 but a path that holds no snapshot; the
engineering log's CLI convention, 130 for an interrupted run (PR #108), is the
batch commands' (upscale, analyze, fingerprint, variants, enrichment retry),
where it tells an interrupted batch from a clean one. One code for every stop
of one command is what a script reads; 130 for this stop alone would split
them.

**A newer schema (item 2).** Round 1's `LooksLikeSnapshotDir`, whose own doc
wanted "a precise mismatch error rather than a generic 'not recognized'
rejection", took any schema above 0, and round 2's `checkSource` kept its
condition, so a snapshot of a newer schema reached the prompt and was refused
after it, exit 1: new behaviour, not a regression. The look refuses it now in
the restore's own words (`schemaNotSupported`, which `takeSource` shares),
exit 1, before the config is read and before the data dir is made or locked; a
valid newer snapshot is never `ErrNotASnapshot` (exit 2). Schemas 1 and 2
pass, the controls.

**A probe's release (item 3).** A probe of a backups folder's lock, the one
`StuckCheckOf` makes beside a marker (`lockHeldElsewhere`) and the one
`ProbeBackupsLock` makes, took this process's holder of the folder (the token)
and the lock, and let them go on its caller's goroutine, so a release that did
not answer held its caller with the token and no mark: bridge restore's look
was refused by its watchdog, in the generic words of a folder that did not
answer; the doctor's read stayed out; the ticker's pass waited until its stop;
and a verification in the process waited its lock wait out behind the token
and answered busy. The release now runs on a goroutine of its own
(`letProbeGo`): the lock where the try took it, the lock file, then the token,
so the token is kept through the unlock and the close; the probe waits for it
within `releaseBound` and past it answers, and the folder is marked in this
process alone under the folder's keys (the spelling probed, the resolved key,
and for a restore's look the given snapshot's folder), naming the release
(`stepProbeRelease`, "letting go of the lock it tried"), until the release
ends, the mark dropped by the release itself once the token is given back. A
mark is raised only by an overrun, never by a release that keeps within its
bound. **No marker is written for a release, whoever probed**: once the
unlock has answered, another process may take the lock, and a marker written
then would stand beside that holder's lock as this probe's, or take the place
of the one it writes; the same holds of a restore's own let-go of its lock
(`hold.release`, which writes none once it has begun). **The mark names the
caller that asked** (`backup.Prober`): bridge restore's look as that restore
(`StuckCheck.Restore`, its snapshot's folder name), the doctor's and the
scheduled snapshot's as their own probes (`StuckCheck.Probe`, never in a
marker: `json:"-"`), never a restore's, so `ProbeByDoctor` and
`ProbeByTicker` are what serve's callers pass, and `Pruning` takes no probe.
`StuckCheckOf` counts its own probe's stalled release as the hold that holds
the folder (it asks this process's marks again after the probe), so the look
refuses before the prompt in words that name it, the doctor's backup-check
line and the ticker's reason name their own probe, a prune skipped behind it
says so (`pruneSkipWords`, `HeldByProbe`), and every verification, claim,
prune or restore here meets the mark at once. Rejected: a mark at the start of
every release, which a release that keeps its bound would raise too (B653's
rule: an overrun alone marks); and keeping the release on the caller's
goroutine with a timer raising the mark, which would still hold the doctor's
read and the ticker's pass for as long as the release stalls.

**What a look could not read (item 4).** `checkSource` wrapped every failure
of the snapshot's lookup (`resolveSnapshotIn`, its first call, and the wrap
after it, :402 at the merge) and of its manifest's read (:407, and its wrap,
:409; the brief's ~339 is round 2's line) in `ErrNotASnapshot`, so a
permission or an I/O failure read as "not a recognized snapshot directory",
exit 2, its cause dropped by the command's words. `ErrNotASnapshot` now wraps
invalid input alone (`notASnapshot`: `fs.ErrNotExist`, which on Windows also
answers a path through a regular file, and `ENOTDIR`), and a decode that
fails or a manifest that records no schema; every other failure keeps its
cause, the snapshot named as given, exit 1. The manifest's read is a seam of
its own (`readSourceManifest`), its decode `decodeManifest`, which
`readManifest` shares:

| What the look meets | Where | Exit |
|---|---|---|
| a missing path | the lookup | 2 |
| a link to nothing | the lookup | 2 |
| a path through a regular file (`ENOTDIR`) | the lookup | 2 |
| a regular file given (`ENOTDIR` at its manifest; Windows: path not found) | the manifest's read | 2 |
| no `manifest.json`, or one that is a link to nothing | the manifest's read | 2 |
| a `manifest.json` that does not decode | the decode | 2 |
| a manifest that records no schema (0, or none) | the decode | 2 |
| a schema newer than this bridge takes | the decode | 1, the restore's words (item 2) |
| a lookup this user may not make (`EACCES`, `EPERM`) | the lookup | 1, its cause |
| a lookup that fails in I/O (`EIO`) | the lookup | 1, its cause |
| a manifest's read this user may not make | the manifest's read | 1, its cause |
| a manifest's read that fails in I/O | the manifest's read | 1, its cause |
| any other failure (a link that loops, a `manifest.json` that is a folder) | either | 1, its cause |

The cases inject the permission and I/O failures through the lookup's seam
(`SetMetadataForTest`) and the read's (`SetReadSourceManifestForTest`), never
by a mode, which root and Windows do not honour, one case a class at each of
the two calls.

**The stall boundary (item 5).** `docs/backup.md` promised a refusal for a
stall or a stop anywhere before the restore writes, while its reads of the
snapshot's other files (the console account, the player's identity, its DSD
state and its reservation, the database's migrations, the copies) run after
`holdSource` on the restore's own goroutine with no watchdog, and its context
is not asked once the admission has answered. The promise now covers the
source's admission and its manifest's read alone, and says the rest is not
bounded.

**What holds a restore's folder (item 6).** The hint said "stop that
command", which can name a bridge restore that already printed its refusal and
ended: a marker counts only while its lock is held, so what holds the folder
is a step of that restore left in a process that is still running, or waiting
on storage that stopped answering (a hung mount, which also keeps a process
from ending). The words now say that, and what to do: check that storage, end
that bridge restore process if one still runs, then run it again; the same in
`stillRunningEnd`, the doctor's own advice (`internal/doctor/backups.go`) and
`docs/backup.md`. The docs and the package comment tell three states apart:
the in-process mark, raised at once (`markStuckAfter`); the marker, written
only once a stale marker's removal under way has answered; and meanwhile what
another process reads, the old stale marker, whose verification or prune it
may name as still running, or no marker at all, where it waits for the lock:
busy is not promised there.

**Test hygiene (items 7 to 10).** The restore goroutine of
`TestBridgeRestoreRefusesASnapshotPrunedWhileItAsked` is drained on every exit
path, until just before the test binary's deadline (`newRestoreRun`, which
launches it and registers its `drainLoopOnCleanup` beside the launch, as the
package's `TestEveryBackgroundGoroutineDrainsOnCleanup` requires; the
goroutine waits for `start` to hand it the run), the drain registered before
the cleanups that let it go on (the prompt's stdin), which run first; the look's
worker of the Unix stalled-manifest case, which outlives the command's answer,
is fed and its end observed (`lookWorkerEnd`, over
`SetOnCheckSourceEndForTest`) before the seams go back and the fixtures go,
and so are the new cancellation and probe cases'. A hook's `RemoveAll` error
is handed to the test goroutine. The `restore` key's presence is read as JSON
reads it (`map[string]json.RawMessage`). `awaitReader` and `awaitEvent` keep
their own 30 s, capped by the test binary's deadline less a reserve
(`waitWithin`), the maximum alone with no deadline.

**The spelling's edges (items 11 to 14).** `spelledAsGiven` looked for the raw
`snap + separator` alone, returned a snapshot at a root as it was, and
replaced every occurrence, a match inside another path included. Its prefixes
are now built with exactly one separator each (`spelledBelow`: a root such as
`E:\` keeps its own), a path the words quote (`%q`, which on Windows doubles
every separator and elsewhere escapes a backslash a name holds) is respelled
in its quoted form, taken where a quote comes just before it, and a match
counts only where a path begins (`pathStart`), in one pass that reads nothing
it wrote (`respellPaths`); no source error quotes a path today (the package's
one `%q` is `player_reservation.go`'s UUID), so the quoted form is defensive,
pinned through a seam that quotes (`SetFailSourceOpenForTest`) and, on Windows,
by every path. A snapshot at a root is spelled as any other, so the restore no
longer respells every error it answers (`sourceHold.asGiven` deferred over all
of `RestoreWithOptions`), which, with the snapshot at a volume root, would
have respelled a destination on that volume: each read of the snapshot
respells its own words where it is made (the console account, the player's
identity and DSD state, the reservation's floor, now read by
`RestoreWithOptions` ahead of `restorePlayerReservation`, a file's stat,
bridge.yaml's read, and each copy's open and reads, `copyOut` and
`givenReader`), and every write of the destination keeps its own words. No
test can make a volume root; the destination's case puts the destination in
the snapshot's own folder, which gives its paths the beginning a volume root
gives every path, and the root's case is `spelledAsGiven`'s own (`/`, and on
Windows `E:\`). Item 12's two places where the resolved spelling may follow
the path given, the lookup failing otherwise in `takeSource` and in
`checkSource` (now prefixed with the path given), are in the package comment,
in B826's exception list and in its inventory, fifty sites with them; and the
lock's file, named by the lock's own words where the snapshot resolves, is the
exception `RestoreWithOptions`' doc and the rules line now state. Item 14:
the admission's refusals, the words' cases and the command's typed-path case
run both ways a link can lead to the snapshot (`spellings`: a link to the
folder that holds it, and a link to the snapshot itself), and
`requireNamedAsGiven` refuses words that name any path in the folder where the
snapshot resolves but its lock file. Their controls are split by what each
piece does (the engineering log's B871 record): the admission's naming
(`heldRefusal` and `sourceLockRefusal` handed the resolved snapshot) and the
source reads' respelling (`asGiven` taking out its replacement) each turn red
their own cases alone. The admission's control showed its case of a hold met
by the spelling given meeting the resolved folder's check instead
(`captureT` resolves a capture's folder, so the check given up marked that
folder alone): the case now gives its check the spelling the restore is
given, and meets `SourceStillHeld`, the check its name says, which its own
control turns red, alone of the cases the controls run. Whether a probe's release writes
a marker is asked of the marks themselves (`backup.MarkersForTest`, by the
unit case and the command's, the doctor's and the ticker's cases), the
marker's write being on a goroutine of its own (`markStuck`), which a read of
the file, at the case's end or once the release has ended, can precede: two
rounds of its control found such reads green.

**Round 2 (2026-11-13; Codex, Carbon and Claude on the round).** The
ticker took a snapshot behind its own probe's stalled release: with no marker
beside the lock, `captureHeldBack`'s `StuckCheckOf` makes no probe, and
`ProbeBackupsLock`'s probe, its release past `releaseBound`, marked the
folder and answered nil, so the pass captured a snapshot whose verification
the mark refused (the boot pass's "did not start"). `ProbeBackupsLock` now
answers its own stalled release as `ErrCheckStillRunning`, a
`*StillRunningError` naming its probe (`letProbeGo` answers the mark it
raised), joined to `ErrBackupsUnlocked` where both hold, and the ticker takes
it before the unlocked branch, saying why in the stuck check's words
(`stillHoldsTheFolder`). Rejected: asking `StuckCheckOf` again after the
probe, a read of the folder on every healthy tick. `StuckCheckOf` asks this
process's marks again after its own probe already (`return stuckAt(key)`),
and the doctor reads `ProbeBackupsLock`'s answer only for
`ErrBackupsUnlocked`, so its lines are as they were. The tests' reads of a
marker waited for its file alone, which `writeMarker` makes before it writes
its JSON, so a read could find it empty: a test now waits for the write's
`"writes"` hook, after the write and the close (`SetOnMarkerWriteForTest`, in
`seams.go` since then so that a check run in a process of its own can say
so). A verification "as another process" in this process reads no marker
beside this process's own hold: `verifyAndThen` asks this process's marks
first, so only `readStuckMarker` and a real process of its own met the empty
file. Two findings were refuted. An audit of `RestoreWithOptions` found no
error of the snapshot's that is not respelled: between the admission and the
console account's read only the destination's seal completion and config
lock run, the migrations check drops the snapshot's read errors, the epoch
reset opens the destination, every copy goes through `copyOut`, and
`playerFilesNeedABarrier` names its files by name alone. And the look's own
ask of its stop was pinned already: its control reddens
`TestARestoreStoppedAsItsLookAnswersIsAPlainStop`'s two ways where the read
answers, the command's case staying green by design, its own ask the second
layer.

**Left open, named for the backlog:** `CleanSnapshotPath` takes the
separator off a root as well (`E:\` becomes `E:`, a drive's working
directory, and `/` becomes nothing), so a snapshot given as a volume root
itself, rather than through a link, is looked for elsewhere; and a Ctrl-C
during the admin port's probe (`refuseIfBridgeMayBeRunning`) reads as a bridge
answering there.

The record is in `ops/engineering-log.md`, 2026-11-12, B871.

## B871 round 5: the stuck marker published whole (2026-11-14)

The B653 PR 1 round 12 line's marker clause since this round: a stuck marker is published whole once the old entry is removed, its bytes in a temporary file created exclusively beside it, written and closed with both errors checked, then linked to the marker's name, which refuses a racing replacement, the temporary file removed on every path; never written in place, or published by a rename, which replaces a racing entry.

Backlog B899: main's CI on macOS (run 43527630159, `test (macos-latest)`, attempt 1) failed `TestARestoreRefusedForAnotherRestoreNamesItselfAndThatRestore` with "the marker does not read: unexpected end of JSON input". `writeMarker` removed the old marker, created the new one with `O_CREATE|O_EXCL`, wrote it and closed it, so a read between the creation and the write met an empty file, and main's `awaitMarker` waited only for the file to exist. Production's reader (`stuckMarker`) takes a marker that does not decode for none, so another process reading in that window waited its lock wait out and answered `ErrBackupsBusy` without naming what held the folder: a worse message, not data integrity.

`writeMarker` now writes the marker's bytes to `.check-still-running.tmp` (`stuckMarkerTmpName`), created exclusively once the old marker is removed, with the folder's owner (`fsutil.KeepOwner`), checks the write's and the close's errors, and publishes it with `os.Link(tmp, marker)`, which fails where anything stands at the marker's name, as the exclusive creation of the marker did, so round 12's refusal of a racing replacement stands (`TestTheStuckMarkerNeverFollowsAReplacement`: a symlink planted after the removal is left untouched, and now nothing is said of it). The temporary file is removed on every path once it is made, "writes" (`SetOnMarkerWriteForTest`) is said only once a marker is published, and `removeMarker` and `clearStaleMarker` remove the temporary name with the marker. The best-effort contract and the removal ordering with `after` stand: the write still waits for a removal of a stale marker under way, then takes its mark's lock, then removes the old marker. A marker is written only for a hold that holds the folder's lock, one hold of a folder holds it at a time in a process, and a releasing hold writes none, so no two writes of one folder's marker meet at the temporary name.

Windows. `os.Link` is `syscall.CreateHardLink(n, o, 0)` (go1.27.3, `src/os/file_windows.go:321`), which fails where the new name exists: Go's own hard-link test (`src/os/os_test.go:900-915`) requires a second `Link` onto an existing name to answer a `*LinkError` whose `Err` is `IsExist`, on every platform that has links. The temporary file's exclusive creation is `CREATE_NEW`, not following a reparse point (`src/syscall/syscall_windows.go:436-438`). A reader opens the marker with `FILE_SHARE_READ | FILE_SHARE_WRITE` and no `FILE_SHARE_DELETE` (`syscall_windows.go:395`; `fsutil.ReadAsFile` opens through `os.Open` there, `internal/fsutil/notafile_other.go:13`), so a reader that holds the old marker makes its removal fail, and the write gives up, as before. It cannot refuse the link: no marker stands at the name once the old one is removed, and no reader opens the temporary file. A reader that opens the new marker before the temporary name is removed keeps that removal from landing (one file, two names): the marker is published and the temporary file stays, until the hold's own `removeMarker` or the next holder's `clearStaleMarker` removes it; meanwhile a later hold of this process writes no marker of that folder (best effort). Nothing new is logged.

A filesystem that makes no hard link (FAT, exFAT) refuses the link, so no marker is published there, as where a marker cannot be written: another process's wait ends `ErrBackupsBusy`.

Rejected:

- A rename into place (`os.Rename`), which replaces a racing entry at the marker's name, the round 12 test's planted symlink among them.
- A temporary name of its own for each write, which a removal could not name: one hold of a folder writes at a time.
- Writing in place where the link fails, which brings back the read that meets an empty marker on exactly those filesystems.
