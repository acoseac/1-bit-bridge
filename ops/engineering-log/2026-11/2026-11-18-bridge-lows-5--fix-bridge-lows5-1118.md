## 2026-11-18 — Bridge lows 5: the pin reads a type switch's case lists past its guard, two comments, and the protocol's dacToken sentence

Branch `fix/bridge-lows5-1118`, from `6282e5a6` (main with #1508). Four commits: A, the
install-check pin (test code alone) and B618's long form; B, two comments; C, one sentence of
`PROTOCOL.md`; D, this record. No production logic changes. Line numbers are the branch's
unless a commit is named.

### A. The pin reads a type switch's case lists past its guard (item 1)

What was true. `seamFindingsIn` (`cmd/bridge/logs_test.go` 538-600 at `6282e5a6`) took an
identifier for the seam only where go/parser's resolver gave it the object of the file's own
top-level declaration of the seam's name, or none in a file that does not declare it (547,
607-613). The resolver walks a type switch's clauses in the scope where it declared the switch's
guard, and resolves each clause's type list there (go1.27.3 `src/go/parser/resolver.go` 344-348
for a clause, 371-384 for the switch). The language declares the guard's variable in each
clause's own block, after its case list: go/types checks a clause's list before it opens the
clause's scope and declares the variable there (go1.27.3 `src/go/types/stmt.go` 746-759). So in
`switch logsInstallCheck := v.(type) { case [unsafe.Sizeof(&logsInstallCheck)]byte: … }` the
list takes the package variable's address, while the parser gave the list's identifier the
guard's object and the pin read it as the guard's local, no finding. And the pin counted every
top-level value spec of the seam's name as its declaration (548-557), a `const` included, with
a finding that it declares something other than `packaging.CheckWindowsInstall`.

As built. `seamFindingsIn(fset, f, src, seam)` (552) takes the file's source too, and the pin
reads each file's bytes and parses those (460-464). `caseListGuards` (698) maps each identifier
of the seam's name in a type switch's case type list to the guards, of that name, of every
switch whose lists hold it, a switch inside another's list adding its guard to the outer one's;
a switch's initializer, its guard and its clauses' bodies lie in none of its lists.
`caseListBindings` (655) takes each such identifier the parser bound to one of those guards and
parses the source again with those guards' names blanked, an underscore and spaces, so every
offset holds and the guards declare nothing: the identifier at the same offset is the seam where
that parse resolves it to its own file's object of the seam's name (or to none, where both
parses find none), and `isSeamIdent` (628) asks that answer first. So the name is read past
those guards alone, and a parameter, a local, the switch's initializer or an outer type switch's
case variable of its name keeps it, as the parser resolves the name through those scopes. Only
the file's top-level `var` specs declare the seam (567). A top-level type, const or func of its
name, or an import, is no variable, takes no write and no `&`, and compiles beside no
declaration of the seam (a name declared twice in the package block, or in both the file block
and the package block, is refused): each is accepted, no declaration of the seam and no
finding, rather than a finding of a kind of its own, which would name a state no package that
builds can reach.

Which and why. The parser's resolution again, given a source in which the guards the language
does not see in a list declare nothing, rather than a lookup of the test's own: the resolver
already gives every other construct its scope, and the second parse keeps that one
implementation for the lookup. go/types is still not the pin's tool: the pin reads every
platform's files, which do not type-check as one package.

Thirteen rows of `TestTheLogsInstallCheckPinSeesEachWayToChangeIt`, the table's header gaining
`unsafe` among the imports it does not need. One finding on the line planted: the seam's address
in a case list, in a file that declares the seam and in one that does not; and in the case list
of a type switch inside another's case list (a func literal in an array length's
`unsafe.Sizeof`), where the name is read past both guards. No finding: the address, in a case
list, of a parameter, a local, the switch's initializer's local and an outer type switch's case
variable of the seam's name; a top-level type, const and func of the name, and an import, each
in a file that does not declare the seam (no declaration); a composite literal's key of the
name. One finding, its pair: the seam's address in a composite literal's value. The type, import
and key rows were green before the fix, as they are after it. The type and import rows' files
declare nothing of the seam (no declaration counted), and the key row's declares it once, as
`logs.go` does; after the table's header their sources are, the type row's (no declaration):

```go
type logsInstallCheck func() packaging.WindowsInstall

var plantCheck logsInstallCheck = packaging.CheckWindowsInstall
```

the import row's (no declaration):

```go
import logsInstallCheck "fmt"

func plant() {
	logsInstallCheck.Println()
}
```

and the key row's (one declaration, its first line):

```go
var logsInstallCheck = packaging.CheckWindowsInstall

type plantHolder struct{ logsInstallCheck func() packaging.WindowsInstall }

func plant(f func() packaging.WindowsInstall) plantHolder {
	return plantHolder{logsInstallCheck: f}
}
```

