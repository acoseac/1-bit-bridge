"""Where every part of the base CLAUDE.md goes in the 2026-10-18 restructure (data only; split.py
applies it). Every prefix here must match exactly one block, every title exactly one heading, and
split.py stops when one does not, so a newer base that moved or reworded an entry fails loudly
instead of routing it somewhere by accident.
"""

DATE = "2026-10-18"

# ---- the rules files ---------------------------------------------------------------------------
#
# Globs follow the evidence: the files each section's entries name, by path or by an identifier
# defined there (split.py --coverage lists what a file's globs miss). `internal/manifest` holds the
# store, the scanner and the job tables side by side, so it is globbed by file name, never whole.

RULES = {
    "scanner.md": {
        "title": "Scanner, deletion passes and manifest writes",
        "intro": "The walk, the extractors and the tag and header parsers, the deletion and "
                 "reconciliation passes, the watcher, and the store writes the scanner makes. The "
                 "highest-stakes area of the codebase: a bug here deletes rows whose files are still on disk.",
        "paths": [
            "internal/manifest/scanner*.go", "internal/manifest/extract*.go", "internal/manifest/dhowden_*.go",
            "internal/manifest/id3*.go", "internal/manifest/mp3format*.go", "internal/manifest/mp4*.go",
            "internal/manifest/ogg_*.go", "internal/manifest/codec*.go", "internal/manifest/container_text*.go",
            "internal/manifest/dsd_header*.go", "internal/manifest/sacd*.go", "internal/manifest/explicit*.go",
            "internal/manifest/mbid*.go", "internal/manifest/watcher*.go", "internal/manifest/reconcile*.go",
            "internal/manifest/deletion_journal*.go", "internal/manifest/first_indexed*.go",
            "internal/manifest/name_collisions*.go", "internal/manifest/eligibility*.go",
            "internal/manifest/folder_art_state*.go", "internal/manifest/provider*.go",
            "internal/manifest/ladder_lock*.go", "internal/manifest/dupe_stamps*.go",
            "internal/manifest/playlist_files*.go", "internal/manifest/sqlfunc*.go", "internal/manifest/store.go",
            "internal/manifest/testdata/gen/**",
            "internal/fsutil/walkroot*.go", "internal/fsutil/notafile*.go", "internal/fsutil/resolve*.go",
            "internal/fsutil/opendir*.go", "internal/fsutil/symlink*.go", "internal/fsutil/spelling*.go",
            "internal/fsutil/lock*.go", "internal/fs/**",
        ],
    },
    "wire.md": {
        "title": "The wire contract",
        "intro": "What the `/v1` API and the other wire surfaces promise the app: the served set, "
                 "optional fields, tombstones, events, query encoding, endpoints and mDNS, and "
                 "`PROTOCOL.md`. The cross-repo half of the contract (the Mirror-PR pair, "
                 "`ProtocolVersion`) is in CLAUDE.md under \"iOS companion\".",
        "paths": [
            "internal/api/**", "PROTOCOL.md", "internal/version/**", "internal/manifest/types.go",
            "internal/manifest/store.go", "internal/manifest/playlists*.go", "internal/manifest/favorites*.go",
            "internal/manifest/playlist_files*.go", "internal/manifest/sync_*.go", "internal/manifest/history*.go",
            "internal/advertise/**", "internal/mdns/**", "internal/urlquery/**", "internal/admin/query*.go",
            "internal/admin/pairing*.go",
        ],
    },
    "lyrics.md": {
        "title": "Lyrics — the one document per track",
        "intro": "Embedded and sidecar lyrics at extraction, the `track_lyrics` rows, `/v1/lyrics`, and "
                 "the network tier that fetches documents from Atlas.",
        "paths": [
            "internal/lyrics/**", "internal/manifest/lyrics_extract*.go", "internal/manifest/atlas_lyrics*.go",
            "internal/atlasharvest/lyrics*.go", "internal/atlasharvest/client*.go", "internal/api/lyrics*.go",
            "internal/manifest/store.go",
        ],
    },
    "enrichment.md": {
        "title": "Enrichment, artwork, fingerprinting and duplicates",
        "intro": "MusicBrainz, Cover Art Archive, Deezer and iTunes, the Atlas harvest and its "
                 "credential, artwork caching, AcoustID fingerprinting, and the duplicates partition "
                 "and the served set.",
        "paths": [
            "internal/enrich/**", "internal/acoustid/**", "internal/atlasharvest/**", "internal/baseurl/**",
            "internal/authredirect/**", "internal/lrucache/**", "internal/dupes/**",
            "internal/manifest/dupe_*.go", "internal/manifest/scanner_dupes*.go",
            "internal/manifest/acoustid_store*.go", "internal/manifest/atlas_meta*.go",
            "internal/manifest/artwork_*.go", "internal/manifest/booklets*.go", "internal/manifest/library_meta*.go",
            "cmd/bridge/enrichment*.go", "cmd/bridge/fingerprint*.go", "cmd/bridge/duplicates*.go",
            "internal/admin/handlers_duplicates*.go", "internal/admin/handlers_enrichment_misses*.go",
            "internal/api/atlas_*.go", "internal/api/artwork*.go", "internal/api/booklet*.go",
            "internal/manifest/store.go",
        ],
    },
    "jobs.md": {
        "title": "Job pools — upscale, optimize, analysis",
        "intro": "The transcode and analysis pools and their gates, the PCM and DSD renditions, the "
                 "rendition stamps and staleness, and the sidecar, variant and artwork reapers that "
                 "delete what the jobs wrote.",
        "paths": [
            "internal/transcode/**", "internal/analyze/**", "internal/albumgain/**", "internal/integrity/**",
            "cmd/bridge/upscale*.go", "cmd/bridge/optimize*.go", "cmd/bridge/analyze*.go", "cmd/bridge/render*.go",
            "cmd/bridge/variants*.go", "cmd/bridge/auto_optimize*.go", "cmd/bridge/rendition_*.go",
            "cmd/bridge/artwork*.go", "cmd/bridge/player_wiring*.go", "cmd/bridge/main.go",
            "internal/api/upscale*.go", "internal/api/waveform*.go", "internal/api/spectrum*.go", "internal/api/files.go",
            "internal/api/api.go", "internal/admin/static/app.js",
            "internal/manifest/variant_failure*.go", "internal/manifest/analysis_failure*.go",
            "internal/manifest/auto_optimize*.go", "internal/manifest/rendition_coverage*.go",
            "internal/manifest/dsd_peaks*.go", "internal/manifest/catalog_refs*.go", "internal/manifest/store.go",
            "internal/manifest/store_vfs*.go", "internal/fsutil/identity*.go",
            "internal/admin/handlers_upscale*.go", "internal/admin/handlers_variants_dir*.go",
            "internal/admin/handlers_jobs*.go", "internal/admin/player_variants*.go",
            "internal/admin/handlers_analysis_unreadable*.go", "internal/admin/variant_scope*.go",
        ],
    },
    "dlna-upnp.md": {
        "title": "DLNA, UPnP and discovery",
        "intro": "The DLNA MediaServer and its listener, SSDP, the renderer and server discovery "
                 "clients, the upstream UPnP ingest and proxy, interface eligibility, and mDNS.",
        "paths": [
            "internal/dlna/**", "internal/upnp/**", "internal/upnpingest/**", "internal/upnpproxy/**",
            "internal/mdns/**", "internal/dsdtone/**", "internal/baseurl/**",
            "cmd/bridge/dlna_*.go", "cmd/bridge/upnp_*.go", "cmd/bridge/mdns_*.go",
            "internal/manifest/store_upnp_routing*.go", "internal/api/upnp_proxy*.go", "internal/api/renderers*.go",
            "internal/admin/handlers_upnp*.go",
        ],
    },
    "config-lifecycle.md": {
        "title": "Config, settings and process lifecycle",
        "intro": "The config and its live and boot halves, managed settings and controls, service "
                 "install and restart, atomic writes and file ownership, the token store's writes, and "
                 "the self-updater's binary swap.",
        "paths": [
            "internal/config/**", "internal/packaging/**", "internal/supervision/**", "internal/updater/**",
            "internal/atomicwrite/**", "internal/logging/**", "internal/auth/**", "internal/tailscale/**",
            "internal/fsutil/owner*.go", "internal/fsutil/container*.go",
            "cmd/bridge/main.go", "cmd/bridge/init.go", "cmd/bridge/jobstate*.go", "cmd/bridge/serve_restart*.go",
            "cmd/bridge/lifecycle*.go", "cmd/bridge/service_*.go", "cmd/bridge/update*.go", "cmd/bridge/variants*.go",
            "internal/admin/settings_apply*.go", "internal/admin/managed_controls*.go",
            "internal/admin/handlers_api.go", "internal/admin/static/app.js", "internal/backup/backup.go",
            "internal/manifest/playlist_covers*.go", "Dockerfile",
        ],
    },
    "sqlite.md": {
        "title": "SQLite predicates, compaction and retention",
        "intro": "How a query reaches its index, FTS5 deletes, `VACUUM` and the WAL, the compaction's "
                 "figures, and the retention windows.",
        "paths": [
            "internal/manifest/store.go", "internal/manifest/compact*.go", "internal/manifest/retention*.go",
            "internal/manifest/sqlhelpers*.go", "internal/manifest/analysis_failure*.go",
            "internal/backup/vacuum*.go", "internal/dsn/**", "internal/sqlitetest/**",
            "cmd/bridge/retention_sweeper*.go", "internal/admin/handlers_database*.go",
            "internal/transcode/projection*.go",
            "internal/admin/handlers_diagnostics*.go", "internal/admin/settings_matrix_doc_test.go",
        ],
    },
    "cli-serve.md": {
        "title": "The CLI and the serve wiring (`cmd/bridge`)",
        "intro": "The subcommands, `bridge init` and its preflight, `bridge doctor`, and `runServe`: "
                 "its gates, loops, locks, shutdown and drains.",
        "paths": [
            "cmd/bridge/**", "internal/doctor/**", "internal/handshakelog/**", "internal/ctxerr/**",
            "internal/backup/**", "internal/tsnet/**", "internal/tailscale/**", "internal/api/events*.go",
            "internal/api/event_broker*.go",
        ],
    },
    "auth-tls.md": {
        "title": "Auth, pairing, TLS and security posture",
        "intro": "The console's loopback and public postures, CSRF, sessions and login tickets, "
                 "pairing, bearer tokens, the TLS certificate and its pin, and what a log line or an "
                 "error body may say.",
        "paths": [
            "internal/auth/**", "internal/adminauth/**", "internal/tls/**", "internal/tlsacme/**",
            "internal/pairing/**", "internal/pairingcode/**", "internal/authredirect/**",
            "internal/admin/admin.go", "internal/admin/middleware_auth*.go", "internal/admin/pairing*.go",
            "internal/admin/handlers_login*.go", "internal/admin/handlers_pairing*.go",
            "internal/admin/handlers_cert*.go", "internal/admin/handlers_autocert*.go",
            "internal/admin/handlers_token_lifecycle*.go", "internal/admin/templates/login*.html",
            "internal/api/api.go", "internal/api/pairing*.go", "internal/api/middleware*.go",
            "internal/api/ratelimit*.go", "cmd/bridge/cert*.go", "cmd/bridge/token*.go", "internal/advertise/**",
            "internal/baseurl/**",
        ],
    },
    "admin-console.md": {
        "title": "Admin console and the web player",
        "intro": "The console's pages, static JavaScript and CSS, its JSON API and its guards, the web "
                 "player, uploads and the trash.",
        "paths": [
            "internal/admin/**", "internal/upload/**", "internal/trash/**", "cmd/bridge/status*.go",
        ],
    },
    "build-ci-tests.md": {
        "title": "Build, CI and test discipline",
        "intro": "How a test here is written, run and trusted, and the CI, SonarCloud and Docker rules. "
                 "It loads with every test file. The PR-time bot-review rules are in CLAUDE.md.",
        "paths": [
            "**/*_test.go", "**/testdata/**", ".github/**", "Makefile", "Dockerfile", ".goreleaser.yaml",
            "compose.yaml", "internal/sweeptest/**", "internal/proctest/**", "internal/sqlitetest/**",
            "internal/pooltest/**", "internal/flactest/**", "internal/dnstest/**", "internal/standintest/**",
            "internal/backup/backuptest/**", "internal/logging/loggingtest/**",
        ],
    },
}

