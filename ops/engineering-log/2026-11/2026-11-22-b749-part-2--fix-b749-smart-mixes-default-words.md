## 2026-11-22 — B749 part 2: Smart Mixes are on by default, in the config's comment and in PROTOCOL.md

Branch `fix/b749-smart-mixes-default-words`, from main `f899d811`. Two texts still called Smart Mixes
off by default and opt-in, though `smartPlaylists.enabled` has defaulted to on since 0.2.0 (the field's
own comment, `internal/config/config.go`, "Default ON — nil means on"; the flip is first in `v0.2.0`):

- the type comment of `SmartPlaylistsConfig` (`internal/config/config.go`), "Disabled by default; opt in
  here", now says it is on by default since 0.2.0 and that `enabled: false` turns it off;
- `PROTOCOL.md`'s Smart Mixes section, "Opt-in (`smartPlaylists.enabled` in `bridge.yaml`)", now says
  the same. The iOS copy of `PROTOCOL.md` follows in its own PR, with its pinned SHA-256;
- two field comments in `internal/admin/handlers_api.go` (the settings view's and the settings
  request's `SmartPlaylistsEnabled`) called the switch opt-in and restart-required. It is neither:
  the route, the health flag and the daily regenerator all read the running config
  (`cmd/bridge/main.go`), so a save takes effect at once. Both now say so.

### What was checked

A search of `PROTOCOL.md`, `docs/`, `README.md` and the console's scripts for Smart Mixes described as
opt-in, off by default or disabled by default found these two; a review of this branch found the two field comments above. The console's "Smart mixes
are off" texts describe the off state when an operator turned it off, and stay. No code changes; the
default itself is unchanged.

### Runs

`make docs-check`; `go vet` (with the release tags) on `./internal/config/`.