its pair returning `plantHolder{logsInstallCheck: *(&logsInstallCheck)}`, marked planted.

Two checks beside the table, each run once on commit A. Every one of the table's 36 planted
sources, written into a package of its own under `.tmp/_bridgelows5` (keepers for the header's
imports, the seam declared beside a file that does not declare it and names nothing of its
name), builds with the go command: the case-list forms are Go a compiler takes. And a go/types
check of every row (`packaging` stubbed in the package) lists each identifier the type checker
resolves to the package-level `logsInstallCheck` as the target of an `=` assignment or range
clause, or as the operand of `&`: for the 34 rows whose verdict is about such an identifier its
lines are the row's planted line, or none, every one; the two rows about a declaration (another
value, a named type) it leaves apart.

### Red first and the controls

Each predicted in writing before it ran. The rows were red first in the tree, over the pin as it
was at `6282e5a6`, before the fix was written; the controls after commit A are `go test -overlay`
builds of `logs_test.go` mutated in memory by exact-once replacements, the tree never edited.
Every run was `-count=1 -v` over the table and the production pin, with the release tags.

| | code | predicted red | seen |
|---|---|---|---|
| red first | the thirteen rows over the pin as it was | the seam's address in a case list (declared in the file, and not), in a nested switch's case list, and the top-level `const` row | those four: "findings [], want one that takes its address, on line 14" (12, 16), and the const row "found logsInstallCheck declared at ["planted.go:10:7"], want 0 declaration(s)" with its finding that it declares something other than packaging.CheckWindowsInstall; the other 32 rows and the production pin green |
| K0 | no mutation | none | none |
| K1 | no identifier read again (`rebound` empty) | the three case-list rows that want a finding | those three |
| K2 | only the innermost guard blanked | the nested switch's row | that one |
| K3 | every misbound identifier taken for the seam, the second parse's answer ignored | the parameter's, the local's, the initializer's and the outer case variable's rows | those four |
| K4 | every top-level value spec a declaration again | the `const` row | that one |
| K5 | the second parse's identifier compared with the first parse's object | the case-list rows in a file that declares the seam (two parses' objects are never equal) | those two; the row in a file that does not declare it green (none equals none) |
| K6 | the clauses' bodies read past the guard too | the three case-list rows, the outer case variable's, and the earlier type switch's and select case's defines row | those five |
| K7 | the filter dropped, every case-list identifier read again | none: an identifier a declaration inside the list binds resolves to it in the second parse too | none |

The production pin stayed green in every control: no file of the package holds a type switch
whose guard is named like the seam, or a top-level `const` of its name.

### B. Two comments (item 2)

The log-size check's comment on a path that is a directory (`internal/doctor/logsize.go` 95)
and the doc of `TestCheckLogSizeWarnsOnDirectory` (`internal/doctor/logsize_test.go` 172) named
the reviewer of PR #708 as the finding's source. They say "Found in review on PR #708" and
"Reported in review on PR #708", and keep what they said of the check. About two dozen other
comments in `internal/doctor` still name a review tool as a finding's source, outside this
bundle's budget.

### C. The protocol sentence (item 3)

`PROTOCOL.md`'s `dsdDAC` table (1011) lists `dacToken` among the members always present, as
this bridge sends it. The object arrived with B728's B8; the DSD choice route, the
`bridgeDSDChoice` feature and `OutputDSDDAC.DACToken` arrived together in `c6d4726d` (#1503),
not in `589ba334` (#1502): `internal/api/outputs_bridge.go` declares `OutputDSDDAC` with no
`DACToken` at `c6d4726d^` (323) and with it at `c6d4726d` (328-334), where `api.go` first
advertises `bridgeDSDChoice` (2040-2047). So a bridge between the two emits `dsdDAC` without
the token, and a bridge older than B8 emits neither. The `dsdDAC` section says, after the
token's paragraph (1088-1090): "Older bridges that emit `dsdDAC`, from before the DSD choice
route (`bridgeDSDChoice` absent), send it without `dacToken`, and a client sends no
`PUT /v1/outputs/{id}/dsd` without one." The file's SHA-256 is
`0947253c927cb5fae3e74c06c2f7349ab9b45d75935071eb6dc0b773c471653b` (345,576 bytes), which the
iOS repo's `docs/BridgeProtocol.md` takes in a mirror PR of its own. `internal/api` and
`internal/player/playback`, whose tests read the file, passed over it.

### Runs

The gate after the last commit (`make fmt` with `git diff --exit-code`, `make vet`, `make
docs-check`, the pin's tests under `-race -count=20`, `make test`, `make build-all`, and `go
vet` for darwin and windows of the touched packages) is in the branch's report: a record cannot
hold the runs made on its own commit.