# ---- routing -----------------------------------------------------------------------------------

# The `##` sections of the base CLAUDE.md, by title prefix: "core" stays (its long blocks become
# one-liners unless the section is in KEEP_WHOLE), "skill:<name>" moves whole into a skill, and
# "rules:<file>" moves whole into a rules file.
TOP = [
    ("Build", "core"),
    ("Architecture at a glance", "core"),
    ("iOS companion (coupled repo)", "core"),
    ("Wire-type discipline", "rules:wire.md"),
    ("Local test fixture", "skill:local-test-fixture"),
    ("Production deployments", "core"),
    ("Agent memory", "core"),
    ("Follow-ups go in the backlog", "core"),
    ("Things that have bitten before", "core"),
    ("Licensing", "core"),
    ("Repo clean-up", "core"),
    ("Documentation refresh on each release", "core"),
    ("External consultation (Gemini)", "skill:gemini-consult"),
    ("Bot-review discipline (PR-time)", "core"),
    ("LOUPE — the recent-work review cycle", "skill:review-procedures"),
    ("External code review (DeepSeek sweep)", "skill:review-procedures"),
    ("Development workflow", "core"),
    ("Post-merge deployment", "core"),
    ("Multi-PR batch workflow", "skill:multi-pr-batch"),
]

# The `###` sections under "Things that have bitten before", by title prefix. A `####` section
# goes with its `###`. "reviews" is the dated review records: one line each in CLAUDE.md, the
# record itself in History-Batches.md (not History-Reviews.md: .gitignore keeps ops/*review*.md untracked).
SECTION_DEST = [
    ("Scanner, deletion passes, and manifest writes", "scanner.md"),
    ("The wire contract", "wire.md"),
    ("Lyrics — the one document per track", "lyrics.md"),
    ("Enrichment — MusicBrainz, Atlas, artwork, fingerprinting", "enrichment.md"),
    ("Duplicates and the served set", "enrichment.md"),
    ("Job pools — upscale, optimize, analysis", "jobs.md"),
    ("DLNA, UPnP and discovery", "dlna-upnp.md"),
    ("Config, settings and process lifecycle", "config-lifecycle.md"),
    ("SQLite predicates and their indexes", "sqlite.md"),
    ("Database compaction and retention", "sqlite.md"),
    ("The CLI and the serve wiring (`cmd/bridge`)", "cli-serve.md"),
    ("Auth, pairing, TLS and security posture", "auth-tls.md"),
    ("Admin console and the web player", "admin-console.md"),
    ("Build, CI, and test discipline", "build-ci-tests.md"),
    ('<a name="review-', "reviews"),
    ('<a name="loupe-', "reviews"),
]

