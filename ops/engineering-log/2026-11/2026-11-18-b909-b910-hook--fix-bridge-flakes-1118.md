## 2026-11-18 — B909's connection hook waits on no other connection's lock

Branch `fix/bridge-flakes-1118`, after the record above (21812d5f). This
commit changes `internal/sqlitetest` alone and appends this record.

`trackConnection` (81c3a553) dropped the connections it had recorded that had
closed or been interrupted by asking each `IsValid` under that connection's
own lock, the one modernc's `Close` holds across SQLite's close (`conn.go`
1074-1102), a WAL database's checkpoint and its fsync among it. So an open, in
every test binary that imports the package, waited for each close under way,
and a slow close on a Windows runner would have slowed every open beside it.
It now asks a connection only where its lock is free at once (`gone`, by
`TryLock`), keeping one a close or an interrupt holds until the next open;
`Cancel` still asks its candidates under the lock, waiting, since its answer
must be read.

`TestOpeningAConnectionWaitsOnNoOtherConnectionsLock` holds a recorded
connection's lock across another connection's open, then requires the next
open after that connection's close to drop it: red 3 of 3 over 21812d5f's
hook ("an open beside another connection's held lock did not end within
10s"), green 20 of 20 with the package under `-race`. Controls: K1, the
prune taking the lock again, red at that line; K2, the prune dropping
nothing, red at "1 connections recorded … once its one was closed, want 0".

The gate on 21812d5f (`make fmt vet test build-all`: 91 packages ok,
cmd/bridge 1,720.9 s) ran before this commit; the gates after it are in the
branch's report.
