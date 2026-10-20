---
name: local-test-fixture
description: "Running a local bridge from a folder of test audio (the loopback recipe, and the public-mode variant with its four traps), forcing re-enrichment or a tag re-extraction, and wiping or editing a bridge database from outside the bridge without tripping the migration v4 unicode_lower trap or the WAL read trap. Use it before starting a test bridge, or before touching a bridge.db by hand."
---

# Local test fixture

Moved verbatim from CLAUDE.md on 2026-10-18. Paths are relative to the repository root.

## Local test fixture

Point `--library` at any folder with a handful of tagged audio files and you've got a working test setup — FLAC / DSF / MP3 / M4A all work. A few dozen tracks across 4–5 artists covers the tag-extraction, enrichment, and playback paths without needing a NAS.

Short path (no service install, doesn't touch launchd):

```sh
make build >/dev/null
./bin/bridge init --yes --no-service \
  --dir /tmp/bridge-live \
  --library ~/Music/test-library \
  --name "Test Library"
./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &
# Admin console: http://127.0.0.1:7789/ — pair from there, or keep using `bridge pair` for scripts.
```

The old hand-authored `bridge.yaml` recipe still works; keeping the init form because it also mints the TLS cert up-front and catches config typos before `serve` is up.

**Public-mode variant** — renders Sign out, the session gate and every public-only piece of chrome (used for the 2026-09-18 phone top-bar report, #934):

```sh
./bin/bridge init --yes --no-service --dir /tmp/bridge-pub --library ~/Music/test-library --name "Test Library" \
  --public --domain localhost --admin-tls-proxy --admin-address 127.0.0.1:7791 --listen-address 127.0.0.1:7790
printf 'upload:\n    enabled: true\ndisableHttp3: true\n' >> /tmp/bridge-pub/bridge.yaml   # uploads on = the space meter renders
echo "throwaway" | ./bin/bridge admin reset-password --config /tmp/bridge-pub/bridge.yaml --from-stdin
./bin/bridge serve --config /tmp/bridge-pub/bridge.yaml &
./bin/bridge admin login-link --config /tmp/bridge-pub/bridge.yaml --ttl 5m   # open http://localhost:7791<path>, press Continue — no password typed
```

Four things that each cost a round: `--admin-tls-proxy` is what lets the console serve plain HTTP (public mode otherwise demands `autocert.enabled`); the browser must use the HOST that `autocert.domain` names — `originMatchesPublicMode` compares every POST's Origin host to it, so `127.0.0.1` against `domain: localhost` fails the ticket redeem with a cross-origin refusal (loopback is a secure context, so the `Secure` session cookie itself works over plain http on either name); `mdns.enabled` must stay `false` — a blanket `sed` on `enabled:` flips it and public mode refuses to start; and the static files are `//go:embed`ded, so every CSS / JS / template change is a `make build` + restart, never a reload. A second bridge on other ports for a before/after comparison needs a different host (`127.0.0.1`, with `autocert.domain: 127.0.0.1`): cookies ignore ports, so two bridges on `localhost` overwrite each other's session.

Force re-enrichment if the DB is already populated from a prior run:

```sh
sqlite3 /tmp/bridge-live/data/bridge.db "UPDATE tracks SET enriched_at = 0;"
```

**`enriched_at = 0` is NOT a tag-re-extraction reset.** It only triggers the MusicBrainz / CoverArt / Deezer enricher (the `WHERE enriched_at = 0` worker query at `internal/manifest/store.go:477`). It does NOT cause the scanner to re-read file tags: the scanner's skip gate (`runScanWorker`) compares the walk's size and mtime with the row's `size` and `mtime_ns` COLUMNS, read through `GetTrackStat`. **This paragraph said until 2026-09-28 that the gate compared the mtime inside `tags_json` (through `GetTrack`), so that `UPDATE tracks SET mtime_ns = 0` does not work. It does**: the gate has read the columns since it moved to `GetTrackStat` (#574), and measured through the Go store, a row whose `mtime_ns` was zeroed is re-extracted on the next scan, on the full upsert leg (its `enriched_at` resets, as for a changed file). So to force tag re-extraction after an `internal/manifest/extractors.go` change (e.g. the PR #208 multi-value Vorbis fix) without touching real file mtimes, zero `mtime_ns` on the affected rows or wipe them, either way through the Go helper below. In code, a change to what extraction produces bumps `ExtractorVersion` instead, whose diff-guard bounds the delta to rows that changed.

**The old `sqlite3 … "DELETE FROM tracks;"` form here is BROKEN since migration v4** (corrected 2026-06-23). v4 added the expression index `tracks(unicode_lower(path))` (+ a `track_variants` twin), and `unicode_lower` is a Go-registered scalar (`internal/manifest/sqlfunc.go` `init()`), so an external `sqlite3` CLI `DELETE`/`UPDATE`/`INSERT` on those tables fails at prepare with `unknown function: unicode_lower()`. Dropping the index doesn't help — it's created only by the version-gated migration, so a restart won't recreate it. Two working paths:

- **Disposable local fixture** — simplest is to delete the DB file and let the startup scan rebuild (loses pairings/tokens; re-pair after):
  ```sh
  kill -TERM $(pgrep -f "bin/bridge serve --config /tmp/bridge-live")   # graceful
  rm -f /tmp/bridge-live/data/bridge.db*
  ./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &            # RunPeriodic's startup scan re-extracts every file
  ```
- **Selective / pairing-preserving / production** — use a throwaway Go helper that blank-imports `internal/manifest` (its `init` registers `unicode_lower`) + `modernc.org/sqlite`, opens the DB with `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`, and runs `DELETE FROM tracks WHERE <predicate>` (e.g. `json_extract(tags_json,'$.sampleRate') IS NULL` — only the rows a new extractor now fills, so only those re-enrich; a full `DELETE FROM tracks` resets `enriched_at` on every row → the MB/CAA/Deezer treadmill). Put it in a `_`-prefixed dir (ignored by `go ./...`/`build-all`, and by the tests that sweep the tree from its root), run with the bridge stopped, then restart to trigger the scan. On a loopback bridge `curl -s -X POST http://127.0.0.1:7789/api/scan` also triggers a scan; public bridges (admin auth-walled) need a restart.

**Verify a selective reset through the bridge after the next scan.** The Go helper above registers `unicode_lower` through `internal/manifest` and uses a WAL connection with foreign keys enabled. With the bridge stopped, delete only the affected track rows, close the helper's connection, then restart and verify the re-extracted metadata. Deleting track rows preserves pairing: bearer tokens are stored separately in `tokens.json` (`internal/auth/auth.go`), and the TLS certificate is a separate file (`internal/tls/tls.go`). This does not require deleting the database or the data directory.

**Re-extracting without deleting:** AGENTS.md's route, zeroing `mtime_ns` on the affected rows through the same helper, makes the next scan re-extract them in place; deleting them re-inserts them and resets their enrichment.

**Observed WAL read trap (2026-06):** an external `sqlite3` `SELECT COUNT(*)` against a live bridge DB kept showing the pre-scan count for minutes after a scan had re-added rows, and a restart showed them. Check a reset through the bridge, after a restart or once the next scan has finished, never by polling the CLI mid-scan.