# Blocks that go somewhere other than their section's destination, by the start of their text:
# a rules file, "skill:<name>", or "core:<## title prefix>" (stays in CLAUDE.md, at the end of
# that section). Each prefix must match exactly one block.
BLOCK_DEST = [
    # Production deployments: the fleet and the runbook pointer are the deploy skill's; the rules
    # that hold without it keep a line in CLAUDE.md (DEPLOY_LINES); the docs/ warning stays.
    ("**See `ops/deployment-runbook.md`** — full runbooks", "skill:production-deploy"),
    # Documentation refresh: release day is the release skill's; the two rules for any doc edit stay.
    ("**Release day: follow [`ops/release-playbook.md`]", "skill:release-playbook"),
    ("- scope and freeze, the three read-only sanity reviews", "skill:release-playbook"),
    ("- validating the candidate: the gate and a goreleaser snapshot", "skill:release-playbook"),
    ("- the annotated tag, the artifact checks", "skill:release-playbook"),
    ("- the gated rollout of the release artifact", "skill:release-playbook"),
    ("- how to run all of it with agents inside the usage limits.", "skill:release-playbook"),
    ("The operator decides the scope, every production deploy", "skill:release-playbook"),
    ("**See `docs/release-process.md`** — which docs to update", "skill:release-playbook"),
    # Build, CI, and test discipline: the PR-time rules bind a PR session that may Read no test
    # file, so they stay in CLAUDE.md beside "Bot-review discipline".
    ("- **Stacked PRs get NO gate CI here**", "core:Bot-review discipline (PR-time)"),
    ("- **A rate-limited bot's silence is not approval", "core:Bot-review discipline (PR-time)"),
    ("- **Read a PR's reviews and comments with `gh api --paginate`", "core:Bot-review discipline (PR-time)"),
    ("- **A fix round needs a FRESH pass from every bot", "core:Bot-review discipline (PR-time)"),
    ("- **Merging with review comments outstanding is a process failure**", "core:Bot-review discipline (PR-time)"),
    # Scanner entries about code another file loads with
    ("- **Shutdown joins every background writer**", "cli-serve.md"),
    ("- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before.** In WAL mode the vacuum's own",
     "sqlite.md"),
    # Auth and security posture: a log rule whose code is the disk probe
    ("- **The disk probe's missing-directory warning names the directory by its role", "jobs.md"),
]
