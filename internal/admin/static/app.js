// 1-bit bridge admin console — vanilla JS, no framework.
//
// Every page refreshes the lightweight /api/stats poll in the background so
// the dashboard numbers tick without a reload. Mutations (pair, revoke,
// add/remove root, save settings) hit the typed JSON endpoints in
// handlers_api.go and reload the row/page on success.

"use strict";

const API = {
  async get(path) {
    const r = await fetch(path, { headers: { accept: "application/json" } });
    if (!r.ok) throw await errorFromResponse(r);
    return r.json();
  },
  async post(path, body) {
    const r = await fetch(path, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: body == null ? null : JSON.stringify(body),
    });
    if (!r.ok) throw await errorFromResponse(r);
    return r.status === 204 ? null : r.json();
  },
  async patch(path, body) {
    const r = await fetch(path, {
      method: "PATCH",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw await errorFromResponse(r);
    return r.json();
  },
  async delete(path, body) {
    const r = await fetch(path, {
      method: "DELETE",
      headers: body ? { "content-type": "application/json" } : {},
      body: body ? JSON.stringify(body) : null,
    });
    if (!r.ok) throw await errorFromResponse(r);
    return r.status === 204 ? null : r.json();
  },
};

// Returns true when the update payload's `lastCheck` field reflects
// a Go `time.Time{}` zero value rather than a real timestamp. The
// server marshals zero time as the literal RFC-3339 `0001-01-01T00:00:00Z`
// (the field carries `omitempty` but that flag doesn't drop zero
// `time.Time` structs — only zero pointers / strings / numerics). The
// dashboard's Updates tile uses this to distinguish "no poll has
// fired yet" from "poll just landed", surfacing the former as
// `scheduled` + `Last check never` instead of `checking… 739736d ago`.
// (User feedback on PR #129.)
//
// Defensive: missing field, empty string, year-prefix `0001` all
// count as the zero value. Anything else is a real timestamp.
function isUpdateLastCheckZero(u) {
  if (!u) return true;
  const v = u.lastCheck;
  if (!v) return true;
  if (typeof v !== "string") return false;
  return v.startsWith("0001-01-01");
}

async function errorFromResponse(r) {
  try {
    const j = await r.json();
    return new Error(j.message || j.error || `${r.status} ${r.statusText}`);
  } catch {
    return new Error(`${r.status} ${r.statusText}`);
  }
}

// Returns true when an error thrown by `API.post("/api/restart")` is
// the expected post-exit shape rather than a real HTTP failure. The
// bridge's restart handler writes 202 then `os.Exit(0)` 100 ms
// later, so we typically catch one of:
//
//   * `TypeError` — fetch's connection-drop signal when the server
//     tears the listener down mid-read.
//   * `SyntaxError` — `r.json()` failing on a TRUNCATED body. The
//     handler does write a JSON body with the 202 now (the drain
//     result), but the process exits 100 ms later, so a slow read can
//     still catch the connection going away mid-parse.
//
// Anything else (notably a plain `Error` from `errorFromResponse`,
// which wraps real 4xx/5xx) is a genuine failure and the caller
// should surface it instead of claiming the restart was signalled.
// (CodeRabbit on PR #124.)
function isExpectedRestartDisconnect(err) {
  return err instanceof TypeError || err instanceof SyntaxError;
}

// --- dashboard ---

// wireUpdatePanel binds the Updates panel's three controls.
//
// Called from initSettings, because that is where the panel lives —
// dashboard.html carries no update-* id at all. It used to sit inside
// the dashboard init, from the era when Stats and Settings were one
// page, which is why "Check now" and "Roll back" were dead even after
// the dispatch bug below was accounted for.
//
// bindInstallButton is NOT idempotent, and renderUpdateTile also calls
// it — but only in the branch where it CREATES the button, so the
// server-rendered one is bound here and the JS-created one at birth.
// Exactly one listener either way; keep that split if either side moves.
function wireUpdatePanel() {
  // "Roll back" swaps the previous binary back in. Guarded by a
  // typed-intent confirm rather than a bare one — this replaces the
  // running binary, and the operator should not be able to do it by
  // reflex from a dialog they were already dismissing.
  const rollbackBtn = document.getElementById("update-rollback");
  if (rollbackBtn) {
    rollbackBtn.addEventListener("click", async () => {
      if (!confirm(
        "Roll back to the previous bridge binary?\n\n" +
        "This swaps the running binary and clears the update state. " +
        "The bridge must be restarted afterwards to load it."
      )) return;
      rollbackBtn.disabled = true;
      rollbackBtn.textContent = "Rolling back…";
      try {
        await API.post("/api/updates/rollback");
        rollbackBtn.textContent = "Rolled back — restart to apply";
      } catch (err) {
        rollbackBtn.textContent = "Roll back";
        rollbackBtn.disabled = false;
        alert("Rollback failed: " + err.message);
      }
    });
  }

  // "Check now" forces a fresh GitHub poll. The handler returns the
  // post-check status so the tile refreshes in one trip.
  const updateCheckBtn = document.getElementById("update-check");
  if (updateCheckBtn) {
    updateCheckBtn.addEventListener("click", async () => {
      const oldText = updateCheckBtn.textContent;
      updateCheckBtn.disabled = true;
      updateCheckBtn.textContent = "Checking…";
      try {
        const u = await API.post("/api/updates/check");
        renderUpdateTile(u);
      } catch (err) {
        renderUpdateTile({ lastError: err.message });
      } finally {
        updateCheckBtn.textContent = oldText;
        updateCheckBtn.disabled = false;
      }
    });
  }

  // "Install & restart" downloads, verifies, swaps the binary, then
  // hits the existing /api/restart endpoint. The two are kept
  // sequential rather than fused into one server-side handler so the
  // user sees distinct success / failure surfaces for each step.
  // The 409 active-sessions branch surfaces an "Install anyway" prompt
  // backed by ?force=1.
  bindInstallButton(document.getElementById("update-install"));
}

// initStats wires the Stats page's controls and paints its harmonic
// wheel.
//
// It was `initDashboard`, dispatched on a tab named "dashboard" — a name
// no page has rendered since the player took over "/" and the operator
// dashboard moved to /stats (#739). Every lookup inside is nil-guarded,
// so nothing failed: the function simply never ran, and "Scan now",
// "Which tracks?" and "Retry missing" have had no click handler at all.
// The update-panel wiring that used to live here moved to initSettings
// along with the panel itself.
function initStats() {
  // The Camelot wheel, inherited from the retired /smartmixes page. Its
  // container is absent unless something has been analyzed, and the
  // helper no-ops on that — so this is safe on a library with no
  // analysis, and on every other page.
  initCamelotWheel();

  const scanBtn = document.getElementById("scan-now");
  if (scanBtn) {
    scanBtn.addEventListener("click", async () => {
      scanBtn.disabled = true;
      try {
        await API.post("/api/scan");
      } finally {
        setTimeout(() => (scanBtn.disabled = false), 500);
      }
    });
  }

  // Enrichment panel: "Which tracks?" opens the per-facet breakdown.
  // Fetch happens on the FIRST open only — the endpoint walks the library
  // with json_extract, so re-fetching on every toggle would make an
  // expensive query a function of how often someone collapses a panel.
  // "Retry missing" clears the cached view, since after a re-queue the
  // old answer is exactly the thing the operator is trying to change.
  const enrichMissesBtn = document.getElementById("enrich-misses-toggle");
  const enrichMissesSection = document.getElementById("enrich-misses");
  let enrichMissesLoaded = false;
  if (enrichMissesBtn && enrichMissesSection) {
    enrichMissesBtn.addEventListener("click", async () => {
      const opening = enrichMissesSection.hidden;
      enrichMissesSection.hidden = !opening;
      enrichMissesBtn.setAttribute("aria-expanded", String(opening));
      enrichMissesBtn.textContent = opening ? "Hide tracks" : "Which tracks?";
      // Only mark it loaded if it actually loaded. Setting the flag
      // before the await left a failed fetch permanently "loaded", so
      // closing and reopening never retried and the error message was
      // terminal until a page reload.
      if (opening && !enrichMissesLoaded) {
        enrichMissesLoaded = await loadEnrichMisses();
      }
    });
  }

  // Enrichment panel: "Retry missing" re-queues enriched-but-incomplete
  // tracks + nudges the harvest re-submit. Success shows the re-queued
  // count briefly; the server 429s repeat clicks inside its rate window.
  const enrichRetryBtn = document.getElementById("enrich-retry");
  if (enrichRetryBtn) {
    const idleText = enrichRetryBtn.textContent;
    enrichRetryBtn.addEventListener("click", async () => {
      enrichRetryBtn.disabled = true;
      enrichRetryBtn.textContent = "Retrying…";
      try {
        const r = await API.post("/api/enrichment/retry");
        enrichRetryBtn.textContent =
          r && r.resetTracks > 0 ? `Re-queued ${r.resetTracks}` : "Re-checked";
        // Repaint from the snapshot the handler computed post-reset. The
        // enrichment SSE event rides the 30s slow ticker, so without this the
        // panel keeps showing "0 tracks in the queue · all caught up" for up
        // to half a minute after a click that just queued thousands.
        if (r && r.enrichment) applyEnrichment(r.enrichment);
        // The breakdown just became stale by construction. Drop it so a
        // reopen re-fetches rather than showing the pre-retry answer.
        enrichMissesLoaded = false;
        if (enrichMissesSection && !enrichMissesSection.hidden) {
          enrichMissesLoaded = await loadEnrichMisses();
        }
      } catch (err) {
        enrichRetryBtn.textContent = idleText;
        alert("Retry failed: " + err.message);
      } finally {
        setTimeout(() => {
          enrichRetryBtn.disabled = false;
          enrichRetryBtn.textContent = idleText;
        }, 4000);
      }
    });
  }

  // Backups + Tailscale wiring moved to initSettings (PR #129)
  // — the panels themselves moved from this dashboard page to the
  // Settings tabs. The SSE-driven `renderTailscaleTile` /
  // backup count refresh paths are page-agnostic (they look up
  // their target elements by id and no-op when missing), so
  // streaming updates on Settings still work without a re-arm.
  // Live updates for stats, updates, tailscale arrive over the SSE
  // stream wired at the bottom of this file (`/api/events`). The
  // dashboard's first paint is server-rendered from template data
  // (TracksIndexed / DeviceCount / etc.); the SSE initial snapshot
  // lands within milliseconds of `EventSource` connect and keeps
  // every tile live without per-page setInterval polls.
}

// applyStats updates every dashboard tile that reflects /api/stats.
// Called by the SSE `stats` event listener with the parsed payload.
// Self-guards via per-element existence checks so calling it on a
// page that doesn't render every tile (e.g. Devices, Settings) is
// a no-op.
function applyStats(s) {
  if (!s) return;
  // Duplicates page: a full scan completing means the stamping pass just
  // re-evaluated serving — refresh that page's data on the true→false
  // edge (no dedicated SSE event; the Diagnostics-page precedent).
  if (dupesLastIsScanning && !s.isScanning) refreshDuplicatesPage();
  dupesLastIsScanning = !!s.isScanning;
  setText("tracks-indexed", s.tracksIndexed);
  setText("device-count", s.deviceCount);
  // Library composition tiles (dashboard only; no-op elsewhere).
  setText("comp-originals", s.tracksIndexed);
  setText("comp-upscaled", s.tracksWithUpscaled ?? 0);
  setText("comp-optimized", s.tracksWithOptimized ?? 0);
  setText("comp-variant-files", s.variantFiles ?? 0);
  setText("comp-variant-bytes", formatBytes(s.variantBytes ?? 0));
  const scanStatus = document.getElementById("scan-status");
  if (scanStatus) {
    scanStatus.innerHTML = s.isScanning
      ? `<span class="badge running">scanning</span><span>· ${s.scanProgress} tracks so far</span>`
      : `<span class="badge idle">idle</span>`;
  }
  // "Last full scan" tile: pre-fix this was server-rendered once at
  // page load and never repainted, so a scan that completed mid-
  // session left the tile showing "never" until manual refresh.
  // /api/stats carries lastFullScan; route it through so the value
  // tracks the in-memory truth at SSE cadence.
  const lastFull = document.getElementById("last-full-scan");
  if (lastFull) {
    lastFull.textContent = s.lastFullScan
      ? formatTimeAgo(new Date(s.lastFullScan))
      : "never";
  }
}

// applyComposition renders the dashboard "Master quality" distribution
// bars from the SSE `composition` event (PCM sampling density, DSD
// streams, codec layout). No-op on pages without the container. The DSD
// section hides when the library has no DSD; the whole block hides on an
// empty/zero-total snapshot.
function applyComposition(c) {
  const root = document.getElementById("master-quality");
  if (!root || !c) return;
  const total = c.total || 0;
  renderDistBar("comp-pcm-bar", "comp-pcm-legend", c.pcm || [], total, "pcm");
  renderDistBar("comp-codec-bar", "comp-codec-legend", c.codecs || [], total, "codec");
  const dsd = c.dsd || [];
  const dsdSection = document.getElementById("comp-dsd-section");
  if (dsdSection) dsdSection.hidden = dsd.length === 0;
  renderDistBar("comp-dsd-bar", "comp-dsd-legend", dsd, total, "dsd");
  root.hidden = total === 0;
}

// renderDistBar paints one proportional stacked bar + legend from a set
// of {label,count} segments. Built via createElement/textContent (never
// innerHTML) so codec/tier labels can't inject markup. Segment colour is
// a chart token from app.css set inline as var(--chart-...): the ordered
// bars (pcm, dsd — tier index IS the rate order) walk the sequential
// yellow ramp so lightness encodes the tier; the codec bar walks the
// fixed categorical order. Tokens, not literals, so both themes resolve
// from the same place the rest of the palette lives.
function renderDistBar(barId, legendId, segs, total, kind) {
  const bar = document.getElementById(barId);
  const legend = document.getElementById(legendId);
  if (!bar || !legend) return;
  bar.textContent = "";
  legend.textContent = "";
  const denom = total > 0 ? total : 1;
  segs.forEach((seg, i) => {
    const pct = (seg.count / denom) * 100;
    // "Unknown" = the bridge knows the codec but not the PCM geometry
    // (it stream-parses rate/bits for FLAC + DSD only; the tag-library
    // path for MP3/M4A/WAV/AIFF doesn't expose them). Render it neutral
    // grey so it reads as "unanalysed", not a quality tier.
    const unknown = seg.label === "Unknown";
    // Sequential ramp for the ordered kinds, fixed categorical order for
    // codec; the ramp clamps at its darkest step rather than wrapping
    // (a wrapped sequential would un-order the encoding). The Unknown
    // bucket keeps its neutral class — its CSS wins via !important, and
    // skipping the inline set keeps the cascade honest.
    const tok = kind === "codec"
      ? `--chart-cat-${(i % 8) + 1}`
      : `--chart-seq-${Math.min(i, 4) + 1}`;
    const span = document.createElement("span");
    span.className = unknown ? "dist-seg is-unknown" : "dist-seg";
    span.style.width = pct.toFixed(2) + "%";
    if (!unknown) span.style.background = `var(${tok})`;
    span.dataset.kind = kind;
    span.title = `${seg.label}: ${seg.count} (${pct.toFixed(1)}%)`;
    bar.appendChild(span);

    const item = document.createElement("span");
    item.className = unknown ? "dist-legend-item is-unknown" : "dist-legend-item";
    item.dataset.kind = kind;
    const swatch = document.createElement("i");
    swatch.className = "dist-swatch";
    if (!unknown) swatch.style.background = `var(${tok})`;
    item.appendChild(swatch);
    item.appendChild(document.createTextNode(`${seg.label} `));
    const b = document.createElement("b");
    b.textContent = String(seg.count);
    item.appendChild(b);
    legend.appendChild(item);
  });
}

// applySources renders the dashboard "Sources" breakdown from the SSE
// `sources` event: filesystem ("on this bridge") vs each configured UPnP
// upstream (with an online / offline / manual status chip, offline rows
// dimmed) vs total. Cosmetic — it just makes the headline provenance
// honest; the offline upstream's tracks stay in the library either way.
//
// Built via createElement/textContent (never innerHTML) so third-party
// server names can't inject markup. The list is cleared each frame
// (replaceChildren) so the 30 s tick can't stack duplicate rows, and
// visibility is assigned in both directions so removing the last upstream
// re-hides the block.
function applySources(data) {
  if (!data) return;

  // Master-quality caption: reveal only when some tracks are UPnP-routed
  // (otherwise the bars already reflect only this bridge). Reversible.
  const note = document.getElementById("master-quality-note");
  const routedTotal = data.routedTotal || 0;
  if (note) note.hidden = routedTotal === 0;

  const list = document.getElementById("sources-list");
  const block = document.getElementById("sources-block");
  if (!list || !block) return;

  const servers = Array.isArray(data.servers) ? data.servers : [];
  // The breakdown only earns its space when a UPnP upstream is actually in
  // the mix; a pure-filesystem bridge shows nothing but its headline total.
  const hasUPnP = !!data.upnpEnabled && (servers.length > 0 || routedTotal > 0);
  block.hidden = !hasUPnP;
  list.replaceChildren(); // clear first — reversible + no 30 s row stacking
  if (!hasUPnP) return;

  const nf = (n) => Number(n || 0).toLocaleString();
  const tracksWord = (n) => (Number(n) === 1 ? "track" : "tracks");

  // addRow appends a dt (label [+ status chip]) / dd (count) pair.
  const addRow = (label, count, opts = {}) => {
    const dt = document.createElement("dt");
    dt.textContent = label;
    if (opts.chip) {
      const chip = document.createElement("span");
      chip.className = "badge " + opts.chip.cls;
      chip.textContent = opts.chip.text;
      dt.appendChild(document.createTextNode(" "));
      dt.appendChild(chip);
    }
    const dd = document.createElement("dd");
    dd.textContent = `${nf(count)} ${tracksWord(count)}`;
    if (opts.dim) {
      dt.classList.add("source-offline");
      dd.classList.add("source-offline");
    }
    if (opts.total) {
      dt.classList.add("source-total");
      dd.classList.add("source-total");
    }
    list.appendChild(dt);
    list.appendChild(dd);
  };

  // Filesystem — the tracks that actually live on this bridge.
  addRow("On this bridge", data.filesystem || 0);

  // One row per configured UPnP upstream.
  let sumRows = 0;
  servers.forEach((srv) => {
    const routed = srv.routedTracks || 0;
    sumRows += routed;
    let chip, dim = false;
    if (!srv.monitored) {
      // Manual-URL upstream: no SSDP presence to watch, so "offline" would
      // be a false alarm — badge it neutral instead.
      chip = { cls: "idle", text: "manual" };
    } else if (srv.online) {
      chip = { cls: "idle", text: "online" };
    } else {
      chip = { cls: "warn", text: "offline" };
      dim = true;
    }
    addRow(srv.name || "UPnP upstream", routed, { chip, dim });
  });

  // Orphan remainder — routing rows whose upstream was just removed and
  // haven't been reaped yet. The server-side budget guarantees this is >= 0.
  const other = routedTotal - sumRows;
  if (other > 0) addRow("Other UPnP sources", other);

  // Total — reconciles: filesystem + servers + other == total.
  addRow("Total", data.total || 0, { total: true });
}

// applyEnrichment renders the dashboard "Enrichment" panel from the SSE
// `enrichment` event: the derived pending/matched/missing counts, a coarse
// ETA, the last-enriched time, and the matched-vs-missing coverage bar. The
// whole panel stays hidden until the first frame lands (cold cache / empty
// library shows nothing rather than zeros). No-op on pages without the panel.
function applyEnrichment(e) {
  const panel = document.getElementById("enrichment-panel");
  if (!panel || !e) return;
  panel.hidden = false;

  const pending = e.pending ?? 0;
  setText("enrich-pending", pending);
  setText("enrich-matched", e.matched ?? 0);
  setText("enrich-missing", e.missing ?? 0);

  const last = document.getElementById("enrich-last");
  if (last) {
    last.textContent = e.lastEnrichedAt
      ? formatTimeAgo(new Date(e.lastEnrichedAt))
      : "never";
  }

  const etaEl = document.getElementById("enrich-eta");
  if (etaEl) etaEl.textContent = formatEnrichEta(pending, e.etaSecondsEstimate);

  // Source row — config-derived label, hidden on frames that omit it.
  const srcDt = document.getElementById("enrich-source-dt");
  const srcDd = document.getElementById("enrich-source");
  if (srcDt && srcDd) {
    const label = ENRICH_SOURCE_LABELS[e.source];
    srcDt.hidden = srcDd.hidden = !label;
    if (label) srcDd.textContent = label;
  }

  // Coverage rows: artist images / bios / album descriptions. Each is
  // omitted from the payload when its data source isn't wired (e.g. bios
  // on a bridge without Atlas) — the row stays hidden in that case.
  setEnrichCoverage("enrich-artist-images", e.artistImages);
  setEnrichCoverage("enrich-artist-bios", e.artistBios);
  setEnrichCoverage("enrich-album-desc", e.albumDescriptions);

  // PDF booklets: "N available · M cached" (different vocabulary from the
  // have/missing coverage rows — availability is upstream-driven).
  const bkDt = document.getElementById("enrich-booklets-dt");
  const bkDd = document.getElementById("enrich-booklets");
  if (bkDt && bkDd) {
    const bk = e.booklets;
    const show = bk && typeof bk.available === "number";
    bkDt.hidden = bkDd.hidden = !show;
    if (show) bkDd.textContent = `${bk.available} available · ${bk.cached} cached`;
  }

  // Reveal "Retry missing" only when some facet actually has gaps.
  const retryBtn = document.getElementById("enrich-retry");
  if (retryBtn) {
    const anyMissing =
      (e.missing ?? 0) > 0 ||
      (e.artistImages?.missing ?? 0) > 0 ||
      (e.artistBios?.missing ?? 0) > 0 ||
      (e.albumDescriptions?.missing ?? 0) > 0;
    retryBtn.hidden = !anyMissing;
  }

  // "Which tracks?" is gated on `missing` ALONE, not the same anyMissing
  // test as Retry. The endpoint enumerates the three per-track enricher
  // facets (artwork / artist / release); it has nothing to say about
  // artist images, bios or descriptions, so offering the drill-down on
  // their account would open an empty panel.
  const missesBtn = document.getElementById("enrich-misses-toggle");
  if (missesBtn) missesBtn.hidden = (e.missing ?? 0) <= 0;

  renderCoverageBar(e.matched ?? 0, e.missing ?? 0);
}

// Human labels for enrichmentResponse.source (deriveEnrichSource values).
const ENRICH_SOURCE_LABELS = {
  musicbrainz: "MusicBrainz (public)",
  atlas: "Atlas (self-hosted)",
  custom: "Custom mirrors",
};

// setEnrichCoverage paints one "N have · M missing" coverage row, keyed by
// the shared id prefix (dt = `${baseId}-dt`, dd = baseId). Hides the pair
// when the facet is absent from the payload.
function setEnrichCoverage(baseId, counts) {
  const dt = document.getElementById(baseId + "-dt");
  const dd = document.getElementById(baseId);
  if (!dt || !dd) return;
  const show = counts && typeof counts.have === "number";
  dt.hidden = dd.hidden = !show;
  if (show) dd.textContent = `${counts.have} have · ${counts.missing} missing`;
}

// Facet labels for the misses drill-down. Keys mirror
// manifest.MissFacet* — artwork / artist / release — which are the three
// things the enricher looks for per track and the exact set
// enrichmentMissPredicateSQL selects on.
const ENRICH_MISS_FACET_LABELS = {
  artwork: "No cover art",
  artist: "No artist MBID",
  release: "No release MBID",
};

// The same facets as bare nouns, for the inline "also missing …" note.
// Two maps rather than one because the section headings read best as
// negations ("No cover art — 6,816 tracks") and the inline note supplies
// its own — reusing the headings there produced "also missing no artist
// mbid, no release mbid", which is how it rendered against the live
// bridge before this map existed.
const ENRICH_MISS_FACET_NOUNS = {
  artwork: "cover art",
  artist: "artist MBID",
  release: "release MBID",
};

// Why the enricher gave up, keyed by the bounded reason constants it
// tallies (skipReason* in internal/enrich). Bounded by design — a
// formatted error string must never become a key — so this map can be
// exhaustive, with an unknown key falling back to the raw reason.
const ENRICH_SKIP_REASON_LABELS = {
  no_search_terms: "Track had no artist/album tags to search on",
  no_mb_match: "Searched, but the source returned no match",
  mb_error: "The enrichment source errored",
};

// loadEnrichMisses fetches GET /api/enrichment/misses and paints the
// per-facet breakdown.
//
// Click-driven only. The query behind it is a json_extract subtree walk
// (the AtlasMetaBreakdownCounts cost class), which is why the endpoint
// caches behind a TTL + singleflight and why this must never be attached
// to a poll or an SSE tick.
//
// Returns whether the panel now holds real data. The caller uses that to
// decide whether it may skip the fetch next time — a failed load must
// stay retryable, or the operator is stuck looking at an error message
// with no way back short of reloading the page.
async function loadEnrichMisses() {
  const section = document.getElementById("enrich-misses");
  const status = document.getElementById("enrich-misses-status");
  const body = document.getElementById("enrich-misses-body");
  if (!section || !status || !body) return false;
  status.textContent = "Looking…";
  body.replaceChildren();
  try {
    const data = await API.get("/api/enrichment/misses");
    renderEnrichMisses(data);
    return true;
  } catch (err) {
    // Say what failed. The pre-existing pattern on this page swallowed
    // errors into a silent no-op, which is indistinguishable from "you
    // have no misses" — the one answer this panel must never fake.
    status.textContent = `Couldn't load the breakdown: ${err.message}`;
    return false;
  }
}

// renderEnrichMisses paints the response. Everything user-visible goes
// through createElement/textContent: track paths and skip reasons are
// third-party strings (tag data and upstream error text).
function renderEnrichMisses(data) {
  const status = document.getElementById("enrich-misses-status");
  const body = document.getElementById("enrich-misses-body");
  if (!status || !body) return;
  body.replaceChildren();

  const scanned = data?.scanned ?? 0;
  const missing = data?.missing ?? 0;
  status.textContent = missing === 0
    ? `Nothing short — all ${scanned} tracks carry a cover, an artist and a release ID.`
    : `${missing} of ${scanned} tracks are short of at least one field. ` +
      `A track missing two things is counted under both, so the rows below add up to more than ${missing}.`;
  if (missing === 0) return;

  const facets = data.facets || {};
  const samples = data.sample || {};
  const truncated = new Set(data.truncated || []);

  for (const key of Object.keys(ENRICH_MISS_FACET_LABELS)) {
    const count = facets[key] ?? 0;
    if (count <= 0) continue;
    const rows = samples[key] || [];

    const details = document.createElement("details");
    details.className = "enrich-miss-facet";
    const summary = document.createElement("summary");
    summary.textContent = `${ENRICH_MISS_FACET_LABELS[key]} — ${count} track${count === 1 ? "" : "s"}`;
    details.appendChild(summary);

    if (truncated.has(key)) {
      const note = document.createElement("p");
      note.className = "hint";
      // Be explicit that the LIST is capped while the COUNT is exact —
      // a silently-truncated list reads as "that's all of them".
      note.textContent =
        `Showing the first ${rows.length}. The count above is exact; ` +
        `run \`bridge enrichment misses\` for the full list.`;
      details.appendChild(note);
    }

    const ul = document.createElement("ul");
    ul.className = "enrich-miss-list";
    for (const row of rows) {
      const li = document.createElement("li");
      li.textContent = row.path;
      // A track short of more than one field is listed under each; say
      // so inline so the same path appearing three times reads as one
      // badly-tagged track rather than three separate problems.
      const others = (row.facets || []).filter((f) => f !== key);
      if (others.length) {
        const also = document.createElement("span");
        also.className = "hint";
        also.textContent = ` — also missing ${others.map((f) => ENRICH_MISS_FACET_NOUNS[f] || f).join(", ")}`;
        li.appendChild(also);
      }
      ul.appendChild(li);
    }
    details.appendChild(ul);
    body.appendChild(details);
  }

  renderEnrichSkipReasons(body, data.skipReasons);
}

// renderEnrichSkipReasons paints the enricher's process-lifetime tally of
// WHY it gave up. Absent on a bridge with no enricher wired, and absent
// when nothing has been skipped since start — in both cases the section
// is simply omitted rather than shown as zeros.
function renderEnrichSkipReasons(body, reasons) {
  const entries = Object.entries(reasons || {}).filter(([, n]) => n > 0);
  if (!entries.length) return;
  entries.sort((a, b) => b[1] - a[1]);

  const wrap = document.createElement("div");
  wrap.className = "enrich-miss-reasons";
  const title = document.createElement("div");
  title.className = "dist-title";
  title.textContent = "Why the enricher gave up";
  wrap.appendChild(title);

  const note = document.createElement("p");
  note.className = "hint";
  // Process-lifetime, not all-time: a restart zeroes it, and it counts
  // ATTEMPTS rather than tracks, so it deliberately doesn't reconcile
  // with the per-facet counts above.
  note.textContent = "Counted since this bridge started, per attempt — not directly comparable with the totals above.";
  wrap.appendChild(note);

  const dl = document.createElement("dl");
  dl.className = "composition";
  for (const [reason, n] of entries) {
    const dt = document.createElement("dt");
    dt.textContent = ENRICH_SKIP_REASON_LABELS[reason] || reason;
    const dd = document.createElement("dd");
    dd.textContent = String(n);
    dl.appendChild(dt);
    dl.appendChild(dd);
  }
  wrap.appendChild(dl);
  body.appendChild(wrap);
}

// formatEnrichEta turns the pending count + coarse server estimate into a
// human string. Guards zero-pending / non-finite FIRST so a stray empty SSE
// frame can never render "NaN seconds". Sub-2min → seconds; else minutes/hours,
// always tagged "(estimate)" so a slowly-draining queue never reads as frozen.
function formatEnrichEta(pending, etaSeconds) {
  if (!pending || pending <= 0) return "all caught up";
  const secs = Number(etaSeconds);
  if (!Number.isFinite(secs) || secs <= 0) return "processing…";
  if (secs < 120) return `processing… (~${Math.round(secs)}s remaining)`;
  const mins = secs / 60;
  if (mins < 90) return `~${Math.round(mins)} min remaining (estimate)`;
  return `~${(mins / 60).toFixed(1)} hours remaining (estimate)`;
}

// renderCoverageBar paints the two-segment matched-vs-missing bar (reusing the
// composition .dist-bar styles; colour comes from data-cov in CSS: matched =
// --ok, missing = --warn). Hidden until at least one track is enriched.
// Built via createElement/textContent (never innerHTML) — same XSS posture as
// renderDistBar, though these labels are static.
function renderCoverageBar(matched, missing) {
  const bar = document.getElementById("enrich-coverage-bar");
  const legend = document.getElementById("enrich-coverage-legend");
  const section = document.getElementById("enrich-coverage-section");
  if (!bar || !legend || !section) return;
  const total = matched + missing;
  section.hidden = total === 0;
  bar.textContent = "";
  legend.textContent = "";
  if (total === 0) return;
  const segs = [
    { label: "Matched", count: matched, cov: "matched" },
    { label: "Missing", count: missing, cov: "missing" },
  ];
  segs.forEach((seg) => {
    const pct = (seg.count / total) * 100;
    const span = document.createElement("span");
    span.className = "dist-seg";
    span.dataset.cov = seg.cov;
    span.style.width = pct.toFixed(2) + "%";
    span.title = `${seg.label}: ${seg.count} (${pct.toFixed(1)}%)`;
    bar.appendChild(span);

    const item = document.createElement("span");
    item.className = "dist-legend-item";
    item.dataset.cov = seg.cov;
    const swatch = document.createElement("i");
    swatch.className = "dist-swatch";
    item.appendChild(swatch);
    item.appendChild(document.createTextNode(`${seg.label} `));
    const b = document.createElement("b");
    b.textContent = String(seg.count);
    item.appendChild(b);
    legend.appendChild(item);
  });
}

// refreshBackups fetches /api/backups and renders the latest count +
// most-recent timestamp into the dashboard's Backups panel. Errors
// degrade gracefully — the panel just shows the placeholder dashes.
async function refreshBackups() {
  try {
    const data = await API.get("/api/backups");
    const list = data.backups || [];
    setText("backup-count", String(list.length));
    setText("backup-root", data.backupsRoot || "—");
    if (list.length > 0) {
      const latest = list[0]; // newest-first
      setText("backup-latest", `${latest.dirName} · ${latest.bridgeVersion}`);
    } else {
      setText("backup-latest", "no snapshots yet");
    }
  } catch {
    // Quietly leave the placeholders alone.
  }
}

// refreshCertInfo populates the "Expires" line under the TLS
// fingerprint panel (Settings → Networking) with the live cert's
// expiry. ≤7 days is rendered red, ≤30 days yellow, otherwise the
// plain count. Errors degrade silently — the panel just shows the
// placeholder dashes. Self-guards via `if (!cell) return` so calling
// it from a page that doesn't render the panel is a no-op.
async function refreshCertInfo() {
  const cell = document.getElementById("cert-expiry");
  if (!cell) return;
  try {
    const info = await API.get("/api/cert");
    if (!info?.notAfter) {
      cell.textContent = "—";
      return;
    }
    const when = new Date(info.notAfter);
    const days = info.daysUntilExpiry;
    let badge = "";
    if (days <= 7) badge = '<span class="badge danger">expiring soon</span> ';
    else if (days <= 30) badge = '<span class="badge running">expiring</span> ';
    cell.innerHTML = `${badge}${when.toLocaleDateString()} (${days} days)`;
  } catch {
    cell.textContent = "—";
  }
}

// renderTailscaleTile updates the Status / Node / Cert dl rows and the
// "iOS clients reach the bridge over Tailscale at <url>" hint. State
// machine matches the plan's 5-cell breakdown:
//
//   • CLIAvailable=false, lastError empty    → tile hidden (host has no Tailscale)
//   • CLIAvailable=false, lastError set      → tile shown with the recovery message
//                                              (e.g. tailscale.mode=disabled sentinel)
//   • magic-DNS empty                        → "MagicDNS not enabled"
//   • lastError set                          → "Cert error" + the LastError text
//   • cert present + fresh                   → "✓ HTTPS certs enabled"
//   • cert absent / expiry within 14 days    → "Detecting…" / "Minting"
//
// The CLIAvailable=false-but-lastError-set branch was added so the
// disabled-mode sentinel ("Tailscale integration disabled. To enable,
// set tailscale.mode...") reaches operators in the admin UI; the
// pre-fix gate `if (!s || !s.cliAvailable)` hid the panel
// unconditionally on cliAvailable=false, so the recovery message
// was never visible (Qodo on PR #148).
function renderTailscaleTile(s) {
  const panel = document.getElementById("tailscale-panel");
  if (!panel) return;
  // Public/autocert mode: the Tailscale auto-pilot doesn't apply at all
  // (the bridge serves an LE cert on its public domain, not a tailnet
  // magic-DNS cert). Hide the tile entirely so it can never render a
  // misleading "Disabled" badge on a VPS where Tailscale was never the
  // point.
  if (s?.publicMode) {
    panel.hidden = true;
    return;
  }
  if (!s || (!s.cliAvailable && !s.lastError)) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;

  const statusEl = document.getElementById("tailscale-status");
  const nodeEl = document.getElementById("tailscale-node");
  const certEl = document.getElementById("tailscale-cert");
  const noteEl = document.getElementById("tailscale-magicdns-url");

  // Status badge — pick state machine first cell that matches.
  // Disabled-mode sentinel takes precedence over the generic "Error"
  // branch so an intentionally-disabled bridge doesn't render a red
  // "Error" badge that would send operators chasing an imaginary
  // misconfiguration. Detected via the cliAvailable=false +
  // lastError-set combination, which is uniquely produced by
  // tailscaleAdminSource.Status() in disabled mode (cli/tsnet
  // success paths set cliAvailable=true; cli/tsnet failure paths
  // set cliAvailable=true with an error suffix).
  // The configured mode (s.mode) disambiguates two states that used to
  // collapse into an identical misleading "Disabled" badge:
  //   - mode === "disabled": the operator genuinely turned it off.
  //   - mode === "cli"/"tsnet" but !cliAvailable: the mode is set but the
  //     auto-pilot isn't running (tailscale CLI missing on this host, or
  //     a mode change in Settings that needs a restart to apply). The
  //     server's lastError now spells out which — show it, but don't call
  //     it "Disabled" (it isn't), which sent operators chasing the wrong fix.
  let badgeClass = "idle", badgeText = "Detecting…", suffix = "";
  if (s.mode === "disabled") {
    badgeClass = "idle";
    badgeText = "Disabled";
    if (s.lastError) suffix = ` <span class="hint">${escapeHTML(s.lastError)}</span>`;
  } else if (!s.cliAvailable && s.lastError) {
    badgeClass = "idle";
    badgeText = "Inactive";
    suffix = ` <span class="hint">${escapeHTML(s.lastError)}</span>`;
  } else if (s.lastError) {
    badgeClass = "danger";
    badgeText = "Error";
    suffix = ` <span class="hint">${escapeHTML(s.lastError)}</span>`;
  } else if (!s.magicDNSName) {
    badgeClass = "running";
    badgeText = "MagicDNS not enabled";
  } else if (s.certPresent) {
    badgeClass = "running";
    badgeText = "HTTPS certs enabled";
  } else {
    badgeClass = "idle";
    badgeText = "No cert yet";
  }
  if (statusEl) {
    statusEl.innerHTML = `<span class="badge ${badgeClass}">${escapeHTML(badgeText)}</span>${suffix}`;
  }

  if (nodeEl) {
    nodeEl.textContent = s.magicDNSName || s.nodeName || "—";
  }

  if (certEl) {
    if (!s.certPresent) {
      certEl.textContent = "—";
    } else {
      const when = new Date(s.certNotAfter);
      const now = new Date();
      const days = Math.max(0, Math.floor((when.getTime() - now.getTime()) / 86_400_000));
      const tooltip = s.certPath ? ` title="${escapeHTML(s.certPath)}"` : "";
      let badge = "";
      if (days <= 7) badge = '<span class="badge danger">expiring soon</span> ';
      else if (days <= 30) badge = '<span class="badge running">expiring</span> ';
      certEl.innerHTML = `${badge}<span${tooltip}>expires in ${days} day${days === 1 ? "" : "s"} (${when.toLocaleDateString()})</span>`;
    }
  }

  if (noteEl) {
    // Use the backend-composed MagicDNSURL so the link reflects the
    // actual `cfg.ListenAddress` port, not a hard-coded :7788
    // (CodeRabbit on PR #102 — operators on non-default listen ports
    // would otherwise see the wrong URL during a manual pair recovery).
    // Fall back to the bare hostname if the backend couldn't compose
    // the URL (e.g. listen port is `:0` in test mode).
    noteEl.textContent = s.magicDNSURL || s.magicDNSName || "—";
  }
}

// bindTailscaleRefreshButton wires the "Re-mint cert" button. Disables
// the button + flips it to "Minting…" while the request is in flight
// to absorb impatient double-clicks before they reach Let's Encrypt's
// per-domain rate limits. The auto-pilot also rate-limits server-side
// (30s window) — the disabled state is the UX layer of the same
// defence.
function bindTailscaleRefreshButton() {
  const btn = document.getElementById("tailscale-refresh");
  if (!btn) return;
  btn.addEventListener("click", async () => {
    const oldText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "Minting…";
    try {
      const s = await API.post("/api/tailscale/refresh-cert");
      renderTailscaleTile(s);
    } catch (err) {
      alert("Re-mint failed: " + (err?.message ?? "unknown error"));
    } finally {
      btn.textContent = oldText;
      btn.disabled = false;
    }
  });
}

// autocertPollTimer holds the active setInterval handle so a
// re-entrant `initSettings()` (SPA navigation re-fires init on
// every page change back to Settings) clears the previous one
// before starting the next. Without this, polling stacks across
// every Settings visit and the bridge's /api/autocert/status
// gets hammered with duplicate requests. CodeRabbit Major review
// on PR #293.
let autocertPollTimer = null;

// refreshAutocertTile fetches /api/autocert/status and repaints
// the autocert panel on the Settings page. Hides the panel when
// Domain is empty (autocert.enabled=false or no autocert closure
// wired) so loopback installs see zero clutter.
//
// State machine:
//   • empty Domain               → tile hidden
//   • LastError set              → "Error" badge + the LastError text
//   • cert absent (no NotAfter)  → "Minting…" / "No cert yet"
//   • cert present + fresh       → "✓ HTTPS cert active" + days remaining
//   • cert present + expiring    → "expiring soon" badge alongside the days
//
// Only the Settings page hosts the autocert panel, so this is a
// noop on other pages (the getElementById returns null).
async function refreshAutocertTile() {
  const panel = document.getElementById("autocert-panel");
  if (!panel) return;
  let snap;
  try {
    snap = await API.get("/api/autocert/status");
  } catch {
    panel.hidden = true;
    return;
  }
  if (!snap || !snap.domain) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;

  const statusEl = document.getElementById("autocert-status");
  const domainEl = document.getElementById("autocert-domain");
  const expiryEl = document.getElementById("autocert-expiry");

  let badgeClass = "idle", badgeText = "Detecting…", suffix = "";
  if (snap.lastError) {
    badgeClass = "danger";
    badgeText = "Error";
    suffix = ` <span class="hint">${escapeHTML(snap.lastError)}</span>`;
  } else if (snap.certPresent) {
    badgeClass = "running";
    badgeText = "HTTPS cert active";
  } else {
    // badgeClass stays "idle" (the default) — no need to re-assign (S4165).
    badgeText = "Minting…";
  }
  if (statusEl) {
    statusEl.innerHTML = `<span class="badge ${badgeClass}">${escapeHTML(badgeText)}</span>${suffix}`;
  }
  if (domainEl) domainEl.textContent = snap.domain || "—";
  if (expiryEl) {
    if (!snap.certPresent || !snap.notAfter) {
      expiryEl.textContent = "—";
    } else {
      const when = new Date(snap.notAfter);
      const now = new Date();
      const days = Math.max(0, Math.floor((when.getTime() - now.getTime()) / 86_400_000));
      let badge = "";
      if (days <= 7) badge = '<span class="badge danger">expiring soon</span> ';
      else if (days <= 30) badge = '<span class="badge running">expiring</span> ';
      expiryEl.innerHTML = `${badge}expires in ${days} day${days === 1 ? "" : "s"} (${when.toLocaleDateString()})`;
    }
  }
}

// bindInstallButton attaches the click handler to the Install &
// restart button. No-op when btn is null (server didn't render the
// button at first paint, e.g. the platform doesn't support install
// or no update was known). The renderUpdateTile path uses the same
// helper when it dynamically materialises the button mid-session.
function bindInstallButton(btn) {
  if (!btn) return;
  btn.addEventListener("click", async () => {
    const supervised = btn.dataset.supervised === "true";
    const prompt = supervised
      ? "Install the new bridge release and restart?\n\nActive iOS downloads will be interrupted and will need to be retried."
      : "Install the new bridge release and stop the bridge?\n\nThis bridge isn't running under a service manager, so it won't auto-restart — you'll need to start it manually after the install. Active iOS downloads will be interrupted and will need to be retried.";
    if (!confirm(prompt)) return;
    await runInstall(btn, false);
  });
}

// runInstall hits POST /api/updates/install (with ?force=1 if the
// user opted past the active-downloads guard) and then POSTs
// /api/restart. The two requests are kept distinct so the install
// vs. restart failure modes stay distinguishable in the UI; the
// server-side handlers are also separate (see admin.go routing) so
// the `force` semantics only affect install, not restart. Recursive
// retry-with-force keeps the call site clean.
async function runInstall(btn, force) {
  const supervised = btn.dataset.supervised === "true";
  const oldText = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Installing…";
  try {
    const path = force ? "/api/updates/install?force=1" : "/api/updates/install";
    await API.post(path);
    btn.textContent = supervised ? "Restarting…" : "Stopping…";
    // AWAIT the restart now, where the old code deliberately did not.
    // The handler drains in-flight streams first, so the response marks
    // the moment the exit is actually imminent — starting the 2.5 s
    // reload timer before that would land the operator on a page served
    // by the OLD process, which then restarts under them.
    //
    // Failures are swallowed on purpose: the connection dropping mid-read
    // is the expected shape of a server on its way out, and the reload
    // below recovers either way.
    await fetch("/api/restart", {
      method: "POST",
      headers: { "content-type": "application/json" },
    }).catch(() => {});
    if (supervised) {
      setTimeout(() => window.location.reload(), 2500);
    } else {
      btn.textContent = "Stopped — start manually";
      // Bail before the finally-style cleanup; the bridge is on
      // its way out and the operator now needs to restart it
      // from a shell.
    }
  } catch (err) {
    if (/409/.test(err.message) || /active-sessions/.test(err.message) || /active downloads/i.test(err.message)) {
      const proceed = confirm("Active downloads are in flight — installing now will interrupt them and could glitch any iOS device currently playing a track.\n\nInstall anyway?");
      if (proceed) {
        return runInstall(btn, true);
      }
    } else {
      alert("Install failed: " + err.message);
    }
    btn.textContent = oldText;
    btn.disabled = false;
  }
}

// renderUpdateTile mutates the dashboard's Updates panel from a Status
// payload. Tolerates partial input (Check-now error path passes only
// {lastError}). Mirrors the server-rendered first paint in
// templates/dashboard.html.
//
// The release-notes anchor is created fresh on each render rather than
// preserved from the prior tree — `status.innerHTML = ...` wipes the
// node, so caching a reference to it is a use-after-detach bug (PR #41
// CodeRabbit review): once the tile transitions through "up to date"
// or "check failed", a subsequent "update available" response could no
// longer surface the link.
function renderUpdateTile(u) {
  // Jobs-page status line (guarded — the dashboard tile below has its
  // own richer rendering).
  if (u && document.getElementById("job-upd-status")) {
    setText("job-upd-status", updateStatusLine(u));
  }
  const status = document.getElementById("update-status");
  const lastCheck = document.getElementById("update-last-check");
  const lastError = document.getElementById("update-last-error");
  const latest = document.getElementById("update-latest");
  if (!status) return;

  // Add or remove the "Install & restart" button to match the
  // server-rendered first-paint logic. The button only exists when
  // the platform supports self-install AND an update is available.
  // Without this, a "checking…" → "update available" transition
  // mid-session would leave a paired client without a button.
  // Roll back is revealed only when a previous binary is actually on
  // disk. It is deliberately NOT gated on updateAvailable: the case it
  // exists for is "the version I just installed is broken", where by
  // definition there is no newer release to offer.
  const rollbackBtn = document.getElementById("update-rollback");
  if (rollbackBtn) rollbackBtn.hidden = !u?.canRollback;

  const actions = document.querySelector(".panel-head .panel-actions");
  let installBtn = document.getElementById("update-install");
  if (actions) {
    const should = u?.updateAvailable && u?.canInstall;
    if (should && !installBtn) {
      // Mirror the supervision-aware label + dataset attribute the
      // server-rendered first-paint path emits — without this the
      // dynamic-create branch would always render "Install &
      // restart" + supervised=undefined, defeating the supervision
      // gate when the operator was on the dashboard during a
      // mid-session "checking…" → "update available" transition.
      // The parent `.panel-actions` carries the flag at first
      // paint so we can read it here without re-fetching from
      // /api/settings. (Qodo on PR #124.)
      const supervised = actions.dataset.supervised === "true";
      installBtn = document.createElement("button");
      installBtn.type = "button";
      installBtn.id = "update-install";
      installBtn.className = "btn btn-primary";
      installBtn.dataset.supervised = supervised ? "true" : "false";
      installBtn.textContent = supervised ? "Install & restart" : "Install & stop";
      bindInstallButton(installBtn);
      actions.insertBefore(installBtn, actions.firstChild);
    } else if (!should && installBtn) {
      installBtn.remove();
    }
  }

  if (u?.updateAvailable && u?.latestVersion) {
    status.innerHTML = `<span class="badge running">update available</span><span>· <code>${escapeHTML(u.latestVersion)}</code></span>`;
    if (u.releaseNotesURL) {
      const notes = document.createElement("a");
      notes.id = "update-notes";
      notes.href = u.releaseNotesURL;
      notes.target = "_blank";
      notes.rel = "noopener";
      notes.textContent = "release notes";
      status.appendChild(document.createTextNode(" "));
      status.appendChild(notes);
    }
  } else if (u?.latestVersion) {
    status.innerHTML = `<span class="badge idle">up to date</span><span>· latest <code>${escapeHTML(u.latestVersion)}</code></span>`;
  } else if (u?.lastError) {
    status.innerHTML = `<span class="badge idle">check failed</span>`;
  } else if (u && isUpdateLastCheckZero(u)) {
    // Distinguish "no check has fired yet" from "check is in
    // flight". Pre-fix the operator saw "checking…" for hours
    // because the default poll interval is 6h and no first check
    // had run after a fresh bridge start. (User feedback on PR
    // #129.)
    status.innerHTML = `<span class="badge idle">scheduled</span>`;
  } else {
    status.innerHTML = `<span class="badge idle">checking…</span>`;
  }

  if (lastCheck && u) {
    if (isUpdateLastCheckZero(u)) {
      // Server marshals Go's `time.Time{}` zero value as
      // `0001-01-01T00:00:00Z` over the wire. The prior
      // truthy-string check missed it and `formatTimeAgo` cheerfully
      // rendered "739736d ago". Render "never" instead.
      lastCheck.textContent = "never";
    } else if (u.lastCheck) {
      lastCheck.textContent = formatTimeAgo(new Date(u.lastCheck));
    }
  }
  if (lastError) {
    if (u?.lastError) {
      lastError.innerHTML = `<code>${escapeHTML(u.lastError)}</code>`;
      lastError.hidden = false;
      const dt = lastError.previousElementSibling;
      if (dt) dt.hidden = false;
    } else {
      lastError.hidden = true;
      lastError.innerHTML = "";
      const dt = lastError.previousElementSibling;
      if (dt) dt.hidden = true;
    }
  }
  if (latest && u?.latestVersion) {
    latest.textContent = u.latestVersion;
    latest.hidden = false;
  }

  // DeferredReason: the auto-installer's gate refused this cycle
  // (currently MinClientVersion compat). Surface as a yellow
  // "deferred" badge so the operator can see why an available
  // update isn't installing automatically.
  const deferred = document.getElementById("update-deferred");
  if (deferred) {
    if (u?.deferredReason) {
      deferred.innerHTML = `<span class="badge running">deferred</span> ${escapeHTML(u.deferredReason)}`;
      deferred.hidden = false;
      const dt = deferred.previousElementSibling;
      if (dt) dt.hidden = false;
    } else {
      deferred.hidden = true;
      deferred.innerHTML = "";
      const dt = deferred.previousElementSibling;
      if (dt) dt.hidden = true;
    }
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function formatTimeAgo(d) {
  const sec = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
  return `${Math.floor(sec / 86400)}d ago`;
}

function setText(id, v) {
  const el = document.getElementById(id);
  if (el) el.textContent = String(v);
}

// --- library ---

// --- Sidebar space meter ----------------------------------------------------
//
// Three numbers, because "free" alone cannot answer the question a quota user
// has. Trash does not free space until it is purged, so the reclaimable figure
// sits next to the free one — otherwise an operator at the ceiling is told they
// are stuck when they are one click from not being.
//
// Progressive: the widget stays hidden unless a floor or quota is configured,
// or free space is already inside twice the floor.
async function refreshSpaceMeter() {
  const el = document.getElementById("space-meter");
  if (!el) return;
  let sp;
  try {
    sp = await API.get("/api/library/space");
  } catch {
    return; // a missing number degrades the widget, it does not break the page
  }
  if (!sp || !sp.probed) return;
  const near = sp.minFreeBytes > 0 && sp.freeBytes < sp.minFreeBytes * 2;
  if (!sp.configured && !near) return;

  el.hidden = false;
  el.classList.toggle("low", sp.minFreeBytes > 0 && sp.freeBytes <= sp.minFreeBytes);

  const used = sp.totalBytes > 0 ? sp.totalBytes - sp.freeBytes : sp.libraryBytes;
  const denom = sp.totalBytes > 0 ? sp.totalBytes : used + sp.freeBytes;
  const fill = document.getElementById("space-meter-fill");
  if (fill && denom > 0) fill.style.width = `${Math.min(100, Math.round((used / denom) * 100))}%`;

  const text = document.getElementById("space-meter-text");
  if (!text) return;
  text.replaceChildren();
  text.appendChild(document.createTextNode(`${formatBytes(sp.freeBytes)} free`));
  if (sp.reclaimableBytes > 0) {
    text.appendChild(document.createTextNode(" · "));
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "space-meter-reclaim";
    btn.textContent = `${formatBytes(sp.reclaimableBytes)} in trash`;
    btn.addEventListener("click", () => { location.href = "/library"; });
    text.appendChild(btn);
  }
}

// --- Upload -----------------------------------------------------------------
//
// The console's file/folder upload. Chunked PUTs against /api/upload/*, which
// is why it can resume and why a 200 MB file is not at the mercy of the admin
// server's ReadTimeout.
//
// Two behaviours here are deliberate rather than incidental:
//
//   * The destination PREVIEW is shown before anything transfers.
//     webkitRelativePath includes the folder you picked, so selecting an artist
//     folder nests one level deeper than selecting one of its albums — and
//     because those land at DIFFERENT paths, nothing collides and you end up
//     with two copies of the same album on disk. Showing the resolved path is
//     the only place that is preventable.
//
//   * Files the server flags as already present are DESELECTED, not removed.
//     The pre-flight warns; it does not decide. A track legitimately on both an
//     album and a compilation is a real library.

const UPLOAD_PARALLEL_FILES = 3;

let uploadState = null;

function initUpload() {
  const panel = document.getElementById("upload-panel");
  if (!panel) return;
  const signal = pageSignal();

  // The panel is hidden until the operator has opted in. An operator who has
  // not enabled uploads should see no upload chrome at all.
  API.get("/api/settings")
    .then((cfg) => {
      if (!cfg || !cfg.uploadEnabled) return;
      panel.hidden = false;
      wireUpload(signal);
    })
    .catch(() => {});
}

function wireUpload(signal) {
  const drop = document.getElementById("upload-drop");
  const filesInput = document.getElementById("upload-files");
  const folderInput = document.getElementById("upload-folder");

  API.get("/api/library/space")
    .then((sp) => setText("upload-root", sp && sp.root ? sp.root : "your library"))
    .catch(() => {});

  filesInput?.addEventListener("change", () => stageFiles(collectFromInput(filesInput)));
  folderInput?.addEventListener("change", () => stageFiles(collectFromInput(folderInput)));

  for (const evt of ["dragenter", "dragover"]) {
    drop?.addEventListener(evt, (e) => {
      e.preventDefault();
      drop.classList.add("dragging");
    });
  }
  for (const evt of ["dragleave", "drop"]) {
    drop?.addEventListener(evt, () => drop.classList.remove("dragging"));
  }
  drop?.addEventListener("drop", async (e) => {
    e.preventDefault();
    const picked = await collectFromDataTransfer(e.dataTransfer);
    stageFiles(picked);
  });
  drop?.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      filesInput?.click();
    }
  });

  document.getElementById("upload-cancel")?.addEventListener("click", resetUpload);
  document.getElementById("upload-start")?.addEventListener("click", startUpload);
  document.getElementById("upload-abort")?.addEventListener("click", () => {
    if (uploadState) uploadState.aborted = true;
  });

  // An in-flight upload must not survive a boosted navigation away.
  signal?.addEventListener("abort", () => {
    if (uploadState) uploadState.aborted = true;
  });
}

function collectFromInput(input) {
  return Array.from(input.files || []).map((f) => ({
    file: f,
    path: f.webkitRelativePath || f.name,
  }));
}

// collectFromDataTransfer walks a dropped folder via webkitGetAsEntry, which is
// the only cross-browser way to get a directory out of a drop (the File System
// Access API's showDirectoryPicker is Chromium-only).
async function collectFromDataTransfer(dt) {
  const out = [];
  const roots = [];
  for (const item of Array.from(dt?.items || [])) {
    const entry = item.webkitGetAsEntry?.();
    if (entry) roots.push(entry);
  }
  if (roots.length === 0) {
    return Array.from(dt?.files || []).map((f) => ({ file: f, path: f.name }));
  }
  const walk = (entry, prefix) =>
    new Promise((resolve) => {
      if (entry.isFile) {
        entry.file(
          (f) => {
            out.push({ file: f, path: prefix + entry.name });
            resolve();
          },
          () => resolve(),
        );
        return;
      }
      const reader = entry.createReader();
      const batch = () =>
        reader.readEntries(async (entries) => {
          if (!entries.length) return resolve();
          await Promise.all(entries.map((e) => walk(e, prefix + entry.name + "/")));
          batch(); // readEntries returns at most 100 per call
        }, () => resolve());
      batch();
    });
  await Promise.all(roots.map((r) => walk(r, "")));
  return out;
}

function stageFiles(picked) {
  const usable = picked.filter((p) => p.file && p.file.size >= 0);
  if (!usable.length) return;
  uploadState = { picked: usable, session: null, aborted: false, preflightSkipped: 0 };
  const note = document.getElementById("upload-dupe-note");
  if (note) note.hidden = true;
  renderUploadReview();
}

function renderUploadReview() {
  const review = document.getElementById("upload-review");
  const list = document.getElementById("upload-preview");
  if (!review || !list || !uploadState) return;

  document.getElementById("upload-progress").hidden = true;
  document.getElementById("upload-result").hidden = true;
  document.getElementById("upload-error").hidden = true;
  review.hidden = false;
  list.replaceChildren();

  const picked = uploadState.picked;
  const total = picked.reduce((n, p) => n + p.file.size, 0);
  setText("upload-count", String(picked.length));
  setText("upload-count-plural", picked.length === 1 ? "" : "s");
  setText("upload-total", formatBytes(total));

  // Preview the RESOLVED destination for a bounded sample — the full list can
  // be thousands of rows and the point is the shape, not the enumeration.
  const shown = picked.slice(0, 8);
  for (const p of shown) {
    const li = document.createElement("li");
    li.textContent = p.path;
    list.appendChild(li);
  }
  if (picked.length > shown.length) {
    const li = document.createElement("li");
    li.className = "muted";
    li.textContent = `…and ${picked.length - shown.length} more`;
    list.appendChild(li);
  }
}

async function startUpload() {
  if (!uploadState) return;
  const err = document.getElementById("upload-error");
  const start = document.getElementById("upload-start");
  err.hidden = true;
  start.disabled = true;
  try {
    const overwrite = document.getElementById("upload-overwrite")?.checked === true;
    let session = await createUploadSession(uploadState.picked, overwrite);

    // The pre-flight WARNS. Anything the server recognises is dropped from the
    // set by default, and the operator is told what happened and can re-run
    // with overwrite if that is what they meant.
    const dupes = session.files.filter((f) => f.duplicateOf);
    if (dupes.length && !overwrite) {
      const keep = uploadState.picked.filter((p) => {
        const f = session.files.find((x) => x.path === p.path);
        return !(f && f.duplicateOf);
      });
      const note = document.getElementById("upload-dupe-note");
      note.hidden = false;
      const one = dupes.length === 1;
      note.textContent =
        `${dupes.length} file${one ? "" : "s"} already ${one ? "exists" : "exist"} in your ` +
        `library (for example ${dupes[0].duplicateOf}) and will be skipped. ` +
        `Tick "Replace files that already exist" if you meant to overwrite them.`;
      await API.delete(`/api/upload/sessions/${encodeURIComponent(session.id)}`);
      if (!keep.length) {
        document.getElementById("upload-review").hidden = true;
        showUploadResult("Nothing to upload — every file is already in the library.");
        return;
      }
      uploadState.preflightSkipped = dupes.length;
      uploadState.picked = keep;
      session = await createUploadSession(keep, overwrite);
    }

    uploadState.session = session;
    document.getElementById("upload-review").hidden = true;
    document.getElementById("upload-progress").hidden = false;
    await transferAll(session);
    if (uploadState.aborted) {
      showUploadResult("Upload stopped. Staged progress is kept — start again to resume.");
      return;
    }
    const res = await API.post(`/api/upload/sessions/${encodeURIComponent(session.id)}/commit`);
    reportCommit(res);
  } catch (e) {
    err.hidden = false;
    err.textContent = e && e.message ? e.message : String(e);
  } finally {
    start.disabled = false;
    document.getElementById("upload-progress").hidden = true;
  }
}

async function createUploadSession(picked, overwrite) {
  return API.post("/api/upload/sessions", {
    overwrite,
    files: picked.map((p) => ({ path: p.path, size: p.file.size })),
  });
}

async function transferAll(session) {
  const byPath = new Map(uploadState.picked.map((p) => [p.path, p.file]));
  const queue = session.files.filter((f) => !f.complete);
  const totalBytes = session.files.reduce((n, f) => n + f.size, 0) || 1;
  let doneBytes = session.files.reduce((n, f) => n + f.offset, 0);

  const bar = document.getElementById("upload-bar");
  const paint = () => {
    bar.value = Math.min(100, Math.round((doneBytes / totalBytes) * 100));
    setText("upload-status", `${formatBytes(doneBytes)} of ${formatBytes(totalBytes)}`);
  };
  paint();

  let next = 0;
  const worker = async () => {
    while (!uploadState.aborted) {
      const i = next++;
      if (i >= queue.length) return;
      const meta = queue[i];
      const file = byPath.get(meta.path);
      if (!file) continue;
      let offset = meta.offset;
      while (offset < meta.size && !uploadState.aborted) {
        const end = Math.min(offset + session.chunkBytes, meta.size);
        const res = await putUploadChunk(session.id, meta.id, offset, file.slice(offset, end));
        doneBytes += res.offset - offset;
        offset = res.offset;
        paint();
      }
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(UPLOAD_PARALLEL_FILES, queue.length) }, worker),
  );
}

async function putUploadChunk(sid, fid, offset, blob) {
  const url =
    `/api/upload/sessions/${encodeURIComponent(sid)}/files/${encodeURIComponent(fid)}` +
    `?offset=${offset}`;
  const r = await fetch(url, {
    method: "PUT",
    // octet-stream is what csrfGuard's narrow allowlist admits, and it is
    // preflight-forced, unlike a multipart form.
    headers: { "content-type": "application/octet-stream" },
    body: blob,
  });
  if (r.status === 409) {
    // The server holds a different offset. It tells us which, so resume
    // rather than guessing.
    const body = await r.json().catch(() => ({}));
    if (typeof body.offset === "number") return { offset: body.offset };
  }
  if (!r.ok) throw await errorFromResponse(r);
  return r.json();
}

function reportCommit(res) {
  const parts = [];
  if (res.committed) parts.push(`${res.committed} added`);
  // Files the PRE-FLIGHT dropped never reached the session, so the server
  // cannot count them. Reporting only the server's number would leave the
  // operator staring at "1 added" after picking three files.
  const preflight = uploadState?.preflightSkipped || 0;
  if (preflight) parts.push(`${preflight} already in your library`);
  if (res.skipped) parts.push(`${res.skipped} skipped (already present)`);
  if (res.failed) parts.push(`${res.failed} failed`);
  let msg = parts.length ? parts.join(" · ") : "Nothing to do.";
  if (res.committed) {
    msg += res.fullScan
      ? " — rescanning the library."
      : ` — rescanning ${res.scanDirs.length} folder${res.scanDirs.length === 1 ? "" : "s"}.`;
  }
  const failed = (res.outcomes || []).filter((o) => o.status === "failed");
  if (failed.length) msg += ` First failure: ${failed[0].path} (${failed[0].reason}).`;
  showUploadResult(msg);
  resetUpload();
}

function showUploadResult(msg) {
  const el = document.getElementById("upload-result");
  el.hidden = false;
  el.textContent = msg;
}

function resetUpload() {
  uploadState = null;
  document.getElementById("upload-review").hidden = true;
  document.getElementById("upload-progress").hidden = true;
  // The duplicate note is deliberately NOT cleared here. resetUpload runs
  // immediately after a commit, and the note is the explanation for the
  // number the operator is about to read — hiding it leaves "1 added" with
  // no account of where the other two went. It is cleared when a NEW
  // selection is staged, which is the moment it goes stale.
  const f = document.getElementById("upload-files");
  const d = document.getElementById("upload-folder");
  if (f) f.value = "";
  if (d) d.value = "";
}

function initLibrary() {
  initUpload();
  initVariantsPanel();
  const form = document.getElementById("add-root-form");
  if (form) {
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const path = form.path.value.trim();
      if (!path) return;
      try {
        await API.post("/api/roots", { path });
        window.location.reload();
      } catch (err) {
        alert("Add failed: " + err.message);
      }
    });
  }
  document.querySelectorAll(".remove-root").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const tr = btn.closest("tr");
      const path = tr.dataset.path;
      if (!confirm(`Remove ${path}?\nTracks under it will vanish from the iOS manifest.`)) return;
      try {
        await API.delete("/api/roots", { path });
        tr.remove();
      } catch (err) {
        alert("Remove failed: " + err.message);
      }
    });
  });
}

// --- networking telemetry (Settings page) ---

// applyEndpoints renders the "Reachable endpoints" panel on the
// Settings page (Networking section) from a parsed endpoints array.
// Called by the SSE `endpoints` event listener — the bridge recomputes
// the list per-call from net.Interfaces() at the SSE handler's 5 s
// medium-tier cadence, so a Tailscale tunnel coming up mid-session
// reflects without operator intervention. Self-guards via
// `if (!list) return` so calling it from a page that doesn't render
// the panel is a no-op.
function applyEndpoints(entries) {
  const list = document.getElementById("endpoints-list");
  if (!list) return;
  if (!Array.isArray(entries) || entries.length === 0) {
    // Real-world: only happens when the bridge is binding to a
    // loopback-only address. The /v1/health endpoints array would
    // also be empty in that case — paired devices treat the listen
    // address directly as the only known URL.
    list.innerHTML = `<li class="endpoints-empty"><em>No external addresses detected. Devices will use the address you provided when pairing.</em></li>`;
    return;
  }
  list.innerHTML = entries
    .map((e) => {
      const cls = String(e.class || "");
      const url = String(e.url || "");
      // Group LAN/Tailscale/mDNS/Public via a class-tag. CSS in
      // app.css colors each group distinctly so the operator can
      // skim by interface type.
      const tagClass = "endpoint-tag " + cls.toLowerCase().replace(/\s+/g, "-");
      return `
        <li class="endpoint-row">
          <span class="${tagClass}">${escapeHTML(cls)}</span>
          <code class="endpoint-url">${escapeHTML(url)}</code>
        </li>`;
    })
    .join("");
}

// renderPendingPairing fetches /api/pairing and paints the
// "Pending join requests" panel on the devices page. Optimistic UI:
// when the operator taps Approve / Decline the JS flips the card to a
// transient state immediately, then the next 3 s poll either confirms
// (card transitions to "approved · awaiting device acknowledgment" or
// to "declined") or the row disappears server-side and the card is
// removed by this renderer. No manual refresh required.
//
// pendingActionLatch tracks rows the operator has just acted on so a
// brief race between optimistic-flip and the next poll can't
// resurrect the prior "pending" badge while the server is still
// computing the transition.
const pendingActionLatch = new Map();

// Cross-page pending-pairing count tracking. Drives the global
// header-bar pairing badge, including the pulse-on-increase animation
// so an operator on Settings / Library / Dashboard sees a new request
// arrive (not just operators who happen to be on Devices when it
// lands). The badge listener is wired in layout.html — it appears on
// every admin page.
//
// Initialised to `null` so the FIRST snapshot is treated as a baseline
// rather than a pulse trigger. Without this, opening a fresh tab while
// a pending request is already in flight would misleadingly pulse the
// badge (the prior frame the operator never saw was 0 → the current
// non-zero value). After the first snapshot lands, we switch to the
// increase-only comparison (CodeRabbit on PR #161).
let lastPendingCount = null;

// applyPairing renders the "Pending join requests" panel from a
// parsed entries array. Called by the SSE `pairing` event listener
// (initial snapshot + ~1 s cadence while a request is in flight,
// because pendingPairingRow.SecondsUntilExpiry decrements every
// second and naturally streams the countdown over the wire). Also
// called directly by handlePairingAction after a tap so the
// optimistic re-render lands without waiting for the next SSE frame.
function applyPairing(entries) {
  // Update the global header badge first — it's present on every
  // admin page, whereas the pending-pairing-panel only exists on
  // Devices. Operators on other pages still need to see new requests
  // arrive.
  updatePairingBadge(entries);

  const panel = document.getElementById("pending-pairing-panel");
  const list = document.getElementById("pending-pairing-list");
  if (!panel || !list) return;
  if (!Array.isArray(entries) || entries.length === 0) {
    // Empty-snapshot teardown: the bridge was just restarted (in-
    // memory pairing store cleared) or every request resolved or
    // timed out. Drop the latch so a fresh request landing in the
    // same `id` slot doesn't inherit stale optimistic state.
    pendingActionLatch.clear();
    panel.hidden = true;
    list.innerHTML = "";
    return;
  }
  panel.hidden = false;
  // Drop latch entries that the server no longer reports — once the
  // row is gone the optimistic flip has served its purpose.
  const live = new Set(entries.map((e) => e.id));
  for (const id of pendingActionLatch.keys()) {
    if (!live.has(id)) pendingActionLatch.delete(id);
  }
  list.innerHTML = entries.map((e) => renderPendingPairingCard(e)).join("");

  // Wire approve / decline buttons every render — innerHTML wiped any
  // prior listeners.
  list.querySelectorAll("[data-pairing-approve]").forEach((btn) => {
    btn.addEventListener("click", () => handlePairingAction(btn, "approve"));
  });
  list.querySelectorAll("[data-pairing-decline]").forEach((btn) => {
    btn.addEventListener("click", () => handlePairingAction(btn, "decline"));
  });
}

// updatePairingBadge — drives the global header pairing badge. Called
// from applyPairing (which itself is called by the SSE `pairing`
// listener on every admin page). Counts only `pending` entries
// because approved/declined/expired states are operator-actioned
// (they shouldn't pulse the operator's attention; the cards on the
// Devices page show the post-action status). Pulse animation fires
// when the count INCREASES — a new request arrived — but NOT when
// the count drops (operator just approved one). prefers-reduced-
// motion strips the pulse and shows a static brighter background
// instead.
function updatePairingBadge(entries) {
  const badge = document.getElementById("pairing-badge");
  if (!badge) return;
  const pendingCount = Array.isArray(entries)
    ? entries.filter((e) => String(e.status || "pending") === "pending").length
    : 0;
  const countEl = badge.querySelector(".pairing-badge-count");
  if (pendingCount === 0) {
    badge.hidden = true;
    if (countEl) {
      countEl.textContent = "0";
      countEl.dataset.count = "0";
    }
  } else {
    badge.hidden = false;
    if (countEl) {
      countEl.textContent = String(pendingCount);
      countEl.dataset.count = String(pendingCount);
    }
    // Increase-only pulse — operator just got a NEW request, deserves
    // attention. Decrease (operator approved one) doesn't re-fire.
    // First snapshot (`lastPendingCount === null`) is the baseline; we
    // skip the pulse and just establish the count, otherwise opening
    // a fresh admin tab while a request is already pending would
    // falsely fire the pulse for an event the operator never missed.
    if (lastPendingCount !== null && pendingCount > lastPendingCount) {
      badge.classList.remove("pairing-badge-pulse");
      // Force reflow so re-adding the class restarts the animation.
      void badge.offsetWidth;
      badge.classList.add("pairing-badge-pulse");
    }
  }
  lastPendingCount = pendingCount;
}

// renderPendingPairing fetches once and applies. Used by
// handlePairingAction to flip the card immediately after Approve /
// Decline rather than waiting for the next SSE frame (the typical
// frame lands within ~500 ms, but the operator's tap deserves
// instant visual feedback).
async function renderPendingPairing() {
  const panel = document.getElementById("pending-pairing-panel");
  const list = document.getElementById("pending-pairing-list");
  if (!panel || !list) return;
  try {
    applyPairing(await API.get("/api/pairing"));
  } catch (e) {
    panel.hidden = false;
    list.innerHTML = `<p class="pairing-error"><em>Couldn't load pending requests: ${escapeHTML(e.message)}</em></p>`;
  }
}

function renderPendingPairingCard(e) {
  const status = String(e.status || "pending");
  const latched = pendingActionLatch.get(e.id);
  const effective = latched || status;
  const code = String(e.verificationCode || "").replace(/(\d{3})(\d{3})/, "$1 $2");
  const sourceLine = [
    e.sourceIP ? `from <code>${escapeHTML(e.sourceIP)}</code>` : "",
    e.clientVersion ? `client ${escapeHTML(e.clientVersion)}` : "",
  ].filter(Boolean).join(" · ");
  let badge = "";
  let actions = "";
  switch (effective) {
    case "pending":
      badge = `<span class="badge running">expires in ${formatPairingCountdown(e.secondsUntilExpiry)}</span>`;
      actions = `
        <div class="pairing-actions">
          <button type="button" class="btn" data-pairing-decline data-id="${escapeHTML(e.id)}">Decline</button>
          <button type="button" class="btn primary" data-pairing-approve data-id="${escapeHTML(e.id)}">Approve</button>
        </div>`;
      break;
    case "approved":
      badge = `<span class="badge idle">approved · awaiting device acknowledgment</span>`;
      break;
    case "declined":
      badge = `<span class="badge danger">declined</span>`;
      break;
    case "expired":
      badge = `<span class="badge danger">expired</span>`;
      break;
    case "cert_rotated":
      badge = `<span class="badge danger">cert rotated · device must request again</span>`;
      break;
    default:
      badge = `<span class="badge">${escapeHTML(effective)}</span>`;
  }
  return `
    <div class="pairing-card pairing-${effective}">
      <div class="pairing-card-head">
        <div class="pairing-name">"${escapeHTML(e.deviceName || "(unnamed)")}"</div>
        ${badge}
      </div>
      <div class="pairing-code">
        <span class="pairing-code-label">Verification code</span>
        <code class="pairing-code-value">${escapeHTML(code)}</code>
      </div>
      <div class="pairing-meta">
        ${sourceLine || ""}
        ${e.fingerprintSuffix ? ` · TLS fingerprint suffix <code>…${escapeHTML(e.fingerprintSuffix)}</code>` : ""}
      </div>
      <p class="pairing-warn">
        Device name is supplied by the client — verify the code on the device screen before approving.
      </p>
      ${actions}
    </div>`;
}

function formatPairingCountdown(sec) {
  const n = Math.max(0, Number(sec) || 0);
  const m = Math.floor(n / 60);
  const s = n % 60;
  return `${m}:${String(s).padStart(2, "0")}`;
}

async function handlePairingAction(btn, action) {
  const id = btn.dataset.id;
  if (!id) return;
  // Disable both buttons in the card while the call is in flight to
  // prevent double-tap submitting both actions.
  const card = btn.closest(".pairing-card");
  card?.querySelectorAll("button").forEach((b) => (b.disabled = true));
  // Latch the optimistic state so the next render doesn't briefly
  // flip back to "pending" before the server has processed.
  pendingActionLatch.set(id, action === "approve" ? "approved" : "declined");
  try {
    await API.post(`/api/pairing/${encodeURIComponent(id)}/${action}`);
  } catch (err) {
    // Server rejected (cert rotated mid-tap, race lost to expire) —
    // drop the latch so the next poll surfaces the actual state.
    pendingActionLatch.delete(id);
    alert(`${action} failed: ${err.message}`);
  }
  // Force an immediate re-render so the card flips without waiting
  // for the next 3 s tick.
  renderPendingPairing();
}

function initDevices() {
  const modal = document.getElementById("pair-modal");
  const openBtn = document.getElementById("pair-open");
  const form = document.getElementById("pair-form");
  const stepForm = document.getElementById("pair-step-form");
  const stepResult = document.getElementById("pair-step-result");

  // Pending pairing requests are hydrated by the SSE stream wired at
  // the bottom of this file. The initial snapshot lands within ms of
  // EventSource connect; thereafter, frames arrive on state change
  // and on the per-second SecondsUntilExpiry countdown while requests
  // are pending. handlePairingAction still calls renderPendingPairing
  // for instant post-tap visual feedback.

  if (openBtn) {
    openBtn.addEventListener("click", () => {
      stepForm.hidden = false;
      stepResult.hidden = true;
      form.reset();
      // reset defaults from input attributes after reset()
      const urlInput = form.querySelector("input[name=url]");
      if (urlInput) urlInput.value = urlInput.defaultValue;
      modal.showModal();
    });
  }
  modal?.querySelectorAll("[data-close]").forEach((b) =>
    b.addEventListener("click", () => {
      modal.close();
      // A brand-new pairing might need to appear in the list.
      if (!stepResult.hidden) window.location.reload();
    })
  );

  form?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = form.name.value.trim();
    const url = form.url.value.trim();
    if (!name || !url) return;
    try {
      const r = await API.post("/api/tokens", { name, url });
      document.getElementById("pair-qr-img").src = r.qrDataURL;
      document.getElementById("pair-url").textContent = r.url;
      document.getElementById("pair-token").textContent = r.rawToken;
      document.getElementById("pair-fp").textContent = r.fingerprint;
      renderPairAlternates(r.alternates || [], r.url);
      stepForm.hidden = true;
      stepResult.hidden = false;
    } catch (err) {
      alert("Pair failed: " + err.message);
    }
  });

  document.querySelectorAll("[data-copy]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const id = btn.dataset.copy;
      const el = document.getElementById(id);
      if (!el) return;
      try {
        await navigator.clipboard.writeText(el.textContent);
        const old = btn.textContent;
        btn.textContent = "Copied";
        setTimeout(() => (btn.textContent = old), 1200);
      } catch {
        // Fallback: select the code so the user can cmd-C.
        const range = document.createRange();
        range.selectNodeContents(el);
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
      }
    });
  });

  document.querySelectorAll(".revoke-token").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const tr = btn.closest("tr");
      const id = tr.dataset.id;
      if (!confirm(`Revoke ${tr.querySelector("td").textContent}? It'll disconnect immediately.`)) return;
      try {
        await API.delete(`/api/tokens/${id}`);
        tr.remove();
      } catch (err) {
        alert("Revoke failed: " + err.message);
      }
    });
  });

  // Rotate: replace the raw bytes of an existing token. Reuses the
  // pair-result modal layout so the operator gets a fresh QR + raw
  // shown ONCE — same UX as Mint, just keyed off `id` instead of a
  // new row. The previous raw stops validating immediately.
  document.querySelectorAll(".rotate-token").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const tr = btn.closest("tr");
      const id = tr.dataset.id;
      const name = tr.dataset.name || tr.querySelector("td").textContent;
      if (!confirm(`Rotate the token for ${name}?\n\nThe device will need to scan a fresh QR (or paste the new raw token) before it can reach the bridge again. The previous raw token stops working immediately.`)) return;
      try {
        const r = await API.post(`/api/tokens/${id}/rotate`, {});
        document.getElementById("pair-qr-img").src = r.qrDataURL;
        document.getElementById("pair-url").textContent = r.url;
        document.getElementById("pair-token").textContent = r.rawToken;
        document.getElementById("pair-fp").textContent = r.fingerprint;
        renderPairAlternates(r.alternates || [], r.url);
        // Tweak the result-section heading so the operator knows
        // they're looking at a rotation, not a fresh pair.
        const heading = stepResult.querySelector("h2");
        if (heading) heading.textContent = "Token rotated — re-scan on the device";
        stepForm.hidden = true;
        stepResult.hidden = false;
        modal.showModal();
      } catch (err) {
        alert("Rotate failed: " + err.message);
      }
    });
  });

  // Set / clear expiry. The PATCH endpoint accepts an explicit JSON
  // null to clear, or an RFC3339 timestamp to set. The prompt uses
  // a duration shorthand (`24h`, `30d`, etc.) for ergonomics; empty
  // input is treated as "clear".
  document.querySelectorAll(".set-expiry-token").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const tr = btn.closest("tr");
      const id = tr.dataset.id;
      const name = tr.dataset.name || tr.querySelector("td").textContent;
      const ans = prompt(
        `Expiry for ${name} — duration from now (e.g. "24h", "30d", "1y") or blank to clear.\n\nLeave blank to remove an existing expiry.`,
        ""
      );
      if (ans === null) return; // user pressed Cancel
      const trimmed = ans.trim();
      let body;
      if (trimmed === "") {
        body = { expiresAt: null };
      } else {
        const ms = parseDurationShorthand(trimmed);
        if (ms === null) {
          alert(`Couldn't parse "${trimmed}" — use 24h, 30d, 1y, etc.`);
          return;
        }
        const when = new Date(Date.now() + ms);
        body = { expiresAt: when.toISOString() };
      }
      try {
        const r = await API.patch(`/api/tokens/${id}`, body);
        const cell = tr.querySelector(".expires-cell");
        if (cell) {
          cell.textContent = r.expiresAt
            ? new Date(r.expiresAt).toLocaleString()
            : "never";
        }
      } catch (err) {
        alert("Set expiry failed: " + err.message);
      }
    });
  });

  // UPnP upstream card moved to its dedicated /upnp page. Devices is
  // now strictly for paired iOS clients.
}

// initUPnP wires the /upnp page (Configured / Discovered / Add manually
// sections + the shared add/edit modal). Mounted from the
// DOMContentLoaded dispatcher below.
function initUPnP() {
  loadUpnpConfigured();
  loadUpnpDiscovered();
  wireUpnpAddManualButton();
  wireUpnpEditModal();
}

// loadUpnpConfigured fetches /api/upnp/servers and renders the
// "Configured" section. Disabled-feature state hides the section so
// the SSR-rendered feature-disabled panel is the only visible content.
async function loadUpnpConfigured() {
  const panel = document.getElementById("upnp-configured-panel");
  const list = document.getElementById("upnp-configured-list");
  if (!panel || !list) return;
  try {
    const r = await API.get("/api/upnp/servers");
    if (!r.enabled) {
      panel.hidden = true;
      return;
    }
    panel.hidden = false;
    renderUpnpConfiguredList(list, r.servers || []);
    // The add-manual panel mirrors the configured-panel visibility:
    // enabled feature → both visible.
    const addPanel = document.getElementById("upnp-add-manual-panel");
    if (addPanel) addPanel.hidden = false;
  } catch (err) {
    panel.hidden = false;
    list.innerHTML = `<p class="muted">UPnP upstream status unavailable: ${escapeHTML(err.message || String(err))}</p>`;
  }
}

// renderUpnpConfiguredList paints one row per configured server. Empty
// config list = a friendly empty state directing the operator to the
// Discovered section.
function renderUpnpConfiguredList(list, servers) {
  if (!servers.length) {
    list.innerHTML = `<p class="muted"><em>No upstream servers configured yet. Look in the <strong>Discovered on LAN</strong> section below, or use <strong>Add manually</strong>.</em></p>`;
    return;
  }
  const rows = servers.map((s) => upnpConfiguredRowHTML(s)).join("");
  list.innerHTML = `<div class="upnp-upstream-rows">${rows}</div>`;
  list.querySelectorAll("[data-upnp-rescan]").forEach((btn) => {
    btn.addEventListener("click", () => onUpnpRescanClick(btn));
  });
  list.querySelectorAll("[data-upnp-edit]").forEach((btn) => {
    btn.addEventListener("click", () => openUpnpEditModal(JSON.parse(btn.dataset.upnpEdit)));
  });
  list.querySelectorAll("[data-upnp-remove]").forEach((btn) => {
    btn.addEventListener("click", () => onUpnpRemoveClick(btn));
  });
}

// upnpConfiguredRowHTML renders one configured-server row.
function upnpConfiguredRowHTML(s) {
  const statusClass = s.discovered ? "ok" : "warn";
  const statusText = s.discovered ? "Discovered" : "Not seen yet";
  const friendly = s.friendlyName ? escapeHTML(s.friendlyName) : "<em class=\"muted\">unresolved</em>";
  const routed = s.routedTracks || 0;
  // Go's time.Time zero value serializes to "0001-01-01T00:00:00Z" — JS
  // parses that as a real year-1 date. Guard explicitly so we render
  // "never" instead of "1/1/0001, 12:00:00 AM".
  const lastWalk = (s.lastWalkFinishedAt && !s.lastWalkFinishedAt.startsWith("0001-01-01"))
    ? new Date(s.lastWalkFinishedAt).toLocaleString()
    : "never";
  const lastWalked = s.lastWalkedCount || 0;
  const lastReaped = s.lastReapedCount || 0;
  const errLine = s.lastWalkErr
    ? `<div class="upnp-upstream-err">${escapeHTML(s.lastWalkErr)}</div>`
    : "";
  const udn = s.configuredUDN || "";
  const manualURL = s.manualDescriptionURL || "";
  // Identity for DELETE / PATCH endpoints — prefer UDN when present, fall
  // back to manualDescriptionURL for SSDP-unreachable entries.
  const identity = udn || manualURL;
  // Edit payload — stashed on the button's data attr so the modal
  // can prefill without a second fetch.
  //
  // pathPrefix / rootObjectID / skipTopLevelContainers MUST carry the
  // server's real stored values. The submit handler PATCHes all four
  // editable fields unconditionally, and JSON.stringify preserves ""
  // and [] (only `undefined` is dropped) — so hardcoding blanks here,
  // as this did until 2026-08-06, meant a plain rename silently
  // cleared all three on save. They now come from
  // GET /api/upnp/servers, which echoes them for exactly this reason.
  const editPayload = JSON.stringify({
    identity, name: s.name, udn, manualURL,
    pathPrefix: s.pathPrefix || "",
    rootObjectID: s.rootObjectID || "",
    skipTopLevelContainers: s.skipTopLevelContainers || [],
  });
  return `
    <div class="upnp-upstream-row" data-name="${escapeHTML(s.name)}">
      <div class="upnp-upstream-head">
        <strong>${escapeHTML(s.name)}</strong>
        <span class="pill ${statusClass}">${statusText}</span>
      </div>
      <div class="upnp-upstream-meta">
        <div>Friendly name: ${friendly}</div>
        <div>Routed tracks: <strong>${routed.toLocaleString()}</strong></div>
        <div>Last walk: ${lastWalk} (walked ${lastWalked.toLocaleString()}, reaped ${lastReaped.toLocaleString()})</div>
        ${errLine}
      </div>
      <div class="upnp-upstream-actions">
        <button type="button" class="btn" data-upnp-rescan data-udn="${escapeHTML(udn)}">Rescan</button>
        <button type="button" class="btn" data-upnp-edit='${escapeHTML(editPayload)}'>Edit</button>
        <button type="button" class="btn danger" data-upnp-remove data-identity="${escapeHTML(identity)}" data-name="${escapeHTML(s.name)}">Remove</button>
      </div>
    </div>`;
}

// loadUpnpDiscovered fetches /api/upnp/discovered and renders the
// "Discovered on LAN" section. Each row has a "Configure…" button that
// opens the add-modal pre-filled with the discovered UDN + friendly
// name.
async function loadUpnpDiscovered() {
  const panel = document.getElementById("upnp-discovered-panel");
  const list = document.getElementById("upnp-discovered-list");
  if (!panel || !list) return;
  try {
    const r = await API.get("/api/upnp/discovered");
    if (!r.enabled) {
      panel.hidden = true;
      return;
    }
    panel.hidden = false;
    renderUpnpDiscoveredList(list, r.servers || []);
  } catch (err) {
    panel.hidden = false;
    list.innerHTML = `<p class="muted">UPnP discovery status unavailable: ${escapeHTML(err.message || String(err))}</p>`;
  }
}

function renderUpnpDiscoveredList(list, servers) {
  if (!servers.length) {
    list.innerHTML = `<p class="muted"><em>No new MediaServers seen on the LAN. The bridge sweeps every 60s by default — if a server you expect isn't here, check that it's powered on, on the same subnet, and that the router isn't blocking SSDP multicast.</em></p>`;
    return;
  }
  const rows = servers.map((s) => upnpDiscoveredRowHTML(s)).join("");
  list.innerHTML = `<div class="upnp-upstream-rows">${rows}</div>`;
  list.querySelectorAll("[data-upnp-configure]").forEach((btn) => {
    btn.addEventListener("click", () => openUpnpEditModal(JSON.parse(btn.dataset.upnpConfigure)));
  });
}

function upnpDiscoveredRowHTML(s) {
  // Pre-escape EITHER the user-supplied friendlyName OR the literal
  // fallback markup — never escape the assembled string downstream,
  // since `escapeHTML('<em ...>')` would render literal angle-bracket
  // text in place of the muted-italic placeholder. Per CodeRabbit
  // minor on PR #357 round-2.
  const friendly = s.friendlyName
    ? escapeHTML(s.friendlyName)
    : '<em class="muted">no friendly name</em>';
  const model = s.modelName || s.modelDescription || "";
  const mfr = s.manufacturer || "";
  const subtitle = [mfr, model].filter(Boolean).join(" · ");
  const seen = s.lastSeenAt ? new Date(s.lastSeenAt).toLocaleString() : "—";
  // Configure-form prefill payload. PathPrefix defaults to a sanitized
  // form of the friendly name (lowercase, alphanumeric); the operator
  // can edit before saving.
  const prefill = JSON.stringify({
    identity: "", // add-mode
    name: s.friendlyName || "",
    udn: s.udn,
    manualURL: "",
    pathPrefix: defaultPathPrefix(s.friendlyName || ""),
    rootObjectID: "",
    skipTopLevelContainers: [],
  });
  return `
    <div class="upnp-upstream-row" data-udn="${escapeHTML(s.udn)}">
      <div class="upnp-upstream-head">
        <strong>${friendly}</strong>
        <span class="pill ok">${escapeHTML(s.udn)}</span>
      </div>
      <div class="upnp-upstream-meta">
        ${subtitle ? `<div>${escapeHTML(subtitle)}</div>` : ""}
        <div>Last seen: ${escapeHTML(seen)}</div>
      </div>
      <div class="upnp-upstream-actions">
        <button type="button" class="btn primary" data-upnp-configure='${escapeHTML(prefill)}'>Configure…</button>
      </div>
    </div>`;
}

// defaultPathPrefix produces a YAML-loader-equivalent sanitized prefix:
// lowercase, alphanumeric only. Mirrors the bridge's UPnPUpstreamServerConfig
// fallback so the operator sees the same default the server would compute.
function defaultPathPrefix(name) {
  return (name || "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "")
    .slice(0, 32);
}

function wireUpnpAddManualButton() {
  const btn = document.getElementById("upnp-add-open");
  if (!btn) return;
  btn.addEventListener("click", () => openUpnpEditModal({
    identity: "", name: "", udn: "", manualURL: "",
    pathPrefix: "", rootObjectID: "", skipTopLevelContainers: [],
  }));
}

// openUpnpEditModal opens the add/edit dialog pre-filled with the
// supplied payload. `identity` === "" means add-mode (POST); non-empty
// means edit-mode (PATCH on that identity).
function openUpnpEditModal(payload) {
  const modal = document.getElementById("upnp-edit-modal");
  const form = document.getElementById("upnp-edit-form");
  const title = document.getElementById("upnp-edit-title");
  const errBox = document.getElementById("upnp-edit-error");
  if (!modal || !form || !title) return;
  title.textContent = payload.identity ? "Edit UPnP server" : "Add UPnP server";
  form.elements.name.value = payload.name || "";
  form.elements.udn.value = payload.udn || "";
  form.elements.manualDescriptionURL.value = payload.manualURL || "";
  form.elements.pathPrefix.value = payload.pathPrefix || "";
  form.elements.rootObjectID.value = payload.rootObjectID || "";
  form.elements.skipTopLevelContainers.value = (payload.skipTopLevelContainers || []).join("\n");
  // UDN + manualURL are identity in edit-mode; lock them so the
  // operator can't accidentally rename the row out from under the
  // PATCH endpoint (which keys on the URL path UDN, not the payload).
  form.elements.udn.readOnly = !!payload.identity;
  form.elements.manualDescriptionURL.readOnly = !!payload.identity;
  form.dataset.editIdentity = payload.identity || "";
  if (errBox) { errBox.hidden = true; errBox.textContent = ""; }
  modal.showModal();
}

function wireUpnpEditModal() {
  const modal = document.getElementById("upnp-edit-modal");
  const form = document.getElementById("upnp-edit-form");
  const cancelBtn = document.getElementById("upnp-edit-cancel");
  if (!modal || !form || !cancelBtn) return;
  cancelBtn.addEventListener("click", () => modal.close());
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const identity = form.dataset.editIdentity || "";
    const body = {
      name: form.elements.name.value.trim(),
      udn: form.elements.udn.value.trim(),
      manualDescriptionURL: form.elements.manualDescriptionURL.value.trim(),
      pathPrefix: form.elements.pathPrefix.value.trim(),
      rootObjectID: form.elements.rootObjectID.value.trim(),
      skipTopLevelContainers: form.elements.skipTopLevelContainers.value
        .split(/\r?\n/)
        .map((s) => s.trim())
        .filter(Boolean),
    };
    const errBox = document.getElementById("upnp-edit-error");
    try {
      if (identity) {
        // Edit mode: PATCH only the editable fields (Name + PathPrefix +
        // RootObjectID + SkipTopLevelContainers). UDN + ManualURL are
        // identity, not editable.
        const patch = {
          name: body.name,
          pathPrefix: body.pathPrefix,
          rootObjectID: body.rootObjectID,
          skipTopLevelContainers: body.skipTopLevelContainers,
        };
        await API.patch("/api/upnp/servers?udn=" + encodeURIComponent(identity), patch);
      } else {
        await API.post("/api/upnp/servers", body);
      }
      modal.close();
      showUpnpRestartBanner();
      loadUpnpConfigured();
      loadUpnpDiscovered();
    } catch (err) {
      if (errBox) {
        errBox.hidden = false;
        errBox.textContent = err.message || String(err);
      }
    }
  });
}

async function onUpnpRescanClick(btn) {
  const udn = btn.dataset.udn || "";
  btn.disabled = true;
  const original = btn.textContent;
  btn.textContent = "Rescanning…";
  try {
    const q = udn ? "?udn=" + encodeURIComponent(udn) : "";
    await API.post("/api/upnp/rescan" + q);
    // Refresh the whole card so post-walk stats land in the UI.
    setTimeout(() => loadUpnpConfigured(), 1500);
  } catch (err) {
    alert("Rescan failed: " + (err.message || err));
  } finally {
    btn.disabled = false;
    btn.textContent = original;
  }
}

// onUpnpRemoveClick handles the Remove button on a configured-server
// row. Confirms with the operator (server name → simple confirm
// dialog; this is a destructive action that drops manifest tracks on
// the next restart), then DELETE /api/upnp/servers?udn={identity}.
async function onUpnpRemoveClick(btn) {
  const identity = btn.dataset.identity || "";
  const name = btn.dataset.name || identity;
  if (!identity) return;
  if (!confirm(`Remove "${name}" from the configured UPnP servers? Its routed tracks will drop from the manifest on the next restart.`)) {
    return;
  }
  btn.disabled = true;
  const original = btn.textContent;
  btn.textContent = "Removing…";
  try {
    await API.delete("/api/upnp/servers?udn=" + encodeURIComponent(identity));
    showUpnpRestartBanner();
    loadUpnpConfigured();
    loadUpnpDiscovered();
  } catch (err) {
    alert("Remove failed: " + (err.message || err));
  } finally {
    btn.disabled = false;
    btn.textContent = original;
  }
}

// RESTART_PENDING_KEY carries "a config change needs a restart" across a
// page navigation. There is no server-side pending-restart state — the
// `restartRequired` flag lives only on a PATCH response, and GET
// /api/settings doesn't carry one — so the browser is where this has to
// live. sessionStorage (not localStorage) scopes it to the tab session:
// a restart the operator never performed shouldn't haunt them next week.
const RESTART_PENDING_KEY = "bridge.restartPending";

/**
 * One field's outcome from a PATCH /api/settings response.
 *
 * The server reports `fields: {name: {status, reason?}}` alongside the
 * legacy `restartRequired` boolean. Falls back to the boolean when the
 * key is absent, which covers exactly one case worth covering: a console
 * served by a bridge older than the per-field report. Same-origin, so the
 * two can only disagree across a mid-upgrade page load — cheap insurance
 * against a stale tab reporting nothing at all.
 */
function applyStatusFor(resp, field) {
  const entry = resp && resp.fields && resp.fields[field];
  if (entry && entry.status) return entry;
  return { status: resp && resp.restartRequired ? "restart" : "live" };
}

/** Field names from a PATCH response whose status is `restart`. */
function fieldsNeedingRestart(resp) {
  const fields = (resp && resp.fields) || {};
  const out = Object.keys(fields).filter((k) => fields[k].status === "restart");
  // Sorted so the same set of edits always reads the same way; an
  // object's key order is insertion-dependent and would otherwise shuffle
  // between saves for no reason the operator can see.
  //
  // With an explicit comparator, not the default: a bare sort() coerces to
  // string and orders by UTF-16 code unit, which is a trap the moment the
  // array holds anything but ASCII. These are field names today, so the
  // two agree — the comparator is what keeps that from being load-bearing.
  out.sort((a, b) => a.localeCompare(b));
  // A bridge too old to send `fields` still sets the boolean.
  if (!out.length && resp && resp.restartRequired) return ["some settings"];
  return out;
}

/**
 * Hide settings a control plane owns on this bridge.
 *
 * A control the operator cannot act on is worse than no control: it will
 * eventually be flipped, do nothing, and be reported as a bug. The server
 * refuses these in PATCH too — this only stops them being offered.
 *
 * Hides the whole enclosing `.field` (label, input and hint together), so
 * a hidden setting leaves no orphaned caption behind. A managed field the
 * page does not render is a no-op, which is what makes the list safe to
 * carry names this build has never heard of.
 */
function hideManagedSettings(managed) {
  if (!Array.isArray(managed) || !managed.length) return;
  for (const name of managed) {
    // Attribute-value lookup, not an interpolated selector: the list
    // comes from bridge.yaml, and a name containing a quote would throw a
    // DOMException that takes the rest of the page's init down with it.
    for (const el of document.querySelectorAll("[name]")) {
      if (el.getAttribute("name") !== name) continue;
      const field = el.closest(".field") || el.closest("label") || el;
      field.hidden = true;
    }
  }
}

function markRestartPending(pending) {
  try {
    if (pending) sessionStorage.setItem(RESTART_PENDING_KEY, "1");
    else sessionStorage.removeItem(RESTART_PENDING_KEY);
  } catch { /* private-mode / disabled storage — degrade to no memory */ }
}

function restartPending() {
  try {
    return sessionStorage.getItem(RESTART_PENDING_KEY) === "1";
  } catch { return false; }
}


// ===========================================================
// Per-page feature trays
// ===========================================================
//
// A small gear beside a heading that opens the switches for THAT
// feature, in place. Settings stays the one exhaustive surface; these
// are the two or three fields the operator is looking at the page for,
// where they are already looking.
//
// The precedent is the Duplicates page, which has carried its serving
// policy inline since it shipped. Every other togglable feature had its
// status on one page (Jobs, Smart mixes, History) and its switch on
// another, so the loop "is this on? → Settings → find the tab → toggle
// → navigate back" ran for a single checkbox.
//
// No new endpoint: PATCH /api/settings is already a partial update with
// pointer-typed fields, so a tray sends only the field it owns and the
// server's own hot-apply / restart-required rules answer for it. That
// is deliberate — a tray must never be able to mean something different
// from the same control on the Settings page.
//
// Trays are INLINE, not floating popovers. An anchored popover needs
// viewport clamping, a z-index in the ledger, outside-click dismissal
// and its own phone layout; a disclosure that expands under its heading
// needs none of those and cannot be clipped by the card it lives in.

let traySeq = 0;

// The shared /api/settings snapshot. One fetch backs every tray on a
// page — the Jobs page mounts nine — and a successful save updates it in
// place so a tray opened afterwards shows the new value rather than the
// one from page load.
//
// Dropped on every page init (see dispatchPageInit), NOT held for the
// session: config changes from places this module never sees — the
// Settings form, the CLI, another tab, a second browser — and a tray
// showing a value from three navigations ago is the same
// two-surfaces-disagree failure the cross-tray re-sync below exists to
// prevent. One request per visit to a page that has trays.
let traySettings = null;
let traySettingsPromise = null;

// Every tray mounted on the CURRENT page, so a save in one can re-sync
// the others: analysisEnabled appears on both the Audio analysis card
// and the Smart mixes card, and two open trays disagreeing about the
// same field is worse than either being stale.
const mountedTrays = new Set();

// Deleting from a Set during its own forEach is well-defined — a removed
// entry simply is not visited — so this needs no copy.
function pruneDetachedTrays() {
  mountedTrays.forEach((entry) => {
    if (!entry.tray.isConnected) mountedTrays.delete(entry);
  });
}

function invalidateTraySettings() {
  traySettings = null;
  traySettingsPromise = null;
}

function traySettingsSnapshot() {
  if (traySettings) return Promise.resolve(traySettings);
  if (!traySettingsPromise) {
    traySettingsPromise = API.get("/api/settings")
      .then((s) => { traySettings = s || {}; return traySettings; })
      .catch((err) => { traySettingsPromise = null; throw err; });
  }
  return traySettingsPromise;
}

/**
 * Build a feature tray. Returns { button, tray } for the caller to
 * place — attachFeatureTray below is the placement every operator page
 * wants, and the player supplies its own because its toolbar is rebuilt
 * on every route.
 *
 * spec = {
 *   title: string,          // names the feature, not the page
 *   blurb?: string,         // one line: what turning this on does
 *   rows: Row[],
 *   link?: { href, text },  // deep link to the full settings section
 * }
 * Row =
 *   { field, type: "switch", label, hint?, restart? }
 * | { field, type: "select", label, hint?, restart?, options: [[value, label], …] }
 * | { field, type: "number", label, hint?, restart?, min?, max?, unit?,
 *     placeholder?, emptyValue? }   // emptyValue: what a CLEARED box sends
 * | { type: "note", text }   // no field: an explanation, not a control
 */
function buildFeatureTray(spec) {
  const id = `feature-tray-${++traySeq}`;
  const button = document.createElement("button");
  button.type = "button";
  button.className = "tray-toggle";
  button.setAttribute("aria-expanded", "false");
  button.setAttribute("aria-controls", id);
  // The gear is decorative; the accessible name comes from the text
  // beside it, which is clipped rather than removed so the control still
  // announces WHICH feature it configures. Two gears on one page with
  // the name "Settings" would be indistinguishable by voice.
  button.innerHTML =
    `<svg class="tray-ico" viewBox="0 0 24 24" aria-hidden="true" focusable="false">` +
    `<use href="#i-settings"/></svg>` +
    `<span class="tray-toggle-label">${escapeHTML(spec.title)} settings</span>`;

  const tray = document.createElement("div");
  tray.className = "feature-tray";
  tray.id = id;
  tray.hidden = true;

  const head = document.createElement("div");
  head.className = "tray-head";
  const h = document.createElement("p");
  h.className = "tray-title";
  h.textContent = spec.title;
  head.appendChild(h);
  if (spec.blurb) {
    const b = document.createElement("p");
    b.className = "tray-blurb";
    b.textContent = spec.blurb;
    head.appendChild(b);
  }
  tray.appendChild(head);

  const status = document.createElement("p");
  status.className = "tray-status";
  status.setAttribute("role", "status");
  status.setAttribute("aria-live", "polite");

  const controls = [];
  const body = document.createElement("div");
  body.className = "tray-rows";
  for (const row of spec.rows || []) {
    const built = buildTrayRow(row, status, controls);
    if (built) body.appendChild(built);
  }
  tray.appendChild(body);
  tray.appendChild(status);

  if (spec.link) {
    const foot = document.createElement("p");
    foot.className = "tray-foot";
    const a = document.createElement("a");
    a.href = spec.link.href;
    a.textContent = spec.link.text;
    foot.appendChild(a);
    tray.appendChild(foot);
  }

  // Prune BEFORE adding, not only when a save happens to re-sync.
  //
  // pageSignal aborts on dispatchPageInit, which the player never calls:
  // navigating /mixes → /albums → /mixes inside the player rebuilds the
  // toolbar without any teardown, so each visit would leave its tray —
  // and the detached DOM it holds — in the set forever. syncTray drops a
  // disconnected entry when it runs, but it only runs on open and after
  // a save, so a reader who never opens one accumulates them silently.
  // (Gemini on PR #763.)
  pruneDetachedTrays();
  const entry = { tray, controls };
  mountedTrays.add(entry);
  pageSignal().addEventListener("abort", () => mountedTrays.delete(entry), { once: true });

  // Warm the shared snapshot at MOUNT, not at first open.
  //
  // The controls start disabled and unchecked, so an open that has to
  // wait for the fetch shows every switch briefly in the OFF position
  // before flipping — a reader watching that has just been told the
  // wrong thing about their own configuration, however briefly. One
  // request per page (all trays share the promise, and the Jobs page
  // mounts nine of them), issued while the reader is still reading the
  // page.
  void traySettingsSnapshot().then(() => syncTray(entry)).catch(() => {
    // Silent: nothing is open yet, so there is nowhere to report it. The
    // open path below retries and shows the error there.
  });

  const close = () => {
    tray.hidden = true;
    button.setAttribute("aria-expanded", "false");
  };
  button.addEventListener("click", () => {
    if (!tray.hidden) { close(); return; }
    tray.hidden = false;
    button.setAttribute("aria-expanded", "true");
    // Normally a no-op: the mount-time warm above has already landed and
    // syncTray has run. This is the retry path for a snapshot that failed
    // or is still in flight, and the only place the failure can be
    // reported, because it is the only place the reader is looking.
    if (!traySettings) {
      status.textContent = "Loading…";
      status.dataset.tone = "";
      traySettingsSnapshot()
        .then(() => { status.textContent = ""; syncTray(entry); })
        .catch((err) => {
          status.dataset.tone = "err";
          status.textContent = `Could not read settings: ${err.message || err}`;
        });
    }
    const first = tray.querySelector("input, select, button, a");
    if (first) first.focus();
  });
  tray.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    e.stopPropagation();
    close();
    button.focus();
  });

  return { button, tray };
}

// buildTrayRow renders one row and registers its control so syncTray can
// push server values into it. A "note" row has no control and is just
// prose — used where a page has something to explain but nothing to
// switch (History, whose recording gate is per-device on iOS by design).
//
// Split into trayControlFor / trayLabelFor rather than one branchy
// function: the two type switches are independent decisions (what
// element the value lives in, and where the badge sits relative to it)
// and reading them apart is what makes the badge rule legible.
function buildTrayRow(row, status, controls) {
  if (row.type === "note") {
    const p = document.createElement("p");
    p.className = "tray-note";
    p.textContent = row.text || "";
    return p;
  }
  if (!row.field) return null;

  const input = trayControlFor(row);
  // Disabled until the snapshot lands: an unpopulated switch shows
  // "off", and a reader who flips it is turning ON something that may
  // already be on — or, worse, watching a PATCH send the value the
  // control happened to default to.
  input.disabled = true;
  input.dataset.field = row.field;

  const wrap = document.createElement("div");
  wrap.className = "tray-row";
  wrap.appendChild(trayLabelFor(row, input));
  if (row.hint) {
    const hint = document.createElement("small");
    hint.className = "tray-hint";
    hint.textContent = row.hint;
    wrap.appendChild(hint);
  }

  const ctl = { row, input };
  controls.push(ctl);
  // change, not input: a number field would otherwise PATCH on every
  // keystroke, and "6" on the way to "60" is a real, saved value.
  input.addEventListener("change", () => saveTrayField(ctl, status));
  return wrap;
}

// trayControlFor builds the element the value lives in.
function trayControlFor(row) {
  if (row.type === "select") {
    const sel = document.createElement("select");
    for (const [value, label] of row.options || []) {
      const opt = document.createElement("option");
      opt.value = value;
      opt.textContent = label;
      sel.appendChild(opt);
    }
    return sel;
  }
  if (row.type === "number") {
    const num = document.createElement("input");
    num.type = "number";
    if (row.min != null) num.min = String(row.min);
    if (row.max != null) num.max = String(row.max);
    // A field whose settings response carries `omitempty` and whose
    // stored value is 0 arrives ABSENT, and an empty box beside a card
    // reading "every 6 h" looks like a disagreement rather than "unset,
    // so the default applies". updateCheckIntervalHours is the one field
    // in that shape — backups report their effective values.
    if (row.placeholder) num.placeholder = row.placeholder;
    return num;
  }
  const box = document.createElement("input");
  box.type = "checkbox";
  box.setAttribute("role", "switch");
  return box;
}

// trayLabelFor wraps the control in its label, with the restart badge on
// the side the layout needs it.
function trayLabelFor(row, input) {
  const label = document.createElement("label");
  // A switch borrows app.css's `label.checkbox` outright rather than
  // restyling one: the console has one toggle, and a tray that grew its
  // own would be a second thing to keep in step with it. `.tray-switch`
  // carries only the compact metrics (a tray is denser than a settings
  // pane), which is why the borrowed class comes first.
  const isField = row.type === "select" || row.type === "number";
  label.className = isField ? "tray-field" : "checkbox tray-switch";

  const text = document.createElement("span");
  text.className = "tray-label";
  text.textContent = row.label;
  const badge = row.restart ? document.createElement("span") : null;
  if (badge) {
    badge.className = "badge warn";
    badge.textContent = "restart";
  }

  if (!isField) {
    // A switch reads left to right — toggle, what it does, then the
    // caveat — which is also the order the Settings page uses.
    label.append(input, text);
    if (badge) label.appendChild(badge);
    return label;
  }

  // Badge with the LABEL, not after the control. In a narrow tray the
  // label takes its own line and the control keeps its unit beside it,
  // so a trailing badge landed alone on a third line and read as a
  // stray chip belonging to nothing.
  const headGroup = document.createElement("span");
  headGroup.className = "tray-field-head";
  headGroup.appendChild(text);
  if (badge) headGroup.appendChild(badge);
  label.append(headGroup, input);
  if (row.unit) {
    const u = document.createElement("span");
    u.className = "tray-unit";
    u.textContent = row.unit;
    label.appendChild(u);
  }
  return label;
}

// trayValueOf reads a control's current value in the shape the PATCH
// field expects — bool for a switch, string for a select, number for a
// number field.
function trayValueOf(ctl) {
  if (ctl.row.type === "select") return ctl.input.value;
  if (ctl.row.type === "number") {
    const n = parseInt(ctl.input.value, 10);
    if (!Number.isNaN(n)) return n;
    // A field whose stored 0 means "use the built-in default" can be
    // cleared back to it, the way the Settings form's own `|| "0"` does.
    // Only where the row says so: for a field with no such sentinel a
    // blank box is a mistake, and silently sending 0 would turn a
    // fat-finger into a saved value.
    return ctl.row.emptyValue ?? null;
  }
  return ctl.input.checked;
}

function trayApplyValue(ctl, value) {
  if (ctl.row.type === "select") ctl.input.value = value == null ? "" : String(value);
  else if (ctl.row.type === "number") ctl.input.value = value == null ? "" : String(value);
  else ctl.input.checked = !!value;
}

// syncTray pushes the shared snapshot into a tray's controls and enables
// them. Called on first open and after any tray's successful save.
function syncTray(entry) {
  if (!traySettings) return;
  // Self-pruning. Operator pages tear their trays down through
  // pageSignal, but the player rebuilds its toolbar on every internal
  // route WITHOUT re-running dispatchPageInit — so a tray left behind by
  // a navigation from /mixes to /albums would stay in the set with a
  // detached node. Cheap, and it means the player needs no hook of its own.
  if (!entry.tray.isConnected) {
    mountedTrays.delete(entry);
    return;
  }
  for (const ctl of entry.controls) {
    trayApplyValue(ctl, traySettings[ctl.row.field]);
    ctl.input.disabled = false;
  }
}

async function saveTrayField(ctl, status) {
  const value = trayValueOf(ctl);
  if (value == null) {
    status.dataset.tone = "err";
    status.textContent = "Enter a number.";
    return;
  }
  status.dataset.tone = "";
  status.textContent = "Saving…";
  ctl.input.disabled = true;
  try {
    const r = await API.patch("/api/settings", { [ctl.row.field]: value });
    if (traySettings) traySettings[ctl.row.field] = value;
    // A tray saves exactly ONE field, so read that field's own answer
    // rather than the request-wide rollup. They agree here today, but
    // the rollup is an OR: the moment a tray ever sends a second field
    // it would start reporting a neighbour's restart as this switch's.
    const applied = applyStatusFor(r, ctl.row.field);
    if (applied.status === "restart") {
      markRestartPending(true);
      status.dataset.tone = "warn";
      // The server's reason, when it has one, beats our generic line:
      // it is the difference between "restart to apply" and "there is no
      // sweeper on this bridge, so it will never apply until you do".
      status.textContent = applied.reason
        ? `Saved — ${applied.reason}`
        : "Saved — restart the bridge to apply.";
      if (applied.reason) status.title = applied.reason;
    } else if (applied.status === "unchanged") {
      status.dataset.tone = "ok";
      status.textContent = "Already set.";
    } else {
      status.dataset.tone = "ok";
      status.textContent = "Saved.";
    }
    // Every OTHER tray on the page, so a shared field can't show two
    // different answers at once.
    for (const other of mountedTrays) syncTray(other);
  } catch (err) {
    // Snap the control back to what the server still holds: leaving it
    // showing the rejected value is the one outcome that lies.
    if (traySettings) trayApplyValue(ctl, traySettings[ctl.row.field]);
    status.dataset.tone = "err";
    status.textContent = `Save failed: ${err.message || err}`;
  } finally {
    ctl.input.disabled = false;
  }
}

/**
 * Mount a tray onto a heading row: the gear goes INSIDE `head`, the tray
 * immediately after it. That lands the panel under the heading and above
 * the card's body on the Jobs page, and under the page title on a page
 * head — the same relationship in both, with no per-caller placement.
 *
 * A missing head is a no-op, not a throw: every operator initX is
 * nil-guarded so a page that renders without a given card stays silent.
 */
function attachFeatureTray(head, spec) {
  if (!head || !head.parentNode) return null;
  const { button, tray } = buildFeatureTray(spec);
  // Into the head's action GROUP when it has one, not the head itself.
  //
  // .panel-head wraps, and a job card in the two-up grid is ~320px wide:
  // heading + badge + trigger button + gear overflows it, so a gear added
  // as a third child of the head dropped onto a line of its own at the
  // LEFT edge — below the heading, nowhere near the controls it belongs
  // with. Inside .panel-actions the whole group wraps together and stays
  // one block.
  (head.querySelector(":scope > .panel-actions") || head).appendChild(button);
  head.parentNode.insertBefore(tray, head.nextSibling);
  return { button, tray };
}

// The player module is an ES module and app.js is a deferred classic
// script, so there is no import between them — the same one-way window
// handshake boot.js already uses for window.__player, in the other
// direction. Exposed as a function rather than the internals so the
// player cannot reach the snapshot cache.
window.BridgeFeatureTray = { build: buildFeatureTray };

// showUpnpRestartBanner injects (or refreshes) a one-time "Restart
// required" banner above the configured panel so the operator knows
// their CRUD action won't take effect until the next bridge restart.
// Idempotent — calling it twice doesn't stack banners.
//
// The link used to target an anchor named `danger` that exists in no
// template, and the Settings page's only restart affordance
// (`#restart-btn`) is `hidden` until a restart-requiring save reveals it
// — so following the banner landed the operator on a page with no
// Restart button at all. The flag below is what lets Settings reveal it
// on load; `#restart-actions` is a real anchor.
// (TestAppJSSettingsAnchorsExistInTheRenderedPage pins that pairing, and
// it scans this file's raw text — don't write a settings-anchor URL in a
// comment here or it reads as a live link.)
function showUpnpRestartBanner() {
  markRestartPending(true);
  const host = document.querySelector(".page.upnp");
  if (!host) return;
  let banner = document.getElementById("upnp-restart-banner");
  if (!banner) {
    banner = document.createElement("div");
    banner.id = "upnp-restart-banner";
    banner.className = "panel banner-warn";
    banner.innerHTML = `
      <strong>Restart required</strong> — changes to the upstream
      server list don't take effect until the bridge restarts.
      Use the <a href="/settings#restart-actions">Restart</a> button on the
      Settings page when you're ready.`;
    host.insertBefore(banner, host.firstChild.nextSibling); // after h1
  }
}

// renderPairAlternates populates the Pair-modal "Other URLs the
// device will try" list. The QR's bridge://pair?...&urls=... payload
// already carries every alternate; the displayed list mirrors that
// so the operator sees what they actually shared with the device
// (pre-fix the modal showed only the primary URL, which mislead an
// operator into thinking only one URL had been shared and the iOS
// app couldn't roam). The primary URL appears first in the alternates
// slice and is rendered separately as `pair-url`, so it's filtered
// out here.
function renderPairAlternates(alternates, primary) {
  const block = document.getElementById("pair-alternates-block");
  const list = document.getElementById("pair-alternates");
  if (!block || !list) return;
  list.replaceChildren();
  const others = (alternates || []).filter((u) => u !== primary);
  if (others.length === 0) {
    block.hidden = true;
    return;
  }
  for (const u of others) {
    const li = document.createElement("li");
    const code = document.createElement("code");
    code.textContent = u;
    li.appendChild(code);
    list.appendChild(li);
  }
  block.hidden = false;
}

// parseDurationShorthand handles the prompt's free-text input —
// "24h", "30d", "1y" — and converts to milliseconds. Returns null
// for anything it can't parse. Deliberately small/forgiving; the
// PATCH endpoint accepts a full RFC3339 if the operator wants more
// precision than this helper offers.
function parseDurationShorthand(s) {
  const m = /^(\d+)\s*([hdwmy])$/i.exec(s);
  if (!m) return null;
  const n = parseInt(m[1], 10);
  const unit = m[2].toLowerCase();
  const hour = 3600 * 1000;
  switch (unit) {
    case "h": return n * hour;
    case "d": return n * 24 * hour;
    case "w": return n * 7 * 24 * hour;
    case "m": return n * 30 * 24 * hour; // approximate; calendar-aware would need a date library
    case "y": return n * 365 * 24 * hour;
  }
  return null;
}

// --- settings ---

// Settings is ONE SCROLLING SCREEN with a sticky jump list.
//
// It was seven tabs, which flattened a long form but made the operator
// hunt: the General tab held three fields, the state that explains why
// a feature is doing nothing lived on a different tab from its toggle,
// and two of the seven were read-only. Everything is on one page now;
// the old tab strip becomes an in-page jump list, and per-section
// <details> carry the fields most operators never touch.
//
// The no-JS fallback is what made this cheap: the template never
// marked panes hidden, so "every section visible" was already the
// pre-init state. This function now only wires navigation — it never
// hides anything, so a JS failure degrades to a plain long form rather
// than an empty page.
//
// ?tab=<id> still works: the Jobs page deep-links into a section, and
// that link now scrolls rather than switching. Same URL, same landing
// place.
function initSettingsTabs() {
  const links = document.querySelectorAll(".jump-link[data-tab]");
  const panes = document.querySelectorAll(".tab-pane[data-tab]");
  if (links.length === 0 || panes.length === 0) return;

  const byId = new Map();
  panes.forEach(p => byId.set(p.dataset.tab, p));

  function markActive(id) {
    links.forEach(l => {
      const on = l.dataset.tab === id;
      l.classList.toggle("active", on);
      if (on) l.setAttribute("aria-current", "true");
      else l.removeAttribute("aria-current");
    });
  }

  function goTo(id, { push }) {
    const pane = byId.get(id);
    if (!pane) return;
    pane.scrollIntoView({ behavior: prefersReducedMotion() ? "auto" : "smooth", block: "start" });
    markActive(id);
    if (push) {
      try { sessionStorage.setItem("settings.activeTab", id); } catch { /* private mode */ }
    }
  }

  links.forEach(l => {
    l.addEventListener("click", (e) => {
      e.preventDefault();
      goTo(l.dataset.tab, { push: true });
    });
  });

  // Scrollspy: highlight whichever section owns the top of the
  // viewport. rootMargin pulls the trigger line down from the very top
  // so a heading that has just scrolled under the sticky header still
  // reads as current.
  if ("IntersectionObserver" in window) {
    const io = new IntersectionObserver((entries) => {
      const visible = entries.filter(e => e.isIntersecting)
        .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
      if (visible.length) markActive(visible[0].target.dataset.tab);
    }, { rootMargin: "-80px 0px -70% 0px" });
    panes.forEach(p => io.observe(p));
  }

  const urlTab = new URLSearchParams(window.location.search).get("tab");
  let saved = null;
  try { saved = sessionStorage.getItem("settings.activeTab"); } catch { /* private mode */ }
  const initial = (urlTab && byId.has(urlTab)) ? urlTab : (saved && byId.has(saved) ? saved : null);
  if (initial) {
    // Jump without smooth scrolling on load — animating to a section
    // the operator asked for by URL just delays it.
    byId.get(initial).scrollIntoView({ block: "start" });
    markActive(initial);
  } else {
    markActive(panes[0].dataset.tab);
  }
}

// renderSettingsPrereqs answers, next to each toggle, the question the
// operator actually has: "it says on — is it doing anything?"
//
// This exists because of a real state on the author's own bridge.
// bridge.yaml said analysis.enabled: true and the toggle rendered on;
// /api/jobs said degradedReason "sox_missing" (the boot-time precheck
// failed, so the pool was never wired); /api/analysis/stats said
// enabled:false, soxAvailable:true (sox is findable NOW); /api/doctor
// said audio-toolchain ok. Four endpoints, four true statements, and
// audio analysis had done nothing for nine days of uptime.
//
// None of those facts was next to the switch. Putting the boot-time
// VERDICT and the live PREREQUISITE side by side — and saying
// "restart to apply" when they disagree — is worth more than any
// default change.
async function renderSettingsPrereqs() {
  const slots = {
    analysis: document.getElementById("prereq-analysis"),
    upscale: document.getElementById("prereq-upscale"),
    fingerprint: document.getElementById("prereq-fingerprint"),
  };
  if (!slots.analysis && !slots.upscale && !slots.fingerprint) return;

  const [jobs, doctor] = await Promise.all([
    API.get("/api/jobs").catch(() => null),
    API.get("/api/doctor").catch(() => null),
  ]);
  const checks = new Map();
  if (doctor && doctor.available && doctor.report && Array.isArray(doctor.report.checks)) {
    for (const c of doctor.report.checks) checks.set(c.name, c);
  }

  const paint = (slot, { running, degradedReason, check, offLabel }) => {
    if (!slot) return;
    slot.hidden = false;
    if (running) {
      slot.dataset.state = "ok";
      slot.textContent = "active";
      return;
    }
    if (degradedReason) {
      // The toggle is on and the feature is not running. Say which
      // way the disagreement points, because the fix differs: a
      // missing tool needs installing, a tool that is present now
      // needs a restart to be picked up.
      const live = check && check.status === "ok";
      slot.dataset.state = "warn";
      slot.textContent = live
        ? "not running — restart to apply"
        : `not running — ${check ? check.summary : degradedReason}`;
      return;
    }
    slot.dataset.state = "off";
    slot.textContent = offLabel;
  };

  const audio = checks.get("audio-toolchain");
  paint(slots.analysis, {
    running: !!(jobs && jobs.analysis && jobs.analysis.enabled && !jobs.analysis.degradedReason),
    degradedReason: jobs && jobs.analysis && jobs.analysis.enabled ? jobs.analysis.degradedReason : "",
    check: audio,
    offLabel: audio && audio.status === "ok" ? "off — sox is available" : "off",
  });
  paint(slots.upscale, {
    running: !!(jobs && jobs.upscale && jobs.upscale.enabled),
    degradedReason: "",
    check: audio,
    offLabel: audio && audio.status === "ok" ? "off — sox is available" : "off",
  });
  const fp = checks.get("fingerprint-toolchain");
  paint(slots.fingerprint, {
    running: !!(jobs && jobs.fingerprint && jobs.fingerprint.enabled),
    degradedReason: "",
    check: fp,
    offLabel: fp && fp.status === "ok" ? "off" : "off — needs fpcalc and an AcoustID key",
  });
}

// numOrUndef parses a numeric form field, yielding undefined (which
// JSON.stringify drops, leaving the server's pointer nil = "unchanged")
// for a blank or malformed value. Distinct from `|| 0` because 0 is a
// real value for at least one of these fields.
function numOrUndef(v) {
  if (v === null || String(v).trim() === "") return undefined;
  const n = Number.parseInt(v, 10);
  return Number.isNaN(n) ? undefined : n;
}

function prefersReducedMotion() {
  return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

// initEnrichmentSource wires the Enrichment tab's source picker: the Atlas
// URL field shows only for source=atlas, "custom" opens the Advanced raw
// fields, and hand-editing a raw URL flips the picker to Custom so the
// submit-time mapping never silently overwrites a manual edit.
function initEnrichmentSource() {
  const sel = document.querySelector('select[name="enrichSource"]');
  if (!sel) return;
  const atlasField = document.getElementById("enrich-atlas-field");
  const advanced = document.getElementById("enrich-advanced");
  const apply = () => {
    if (atlasField) atlasField.hidden = sel.value !== "atlas";
    if (advanced && sel.value === "custom") advanced.open = true;
  };
  sel.addEventListener("change", apply);
  for (const name of ["enrichMusicBrainzBaseURL", "enrichCoverArtBaseURL"]) {
    document.querySelector(`[name="${name}"]`)?.addEventListener("input", () => {
      if (sel.value !== "custom") {
        sel.value = "custom";
        apply();
      }
    });
  }
}

// mapEnrichSourceToBases resolves the source picker + Atlas URL + raw fields
// into the two base URLs the PATCH actually carries (the config schema is
// unchanged — URLs stay the single source of truth). Returns null with an
// error message when Atlas is selected without a URL.
function mapEnrichSourceToBases(fd) {
  const src = fd.get("enrichSource") || "custom";
  if (src === "musicbrainz") {
    return { mb: "", ca: "" }; // public defaults
  }
  if (src === "atlas") {
    // Trailing-slash trim via a loop, not /\/+$/ — Sonar flags that regex
    // class as super-linear under backtracking.
    let a = (fd.get("enrichAtlasURL") || "").trim();
    while (a.endsWith("/")) a = a.slice(0, -1);
    if (!a) return { err: "Atlas URL is required when the enrichment source is Atlas." };
    return { mb: a + "/ws/2", ca: a };
  }
  return {
    mb: fd.get("enrichMusicBrainzBaseURL") || "",
    ca: fd.get("enrichCoverArtBaseURL") || "",
  };
}

function initSettings() {
  initSettingsTabs();
  // Hide control-plane-owned settings before anything else wires them:
  // the operator should never see a switch this bridge will refuse.
  // Fire-and-forget — a failed read leaves every field visible, which is
  // the pre-existing behaviour and strictly better than a blank page.
  API.get("/api/settings")
    .then(d => hideManagedSettings(d && d.managedSettings))
    .catch(() => {});
  // The Updates panel lives on this page; its wiring used to sit in the
  // dashboard init, from when Stats and Settings were one page.
  wireUpdatePanel();
  void renderSettingsPrereqs();
  initEnrichmentSource();
  // Cert info is a one-shot fetch — the cert doesn't change without
  // a restart, so polling it is wasted work. The endpoints panel is
  // hydrated by the SSE stream wired at the bottom of this file.
  refreshCertInfo();

  // Backups panel (moved from dashboard in PR #129) — refresh
  // count/most-recent on settings page load + wire the snapshot-now
  // button. The download/export button is intentionally missing —
  // backups contain the TLS private key and token hashes, and a
  // one-click web download would be a credential extraction surface.
  // Operators move snapshots offsite with scp/rsync.
  const backupBtn = document.getElementById("backup-now");
  if (backupBtn) {
    backupBtn.addEventListener("click", async () => {
      const oldText = backupBtn.textContent;
      backupBtn.disabled = true;
      backupBtn.textContent = "Snapshotting…";
      try {
        await API.post("/api/backups");
        await refreshBackups();
      } catch (err) {
        alert("Snapshot failed: " + err.message);
      } finally {
        backupBtn.textContent = oldText;
        backupBtn.disabled = false;
      }
    });
  }
  refreshBackups();

  // Tailscale HTTPS panel (moved from dashboard in PR #129) — bind
  // the Re-mint cert button. The panel itself stays hidden until
  // the SSE-driven `renderTailscaleTile` reveals it on a tailscaled
  // node detection event; that handler is page-agnostic and finds
  // the element by id regardless of which page hosts it.
  bindTailscaleRefreshButton();

  // Autocert (ACME / Let's Encrypt) panel (PR 3) — fetch once on
  // page load + repaint on a slow tick (cert renewal is ~60 days,
  // so a 60 s poll is well within the freshness window). Hidden
  // by default; refreshAutocertTile reveals it when the closure
  // returns a non-empty Domain (= operator configured
  // `autocert.enabled: true`).
  //
  // Re-init guard: initSettings() is re-entrant on SPA navigation
  // back to Settings; clear any previous timer before starting a
  // fresh one to prevent poll stacking (CodeRabbit Major on PR
  // #293).
  refreshAutocertTile();
  if (autocertPollTimer) {
    clearInterval(autocertPollTimer);
    autocertPollTimer = null;
  }
  // pageInterval clears the poll when the Settings page scope is torn
  // down (a boost navigation away), so it can't keep hitting the network
  // in the background. The clear-before-set above stays as a defensive
  // no-op — dispatchPageInit runs each init once per scope.
  autocertPollTimer = pageInterval(refreshAutocertTile, 60_000);

  const form = document.getElementById("settings-form");
  const msg = document.getElementById("settings-msg");
  const restartBtn = document.getElementById("restart-btn");
  if (!form) return;

  // Reveal the Restart button on load when a restart is already pending
  // from an earlier action in this session — a settings save, or a UPnP
  // upstream add/remove/edit on the /upnp page (whose banner links here).
  // Without this the button stays `hidden` until the operator happens to
  // make another restart-requiring save on this page, so following that
  // banner led to a page with no way to restart.
  if (restartBtn && restartPending()) {
    restartBtn.hidden = false;
    // The hash lands before this runs, and the browser won't scroll to
    // an element that was hidden at that moment — do it ourselves.
    if (location.hash === "#restart-actions") {
      document.getElementById("restart-actions")
        ?.scrollIntoView({ block: "center" });
    }
  }

  // Snapshot the customEndpoints textarea at page-load so the submit
  // handler can detect operator-driven changes and warn before
  // submitting. The bridge doesn't auto-rotate the cert when this list
  // changes (preserves iOS pinning until the operator explicitly
  // rotates), but the cert STAYS stale relative to the new list until
  // the operator hits Rotate — which loses every paired iOS device.
  // Loud confirm preempts the "saved but doesn't work, why?"
  // troubleshooting cycle.
  const customEndpointsField = form.querySelector("[name=customEndpoints]");
  const customEndpointsOriginal = customEndpointsField
    ? normaliseCustomEndpointsText(customEndpointsField.value)
    : "";

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData(form);

    // Custom-endpoints diff check. Only confirm when the normalised
    // form differs — whitespace / blank-line edits don't trip the
    // dialog, since the server normalises the same way.
    const customNow = normaliseCustomEndpointsText(fd.get("customEndpoints") || "");
    if (customNow !== customEndpointsOriginal) {
      const ok = confirm(
        "Saving will change the advertised endpoint list, but the TLS " +
        "certificate's SAN coverage stays unchanged until you rotate it.\n\n" +
        "iOS devices will only be able to connect to a custom endpoint " +
        "AFTER you rotate the cert (Cert tile → Rotate) and re-pair every " +
        "device.\n\nProceed with saving?"
      );
      if (!ok) return;
    }

    // Resolve the Enrichment tab's source picker into the two base URLs
    // the server expects (PATCH surface unchanged; picker is UI-only).
    const enrichBases = mapEnrichSourceToBases(fd);
    if (enrichBases.err) {
      showMsg(msg, "err", enrichBases.err);
      return;
    }

    const body = {
      libraryName: fd.get("libraryName"),
      listenAddress: fd.get("listenAddress"),
      adminAddress: fd.get("adminAddress"),
      scanIntervalSec: parseInt(fd.get("scanIntervalSec"), 10),
      // Phase C update settings. Checkbox is "on"/null per FormData
      // semantics; coerce to bool so the server's pointer-typed
      // patch field always receives a real value (not null/missing).
      updateAutoInstall: fd.get("updateAutoInstall") === "on",
      updateQuietHours: fd.get("updateQuietHours") || "",
      updateCheckIntervalHours: parseInt(fd.get("updateCheckIntervalHours") || "0", 10),
      // Custom endpoints come in as a single textarea string ("URL\n
      // URL\n…"). Send the textarea form — the server splits on
      // newlines or commas and validates each entry. Sending the
      // explicit string (not the parsed array) lets the server be
      // the single source of truth on splitting/validation.
      customEndpointsText: fd.get("customEndpoints") || "",
      // v1.2 Audio quality opt-in — same checkbox-coerce-to-bool
      // pattern as updateAutoInstall above so the server's
      // pointer-typed patch field always receives a real value.
      upscaleEnabled: fd.get("upscaleEnabled") === "on",
      analysisEnabled: fd.get("analysisEnabled") === "on",
      smartPlaylistsEnabled: fd.get("smartPlaylistsEnabled") === "on",
      // CarPlay-optimize gate (default ON in YAML; only active while
      // upscaleEnabled). Restart-required — the optimize closures are
      // resolved once at `bridge serve` startup.
      optimizeEnabled: fd.get("optimizeEnabled") === "on",
      // Background pre-generation of those optimize variants. NOT
      // restart-required — the sweeper reads the flag live and the PATCH
      // nudges it, so an off→on flip starts work immediately.
      autoOptimizeEnabled: fd.get("autoOptimizeEnabled") === "on",
      // fsnotify library watcher opt-in. Restart-required — the
      // watcher goroutine starts at `bridge serve` startup.
      libraryWatchEnabled: fd.get("libraryWatchEnabled") === "on",
      uploadEnabled: fd.get("uploadEnabled") === "on",
      // Backup cadence + retention. These became editable with the
      // settings consolidation and MUST be listed here: this payload is
      // an explicit allowlist, not a FormData dump, so a field that
      // renders but isn't mapped saves nothing while the page still
      // reports "Saved." — which is worse than not offering it. Caught
      // exactly that way in the browser.
      //
      // parseInt with a NaN guard rather than `|| 0`: 0 is a MEANINGFUL
      // value for the interval (it disables the periodic ticker), so
      // coercing a blank field to 0 would silently turn backups off.
      backupIntervalHours: numOrUndef(fd.get("backupIntervalHours")),
      backupKeep: numOrUndef(fd.get("backupKeep")),
      // Enrich upstream base URLs, resolved from the source picker above
      // (blank = public MusicBrainz / Cover Art defaults; atlas = derived
      // <url>/ws/2 + <url>; custom = the raw Advanced fields). Server
      // validates + normalizes; restart-required.
      enrichMusicBrainzBaseURL: enrichBases.mb,
      enrichCoverArtBaseURL: enrichBases.ca,
      // Rich-tier Atlas metadata opt-in (bios/descriptions via the app
      // ferry). Restart-required; same checkbox-coerce pattern.
      atlasEnabled: fd.get("atlasEnabled") === "on",
      // Acoustic fingerprinting: toggle + set-only key field. A blank key
      // input means "keep the current key" — drop the field entirely
      // (undefined → removed by JSON.stringify → server pointer stays
      // nil) rather than relying on the server's blank-is-noop guard.
      fingerprintEnabled: fd.get("fingerprintEnabled") === "on",
      fingerprintApiKey: (fd.get("fingerprintApiKey") || "").trim() || undefined,
      // PR 4 — Tailscale + mDNS hot-reload fields. Tailscale
      // dropdown value is one of "cli" / "tsnet" / "disabled".
      // mDNS checkbox is the FormData "on"/null shape — coerce
      // to bool, with one caveat: in public mode the field is
      // hidden, so fd.get returns null. Send a literal `false`
      // in that case to keep the server's pointer-typed patch
      // field happy (Validate refuses public+true anyway, so
      // we never accidentally enable mDNS by sending false).
      tailscaleMode: fd.get("tailscaleMode") || "",
      mdnsEnabled: fd.get("mdnsEnabled") === "on",
      // UPnP/DLNA toggle (restart-required). In public mode the checkbox
      // is rendered `disabled`, so omit the field entirely there (undefined
      // → dropped by JSON.stringify → server's pointer stays nil → no
      // change). Coercing a disabled checkbox to false would silently
      // overwrite a stored dlnaEnabled=true and trigger a spurious
      // restartRequired (Gemini on PR #342).
      dlnaEnabled: form.querySelector('input[name="dlnaEnabled"]')?.disabled
        ? undefined
        : fd.get("dlnaEnabled") === "on",
    };
    try {
      const r = await API.patch("/api/settings", body);
      // Sync the Advanced raw inputs to what was actually saved so the
      // picker view and the raw view can't disagree after a save.
      const mbRaw = form.querySelector('[name="enrichMusicBrainzBaseURL"]');
      const caRaw = form.querySelector('[name="enrichCoverArtBaseURL"]');
      if (mbRaw) mbRaw.value = enrichBases.mb;
      if (caRaw) caRaw.value = enrichBases.ca;
      // A key was just persisted: clear the secret from the DOM and flip
      // the placeholder to the on-file state so a later unrelated save
      // can't re-send it and the UI reflects that a key is stored.
      const fpKey = form.querySelector('[name="fingerprintApiKey"]');
      if (fpKey && body.fingerprintApiKey) {
        fpKey.value = "";
        fpKey.placeholder = "key on file — leave blank to keep it";
      }
      // Name the pending fields instead of "some fields": this form
      // submits ~20 at once, and "some" leaves the operator to guess
      // which of their edits is the one still waiting.
      const pending = fieldsNeedingRestart(r);
      showMsg(msg, r.restartRequired ? "warn" : "ok",
        r.restartRequired
          ? `Saved. Needs a restart to apply: ${pending.join(", ")}.`
          : "Saved.");
      // Sticky across navigation: a save that needs a restart must still
      // be pending when the operator comes back to this page. Never
      // CLEAR it here — an unrelated hot-applied save doesn't undo a
      // restart another change is still waiting on.
      if (r.restartRequired) {
        markRestartPending(true);
        restartBtn.hidden = false;
      } else if (!restartPending()) {
        restartBtn.hidden = true;
      }
    } catch (err) {
      showMsg(msg, "err", "Save failed: " + err.message);
    }
  });

  // The dataset attribute is rendered by the settings template from
  // `settingsResponse.IsSupervised`, which the server populates from
  // `supervision.IsSupervised()`. When false, the bridge isn't under
  // launchd / systemd / SCM and `os.Exit(0)` won't be relaunched —
  // pre-fix this confirm dialog claimed otherwise ("until the
  // service manager relaunches it") and the operator was left
  // staring at a dead admin page wondering why nothing came back.
  // Branch on the live flag so the dialog tells the truth.
  restartBtn?.addEventListener("click", async () => {
    const supervised = restartBtn.dataset.supervised === "true";
    const prompt = supervised
      ? "Restart the bridge now? The page will become unreachable until the service manager relaunches it (~1–2 s)."
      : "Stop the bridge now? This bridge isn't running under a service manager, so the page will go down and you'll need to start it again manually.";
    if (!confirm(prompt)) return;
    showMsg(msg, "warn", "Waiting for in-flight streams to finish…");
    try {
      // The handler now waits for /v1/read + /v1/download to drain before
      // exiting, and only answers once it has. Awaiting it is therefore
      // the moment the restart is genuinely about to happen — and the
      // body says whether anyone got cut off.
      const r = await API.post("/api/restart");
      // Honoured — the pending-restart debt is settled.
      markRestartPending(false);
      let post = supervised
        ? "Restart signalled. Reload the page in a few seconds."
        : "Stop signalled. Start the bridge again manually, then reload.";
      if (r && r.drained === false && r.inflight > 0) {
        // Say it plainly rather than reporting a clean restart: someone
        // was listening and we went ahead anyway.
        post = `${post} (${r.inflight} stream${r.inflight === 1 ? "" : "s"} still ` +
          `in flight after ${Math.round((r.waitedMs || 0) / 1000)}s — interrupted.)`;
      }
      showMsg(msg, "warn", post);
    } catch (err) {
      if (isExpectedRestartDisconnect(err)) {
        // The bridge writes 202 then `os.Exit(0)` after a 100 ms
        // grace window — we may catch the response (`TypeError`
        // when the connection drops mid-read, or `SyntaxError`
        // when the empty body fails JSON.parse). Both mean
        // "request was honoured, server is on its way out."
        markRestartPending(false);
        const post = supervised
          ? "Restart signalled (server went away)."
          : "Stop signalled (server went away). Start it again manually.";
        showMsg(msg, "warn", post);
      } else {
        // A real HTTP error (4xx/5xx) is wrapped by `errorFrom-
        // Response` as a plain `Error` whose message starts with
        // the status code. Don't claim "signalled" — the request
        // never landed. CodeRabbit on PR #124 caught the prior
        // catch path conflating these two branches.
        const verb = supervised ? "Restart" : "Stop";
        showMsg(msg, "err", `${verb} request failed: ${err.message}`);
      }
    }
  });

  // Audio-quality + audio-analysis tiles are driven by the SSE
  // `upscale` / `analysis` events (applyUpscaleStats / applyAnalysisStats)
  // — the initial-emit hydrates them on page load and the 5 s medium tick
  // refreshes them, diff-suppressed. No per-tab 5 s pollers. The
  // clear-all-variants modal still does a one-shot refresh for immediate
  // post-delete feedback.
  initUpscaleClearAllModal();
  initUpscaleTarget();
}

// Required typed phrase for the "Clear all upscaled variants"
// destructive action. Matches the `bridge artwork --gc
// --confirm GC-ARTWORK` CLI convention — exact case + dash
// sensitivity so an accidental click can't tab-+-Enter through.
const UPSCALE_CLEAR_PHRASE = "delete-all-variants";

function initUpscaleClearAllModal() {
  const btn = document.getElementById("upscale-clear-all-btn");
  const modal = document.getElementById("upscale-clear-modal");
  if (!btn || !modal) return; // settings page not loaded yet
  const input = document.getElementById("upscale-clear-modal-input");
  const submitBtn = document.getElementById("upscale-clear-modal-submit");
  const statusEl = document.getElementById("upscale-clear-modal-status");

  btn.addEventListener("click", () => {
    input.value = "";
    submitBtn.disabled = true;
    statusEl.hidden = true;
    statusEl.textContent = "";
    modal.showModal();
    // Defer focus to next tick so the dialog has time to mount.
    setTimeout(() => input.focus(), 0);
  });

  modal.querySelectorAll("[data-close]").forEach((closeBtn) =>
    closeBtn.addEventListener("click", () => modal.close())
  );

  input.addEventListener("input", () => {
    submitBtn.disabled = input.value !== UPSCALE_CLEAR_PHRASE;
  });

  submitBtn.addEventListener("click", async () => {
    // Belt-and-braces: re-check the phrase here too. The disabled
    // gate above should make this branch unreachable, but a future
    // JS regression that leaves the button enabled mustn't be able
    // to fire the delete without the operator's exact typed
    // confirmation.
    if (input.value !== UPSCALE_CLEAR_PHRASE) return;
    submitBtn.disabled = true;
    statusEl.hidden = false;
    statusEl.textContent = "Deleting every variant…";
    try {
      const res = await fetch("/api/upscale/variants?confirm=true", {
        method: "DELETE",
        headers: { "Content-Type": "application/json" },
      });
      if (res.status === 503) {
        // Bridge upscale feature toggled off mid-flight. Re-enable
        // the submit button so the operator can retry after
        // toggling the feature back on without having to close
        // and re-open the modal (which would also force them to
        // re-type the confirmation phrase). Mirror of the
        // variant-delete 503 handler — same
        // reasoning, same fix shape. CodeRabbit Minor on PR #220
        // (post-merge — comment landed 37 s after merge).
        statusEl.textContent = "Upscale feature is disabled on this bridge.";
        submitBtn.disabled = false;
        return;
      }
      if (!res.ok) {
        const body = await res.text();
        throw new Error(body || `HTTP ${res.status}`);
      }
      const data = await res.json();
      statusEl.textContent =
        `Done · deleted ${data.deletedCount} variants, ` +
        `freed ${formatBytes(data.freedBytes ?? 0)}.`;
      // Refresh stats so the "Cached variants" / "Cached on disk"
      // counters reflect the post-delete state. The SSE `upscale` frame
      // would catch up within 5 s anyway, but this one-shot fetch closes
      // the visual loop on the operator's action immediately.
      refreshUpscaleStats();
      // Auto-close after a short readback window so the operator
      // sees the result before the modal disappears.
      setTimeout(() => {
        if (modal.open) modal.close();
      }, 1500);
    } catch (err) {
      statusEl.textContent = `Couldn’t delete: ${err.message}`;
      submitBtn.disabled = false; // allow retry without retyping
    }
  });
}

// applyUpscaleStats renders the Settings "Audio quality" tile from an
// /api/upscale/stats payload. Driven by the SSE `upscale` event (and the
// clear-all modal's one-shot refreshUpscaleStats). Looks up the tile
// itself, so it's a no-op on every page except Settings.
function applyUpscaleStats(r) {
  const tile = document.getElementById("upscale-stats");
  if (!tile || !r) return;
  // Hide the whole tile when the feature has never been used (no cached
  // variants AND feature is currently off). A disabled feature with
  // cached files keeps the tile up so the operator sees historical state
  // and disk usage.
  const hasHistory = r.cachedVariants > 0;
  if (!r.enabled && !hasHistory) {
    tile.hidden = true;
    return;
  }
  tile.hidden = false;
  // Honest per-kind split — the combined "cached variants" line used
  // to lump upscaled + optimized together, so an all-optimize library
  // read as if it had upscaled work it never did.
  const upN = r.upscaledVariants ?? 0;
  const optN = r.optimizedVariants ?? 0;
  setText("upscale-upscaled", `${upN} file${upN === 1 ? "" : "s"} · ${formatBytes(r.upscaledBytes ?? 0)}`);
  setText("upscale-optimized", `${optN} file${optN === 1 ? "" : "s"} · ${formatBytes(r.optimizedBytes ?? 0)}`);
  setText("upscale-cached-count", r.cachedVariants ?? 0);
  setText("upscale-cached-bytes", formatBytes(r.cachedBytes ?? 0));
  if (r.pool) {
    setText("upscale-workers", r.pool.workers);
    setText("upscale-queue", r.pool.queueLen + " / " + r.pool.queueCap);
    setText("upscale-inflight", r.pool.inflight);
    setText("upscale-done", r.pool.done);
    setText("upscale-failed", r.pool.failed);
  } else {
    // Feature is off but we have cached variants — show the historical
    // fields, em-dash the live ones to communicate "no live pool".
    setText("upscale-workers", "—");
    setText("upscale-queue", "—");
    setText("upscale-inflight", "—");
    setText("upscale-done", "—");
    setText("upscale-failed", "—");
  }
  // Toggle the "Clear all upscaled variants" button: nothing to clear
  // when the cache is empty, so disable to communicate that visually.
  const clearBtn = document.getElementById("upscale-clear-all-btn");
  if (clearBtn) {
    clearBtn.disabled = !(r.cachedVariants > 0);
  }
}

// initUpscaleTarget seeds the Settings → Audio "Upscale target" picker
// from GET /api/upscale/target and wires the Apply button to the
// existing PATCH /api/upscale/target endpoint (the same one the batch
// coordinator reads per submission — hot-applied, no restart). The
// selects are deliberately name-less so they never serialise into the
// surrounding settings <form>'s Save payload; this control owns its
// own apply path. No-op on pages without the picker.
function initUpscaleTarget() {
  const rateSel = document.getElementById("upscale-target-rate");
  const bitsSel = document.getElementById("upscale-target-bits");
  const applyBtn = document.getElementById("upscale-target-apply");
  const msg = document.getElementById("upscale-target-msg");
  if (!rateSel || !bitsSel || !applyBtn) return;

  // Select `value` on a <select> silently no-ops when no <option>
  // matches — a bootstrap-seeded or hand-PATCHed off-list value would
  // then render as whatever option happens to be first, and Apply
  // would silently rewrite the stored target. Inject the live value
  // as an option instead so the picker always shows reality.
  const selectValue = (sel, value) => {
    const v = String(value);
    if (![...sel.options].some((o) => o.value === v)) {
      const opt = document.createElement("option");
      opt.value = v;
      opt.textContent = sel === rateSel ? `${(value / 1000).toFixed(1)} kHz` : `${v}-bit`;
      sel.appendChild(opt);
    }
    sel.value = v;
  };

  (async () => {
    try {
      const t = await API.get("/api/upscale/target");
      if (t.targetRate > 0) selectValue(rateSel, t.targetRate);
      if (t.targetBits > 0) selectValue(bitsSel, t.targetBits);
      applyBtn.disabled = false;
    } catch (err) {
      // 503 = manifest store not wired (test harness) — leave the
      // button disabled so Apply can't fire against a dead endpoint.
      showMsg(msg, "err", `Couldn’t load the current target: ${err.message}`);
    }
  })();

  applyBtn.addEventListener("click", async () => {
    const body = {
      targetRate: parseInt(rateSel.value, 10),
      targetBits: parseInt(bitsSel.value, 10),
    };
    applyBtn.disabled = true;
    try {
      const r = await API.patch("/api/upscale/target", body);
      showMsg(msg, "ok",
        `Target set to ${(r.targetRate / 1000).toFixed(1)} kHz / ${r.targetBits}-bit — applies to the next generation request.`);
    } catch (err) {
      showMsg(msg, "err", `Couldn’t set the target: ${err.message}`);
    } finally {
      applyBtn.disabled = false;
    }
  });
}

// refreshUpscaleStats does a one-shot fetch + render — used by the
// clear-all modal to close the visual loop immediately (the SSE `upscale`
// frame would otherwise catch up within 5 s).
async function refreshUpscaleStats() {
  try {
    applyUpscaleStats(await API.get("/api/upscale/stats"));
  } catch (err) {
    console.warn("upscale stats fetch failed:", err);
  }
}

// applyUpscale is the `upscale` SSE entry point — it drives BOTH the
// Settings "Audio quality" tile and the Jobs page "Workers" grid. Each
// renderer is a no-op on the page that lacks its container, so one event
// feeds whichever surface is open.
// The pool's lifetime completion count as of the last frame, so the
// player can tell when generated variants have actually landed.
// `null` until the first frame: the initial snapshot on a bridge that
// has done work since boot must not read as a batch finishing now.
let lastUpscaleCompleted = null;

function applyUpscale(r) {
  applyUpscaleStats(r); // Settings tile
  renderWorkerGrid(r); // Jobs page live pipeline
  notifyUpscaleProgress(r); // Player variant panels
}

/**
 * Tell the player when generated variants have actually landed.
 *
 * The player rides this stream rather than opening its own — boot.js is
 * loaded after app.js precisely so it can reuse this one EventSource.
 *
 * The signal is the pool's lifetime DONE + FAILED count advancing, not
 * a busy→idle edge. The edge looked like the natural choice and is
 * wrong: a small batch can start and finish between two frames, so the
 * client never observes `busy` at all and the edge never fires. Two
 * tracks on two workers reproduced it every time — the server had the
 * variants, the panel sat at 0 / 2 until a manual reload.
 *
 * The counters do not have that hole, because SSE frames are
 * diff-suppressed: a completion CHANGES the payload, so a completion
 * always produces a frame, however brief the work was.
 *
 * `settled` additionally reports that the queue is now empty, which the
 * player uses to bypass its own throttle — a long batch creeps under
 * the throttle, and its final result must never be the update that gets
 * throttled away.
 */
function notifyUpscaleProgress(r) {
  const pool = r?.pool;
  if (!pool) return;
  const completed = (pool.done || 0) + (pool.failed || 0);
  const idle = !(pool.inflight > 0 || pool.queueLen > 0);
  const advanced = lastUpscaleCompleted !== null && completed > lastUpscaleCompleted;
  lastUpscaleCompleted = completed;
  if (!advanced) return;
  window.dispatchEvent(new CustomEvent("bridge:upscale", {
    detail: { settled: idle },
  }));
}

// workerElapsedTimer ticks the per-worker elapsed labels locally (1 s) so
// the SSE frame can stay diff-suppressed while a job runs (it carries an
// immutable startedAtUnixMs, not a server-computed elapsed). Only runs
// while at least one worker is busy.
let workerElapsedTimer = null;

// renderWorkerGrid paints the Jobs page "Workers" panel from the SSE
// `upscale` payload's pool.activeWorkers. No-op off the Jobs page. The
// panel hides when the upscale feature is off (no pool → no workers).
function renderWorkerGrid(r) {
  const panel = document.getElementById("workers-panel");
  const grid = document.getElementById("workers-grid");
  if (!panel || !grid) return; // not on the Jobs page
  const workers = (r && r.pool && r.pool.activeWorkers) || [];
  if (workers.length === 0) {
    panel.hidden = true;
    grid.textContent = "";
    stopWorkerElapsedTicker();
    return;
  }
  panel.hidden = false;
  grid.textContent = "";
  let anyBusy = false;
  workers.forEach((w) => {
    const row = document.createElement("div");
    row.className = w.busy ? "worker-row busy" : "worker-row idle";

    const head = document.createElement("div");
    head.className = "worker-head";
    const id = document.createElement("span");
    id.className = "worker-id";
    id.textContent = `Worker #${(w.workerId ?? 0) + 1}`;
    head.appendChild(id);

    const status = document.createElement("span");
    status.className = "worker-status";
    if (w.busy) {
      // Normalise backslashes first — a Windows host's library-relative
      // paths can carry `\` separators (Gemini on #437).
      const name = w.sourceRel ? w.sourceRel.replace(/\\/g, "/").split("/").pop() : "(working)";
      status.textContent = name;
      status.title = w.sourceRel || "";
    } else {
      status.textContent = "Idle";
    }
    head.appendChild(status);

    if (w.busy && w.startedAtUnixMs) {
      anyBusy = true;
      const el = document.createElement("span");
      el.className = "worker-elapsed";
      el.dataset.started = String(w.startedAtUnixMs);
      el.textContent = elapsedStr(w.startedAtUnixMs);
      head.appendChild(el);
    }
    row.appendChild(head);

    if (w.busy) {
      const chain = document.createElement("div");
      chain.className = "worker-chain";
      chain.textContent = signalChain(w);
      row.appendChild(chain);
    }
    grid.appendChild(row);
  });
  if (anyBusy) startWorkerElapsedTicker();
  else stopWorkerElapsedTicker();
}

// signalChain renders an audiophile signal-chain string for a busy
// worker, e.g. "Upscale: 44.1 kHz/16-bit ➔ 176.4 kHz/24-bit · SoX
// very-high · linear phase · clip-guarded". Source bit depth is shown
// only when known (the bridge extracts it for FLAC/DSD; 0 = unknown is
// omitted). The "linear phase" + "clip-guarded" labels are truthful
// build-constants: every conversion pins `rate … -L` (linear phase) and
// `-G` (gain-guard) — see JobSpec.SoxArgs in internal/transcode. They're
// rendered as static labels rather than per-job fields because they
// don't vary; if the phase/guard ever becomes per-job, promote them to
// ActiveJob fields so the label can't drift from the actual command.
function signalChain(w) {
  const src = fmtRate(w.sourceSampleRate) + (w.sourceBits ? `/${w.sourceBits}-bit` : "");
  const tgt = fmtRate(w.targetSampleRate) + (w.targetBits ? `/${w.targetBits}-bit` : "");
  const kind = w.kind === "optimize" ? "Optimize" : "Upscale";
  const q = w.quality ? ` · SoX ${w.quality}` : "";
  // Gate the DSP labels on the same signal (a real sox conversion) that
  // gates the quality preset, so a placeholder/idle row stays terse.
  const dsp = w.quality ? " · linear phase · clip-guarded" : "";
  return `${kind}: ${src} ➔ ${tgt}${q}${dsp}`;
}

function fmtRate(hz) {
  if (!hz) return "?";
  const k = hz / 1000;
  return (Number.isInteger(k) ? k : k.toFixed(1)) + " kHz";
}

function elapsedStr(startedMs) {
  const s = Math.max(0, Math.floor((Date.now() - startedMs) / 1000));
  const m = Math.floor(s / 60);
  const sec = s % 60;
  return m > 0 ? `${m}m ${sec}s` : `${sec}s`;
}

function startWorkerElapsedTicker() {
  if (workerElapsedTimer) return;
  workerElapsedTimer = setInterval(() => {
    const els = document.querySelectorAll(".worker-elapsed");
    if (els.length === 0) {
      stopWorkerElapsedTicker();
      return;
    }
    els.forEach((el) => {
      const s = el.dataset.started;
      if (s) el.textContent = elapsedStr(Number(s));
    });
  }, 1000);
}

function stopWorkerElapsedTicker() {
  if (workerElapsedTimer) {
    clearInterval(workerElapsedTimer);
    workerElapsedTimer = null;
  }
}

// applyAnalysisStats renders the Settings "Audio analysis" tile from an
// /api/analysis/stats payload (SSE `analysis` event). No-op off Settings.
function applyAnalysisStats(r) {
  if (!r) return;
  // Jobs-page bindings (SSE is fresher than the 10 s /api/jobs poll
  // for the pool + sweep lines). Guarded on the card's presence so
  // this stays a no-op on every other page.
  if (document.getElementById("job-analysis-card")) {
    const q = document.getElementById("job-analysis-queue");
    if (q) {
      q.textContent = r.pool
        ? `${r.pool.queueLen} queued · ${r.pool.inflight} in flight · ${r.pool.done} done · ${r.pool.failed} failed (${r.pool.workers} worker${r.pool.workers === 1 ? "" : "s"})`
        : "—";
    }
    if (r.sweep) {
      setText("job-analysis-sweep", r.sweep.running
        ? "sweeping now"
        : r.sweep.lastFinishedAt ? `last swept ${agoOrDash(r.sweep.lastFinishedAt)}` : "not yet run");
      setText("job-analysis-next", formatInFuture(r.sweep.nextDueAt));
    }
  }
  const tile = document.getElementById("analysis-stats");
  if (!tile) return;
  // Hide the tile when the feature has never been used (no cached
  // waveforms AND currently off). A disabled feature with cached files
  // keeps the tile up so the operator sees historical state.
  const hasHistory = (r.cachedWaveforms ?? 0) > 0;
  if (!r.enabled && !hasHistory) {
    tile.hidden = true;
    return;
  }
  tile.hidden = false;
  setText("analysis-cached-count", r.cachedWaveforms ?? 0);
  setText("analysis-cached-bytes", formatBytes(r.cachedBytes ?? 0));
  setText("analysis-storage-path", r.storagePath ?? "—");
}

function formatBytes(n) {
  if (!n) return "0 B";
  const kb = 1024, mb = kb * 1024, gb = mb * 1024;
  if (n >= gb) return (n / gb).toFixed(n >= 10 * gb ? 0 : 1) + " GB";
  if (n >= mb) return (n / mb).toFixed(n >= 10 * mb ? 0 : 1) + " MB";
  if (n >= kb) return (n / kb).toFixed(n >= 10 * kb ? 0 : 1) + " KB";
  return n + " B";
}

function showMsg(el, kind, text) {
  if (!el) return;
  el.textContent = text;
  el.className = "status " + kind;
  el.hidden = false;
}

// normaliseCustomEndpointsText canonicalises a textarea value so the
// "did the operator change anything?" diff doesn't trip on cosmetic
// whitespace. Mirrors the server-side splitter:
//   - Replace commas with newlines so a paste-friendly comma list and
//     a one-per-line list produce identical normal forms.
//   - Trim each line; drop blanks.
//
// **Order is preserved** (Qodo bot review on PR #93). The server's
// splitter and `advertise.Endpoints()` both keep input order, and the
// position of each ClassCustom entry affects iOS connection-attempt
// priority. Sorting in the diff would suppress the confirm dialog for
// a reorder-only edit, but the change WOULD be persisted and shift
// which custom URL iOS tries first — confusing for the operator.
// Reorder-only edits now correctly fire the dialog so the operator
// can confirm or cancel before saving.
function normaliseCustomEndpointsText(s) {
  return String(s)
    .replace(/,/g, "\n")
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "")
    .join("\n");
}

// --- live updates over SSE ---
//
// Replaces the previous per-page setInterval polling against
// /api/stats (3 s), /api/endpoints (30 s), /api/pairing (3 s),
// /api/updates (3 s, inside the stats tick), and /api/tailscale/status
// (30 s). The bridge multiplexes named events at three cadences over
// a single connection (see internal/admin/handlers_events.go), and
// diff-suppresses unchanged frames so an idle bridge produces zero
// wire traffic between heartbeats.
//
// Each apply* / render* handler is page-aware: it self-guards on the
// presence of its target DOM elements, so this single EventSource
// works on every admin page (Dashboard / Devices / Settings /
// Library) without per-page wiring. EventSource auto-reconnects on
// transport error; the connection-status badge surfaces "reconnecting"
// to the operator until onopen fires again.

function applyConnState(state) {
  // States: "connected" (steady, subtly green or hidden),
  // "reconnecting" (amber pill — EventSource is between attempts),
  // "disconnected" (red pill — used only for terminal close, which
  // EventSource doesn't reach on its own; reserved for future use).
  const el = document.getElementById("conn-status");
  if (!el) return;
  el.dataset.state = state;
  el.textContent =
    state === "connected" ? "Live" :
    state === "reconnecting" ? "Reconnecting…" :
    "Disconnected";
}

// Active EventSource. Tracked at module scope so the
// visibility-change handler can close + replace it on a long sleep
// resume (laptop closed for hours; the in-flight ES may have silently
// failed to reconnect post-wake).
let activeEventSource = null;
// Last time we saw evidence the SSE stream was alive — bumped on
// `onopen`, on every successful event, AND on visibilitychange to
// `visible`. Used by the visibility-restore handler to decide whether
// the user has been away long enough to warrant a force-cycle
// (CodeRabbit on PR #161 caught that the prior `lastEventSourceConnectAt`
// was stream-creation time, NOT user-last-seen time, so a long-running
// stream + short tab switch would falsely trigger reconnect).
let lastEventSourceSeenAt = 0;
const SSE_RECONNECT_IDLE_THRESHOLD_MS = 60 * 1000;

function startEventStream() {
  // EventSource is a built-in browser API; no polyfill needed for
  // any iOS / desktop browser the admin console targets.
  const es = new EventSource("/api/events");
  activeEventSource = es;
  lastEventSourceSeenAt = Date.now();

  // Wrap each handler so we can refresh `lastEventSourceSeenAt` on
  // every successful frame — that way a long-running stream that's
  // continuously delivering events keeps its "seen" timestamp fresh
  // and the visibility-restore handler doesn't mis-classify it as
  // idle when the user briefly switches tabs.
  const seen = (fn) => (e) => { lastEventSourceSeenAt = Date.now(); fn(e); };
  es.addEventListener("stats",       seen((e) => safeApply("stats",       e.data, applyStats)));
  es.addEventListener("composition", seen((e) => safeApply("composition", e.data, applyComposition)));
  es.addEventListener("sources",     seen((e) => safeApply("sources",     e.data, applySources)));
  es.addEventListener("enrichment",  seen((e) => safeApply("enrichment",  e.data, applyEnrichment)));
  es.addEventListener("endpoints",   seen((e) => safeApply("endpoints",   e.data, applyEndpoints)));
  es.addEventListener("pairing",     seen((e) => safeApply("pairing",     e.data, applyPairing)));
  es.addEventListener("updates",     seen((e) => safeApply("updates",     e.data, renderUpdateTile)));
  es.addEventListener("tailscale",   seen((e) => safeApply("tailscale",   e.data, renderTailscaleTile)));
  es.addEventListener("upscale",     seen((e) => safeApply("upscale",     e.data, applyUpscale)));
  es.addEventListener("analysis",    seen((e) => safeApply("analysis",    e.data, applyAnalysisStats)));

  es.onopen = () => {
    lastEventSourceSeenAt = Date.now();
    applyConnState("connected");
  };
  // EventSource fires onerror on every transport hiccup AND between
  // reconnect attempts. Transient network blips → readyState === 0
  // (CONNECTING) and the browser will retry on its own backoff.
  // Terminal failures → readyState === 2 (CLOSED), at which point
  // we surface "disconnected" instead of "reconnecting".
  es.onerror = () => {
    applyConnState(es.readyState === EventSource.CLOSED ? "disconnected" : "reconnecting");
  };
}

// SSE reconnection resilience for the global pairing badge — the
// badge is only useful if SSE keeps delivering events through laptop
// sleep / wake cycles. The browser's auto-reconnect handles the
// common case, but a long sleep (laptop closed for hours) can leave
// the in-flight EventSource silently failed post-wake. When the tab
// becomes visible again after >60 s away, force-close + re-arm the
// EventSource and immediately fetch /api/pairing once to backfill any
// pairing requests that arrived during the sleep gap.
function handleVisibilityRestore() {
  if (document.visibilityState !== "visible") return;
  const idleMs = Date.now() - lastEventSourceSeenAt;
  // Refresh `seen` on visibility change either way, so a brief
  // tab-out followed by a longer tab-out doesn't accumulate idle time
  // across the gap. Only the gap between successive `visible` states
  // counts.
  lastEventSourceSeenAt = Date.now();
  if (idleMs < SSE_RECONNECT_IDLE_THRESHOLD_MS) return;
  // Force-cycle the connection — auto-reconnect doesn't always cover
  // post-sleep recovery cleanly.
  if (activeEventSource) {
    try { activeEventSource.close(); } catch { /* ignore */ }
  }
  startEventStream();
  // Belt-and-braces: backfill the pairing snapshot directly so the
  // global badge updates immediately even if the first SSE frame
  // takes a moment to arrive.
  (async () => {
    try {
      applyPairing(await API.get("/api/pairing"));
    } catch {
      // Transient — the next SSE frame will catch up.
    }
  })();
}

function safeApply(name, raw, fn) {
  try {
    fn(JSON.parse(raw));
  } catch (err) {
    // Bad JSON is an admin-side bug, not an operator-actionable
    // condition. Log to the console rather than alert() — the SSE
    // stream may deliver dozens of events per minute under load and
    // any popup loop would be hostile.
    console.error(`SSE ${name}: parse/apply failed`, err);
  }
}

// =============================================================
// Jobs page — background-activity cards + upscale batch history
// =============================================================

// makeVisibilityChain builds a setTimeout chain (not setInterval, so a
// slow response can't stack overlapping requests — CodeRabbit on PR
// #205) that PAUSES while the tab is hidden: admin tabs live for days
// in the background, and unconditional polling would be pure server
// load for data nobody is looking at. When the chain pauses (tick
// fires while hidden) it stops re-arming; resume() re-enters it — the
// caller wires that to visibilitychange. The page-root guard stops a
// stale chain from a previous page's DOM surviving into a future SPA-
// style navigation.
function makeVisibilityChain(fn, ms) {
  let timer = null;
  let ranOnce = false;
  let inFlight = false;
  const tick = async () => {
    timer = null;
    if (!document.getElementById("jobs-page-root")) return;
    // First paint always runs — a page opened in a background tab
    // still gets content for when it's foregrounded. Only the
    // RECURRING polls pause while hidden; resume() re-arms them.
    if (document.hidden && ranOnce) return;
    ranOnce = true;
    // Re-entry guard: while fn() is awaited, timer is null — a
    // visibilitychange in that window would otherwise let resume()
    // start a SECOND concurrent chain that never converges (Gemini
    // HIGH on PR #621).
    if (inFlight) return;
    inFlight = true;
    try { await fn(); } catch { /* fn owns its error surface */ }
    finally { inFlight = false; }
    timer = setTimeout(tick, ms);
  };
  return {
    start() { if (timer === null && !inFlight) tick(); },
    resume() { if (timer === null && !inFlight && !document.hidden) tick(); },
  };
}

function initJobs() {
  if (!document.getElementById("jobs-page-root")) return;
  mountJobTrays();

  // Two chains: the 5 s upscale-batch table (fast — live counters
  // during a batch) and the 10 s /api/jobs snapshot (slow-moving
  // per-job state). Both pause in background tabs.
  const batches = makeVisibilityChain(jobsRefresh, 5000);
  const snapshot = makeVisibilityChain(jobsSnapshotRefresh, 10000);
  batches.start();
  snapshot.start();
  document.addEventListener("visibilitychange", () => {
    batches.resume();
    snapshot.resume();
  }, { signal: pageSignal() });

  wireJobButton("jobs-scan-now", () => API.post("/api/scan"), "Scan started");
  wireJobButton("jobs-analyze-now", () => API.post("/api/analysis/sweep"), "Sweep queued");
  wireJobButton("jobs-fp-now", () => API.post("/api/fingerprint/sweep"), "Sweep queued");
  wireJobButton("jobs-dupes-restamp", () => API.post("/api/duplicates/sweep"), "Re-evaluate queued");
  wireJobButton("jobs-ao-now", () => API.post("/api/upscale/auto-optimize/sweep"), "Sweep queued");

  // Fingerprint Enable: a settings PATCH rather than a job trigger, so it
  // gets its own handler instead of wireJobButton — the post-click state
  // must LATCH (the /api/jobs snapshot keeps reporting the startup flag
  // until the restart, and a refresh must not reset the button).
  const fpEnable = document.getElementById("jobs-fp-enable");
  fpEnable?.addEventListener("click", async () => {
    fpEnable.disabled = true;
    try {
      await API.patch("/api/settings", { fingerprintEnabled: true });
      fpEnable.dataset.latched = "true";
      fpEnable.textContent = "Enabled — restart to apply";
      const hint = document.getElementById("job-fp-hint");
      if (hint) {
        hint.textContent =
          "Enabled. Add your AcoustID key (if you haven't yet) and restart " +
          "the bridge to start fingerprinting. ";
        const a = document.createElement("a");
        a.href = "/settings?tab=enrichment";
        a.textContent = "Fingerprint settings";
        hint.appendChild(a);
      }
    } catch (err) {
      fpEnable.disabled = false;
      fpEnable.textContent = "Enable failed — retry";
    }
  });
  wireJobButton("jobs-backup-now", () => API.post("/api/backups"), "Snapshot written");
  wireJobButton("jobs-mix-regen", async () => {
    // Synchronous server-side regeneration — can take a while on a
    // big library, hence the disabled+spinner treatment from
    // wireJobButton's in-flight state.
    const r = await API.post("/api/smart-playlists/regenerate");
    return `${r.families ?? 0} families`;
  }, null);
  wireJobButton("jobs-upd-check", async () => {
    const r = await API.post("/api/updates/check");
    setText("job-upd-status", updateStatusLine(r));
    return "Checked";
  }, null);

  // "Retry missing" on the enrichment card — same endpoint + repaint
  // contract as the dashboard's button (initDashboard wires that one;
  // this page wires its own copy).
  const retry = document.getElementById("enrich-retry");
  if (retry) {
    retry.addEventListener("click", async () => {
      retry.disabled = true;
      try {
        const r = await API.post("/api/enrichment/retry");
        if (r && r.enrichment) applyEnrichment(r.enrichment);
        retry.textContent = "Re-queued";
      } catch (err) {
        retry.textContent = err.message.includes("rate_limited") ? "Try again in a minute" : "Retry failed";
      }
      setTimeout(() => { retry.textContent = "Retry missing"; retry.disabled = false; }, 4000);
    });
  }
}


// mountJobTrays hangs a feature tray off every job card that HAS a
// switch. The Jobs page shows what each job is doing; before this, the
// switch that governs it lived one page away, so "is analysis on?" was
// answered here and "turn it on" was answered in Settings.
//
// The restart badges mirror the Settings page field-for-field on
// purpose. They are a PREDICTION — the authoritative answer is the
// restartRequired flag on the PATCH response, which the tray reports
// after the save either way — so a badge that disagreed with Settings
// would be two different predictions for one field. autoOptimizeEnabled
// carries none, exactly as it carries none there: it hot-applies
// whenever a sweeper is wired.
function mountJobTrays() {
  const head = (id) => document.querySelector(`#${id} .panel-head`);

  attachFeatureTray(head("job-scan-card"), {
    title: "Library scanner",
    blurb: "The periodic full walk is always on. These change how often it " +
      "runs and whether newly-dropped files are picked up before the next one.",
    rows: [
      {
        field: "scanIntervalSec", type: "number", min: 60,
        label: "Full scan every", unit: "seconds",
        hint: "Defaults to 21600 (6 h). Raise it for a mechanical NAS you want " +
          "to let spin down; the on-demand trigger above still works either way.",
      },
      {
        field: "libraryWatchEnabled", type: "switch", restart: true,
        label: "Watch folders for changes",
        hint: "Files appear within seconds instead of at the next scan. Best on " +
          "local disks — a NAS on a flaky network can thrash incremental scans.",
      },
    ],
    link: { href: "/settings?tab=general", text: "Library settings →" },
  });

  attachFeatureTray(head("enrichment-panel"), {
    title: "Enrichment",
    blurb: "Covers, artist and release IDs are always fetched. This adds the " +
      "rich tier — biographies and release descriptions — from your own Atlas.",
    rows: [
      {
        field: "atlasEnabled", type: "switch", restart: true,
        label: "Atlas biographies and descriptions",
        hint: "Needs a reachable Atlas mirror; set its URL under enrichment settings.",
      },
    ],
    link: { href: "/settings?tab=enrichment", text: "Enrichment settings →" },
  });

  attachFeatureTray(head("job-analysis-card"), {
    title: "Audio analysis",
    blurb: "Decodes each track once through sox to pre-compute a waveform, " +
      "loudness, key and tempo.",
    rows: [
      {
        field: "analysisEnabled", type: "switch", label: "Analyse the library",
        hint: "Sidecars survive a toggle, so turning this off and on again " +
          "costs nothing already computed.",
      },
    ],
    link: { href: "/settings?tab=audio", text: "Audio settings →" },
  });

  attachFeatureTray(head("job-fp-card"), {
    title: "Acoustic fingerprinting",
    blurb: "Identifies tracks whose tags no text match can fix, by fingerprinting " +
      "the audio itself and asking AcoustID.",
    rows: [
      {
        field: "fingerprintEnabled", type: "switch", label: "Fingerprint unmatched tracks",
        hint: "Needs fpcalc on the bridge host and a free AcoustID application " +
          "key — without either it degrades to off at startup.",
      },
    ],
    link: { href: "/settings?tab=enrichment", text: "Fingerprint settings →" },
  });

  attachFeatureTray(head("job-ao-card"), {
    title: "CarPlay pre-generation",
    blurb: "Builds the optimized copies ahead of time instead of waiting for a " +
      "device to ask for one. All three switches have to be on for anything to run.",
    rows: [
      { field: "upscaleEnabled", type: "switch", label: "PCM upscaling" },
      { field: "optimizeEnabled", type: "switch", label: "CarPlay-optimized variants" },
      {
        field: "autoOptimizeEnabled", type: "switch",
        label: "Pre-generate them",
        hint: "Applies immediately. Spends disk and CPU on tracks nobody has " +
          "asked for yet, newest first, and stops before the variants volume fills.",
      },
    ],
    link: { href: "/settings?tab=audio", text: "Audio settings →" },
  });

  attachFeatureTray(head("job-dupes-card"), {
    title: "Duplicate serving",
    blurb: "Which copy of a duplicated recording the bridge serves. Nothing is " +
      "ever deleted or moved — this changes what is offered, not what is stored.",
    rows: [
      {
        field: "duplicatesFilter", type: "select",
        label: "Policy",
        options: [
          ["highest-quality", "Highest quality"],
          ["same-format", "Same format only"],
          ["off", "Off — serve everything"],
        ],
        hint: "Applies immediately: saving re-runs the stamping pass. " +
          "The full explanation of each policy is on the Duplicates page.",
      },
    ],
    link: { href: "/library/duplicates", text: "Duplicates →" },
  });

  attachFeatureTray(head("job-mix-card"), {
    title: "Smart mixes",
    blurb: "Auto-generated playlists built from listening history, rebuilt daily.",
    rows: [
      {
        field: "smartPlaylistsEnabled", type: "switch", label: "Generate smart mixes",
      },
      {
        field: "analysisEnabled", type: "switch", label: "Audio analysis",
        hint: "Only the harmonic Auto Mix needs it; the history-based families " +
          "generate without it.",
      },
    ],
    link: { href: "/mixes", text: "Smart mixes →" },
  });

  attachFeatureTray(head("job-backup-card"), {
    title: "Backups",
    blurb: "Periodic snapshots of the manifest database, token store and certificates.",
    rows: [
      {
        field: "backupIntervalHours", type: "number", min: 0,
        label: "Snapshot every", unit: "hours",
        hint: "0 turns the periodic snapshot off; the button above still writes one.",
      },
      {
        field: "backupKeep", type: "number", min: 1,
        label: "Keep", unit: "snapshots",
      },
    ],
    link: { href: "/settings?tab=backups", text: "Backup settings →" },
  });

  attachFeatureTray(head("job-upd-card"), {
    title: "Update checks",
    blurb: "How often the bridge asks GitHub for a newer release, and whether it " +
      "installs one on its own.",
    rows: [
      {
        field: "updateCheckIntervalHours", type: "number", min: 1,
        label: "Check every", unit: "hours", placeholder: "6 (default)",
        // 0 IS the unset sentinel for this field (cmd/bridge only
        // overrides the updater's own 6 h when the config value is > 0),
        // so clearing the box has to mean the default rather than "Enter
        // a number." — which is what the hint promised and the generic
        // NaN guard refused. (CodeRabbit on PR #763.)
        emptyValue: 0,
        hint: "Clear it to go back to the bridge's default of 6 hours.",
      },
      {
        field: "updateAutoInstall", type: "switch", label: "Install updates automatically",
        hint: "Verifies the signature, swaps the binary and restarts. Quiet hours " +
          "are set under update settings.",
      },
    ],
    link: { href: "/settings?tab=updates", text: "Update settings →" },
  });
}

// wireJobButton — shared trigger-button UX: disable while in flight,
// flash the outcome (the action's own return string, or `okText`),
// restore after 4 s. All POSTs route through API.post so the CSRF
// content-type discipline holds.
function wireJobButton(id, action, okText) {
  const btn = document.getElementById(id);
  if (!btn) return;
  const original = btn.textContent;
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    btn.textContent = "Working…";
    try {
      const out = await action();
      btn.textContent = typeof out === "string" && out ? out : (okText || "Done");
    } catch (err) {
      btn.textContent = "Failed";
      console.warn(`${id}:`, err.message);
    }
    setTimeout(() => { btn.textContent = original; btn.disabled = false; }, 4000);
  });
}

// --- /api/jobs snapshot → cards ---

async function jobsSnapshotRefresh() {
  const data = await API.get("/api/jobs");
  renderJobCards(data);
}

// zeroTimeISO — Go's zero time.Time marshals as "0001-01-01T…"; every
// jobs DTO uses *time.Time+omitempty so it shouldn't reach the wire,
// but a future bare-time.Time field would (the PR #68 lesson) — treat
// it as absent rather than rendering a nonsense value.
function isAbsentTime(iso) {
  return !iso || String(iso).startsWith("0001-01-01");
}

// formatInFuture — "in 42m" / "in 3h" for next-due timestamps. The
// server ships absolute times only (no ticking countdowns on the wire,
// the PR #107 diff lesson); the browser derives the countdown.
function formatInFuture(iso) {
  if (isAbsentTime(iso)) return "—";
  const sec = Math.floor((new Date(iso).getTime() - Date.now()) / 1000);
  if (sec <= 0) return "due now";
  if (sec < 60) return `in ${sec}s`;
  if (sec < 3600) return `in ${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `in ${Math.floor(sec / 3600)}h`;
  return `in ${Math.floor(sec / 86400)}d`;
}

function agoOrDash(iso) {
  return isAbsentTime(iso) ? "—" : formatTimeAgo(new Date(iso));
}

// formatAutoOptimizeRemaining renders the outstanding backlog. Absent /
// disabled reads as unknown rather than "all caught up" — a turned-off
// sweeper has no opinion about the backlog, and claiming zero would be a
// lie the operator can act on.
function formatAutoOptimizeRemaining(last) {
  if (!last || last.disabled) return "—";
  if (last.remaining > 0) return `${last.remaining} tracks want a variant`;
  return "all caught up";
}

// formatAutoOptimizeResult renders one auto-optimize sweep's outcome.
//
// The two "stopped early" reasons are what an operator actually needs:
// without them a sweep that enqueued nothing because the volume is nearly
// full is indistinguishable from one that had nothing left to do.
function formatAutoOptimizeResult(last) {
  if (!last) return "—";
  if (last.disabled) return "turned off";
  const parts = [`${last.enqueued} queued`];
  if (last.regenerated) parts.push(`${last.regenerated} refreshed (source changed)`);
  if (last.alreadyInflight) parts.push(`${last.alreadyInflight} already building`);
  if (last.unresolvable) parts.push(`${last.unresolvable} unreadable`);
  let text = parts.join(" · ");
  if (last.diskFloorReached) {
    text += ` — paused, ${formatBytes(last.freeBytes)} free is at the ${formatBytes(last.minFreeBytes)} floor`;
  } else if (last.queueSaturated) {
    text += " — queue full, rest deferred";
  }
  return text;
}

function setBadge(id, cls, text) {
  const el = document.getElementById(id);
  if (!el) return;
  el.className = "badge " + cls;
  el.textContent = text;
}

// Bounded degraded-reason keys → operator copy (server sends keys, not
// prose — same discipline as the enricher's skip reasons).
const JOB_DEGRADED_LABELS = {
  sox_missing: "sox is not installed on the bridge host",
  fpcalc_missing: "fpcalc is not installed on the bridge host",
  no_api_key: "no AcoustID API key configured (ACOUSTID_API_KEY)",
};

function renderJobCards(j) {
  if (!j || !document.getElementById("jobs-page-root")) return;

  // Scanner (live status + last-scan ride the SSE stats frame via the
  // shared scan-status / last-full-scan ids).
  setText("job-scan-next", formatInFuture(j.scanner?.nextScanDue));
  setText("job-scan-cadence", j.scanner?.intervalSec ? `every ${formatDuration(j.scanner.intervalSec)}` : "—");
  setText("job-scan-watcher", j.scanner?.watcherEnabled
    ? "on — new files indexed within seconds"
    : "off — changes land on the next periodic scan");

  // Enrichment extras (counts ride the SSE enrichment frame).
  setText("job-harvest-state", j.enrichment?.harvestActive
    ? "active — bios, descriptions & premium covers"
    : "off");

  // Audio analysis.
  const an = j.analysis || {};
  const analyzeBtn = document.getElementById("jobs-analyze-now");
  if (an.active) {
    setBadge("job-analysis-state", "running", "active");
  } else if (an.enabled) {
    setBadge("job-analysis-state", "warn", "degraded");
  } else {
    setBadge("job-analysis-state", "idle", "off");
  }
  if (analyzeBtn) analyzeBtn.hidden = !an.active;
  const hint = document.getElementById("job-analysis-hint");
  if (hint && an.enabled && !an.active && an.degradedReason) {
    hint.textContent = `Enabled but inactive: ${JOB_DEGRADED_LABELS[an.degradedReason] || an.degradedReason}. Restart after fixing.`;
  }
  renderAnalysisCoverage(an.coverage);
  const sweep = an.sweep;
  if (sweep) {
    setText("job-analysis-sweep", sweep.running
      ? "sweeping now"
      : sweep.lastFinishedAt ? `last swept ${agoOrDash(sweep.lastFinishedAt)}` : "not yet run");
    setText("job-analysis-next", formatInFuture(sweep.nextDueAt));
  }

  // Fingerprint.
  const fp = j.fingerprint;
  const fpBtn = document.getElementById("jobs-fp-now");
  if (fp) {
    if (fp.active) setBadge("job-fp-state", "running", "active");
    else if (fp.enabled) setBadge("job-fp-state", "warn", "degraded");
    else setBadge("job-fp-state", "idle", "off");
    if (fpBtn) fpBtn.hidden = !fp.active;
    // The Enable button shows only while the STARTUP flag is off. After a
    // click, the /api/jobs closure keeps reporting the startup value until
    // the restart, so the click handler latches the button and this
    // refresh must not un-latch it back to "Enable".
    const fpEnable = document.getElementById("jobs-fp-enable");
    if (fpEnable && fpEnable.dataset.latched !== "true") fpEnable.hidden = fp.enabled;
    const fpHint = document.getElementById("job-fp-hint");
    if (fpHint && fp.enabled && !fp.active && fp.degradedReason) {
      // textContent wipes the static settings link along with the old
      // text; re-append it so the fix for a degraded state (missing key)
      // stays one click away.
      fpHint.textContent = `Enabled but inactive: ${JOB_DEGRADED_LABELS[fp.degradedReason] || fp.degradedReason}. Restart after fixing. `;
      const fpLink = document.createElement("a");
      fpLink.href = "/settings?tab=enrichment";
      fpLink.textContent = "Fingerprint settings";
      fpHint.appendChild(fpLink);
    }
    setText("job-fp-last", fp.running ? "sweeping now" : agoOrDash(fp.lastFinishedAt));
    setText("job-fp-next", formatInFuture(fp.nextDueAt));
    setText("job-fp-counts", fp.last
      ? `${fp.last.candidates} examined · ${fp.last.resolved} identified · ${fp.last.requeued} re-queued`
      : "—");
  }

  // CarPlay pre-generation (auto-optimize). The whole card stays hidden
  // when the field is absent — that means no upscale pool on this bridge,
  // so a card explaining a feature that can't run would be noise.
  const ao = j.autoOptimize;
  const aoCard = document.getElementById("job-ao-card");
  if (aoCard) aoCard.hidden = !ao;
  if (ao) {
    setBadge("job-ao-state", ao.active ? "running" : "idle", ao.active ? "on" : "off");
    const aoBtn = document.getElementById("jobs-ao-now");
    if (aoBtn) aoBtn.hidden = !ao.active;
    const last = ao.last;
    setText("job-ao-remaining", formatAutoOptimizeRemaining(last));
    setText("job-ao-last", ao.running ? "sweeping now" : agoOrDash(ao.lastFinishedAt));
    setText("job-ao-next", formatInFuture(ao.nextDueAt));
    setText("job-ao-counts", formatAutoOptimizeResult(last));
  }

  // Duplicate serving.
  const dup = j.duplicates || {};
  if (dup.policy === "off") setBadge("job-dupes-state", "idle", "off");
  else setBadge("job-dupes-state", "running", dup.policy || "—");
  setText("job-dupes-groups", dup.stamped ? String(dup.groups) : "—");
  setText("job-dupes-suppressed", dup.stamped ? `${dup.suppressed} copies excluded from serving` : "not yet evaluated");
  setText("job-dupes-last", dup.run?.running ? "re-evaluating now" : agoOrDash(dup.stampedAt));

  // Smart mixes.
  const mix = j.smartMixes || {};
  setBadge("job-mix-state", mix.enabled ? "running" : "idle", mix.enabled ? "on" : "off");
  const mixBtn = document.getElementById("jobs-mix-regen");
  if (mixBtn) mixBtn.hidden = !mix.enabled;
  setText("job-mix-last", mix.run ? agoOrDash(mix.run.lastFinishedAt) : "—");
  setText("job-mix-next", mix.run ? formatInFuture(mix.run.nextDueAt) : "—");
  setText("job-mix-cadence", mix.intervalSec ? `every ${formatDuration(mix.intervalSec)}` : "—");

  // Backups.
  const bk = j.backups || {};
  setText("job-backup-state", bk.intervalHours > 0
    ? `scheduled every ${bk.intervalHours} h`
    : "scheduler off — on-demand only");
  setText("job-backup-last", agoOrDash(bk.lastBackupAt));
  setText("job-backup-next", bk.run ? formatInFuture(bk.run.nextDueAt) : "—");
  setText("job-backup-keep", bk.keep > 0 ? `keep last ${bk.keep}` : "keep all");

  // Updates.
  const upd = j.updates || {};
  setText("job-upd-cadence", upd.checkIntervalHours ? `every ${upd.checkIntervalHours} h` : "—");
  setText("job-upd-auto", upd.autoInstall ? "on" : "off — install from the Dashboard");

  // Maintenance + UPnP.
  const mt = j.maintenance || {};
  setText("job-maint-integrity", mt.variantIntegrityActive ? "hourly sweep" : "off (upscale off or disabled)");
  setText("job-maint-gc", mt.orphanSidecarGC ? "on" : "off (default)");
  setText("job-maint-artwork", mt.artworkCacheLRU ? "capped — LRU eviction every 15 min" : "unlimited");
  const up = j.upnp || {};
  setText("job-upnp", up.enabled
    ? `on — ${up.configuredServers} upstream server${up.configuredServers === 1 ? "" : "s"}`
    : "off");
}

// renderAnalysisCoverage — analysed-vs-eligible bar + the exclusion
// line that answers "why is analysed < total tracks". Reuses the
// dist-bar visual language (matched = analysed, missing = remaining).
function renderAnalysisCoverage(cov) {
  const section = document.getElementById("job-analysis-coverage-section");
  const bar = document.getElementById("job-analysis-coverage-bar");
  const legend = document.getElementById("job-analysis-coverage-legend");
  const excl = document.getElementById("job-analysis-excluded");
  if (!section || !bar || !legend || !excl) return;
  if (!cov || !cov.eligible) {
    section.hidden = true;
    return;
  }
  section.hidden = false;
  bar.textContent = "";
  legend.textContent = "";
  const analysed = Math.min(cov.analysed ?? 0, cov.eligible);
  const remaining = Math.max(0, cov.eligible - analysed);
  const segs = [
    { label: "Analysed", count: analysed, cov: "matched" },
    { label: "Remaining", count: remaining, cov: "missing" },
  ];
  for (const seg of segs) {
    if (seg.count === 0) continue;
    const pct = (seg.count / cov.eligible) * 100;
    const span = document.createElement("span");
    span.className = "dist-seg";
    span.dataset.cov = seg.cov;
    span.style.width = pct.toFixed(2) + "%";
    span.title = `${seg.label}: ${seg.count} (${pct.toFixed(1)}%)`;
    bar.appendChild(span);
    const item = document.createElement("span");
    item.className = "dist-legend-item";
    item.dataset.cov = seg.cov;
    const swatch = document.createElement("i");
    swatch.className = "dist-swatch";
    item.appendChild(swatch);
    item.appendChild(document.createTextNode(`${seg.label} `));
    const b = document.createElement("b");
    b.textContent = String(seg.count);
    item.appendChild(b);
    legend.appendChild(item);
  }
  const parts = [];
  if (cov.dsdExcluded > 0) parts.push(`${cov.dsdExcluded} DSD excluded by design (sox can't decode DSD)`);
  if (cov.zeroByteExcluded > 0) parts.push(`${cov.zeroByteExcluded} unreadable (zero-byte)`);
  if (cov.stale > 0) parts.push(`${cov.stale} awaiting re-analysis (schema update)`);
  excl.textContent = parts.length
    ? `Not counted as eligible: ${parts.join(" · ")}.`
    : "Every track is eligible.";
}

function updateStatusLine(r) {
  if (!r) return "—";
  if (r.updateAvailable) return `update available: ${r.latestVersion || "?"}`;
  if (r.lastError) return `check failed: ${r.lastError}`;
  return "up to date";
}

async function jobsRefresh() {
  try {
    const res = await fetch("/api/upscale/batches?limit=100");
    if (res.status === 503) {
      document.getElementById("jobs-body").innerHTML =
        '<tr><td colspan="8"><em>Upscale is disabled on this bridge.</em></td></tr>';
      return;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = await res.json();
    jobsRender(data);
    document.getElementById("jobs-error").hidden = true;
  } catch (err) {
    document.getElementById("jobs-error").hidden = false;
    document.getElementById("jobs-error").textContent =
      `Couldn’t load jobs: ${err.message}`;
  }
}

function jobsRender(payload) {
  const rows = payload.batches || [];
  const tp = payload.throughput;
  // Hide the panel + reset its fields when there isn't a fresh
  // sample window. Pre-fix the panel stayed visible with stale
  // values when a later poll yielded `samples < 3` (e.g., after
  // the bridge restarted and the rolling window emptied). Per
  // CodeRabbit minor on PR #205 round 2.
  if (tp && tp.samples >= 3) {
    document.getElementById("jobs-throughput-panel").hidden = false;
    document.getElementById("jobs-throughput-rate").textContent =
      tp.jobsPerHour.toFixed(0);
    document.getElementById("jobs-throughput-eta").textContent =
      formatDuration(tp.etaSeconds);
    document.getElementById("jobs-throughput-samples").textContent =
      ` · based on ${tp.samples} recent jobs`;
  } else {
    document.getElementById("jobs-throughput-panel").hidden = true;
    document.getElementById("jobs-throughput-rate").textContent = "—";
    document.getElementById("jobs-throughput-eta").textContent = "—";
    document.getElementById("jobs-throughput-samples").textContent = "";
  }
  const body = document.getElementById("jobs-body");
  if (rows.length === 0) {
    body.innerHTML = '<tr><td colspan="8"><em>No batches yet.</em></td></tr>';
    return;
  }
  body.innerHTML = "";
  for (const r of rows) {
    const tr = document.createElement("tr");
    tr.dataset.id = r.id;
    const scopeLabel = r.path || "(whole library)";
    // `r.updatedAt` is RFC 3339 (server-side time.Time JSON
    // marshalling). `new Date(string)` parses it safely.
    const updated = new Date(r.updatedAt).toLocaleString();
    const target = `${(r.targetRate / 1000).toFixed(1)} kHz · ${r.targetBits}-bit`;
    tr.innerHTML = `
      <td data-label="Status"><span class="status status-${r.status}">${r.status}</span></td>
      <td data-label="Scope"><code>${escapeHTML(scopeLabel)}</code></td>
      <td data-label="Target">${target}</td>
      <td class="num" data-label="Done">${r.processedFiles}</td>
      <td class="num" data-label="Failed">${r.failedFiles}</td>
      <td class="num" data-label="Total">${r.totalFiles}</td>
      <td data-label="Updated"><time>${escapeHTML(updated)}</time></td>
      <td class="row-actions" data-label="Actions"></td>
    `;
    if (r.status === "pending" || r.status === "running") {
      const cancelBtn = document.createElement("button");
      cancelBtn.type = "button";
      cancelBtn.className = "btn danger";
      cancelBtn.textContent = "Cancel";
      cancelBtn.addEventListener("click", () => jobsCancel(r.id));
      tr.lastElementChild.appendChild(cancelBtn);
    }
    body.appendChild(tr);
    if (r.error) {
      const errRow = document.createElement("tr");
      errRow.className = "job-error-row";
      errRow.innerHTML = `<td colspan="8" class="error">${escapeHTML(r.error)}</td>`;
      body.appendChild(errRow);
    }
    // Skip-count sub-line. Surfaces "X tracks skipped" when Submit /
    // SubmitOptimize saw projection-eligible-by-format tracks but
    // didn't enqueue them (already at target, lossy, DSD, etc.).
    // Backed by upscale_batches.skipped_files (migration v9). Hidden
    // on rows where skippedFiles is 0 / missing — keeps pre-feature
    // batches and clean-everything-eligible batches uncluttered.
    if (r.skippedFiles && r.skippedFiles > 0) {
      const skipRow = document.createElement("tr");
      skipRow.className = "job-skipped-row";
      skipRow.innerHTML = `<td colspan="8" class="hint">${r.skippedFiles} track${r.skippedFiles === 1 ? "" : "s"} skipped (ineligible for this batch kind — already at target, lossy, DSD, or unknown format)</td>`;
      body.appendChild(skipRow);
    }
  }
}

async function jobsCancel(id) {
  if (!confirm("Cancel this batch? Workers will finish their current track but no new files will be enqueued.")) return;
  try {
    const res = await fetch(`/api/upscale/batches/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    jobsRefresh();
  } catch (err) {
    alert(`Cancel failed: ${err.message}`);
  }
}

function formatDuration(seconds) {
  if (!seconds || seconds <= 0) return "—";
  if (seconds < 60) return `${seconds.toFixed(0)} s`;
  if (seconds < 3600) return `${(seconds / 60).toFixed(1)} min`;
  return `${(seconds / 3600).toFixed(1)} h`;
}

// Mobile hamburger nav. Toggles `data-nav-open` on the <header> + the
// `aria-expanded` attr on the button. The CSS at <=640px reveals the
// button, hides the nav by default, and renders an absolute-positioned
// dropdown when data-nav-open=true. Closes on outside click, Escape,
// or any link tap (the page is about to navigate anyway).
function initMobileNav() {
  const toggle = document.getElementById("nav-toggle");
  const header = document.querySelector("header");
  const nav = document.getElementById("primary-nav");
  if (!toggle || !header || !nav) return;

  // `aria-expanded` stays on setAttribute — ARIA attrs aren't part of
  // the data-* family that dataset projects.
  const isOpen = () => header.dataset.navOpen === "true";
  function setOpen(open) {
    header.dataset.navOpen = open ? "true" : "false";
    toggle.setAttribute("aria-expanded", open ? "true" : "false");
    // ARIA disclosure pattern: move keyboard focus into the menu
    // on open. Mouse users don't see the ring (:focus-visible).
    if (open) nav.querySelector("a")?.focus();
  }

  toggle.addEventListener("click", (e) => {
    e.stopPropagation();
    setOpen(!isOpen());
  });

  document.addEventListener("click", (e) => {
    if (!isOpen()) return;
    if (!header.contains(e.target)) setOpen(false);
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && isOpen()) {
      setOpen(false);
      // Return focus to the toggle so a keyboard user can re-open
      // without re-tabbing — completes the disclosure-pattern loop.
      toggle.focus();
    }
  });

  nav.querySelectorAll("a").forEach((link) => {
    link.addEventListener("click", () => setOpen(false));
  });
}

// ---- Diagnostics page ----

// DIAGNOSTICS_POLL_MS: the numbers here are cheap (atomic counters and
// sliding-window quantiles, no database), so this can poll rather than
// ride the SSE stream. It is NOT on SSE deliberately: every field changes
// continuously, so a diff-suppressed event would fire on every tick and
// the frames would go to every open tab regardless of which page it is
// showing. A poll scoped to this page costs nothing when nobody is
// looking at it.
const DIAGNOSTICS_POLL_MS = 5000;

// formatSeconds renders a quantile. Sub-millisecond values are the normal
// case for a healthy lock wait, and "0.00s" would read as "not measured";
// the unit scales instead so a healthy bridge shows a small number rather
// than a zero.
function formatSeconds(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return "—";
  if (n < 0.001) return `${(n * 1e6).toFixed(0)} µs`;
  if (n < 1) return `${(n * 1000).toFixed(1)} ms`;
  return `${n.toFixed(2)} s`;
}

// formatUptime renders whole seconds as a coarse human duration.
function formatUptime(secs) {
  const n = Number(secs);
  if (!Number.isFinite(n) || n <= 0) return "—";
  if (n < 60) return `${Math.round(n)}s`;
  if (n < 3600) return `${Math.floor(n / 60)}m ${Math.round(n % 60)}s`;
  if (n < 86400) return `${Math.floor(n / 3600)}h ${Math.floor((n % 3600) / 60)}m`;
  return `${Math.floor(n / 86400)}d ${Math.floor((n % 86400) / 3600)}h`;
}

function setDiagText(id, text) {
  const el = document.getElementById(id);
  if (el) el.textContent = text;
}

// applyDiagnostics paints one /api/diagnostics snapshot.
function applyDiagnostics(d) {
  if (!d) return;
  setDiagText("diag-sqlite-p50", formatSeconds(d.sqliteLockWaitP50));
  setDiagText("diag-sqlite-p99", formatSeconds(d.sqliteLockWaitP99));

  // "No lookups yet" is NOT "0% hit ratio". Painting a 0% on a bridge
  // that has simply never enriched anything would read as a broken
  // cache, which is the opposite of the truth.
  const lookups = Number(d.mbCacheLookups) || 0;
  setDiagText("diag-mb-ratio", lookups === 0
    ? "no lookups yet"
    : `${(Number(d.mbCacheHitRatio) * 100).toFixed(1)}%`);
  setDiagText("diag-mb-lookups", lookups.toLocaleString());

  setDiagText("diag-upscale-inflight", String(d.upscaleJobsInFlight ?? 0));
  setDiagText("diag-upscale-done", (Number(d.upscaleJobsCompletedTotal) || 0).toLocaleString());
  setDiagText("diag-upscale-p50", formatSeconds(d.upscaleDurationP50));
  setDiagText("diag-upscale-p99", formatSeconds(d.upscaleDurationP99));

  // Tailscale rows only mean something in tsnet mode. On a CLI-mode or
  // disabled bridge the collector reports "down" with zero peers, which
  // would read as a broken tailnet rather than one that was never
  // configured — so hide the panel instead.
  const tsPanel = document.getElementById("diag-tailscale-panel");
  if (tsPanel) {
    const state = d.tailscaleNodeState || "down";
    // Allowlist, not a denylist. A plain loopback install with no
    // Tailscale config reports "disabled", and a CLI-mode bridge reports
    // "down" — neither has a tailnet to describe, and a panel reading
    // "disabled · 0 peers" looks like a fault rather than an absence.
    // Verified against a fresh fixture, which returns "disabled": an
    // earlier denylist checking only "down" showed the panel there.
    // Listing the states that DO mean something also keeps a future
    // fourth state from defaulting to visible.
    tsPanel.hidden = !(state === "running" || state === "starting");
    setDiagText("diag-ts-state", state);
    setDiagText("diag-ts-peers", String(d.tailscalePeersOnline ?? 0));
  }

  renderLogEventCounts(d.logEventCounts);
  setDiagText("diag-uptime", formatUptime(d.serverUptime));
}

// logExportAvailable caches the /api/logs/status verdict: null while the
// answer is outstanding, then true / false.
//
// The level tally reads it to decide whether its entries are buttons at all.
// It starts null and renders as plain text until proven otherwise, so the
// unknown window never offers an affordance that cannot be honoured — the
// safe direction, since the tally paints on the first diagnostics poll and
// would otherwise spend that tick showing a dead button.
let logExportAvailable = null;

// lastLogEventCounts is the most recent tally, kept so the status answer can
// repaint immediately instead of waiting out a poll interval.
let lastLogEventCounts = null;

// setLogExportAvailable records the verdict and repaints the tally.
function setLogExportAvailable(available) {
  if (logExportAvailable === available) return;
  logExportAvailable = available;
  if (lastLogEventCounts !== null) renderLogEventCounts(lastLogEventCounts);
}

// renderLogEventCounts paints the per-level tally. Built with
// createElement/textContent — the level keys come from the logging
// package rather than user input, but this is a list rendered from a
// server map and the page's posture is uniform.
function renderLogEventCounts(counts) {
  const dl = document.getElementById("diag-log-events");
  if (!dl) return;
  lastLogEventCounts = counts || null;
  const hint = document.getElementById("diag-log-events-hint");
  const entries = Object.entries(counts || {}).filter(([, n]) => n > 0);
  dl.replaceChildren();
  if (!entries.length) {
    const dt = document.createElement("dt");
    dt.textContent = "—";
    const dd = document.createElement("dd");
    dd.textContent = "no events recorded yet";
    dl.appendChild(dt);
    dl.appendChild(dd);
    if (hint) hint.hidden = true;
    return;
  }
  // Severity order, not count order: an operator scanning this wants
  // errors first regardless of how many debug lines are above them.
  // The server emits UPPERCASE level keys ("INFO", "ERROR"), so rank on
  // a lowercased key — matching on lowercase silently ranked every level
  // at the fallback and sorted them alphabetically instead, putting
  // DEBUG above ERROR. Caught against a live fixture, not in review.
  const rank = { error: 0, warn: 1, info: 2, debug: 3 };
  const rankOf = (k) => rank[String(k).toLowerCase()] ?? 9;
  entries.sort((a, b) => rankOf(a[0]) - rankOf(b[0]) || a[0].localeCompare(b[0]));
  // Each level is a button that arms the export below for that severity.
  // The counts are the natural entry point — an operator who sees a
  // climbing warn tally wants those lines, and making them click through
  // to a pre-filled export saves re-deriving the filter by hand.
  //
  // A button ONLY when there is an export to arm. With no log file the
  // controls below are hidden, so arming them sets a hidden <select> and
  // scrolls to a panel already on screen — a click with no observable effect
  // whatsoever, which reads as a broken console rather than as this install
  // logging somewhere else. Reported live on bridge.ars.md, whose systemd
  // unit sends output to the journal and therefore has no file at all.
  for (const [level, n] of entries) {
    const dt = document.createElement("dt");
    const label = String(level).toLowerCase();
    if (logExportAvailable) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "level-link";
      btn.textContent = label;
      btn.dataset.level = label;
      btn.title = `Prepare a log export at ${label} and above`;
      btn.addEventListener("click", () => armLogExport(btn.dataset.level));
      dt.appendChild(btn);
    } else {
      dt.textContent = label;
    }
    const dd = document.createElement("dd");
    dd.textContent = Number(n).toLocaleString();
    dl.appendChild(dt);
    dl.appendChild(dd);
  }
  // The hint promises a control "below"; hide it when there is none.
  if (hint) hint.hidden = !logExportAvailable;
}

// ---- Log export (Diagnostics page) ----

// armLogExport points the export controls at one level and scrolls to them.
//
// Does NOT start a download. The operator still picks a period and decides
// about redaction — clicking a count is "show me how to get these", not
// "send these somewhere", and a click that immediately wrote a file
// containing absolute paths would be the wrong default on a page whose
// whole posture is that sharing should be deliberate.
function armLogExport(level) {
  const sel = document.getElementById("logs-level");
  const panel = document.getElementById("logs-panel");
  if (!sel || !panel) return;
  // DEBUG has no "and above" of its own — it IS everything.
  sel.value = level === "debug" ? "all" : level;
  panel.scrollIntoView({ behavior: "smooth", block: "center" });
  sel.focus({ preventScroll: true });
}

// logExportQuery serialises the three controls.
function logExportQuery() {
  const q = new URLSearchParams();
  const level = document.getElementById("logs-level");
  const since = document.getElementById("logs-since");
  const redact = document.getElementById("logs-redact");
  if (level) q.set("level", level.value);
  if (since) q.set("since", since.value);
  if (redact) q.set("redact", redact.checked ? "true" : "false");
  return q;
}

// loadLogStatus asks whether this install has a log file at all.
//
// It often does not: the bridge logs to stderr, and only a SERVICE install
// redirects that to a file. Rendering the reason beats offering a button
// that 404s, which would read as a broken console rather than as the truth
// about how this bridge was started.
async function loadLogStatus() {
  const status = document.getElementById("logs-status");
  const controls = document.getElementById("logs-controls");
  const actions = document.getElementById("logs-actions");
  if (!status) return;
  try {
    const s = await API.get("/api/logs/status");
    setLogExportAvailable(s.available === true);
    if (!s.available) {
      status.textContent = s.reason || "no log file available";
      if (controls) controls.hidden = true;
      if (actions) actions.hidden = true;
      return;
    }
    const size = formatBytes(s.sizeBytes || 0);
    status.textContent = s.truncates
      ? `${s.path} — ${size}. Larger than one export can scan; the most recent portion is exported and the file says so.`
      : `${s.path} — ${size}.`;
    if (controls) controls.hidden = false;
    if (actions) actions.hidden = false;
    for (const id of ["logs-redact-hint", "logs-bundle-hint"]) {
      const el = document.getElementById(id);
      if (el) el.hidden = false;
    }
  } catch (err) {
    // Unreachable status is not "available" — leave the tally as plain text
    // rather than offering to arm controls that are still hidden.
    setLogExportAvailable(false);
    status.textContent = `could not check for a log file: ${err.message}`;
  }
}

function initLogExport() {
  loadLogStatus();
  const dl = document.getElementById("logs-download");
  if (dl) {
    dl.addEventListener("click", () => {
      globalThis.location = `/api/logs/export?${logExportQuery().toString()}`;
    });
  }
  const bundle = document.getElementById("logs-bundle");
  if (bundle) {
    bundle.addEventListener("click", () => {
      // Only `redact` is honoured by the bundle — it fixes its own window
      // and level — so sending the rest would imply a control the endpoint
      // deliberately ignores.
      const redact = document.getElementById("logs-redact");
      const q = new URLSearchParams();
      q.set("redact", redact && !redact.checked ? "false" : "true");
      globalThis.location = `/api/logs/bundle?${q.toString()}`;
    });
  }
}

// ---- Preflight (doctor) panel on the Diagnostics page ----

// runDoctor executes the preflight checks and paints the result.
//
// Click-driven only: the checks stat the filesystem and may exec sox /
// fpcalc, so polling them would turn a diagnostic into a load source.
async function runDoctor() {
  const btn = document.getElementById("doctor-run");
  const status = document.getElementById("doctor-status");
  const results = document.getElementById("doctor-results");
  if (!status || !results) return;
  if (btn) { btn.disabled = true; btn.textContent = "Running…"; }
  status.textContent = "Running checks…";
  results.replaceChildren();
  try {
    const d = await API.get("/api/doctor");
    if (!d || !d.available) {
      status.textContent = d?.reason || "Preflight is not available on this bridge.";
      return;
    }
    renderDoctorReport(d.report);
  } catch (err) {
    status.textContent = `Couldn't run the checks: ${err.message}`;
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = "Run checks"; }
  }
}

// renderDoctorReport paints one report. Built with
// createElement/textContent: summaries and hints carry filesystem paths
// and upstream error text.
function renderDoctorReport(report) {
  const status = document.getElementById("doctor-status");
  const results = document.getElementById("doctor-results");
  if (!status || !results) return;
  const checks = report?.checks || [];
  results.replaceChildren();
  if (!checks.length) {
    status.textContent = "No checks reported.";
    return;
  }
  const fail = report.fail ?? 0;
  const warn = report.warn ?? 0;
  // Lead with the verdict, in the operator's terms. "11 ok, 0 warn,
  // 0 fail" is the CLI footer; saying whether anything needs attention
  // first means the common case is readable without parsing counts.
  status.textContent = fail > 0
    ? `${fail} check${fail === 1 ? "" : "s"} failing — see below.`
    : warn > 0
      ? `No failures, ${warn} warning${warn === 1 ? "" : "s"}.`
      : "All checks passed.";

  const ul = document.createElement("ul");
  ul.className = "doctor-list";
  // Worst first: on a report with one failure among a dozen passes, the
  // failure is the entire reason the operator pressed the button.
  const rank = { fail: 0, warn: 1, ok: 2 };
  const ordered = [...checks].sort(
    (a, b) => (rank[a.status] ?? 9) - (rank[b.status] ?? 9)
  );
  for (const c of ordered) {
    const li = document.createElement("li");
    li.className = "doctor-check";
    li.dataset.status = c.status || "ok";

    const badge = document.createElement("span");
    badge.className = "doctor-badge";
    badge.textContent = c.status || "ok";
    li.appendChild(badge);

    const name = document.createElement("span");
    name.className = "doctor-name";
    name.textContent = c.name || "";
    li.appendChild(name);

    const summary = document.createElement("span");
    summary.className = "doctor-summary";
    summary.textContent = c.summary || "";
    li.appendChild(summary);

    if (c.hint) {
      const hint = document.createElement("div");
      hint.className = "doctor-hint";
      hint.textContent = c.hint;
      li.appendChild(hint);
    }
    ul.appendChild(li);
  }
  results.appendChild(ul);
}

async function loadDiagnostics() {
  try {
    applyDiagnostics(await API.get("/api/diagnostics"));
  } catch (err) {
    // Surface it rather than leaving every row on its em-dash
    // placeholder, which is indistinguishable from "nothing measured".
    setDiagText("diag-uptime", `unavailable: ${err.message}`);
  }
}

function initDiagnostics() {
  // Actually STOP the interval while the tab is hidden, rather than only
  // refreshing on return. The first version of this carried a comment
  // saying it paused and did not: the timer kept firing every 5s in the
  // background, which is the whole cost the comment claimed to avoid.
  let timer = null;
  const start = () => {
    if (timer === null) timer = setInterval(loadDiagnostics, DIAGNOSTICS_POLL_MS);
  };
  const stop = () => {
    if (timer !== null) {
      clearInterval(timer);
      timer = null;
    }
  };

  loadDiagnostics();
  start();

  // Preflight is click-driven and never polled — the checks stat the
  // filesystem and may exec sox / fpcalc.
  const doctorBtn = document.getElementById("doctor-run");
  if (doctorBtn) doctorBtn.addEventListener("click", runDoctor);

  // Log export. Status is fetched once on load, not polled: whether a log
  // file exists is a property of how this bridge was STARTED, so it cannot
  // change while the page is open.
  initLogExport();

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      stop();
      return;
    }
    // Repaint immediately on return so the operator doesn't stare at
    // values frozen from before the tab was backgrounded.
    loadDiagnostics();
    start();
  }, { signal: pageSignal() });
  window.addEventListener("pagehide", stop, { signal: pageSignal() });
  // Under partial-boost, leaving Diagnostics is not a page load, so the
  // 5 s poll would keep firing in the background. Stop it when the page
  // scope is torn down. (On a full load the interval dies with the page.)
  pageSignal().addEventListener("abort", stop, { once: true });
}

// --- boot ---

// ---- Listening history page ----
//
// Telemetry only. Playlists and favorites used to share this page with
// it, duplicating the player's own Playlists and Favorites views; they
// consolidated into Browse, taking their provenance and unresolved-member
// detail with them (see handlers_player_collections.go). What stayed is
// the one thing that was never anywhere else.

// History paging cursor state (module-scoped for the "Load more" button).
let historyCursor = 0;
let historyLoading = false;
// The device prefix the table is filtered to, "" for every device. Also
// module-scoped: "Load more" has to keep paging the SAME filter, and
// re-reading the select at click time would page a filter the operator
// changed mid-scroll.
let historyDevice = "";

function initHistory() {
  const root = document.getElementById("history-events-body");
  if (!root) return;

  attachFeatureTray(document.querySelector(".page.history .page-head"), {
    title: "Listening history",
    blurb: "Recording is a per-device switch in the 1-bit app, not a bridge " +
      "setting — each paired device decides whether to send its plays here.",
    rows: [
      {
        type: "note",
        text: "There is deliberately no server-side kill switch: every paired " +
          "device is yours, each already chooses for itself under Edit Bridge → " +
          "Sync & History, and a gate here would only shadow that choice. To " +
          "stop a device reporting, turn it off on the device — or unpair it.",
      },
      {
        field: "smartPlaylistsEnabled", type: "switch", label: "Build smart mixes from this history",
        hint: "Heavy Rotation, Forgotten Favorites and the rest are generated " +
          "from these plays. Off means the history is still recorded, just not used.",
      },
    ],
    link: { href: "/devices", text: "Paired devices →" },
  });

  loadHistorySummary();
  loadHistoryDeviceFilter();
  historyCursor = 0;
  historyDevice = "";
  loadHistoryEvents(true);

  document.querySelectorAll(".export-history").forEach((btn) => {
    btn.addEventListener("click", () => {
      const q = new URLSearchParams({ format: btn.dataset.format });
      // The export follows the table's filter. An "Export" beside a
      // table showing one device that silently wrote every device's
      // plays would be the wrong file with the right name.
      if (historyDevice) q.set("device", historyDevice);
      globalThis.location = `/api/history/export?${q.toString()}`;
    });
  });
  const sel = document.getElementById("history-device");
  if (sel) {
    sel.addEventListener("change", () => {
      historyDevice = sel.value || "";
      historyCursor = 0;
      loadHistoryEvents(true);
    });
  }
  const moreBtn = document.getElementById("history-load-more");
  if (moreBtn) moreBtn.addEventListener("click", () => loadHistoryEvents(false));
}

// loadDeviceNames maps a redacted device-token prefix to the device's
// name, from the registrations /api/devices already returns.
//
// Failure is non-fatal and returns an empty map: the caller falls back to
// the prefix, which is exactly the pre-existing display. A device list
// that fails to load must not take the history table down with it.
async function loadDeviceNames() {
  try {
    const d = await API.get("/api/devices");
    const out = new Map();
    for (const dev of d?.devices || []) {
      if (dev.deviceTokenPrefix && dev.deviceName) out.set(dev.deviceTokenPrefix, dev.deviceName);
    }
    return out;
  } catch {
    return new Map();
  }
}

// loadHistoryDeviceFilter fills the per-device dropdown.
//
// GET /api/history/events has accepted a `device` prefix since it
// shipped and the console never offered one, so "what has the study
// speaker been playing?" was answerable by the API and not by the page.
//
// A roster that fails to load leaves the select with its "All devices"
// option, which is the unfiltered view the page opens on anyway.
async function loadHistoryDeviceFilter() {
  const sel = document.getElementById("history-device");
  if (!sel) return;
  const names = await loadDeviceNames();
  for (const [prefix, name] of names) {
    const opt = document.createElement("option");
    opt.value = prefix;
    opt.textContent = name;
    opt.title = prefix;
    sel.appendChild(opt);
  }
  // Nothing to choose between: one device (or none) makes the control a
  // decoration that implies a comparison the data can't offer.
  sel.parentElement.hidden = names.size < 2;
}

async function loadHistorySummary() {
  try {
    const data = await API.get("/api/history");
    setText("history-total", (data.totalEvents || 0).toLocaleString());
    renderHistogram("history-codecs", data.codecs);
    renderHistogram("history-routes", data.routes);
    renderHistogram("history-top", data.topTracks, true);
    // The tiles are the same three histograms' leading buckets, read as
    // sentences. No extra query: every number here is already in the
    // response the histograms below are drawn from, so the summary
    // cannot disagree with the detail under it.
    fillHistoryLeader("history-top-track", data.topTracks, data.totalEvents, true);
    fillHistoryLeader("history-top-route", data.routes, data.totalEvents, false);
    fillHistoryLeader("history-top-codec", data.codecs, data.totalEvents, false);
  } catch (err) {
    setText("history-total", "—");
    console.warn("history summary:", err);
  }
}

// fillHistoryLeader writes one tile: the biggest bucket's label, with its
// share of all plays underneath.
//
// Share is of TOTAL events, not of the histogram's own sum: the codec and
// route histograms only cover events that reported one, so a percentage
// of their own total would read as "78% of plays" while meaning "78% of
// the plays that said".
function fillHistoryLeader(id, buckets, total, basename) {
  const el = document.getElementById(id);
  const foot = document.getElementById(`${id}-foot`);
  const top = (buckets || [])[0];
  if (!el) return;
  if (!top) {
    el.textContent = "—";
    if (foot) foot.textContent = "no plays recorded yet";
    return;
  }
  const label = basename ? (top.label || "").split("/").pop() : top.label;
  el.textContent = label || "(unknown)";
  el.title = top.label || "";
  if (!foot) return;
  const share = total > 0 ? Math.round((top.count / total) * 100) : 0;
  foot.textContent = `${top.count.toLocaleString()} plays · ${share}% of all`;
}

function renderHistogram(id, buckets, basename) {
  const el = document.getElementById(id);
  if (!el) return;
  const rows = buckets || [];
  if (rows.length === 0) {
    el.innerHTML = `<li class="muted">No data yet.</li>`;
    return;
  }
  const max = Math.max(...rows.map((b) => b.count), 1);
  el.innerHTML = rows.slice(0, 12).map((b) => {
    let label = b.label || "(unknown)";
    if (basename) label = label.split("/").pop();
    const pct = Math.round((b.count / max) * 100);
    return `<li title="${escapeHTML(b.label || "")}">
      <span class="histogram-bar" style="width:${pct}%"></span>
      <span class="histogram-label">${escapeHTML(label)}</span>
      <span class="histogram-count">${b.count}</span>
    </li>`;
  }).join("");
}

async function loadHistoryEvents(reset) {
  const body = document.getElementById("history-events-body");
  const moreBtn = document.getElementById("history-load-more");
  if (!body || historyLoading) return;
  historyLoading = true;
  try {
    const q = new URLSearchParams({ limit: "50" });
    if (historyDevice) q.set("device", historyDevice);
    if (!reset && historyCursor > 0) q.set("after", String(historyCursor));
    const data = await API.get(`/api/history/events?${q.toString()}`);
    const events = data.events || [];
    const rowsHTML = events.map((e) => `
      <tr>
        <td data-label="When">${e.startedAt ? formatTimeAgo(new Date(e.startedAt)) : "—"}</td>
        <td data-label="Track"><code>${escapeHTML((e.path || "").split("/").pop())}</code></td>
        <td data-label="Device">${escapeHTML(e.sourceDevice || "—")}</td>
        <td data-label="Codec">${escapeHTML(e.codec || "—")}</td>
        <td data-label="Route" title="${escapeHTML(e.deviceName || "")}">${escapeHTML(e.route || "—")}</td>
        <td class="num" data-label="Rate">${e.outputRate ? (e.outputRate / 1000).toFixed(1) + "k" : "—"}</td>
        <td class="num" data-label="Played">${Math.round(e.durationUsed || 0)}s</td>
      </tr>`).join("");
    if (reset) {
      body.innerHTML = events.length === 0
        ? `<tr><td colspan="7"><em>${historyDevice
            ? "No plays recorded for that device yet."
            : "No plays recorded yet."}</em></td></tr>`
        : rowsHTML;
    } else if (events.length > 0) {
      body.insertAdjacentHTML("beforeend", rowsHTML);
    }
    if (data.nextCursor && events.length > 0) {
      historyCursor = data.nextCursor;
    }
    // Show "Load more" only when the server handed back a cursor AND a
    // full page — guards the exact-page-boundary case where 50 events
    // come back with no nextCursor (the next click would re-send the
    // same cursor). CodeRabbit on PR #341.
    if (moreBtn) moreBtn.hidden = !(data.nextCursor && events.length >= 50);
  } catch (err) {
    if (reset) body.innerHTML = `<tr><td colspan="7" class="error">Failed to load history.</td></tr>`;
    console.warn("history events:", err);
  } finally {
    historyLoading = false;
  }
}

// Read a File as a base64 data URL (data:<mime>;base64,...). The bridge's
// cover-upload handler strips the data: prefix.
function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = () => reject(new Error("could not read file"));
    reader.readAsDataURL(file);
  });
}

// Smart mixes page: wire the "Regenerate now" button + per-family custom-cover
// upload/remove controls, reloading so the freshly-rendered state shows.
// =============================================================
// Camelot wheel — harmonic key-coverage (Smart Mixes page)
// =============================================================

const CAMELOT_NS = "http://www.w3.org/2000/svg";

// initCamelotWheel builds the 24-segment Camelot wheel from the embedded
// key-coverage JSON: inner ring = minor (A), outer ring = major (B), 1 at
// 12 o'clock going clockwise. Segments shade by track count; hovering a
// key highlights its harmonic neighbours. No-op off the Smart Mixes page
// (or when the feature is disabled — the container isn't rendered).
function initCamelotWheel() {
  const host = document.getElementById("camelot-wheel");
  const dataEl = document.getElementById("key-coverage-data");
  if (!host || !dataEl) return;
  let coverage = {};
  try {
    coverage = JSON.parse(dataEl.textContent) || {};
  } catch {
    coverage = {};
  }

  const cx = 200, cy = 200;
  // Ring GEOMETRY is the major/minor encoding; colour is redundant with
  // it, so the fills come from the theme's wheel tokens (major = brand
  // yellow, minor = warm grey) and fill-opacity keeps carrying the
  // per-key intensity.
  const rings = {
    A: { rInner: 62, rOuter: 116, fill: "var(--chart-wheel-minor)" }, // minor (inner)
    B: { rInner: 120, rOuter: 182, fill: "var(--chart-wheel-major)" }, // major (outer)
  };
  let maxCount = 0;
  for (const n of Object.values(coverage)) if (n > maxCount) maxCount = n;

  const svg = document.createElementNS(CAMELOT_NS, "svg");
  svg.setAttribute("viewBox", "0 0 400 400"); // responsive — no fixed px
  svg.setAttribute("class", "camelot-svg");
  // role + name live on the real <svg>, not on the host div. A div with
  // role="img" is a claim about an element that is not one; the graphic
  // itself is, and it is the element assistive tech should stop on.
  // Read off the host so the template still owns the wording.
  svg.setAttribute("role", "img");
  const wheelLabel = host.dataset.label;
  if (wheelLabel) svg.setAttribute("aria-label", wheelLabel);

  const segByCode = new Map();
  for (let num = 1; num <= 12; num++) {
    const centerDeg = -90 + (num - 1) * 30; // 1 at top, clockwise
    const a0 = centerDeg - 15, a1 = centerDeg + 15;
    for (const letter of ["B", "A"]) {
      const ring = rings[letter];
      const code = `${num}${letter}`;
      const count = coverage[code] || 0;
      const intensity = maxCount > 0 ? count / maxCount : 0;

      const seg = document.createElementNS(CAMELOT_NS, "path");
      seg.setAttribute("d", camelotWedge(cx, cy, ring.rInner, ring.rOuter, a0, a1));
      seg.setAttribute("class", count > 0 ? "camelot-seg" : "camelot-seg empty");
      seg.style.fill = ring.fill;
      seg.style.fillOpacity = count > 0 ? (0.18 + 0.82 * intensity).toFixed(3) : "0.06";
      seg.dataset.code = code;
      const title = document.createElementNS(CAMELOT_NS, "title");
      title.textContent = `${code} — ${count} track${count === 1 ? "" : "s"}`;
      seg.appendChild(title);
      seg.addEventListener("mouseenter", () => highlightCamelot(code, segByCode, coverage));
      seg.addEventListener("mouseleave", () => clearCamelot(segByCode));
      // Click / tap / keyboard → list the tracks in this key.
      // Only keyed segments (count > 0) are interactive — an empty key
      // would deep-link to an empty list. touchstart ALSO previews the
      // readout so tablet users get the metrics as they tap (the
      // synthesized click then performs the navigation).
      if (count > 0) {
        seg.classList.add("is-clickable");
        seg.setAttribute("role", "button");
        seg.setAttribute("tabindex", "0");
        seg.setAttribute("aria-label",
          `${code}: ${count} track${count === 1 ? "" : "s"} — list them`);
        seg.addEventListener("click", () => camelotOpenKeyByCode(code));
        seg.addEventListener("keydown", (e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            camelotOpenKeyByCode(code);
          }
        });
        seg.addEventListener("touchstart",
          () => highlightCamelot(code, segByCode, coverage), { passive: true });
      }
      svg.appendChild(seg);
      segByCode.set(code, seg);

      const midR = (ring.rInner + ring.rOuter) / 2;
      const [lx, ly] = camelotPolar(cx, cy, midR, centerDeg);
      const label = document.createElementNS(CAMELOT_NS, "text");
      label.setAttribute("x", lx.toFixed(1));
      label.setAttribute("y", ly.toFixed(1));
      label.setAttribute("class", "camelot-label");
      label.textContent = code;
      svg.appendChild(label);
    }
  }
  host.textContent = "";
  host.appendChild(svg);

  const total = Object.values(coverage).reduce((a, b) => a + b, 0);
  const readout = document.getElementById("camelot-readout");
  if (readout) {
    const summary = total > 0
      ? `${total} analysed track${total === 1 ? "" : "s"} keyed across ${Object.keys(coverage).length} of 24 wheel positions. Hover a key for its harmonic neighbours.`
      : "No analysed keys yet — enable Audio analysis and run it to populate the wheel.";
    readout.dataset.summary = summary;
    readout.textContent = summary;
  }
}

// camelotCompatible returns the harmonically-compatible codes for a wheel
// code: relative (same number, opposite letter) + adjacent (±1, same
// letter). The ±1 uses a SAFE cyclic wrap because JS `%` is remainder,
// not modulo — a bare (n-1)%12 underflows at the 12↔1 seam.
function camelotCompatible(code) {
  const m = /^(\d+)([AB])$/.exec(code);
  if (!m) return [];
  const n = parseInt(m[1], 10);
  const letter = m[2];
  const other = letter === "A" ? "B" : "A";
  const prev = ((n - 2 + 12) % 12) + 1; // n-1 with wrap (prev(1)=12)
  const next = (n % 12) + 1; // n+1 with wrap (next(12)=1)
  return [`${n}${other}`, `${prev}${letter}`, `${next}${letter}`];
}

function highlightCamelot(code, segByCode, coverage) {
  const compat = new Set(camelotCompatible(code));
  segByCode.forEach((seg, c) => {
    seg.classList.remove("is-hover", "is-compat", "is-dim");
    if (c === code) seg.classList.add("is-hover");
    else if (compat.has(c)) seg.classList.add("is-compat");
    else seg.classList.add("is-dim");
  });
  const readout = document.getElementById("camelot-readout");
  if (readout) {
    const cnt = coverage[code] || 0;
    const compatList = [...compat].map((c) => `${c} (${coverage[c] || 0})`).join(", ");
    readout.textContent = `${code}: ${cnt} track${cnt === 1 ? "" : "s"} · harmonic neighbours → ${compatList}`;
  }
}

function clearCamelot(segByCode) {
  segByCode.forEach((seg) => seg.classList.remove("is-hover", "is-compat", "is-dim"));
  const readout = document.getElementById("camelot-readout");
  if (readout && readout.dataset.summary) readout.textContent = readout.dataset.summary;
}

// camelotOpenKeyByCode deep-links to the player's key-filtered track
// list for a wheel code (e.g. "8A").
//
// It used to point at the Library Inspector, which owned the only view
// that answered this. When that page was retired the obvious
// replacement was /folders, and that would have been wrong: a harmonic
// key is not a place, and a folder tree cannot filter by one. It goes
// to /tracks, which is a list of tracks — the shape the question has.
function camelotOpenKeyByCode(code) {
  if (!/^\d+[AB]$/i.test(code)) return;
  window.location.href = `/tracks?camelot=${encodeURIComponent(code)}`;
}

function camelotPolar(cx, cy, r, deg) {
  const rad = (deg * Math.PI) / 180;
  return [cx + r * Math.cos(rad), cy + r * Math.sin(rad)];
}

// camelotWedge builds the SVG path for one annular wedge (a ring segment).
function camelotWedge(cx, cy, rInner, rOuter, a0, a1) {
  const [ox0, oy0] = camelotPolar(cx, cy, rOuter, a0);
  const [ox1, oy1] = camelotPolar(cx, cy, rOuter, a1);
  const [ix1, iy1] = camelotPolar(cx, cy, rInner, a1);
  const [ix0, iy0] = camelotPolar(cx, cy, rInner, a0);
  const large = Math.abs(a1 - a0) > 180 ? 1 : 0;
  return [
    `M ${ox0.toFixed(2)} ${oy0.toFixed(2)}`,
    `A ${rOuter} ${rOuter} 0 ${large} 1 ${ox1.toFixed(2)} ${oy1.toFixed(2)}`,
    `L ${ix1.toFixed(2)} ${iy1.toFixed(2)}`,
    `A ${rInner} ${rInner} 0 ${large} 0 ${ix0.toFixed(2)} ${iy0.toFixed(2)}`,
    "Z",
  ].join(" ");
}

// Light / dark / system theme toggle. Cycles system → light → dark and
// persists to localStorage["bridge-theme"]. applyTheme mirrors the pre-paint
// IIFE in layout.html <head> (which prevents the flash-of-wrong-theme on load).
// No matchMedia listener is needed: "system" removes the attribute and the CSS
// :root:not([data-theme]) dark media query tracks OS changes natively.
const THEME_KEY = "bridge-theme";
const THEME_ORDER = ["system", "light", "dark"];
const THEME_LABEL = { system: "System", light: "Light", dark: "Dark" };
const THEME_GLYPH = { system: "◑", light: "☀", dark: "☾" }; // ◑ ☀ ☾

function currentTheme() {
  try {
    const t = localStorage.getItem(THEME_KEY);
    if (t === "light" || t === "dark") return t;
  } catch (e) { /* storage disabled / private mode */ }
  return "system";
}

function applyTheme(mode, persist) {
  const root = document.documentElement;
  if (mode === "light" || mode === "dark") {
    root.setAttribute("data-theme", mode);
  } else {
    mode = "system";
    root.removeAttribute("data-theme");
  }
  if (persist) {
    try { localStorage.setItem(THEME_KEY, mode); } catch (e) { /* ignore */ }
  }
  const btn = document.getElementById("theme-toggle");
  if (btn) {
    btn.textContent = THEME_GLYPH[mode] + " " + THEME_LABEL[mode];
    btn.setAttribute("aria-label", "Theme: " + mode + " (click to change)");
  }
}

function initTheme() {
  // Sync the button label with the persisted choice (the pre-paint IIFE
  // already applied data-theme, so this doesn't repaint).
  applyTheme(currentTheme(), false);
  const btn = document.getElementById("theme-toggle");
  if (!btn) return;
  btn.addEventListener("click", () => {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(currentTheme()) + 1) % THEME_ORDER.length];
    applyTheme(next, true);
  });
}

// Public-mode sign-out (M10). The button renders only when IsPublic
// (layout.html), so this is a no-op on loopback installs. POST /logout
// invalidates the session server-side and clears the cookie; the
// redirect to /login happens regardless of the POST outcome — an
// operator who wants out lands on the login page even if the request
// failed (the session still dies at its server-side cap).
function initLogout() {
  const btn = document.getElementById("logout-btn");
  if (!btn) return;
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    try {
      await API.post("/logout");
    } catch (e) { /* fall through to the redirect either way */ }
    window.location.href = "/login";
  });
}

// ---------------- Duplicates page ----------------
//
// Data flow: summary tiles + tier table read GET /api/duplicates/summary
// (the persisted stamping-pass document — cheap, no polling); the group
// list pages GET /api/duplicates/groups. Refreshes happen on explicit
// edges only: policy save, "Re-evaluate now", and the SSE stats event's
// isScanning true→false edge (see applyStats) — no timers.
let dupesLastIsScanning = false;
let dupesGroupsCursor = "";
let dupesRefreshTimers = [];

function refreshDuplicatesPage() {
  if (document.body.dataset.active !== "duplicates") return;
  refreshDupesSummary();
  loadDupeGroups(true);
}

// The restamp is asynchronous (a coalescing nudge to the sweeper), so a
// policy save / re-evaluate schedules two refreshes: one for the fast
// common case, one to converge if the pass took longer.
function scheduleDupesRefresh() {
  for (const t of dupesRefreshTimers) clearTimeout(t);
  dupesRefreshTimers = [
    setTimeout(refreshDuplicatesPage, 1200),
    setTimeout(refreshDuplicatesPage, 5000),
  ];
}

async function refreshDupesSummary() {
  let sum;
  try {
    sum = await API.get("/api/duplicates/summary");
  } catch (err) {
    console.warn("duplicates summary", err);
    return;
  }
  // Policy radio reflects the live setting (checked state only — never
  // clobber a selection the user is mid-flight on: radios fire change
  // immediately, so a mismatch here only exists on first paint or after
  // an external change).
  const radio = document.querySelector(`#dupes-policy input[value="${CSS.escape(sum.policy || "")}"]`);
  if (radio && !radio.checked) radio.checked = true;

  const stampLine = document.getElementById("dupes-stamp-line");
  if (!sum.stamped) {
    if (stampLine) {
      stampLine.hidden = false;
      // Say WHICH wait this is: during a scan the first evaluation is
      // already on its way (the scan tail runs it), and Re-evaluate is
      // deliberately deferred — an unexplained empty state here read as
      // a dead button on the first production deploy.
      stampLine.textContent = sum.scanInFlight
        ? "A library scan is running — the first duplicate evaluation happens automatically when it finishes."
        : "No stamping pass has run yet — counts appear after the first full scan.";
    }
    return;
  }
  if (stampLine) {
    if (sum.scanInFlight) {
      stampLine.hidden = false;
      stampLine.textContent = "A library scan is running — duplicates re-evaluate automatically at its tail; the counts below are from the previous pass.";
    } else if (sum.stampedPolicy && sum.policy && sum.stampedPolicy !== sum.policy) {
      stampLine.hidden = false;
      stampLine.textContent = `Re-evaluating under the new policy… (counts below still reflect “${sum.stampedPolicy}”)`;
    } else {
      stampLine.hidden = true;
    }
  }
  setText("dupes-groups", sum.groups);
  setText("dupes-suppressed", sum.suppressed);
  setText("dupes-served", sum.served);
  const foot = document.getElementById("dupes-groups-foot");
  if (foot) foot.textContent = `across ${sum.scanned} scanned tracks`;
  const stampedAt = document.getElementById("dupes-stamped-at");
  if (stampedAt && sum.stampedAt) stampedAt.textContent = `as of ${formatTimeAgo(new Date(sum.stampedAt))}`;
  document.getElementById("dupes-tiles")?.removeAttribute("hidden");
  // Rides the tiles: the scope note is only meaningful once there are
  // numbers to scope, and every one of them counts the scanned filesystem
  // library rather than the bridge-wide served total.
  document.getElementById("dupes-scope-note")?.removeAttribute("hidden");
  // Audio-checksum evidence coverage — the identical/different-audio
  // tiers need FULL per-group coverage, so while the ExtractorVersion-3
  // re-extract backfills, say the evidence is still arriving.
  const md5Line = document.getElementById("dupes-md5-coverage");
  if (md5Line) {
    if (sum.md5Total > 0) {
      md5Line.hidden = false;
      md5Line.textContent = `Audio-checksum evidence: ${sum.md5Known} of ${sum.md5Total} group members` +
        (sum.md5Known < sum.md5Total ? " — still arriving; groups refine into the audio tiers as it completes." : ".");
    } else {
      md5Line.hidden = true;
    }
  }

  const rows = document.getElementById("dupes-tier-rows");
  if (rows) {
    rows.replaceChildren();
    for (const t of sum.tiers || []) {
      const tr = document.createElement("tr");
      if (!t.groups) tr.classList.add("dupes-tier-empty");
      const cells = [
        t.tier,
        String(t.groups),
        String(t.redundantFiles),
        formatBytes(t.bytesInNonLargestCopies || 0),
        String(t.suppressed),
      ];
      for (const c of cells) {
        const td = document.createElement("td");
        td.textContent = c;
        tr.appendChild(td);
      }
      rows.appendChild(tr);
    }
    document.getElementById("dupes-tier-panel")?.removeAttribute("hidden");
  }
  document.getElementById("dupes-groups-panel")?.removeAttribute("hidden");
}

function dupeMemberGeometry(m) {
  const parts = [];
  parts.push(m.codec || "unknown");
  if (m.sampleRate > 0 && m.bitsPerSample > 0) parts.push(`${m.sampleRate}/${m.bitsPerSample}`);
  if (m.isDSD) parts.push("DSD");
  if (m.durationSec > 0) {
    const mins = Math.floor(m.durationSec / 60);
    const secs = String(Math.floor(m.durationSec % 60)).padStart(2, "0");
    parts.push(`${mins}:${secs}`);
  }
  parts.push(formatBytes(m.sizeBytes || 0));
  return parts.join(" · ");
}

async function loadDupeGroups(reset) {
  const list = document.getElementById("dupes-groups-list");
  if (!list) return;
  if (reset) {
    dupesGroupsCursor = "";
    list.replaceChildren();
  }
  const tier = document.getElementById("dupes-tier-filter")?.value || "";
  const params = new URLSearchParams();
  if (tier) params.set("tier", tier);
  if (dupesGroupsCursor) params.set("cursor", dupesGroupsCursor);
  let resp;
  try {
    resp = await API.get(`/api/duplicates/groups${params.size ? "?" + params.toString() : ""}`);
  } catch (err) {
    console.warn("duplicates groups", err);
    return;
  }
  for (const g of resp.groups || []) {
    // All content rendered via createElement/textContent — titles,
    // albums and paths are library-supplied strings.
    const details = document.createElement("details");
    details.className = "dupes-group";
    const summary = document.createElement("summary");
    const head = g.members?.[0] || {};
    const title = head.title || g.members?.[0]?.path || g.groupID;
    const who = [head.albumArtist, head.album].filter(Boolean).join(" — ");
    summary.textContent = `${title}${who ? "  ·  " + who : ""}`;
    const badge = document.createElement("span");
    badge.className = "badge idle dupes-tier-badge";
    badge.textContent = g.tier;
    summary.appendChild(badge);
    details.appendChild(summary);
    for (const m of g.members || []) {
      const row = document.createElement("div");
      row.className = "dupes-member";
      const geo = document.createElement("span");
      geo.className = "dupes-geo";
      geo.textContent = dupeMemberGeometry(m);
      const path = document.createElement("span");
      path.className = "dupes-path";
      path.textContent = m.path;
      const state = document.createElement("span");
      state.className = m.suppressed ? "badge idle" : "badge running";
      state.textContent = m.suppressed ? "suppressed" : "serving";
      row.append(geo, path, state);
      details.appendChild(row);
    }
    list.appendChild(details);
  }
  if (reset && !(resp.groups || []).length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = tier
      ? "No groups in this tier."
      : "No duplicate groups detected — nothing to see here.";
    list.appendChild(empty);
  }
  dupesGroupsCursor = resp.nextCursor || "";
  const more = document.getElementById("dupes-load-more");
  if (more) more.hidden = !dupesGroupsCursor;
}

function initDuplicates() {
  if (!document.getElementById("duplicates-page-root")) return;
  refreshDupesSummary();
  loadDupeGroups(true);
  document.getElementById("dupes-policy")?.addEventListener("change", async (e) => {
    const v = e.target?.value;
    if (!v) return;
    try {
      await API.patch("/api/settings", { duplicatesFilter: v });
    } catch (err) {
      alert(`Saving the policy failed: ${err.message || err}`);
      refreshDupesSummary(); // snap the radio back to reality
      return;
    }
    scheduleDupesRefresh();
  });
  document.getElementById("dupes-reevaluate")?.addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    try {
      const ack = await API.post("/api/duplicates/sweep");
      if (ack && ack.scanInFlight) {
        // The nudge is deferred while a scan runs (its tail stamps under
        // the current policy) — say so instead of silently no-opping.
        const stampLine = document.getElementById("dupes-stamp-line");
        if (stampLine) {
          stampLine.hidden = false;
          stampLine.textContent = "A library scan is running — the re-evaluation happens automatically when it finishes.";
        }
      } else {
        scheduleDupesRefresh();
      }
    } catch (err) {
      alert(`Re-evaluate failed: ${err.message || err}`);
    } finally {
      setTimeout(() => { btn.disabled = false; }, 1500);
    }
  });
  document.getElementById("dupes-tier-filter")?.addEventListener("change", () => loadDupeGroups(true));
  document.getElementById("dupes-load-more")?.addEventListener("click", () => loadDupeGroups(false));
}

// ===========================================================================
// Partial-boost client router (PR 11)
//
// Leaving the library player for an operator page (Stats / Settings / Server)
// used to be a full page load, which destroys the DOM and with it the
// player's <audio> element — so playback stopped. This router turns those
// navigations into an in-place swap of <main> only: the persistent chrome
// (header, top-nav) and the player module's <audio> + now-playing bar, which
// both live on <body>, are never touched, so audio keeps playing.
//
// The server renders the target as a content-only fragment on X-Bridge-Partial
// (see renderPage). This side fetches it, swaps <main>, re-runs the target's
// init, and recycles the SSE stream so freshly-injected tiles get a snapshot.
//
// Two structural hazards, both handled below:
//   1. Operator initX register document/window-level listeners and intervals
//      that nothing removed. On a full load that never mattered; under boost
//      they would stack a fresh copy per visit. Every such registration is
//      scoped to an AbortController (pageSignal) that is aborted before the
//      next swap. Element-level listeners need no scope — they die with the
//      swapped DOM node.
//   2. innerHTML does not execute <script> elements. runInlineScripts
//      re-creates the classic ones so a content template's inline script
//      still runs; JSON data islands (<script type="application/json">) are
//      left in place and read as text, exactly as on a full load.
//
// Escape hatch: ?boost=0 on the initial URL disables the whole thing and
// every navigation is an ordinary full load again. And any single boost that
// can't complete cleanly falls back to location.assign — a hard load of the
// same target — so this is a pure enhancement with no new failure mode.
// ===========================================================================

// pageAbort scopes the per-page teardown. pageSignal() hands operator initX a
// signal to pass as { signal } to document/window addEventListener calls (and
// to hang interval cleanup off); dispatchPageInit aborts it before running the
// next page's init.
let pageAbort = null;

function pageSignal() {
  if (!pageAbort) pageAbort = new AbortController();
  return pageAbort.signal;
}

// pageInterval is setInterval bound to the page scope: the timer is cleared
// when the scope is torn down, so a poller can't keep firing after the
// operator has boosted to another page.
function pageInterval(fn, ms) {
  const id = setInterval(fn, ms);
  pageSignal().addEventListener("abort", () => clearInterval(id), { once: true });
  return id;
}

// dispatchPageInit is the SINGLE entry point for operator page init, used by
// both first paint (DOMContentLoaded) and every boost swap, so the two can
// never drift. It resets the page scope first: the previous page's listeners
// and intervals are aborted, then a fresh controller backs the new page's.
function dispatchPageInit(tab) {
  if (pageAbort) pageAbort.abort();
  pageAbort = new AbortController();
  invalidateTraySettings();
  switch (tab) {
    case "stats": initStats(); break;
    case "library": initLibrary(); break;
    case "duplicates": initDuplicates(); break;
    case "jobs": initJobs(); break;
    case "devices": initDevices(); break;
    case "upnp": initUPnP(); break;
    case "history": initHistory(); break;
    case "settings": initSettings(); break;
    case "diagnostics": initDiagnostics(); break;
    // "player" has no per-page initX — that is boot.js's job. Every other
    // tab MUST appear above: a case labelled with a tab no page renders
    // is silent, and leaves its controls dead with correct-looking
    // markup. TestEveryPageTabHasAnInitCase pins the two together.
  }
}

// recycleEventStream closes and reopens the SSE connection. The server
// byte-diff-suppresses frames PER CONNECTION, so a just-injected panel would
// get nothing until a value actually changed (up to 30 s). A fresh connection
// is sent a full initial snapshot of every tile — the resnapshot the plan
// calls for, reusing exactly the mechanism handleVisibilityRestore relies on.
function recycleEventStream() {
  if (activeEventSource) {
    try { activeEventSource.close(); } catch { /* ignore */ }
  }
  startEventStream();
}

// BOOST_ENABLED gates the whole router. AbortController + fetch are required
// for the machinery, so a browser without them stays on full loads.
//
// ?boost=0 is the documented escape hatch, and it LATCHES for the tab session
// via sessionStorage — otherwise it would disable boost for exactly one page
// load and then re-enable on the very next (full-load) navigation, which is
// useless when the reason you set it is that boost is misbehaving. Clear the
// latch by opening a fresh tab; it is deliberately not a persistent, all-tabs
// kill switch.
const BOOST_ENABLED = (() => {
  if (typeof window.AbortController !== "function" || typeof window.fetch !== "function") {
    return false;
  }
  const off = new URLSearchParams(location.search).get("boost") === "0";
  try {
    if (off) sessionStorage.setItem("bridge.boostOff", "1");
    return sessionStorage.getItem("bridge.boostOff") !== "1";
  } catch {
    // Private mode / storage disabled: fall back to the per-URL reading.
    return !off;
  }
})();

// isPlayerPath defers to the player module. When the module isn't present
// (failed to load), everything reads as an operator route, so "/" is fetched
// as a partial and — unable to mount without the module — falls back to a
// hard load. Safe either way.
function boostIsPlayerPath(pathname) {
  const p = window.__player;
  return !!(p && typeof p.isPlayerPath === "function" && p.isPlayerPath(pathname));
}

// lastBoostPath is the pathname of the currently-mounted page. A popstate
// that kept the pathname is an in-page state change (the player's views, the
// player's search query) owned by that page's own handler, not a page swap.
let lastBoostPath = location.pathname;

// boostLiveRegion is a single polite live region for route-change
// announcements — SPA navigation is otherwise silent to a screen reader.
let boostLiveRegion = null;
function boostAnnounce(text) {
  if (!boostLiveRegion) {
    boostLiveRegion = document.createElement("div");
    boostLiveRegion.className = "sr-only";
    boostLiveRegion.setAttribute("aria-live", "polite");
    boostLiveRegion.setAttribute("aria-atomic", "true");
    document.body.appendChild(boostLiveRegion);
  }
  boostLiveRegion.textContent = text;
}

// runInlineScripts re-creates the classic <script> elements in a
// freshly-injected fragment so they execute — innerHTML parses them into the
// DOM but never runs them. JSON data islands and module scripts are left
// alone: the former are read as text by their consumers, and no content
// template ships a module script.
function runInlineScripts(root) {
  for (const old of root.querySelectorAll("script")) {
    const type = (old.getAttribute("type") || "").toLowerCase();
    if (type === "application/json" || type === "module") continue;
    const s = document.createElement("script");
    for (const attr of old.attributes) s.setAttribute(attr.name, attr.value);
    // An inline (no-src) script is wrapped in a block so any top-level
    // const/let it declares is block-scoped. Re-running the same script on a
    // later swap would otherwise throw a redeclaration SyntaxError against
    // the global binding the first run left behind; var / function still
    // hoist as before. A src script has no inline body to redeclare.
    s.textContent = old.src ? old.textContent : `{\n${old.textContent}\n}`;
    old.replaceWith(s);
  }
}

// boostUpdateTopNav sets aria-current on the one sidebar entry that matches
// the page we just swapped in.
//
// Three arms, checked in order, each against EVERY entry before the next is
// considered — a three-pass scan, not one pass with an OR:
//
//   1. playerNav, when the page is a player route with a sidebar entry of
//      its own (Smart mixes). Every player route renders the same tab and
//      the same section, so nothing else can tell /mixes from /albums.
//   2. the tab, which is how every operator page is keyed (jobs, devices,
//      upnp, …) now that the sidebar has absorbed the old .subnav. Entries
//      carrying a data-player-section are SKIPPED here: they belong to arm
//      1 alone, and they share data-tab="player" with Browse, so matching
//      them by tab would depend on their order in the markup.
//   3. the section, the fallback that keeps Browse lit for the player's
//      other sub-routes (/albums, /artists, /playlists, …).
//
// A single-pass OR would light two entries at once: "data" carries a tab of
// its own while still belonging to the "server" section.
//
// All three values arrive from the server on X-Bridge-Active /
// X-Bridge-Section / X-Bridge-Player-Nav, so sectionForTab and
// playerNavEntry stay the single source of truth and this never re-derives
// them. boot.js applies the same rule again for player navigations that
// never reach the server at all.
function boostUpdateTopNav(active, section, playerNav) {
  const links = [...document.querySelectorAll("#primary-nav a")];
  const match =
    (playerNav && links.find((a) => a.dataset.playerSection === playerNav)) ||
    links.find((a) => !a.dataset.playerSection && a.dataset.tab === active) ||
    links.find((a) => a.dataset.tab === section) ||
    null;
  markNavCurrent(links, match);
}

// markNavCurrent paints exactly one entry, and clears the rest.
//
// Shared with the player's own router (via updateSidebarNav in boot.js,
// which resolves its match a different way): both must leave exactly one
// aria-current, because that attribute is what the CSS paints from AND
// what a screen reader announces, so two of them is a visual bug and an
// a11y bug at once.
function markNavCurrent(links, match) {
  for (const a of links) {
    if (a === match) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// boostFocusMain moves focus to the new view's first heading and announces
// its title, so a keyboard / screen-reader user gets the same signal a full
// page load would have given. Without this, client-side navigation is silent.
function boostFocusMain(main) {
  const h = main.querySelector("h1");
  const label = (h && h.textContent.trim()) || "";
  if (h) {
    if (!h.hasAttribute("tabindex")) h.setAttribute("tabindex", "-1");
    try { h.focus({ preventScroll: true }); } catch { /* ignore */ }
  }
  if (label) {
    boostAnnounce(label);
    const name = document.querySelector("header .name");
    document.title = name ? `${label} — ${name.textContent}` : label;
  }
}

// boostGen serialises overlapping navigations. Every boostSwap claims the
// next generation up front; if a newer navigation starts while this one is
// awaiting the network, its response is discarded rather than swapped in — so
// two fast clicks (or fast back/forward) can't let a slow-resolving earlier
// fetch overwrite the page the operator actually landed on. Same idea as the
// the player's route generation counter.
let boostGen = 0;

// boostSwap fetches the target as a content-only fragment and puts it in
// <main>, tearing down the outgoing page's scope first and running the
// incoming page's init after. Returns true on success OR when superseded (the
// caller must not hard-navigate then); false means the caller should
// hard-navigate. No history is touched until the fetch succeeds, so a failure
// that falls back to a hard load leaves no phantom entry.
async function boostSwap(url, opts = {}) {
  const gen = ++boostGen;
  let resp;
  try {
    resp = await fetch(url, {
      headers: { "X-Bridge-Partial": "1" },
      credentials: "same-origin",
    });
  } catch {
    return false; // network error → hard load
  }
  if (gen !== boostGen) return true; // superseded by a newer navigation
  // A redirect (public-mode session expiry bouncing to /login) or any
  // non-OK status isn't a boostable page — fall back so the operator lands
  // on the real target.
  if (!resp.ok || resp.redirected) return false;
  const active = resp.headers.get("X-Bridge-Active");
  const section = resp.headers.get("X-Bridge-Section");
  if (!active || !section) return false; // not a boost-aware response
  let html;
  try {
    html = await resp.text();
  } catch {
    return false;
  }
  if (gen !== boostGen) return true; // superseded while reading the body

  const main = document.querySelector("main");
  if (!main) return false;

  // Commit history only now that the fetch has succeeded, and BEFORE the
  // swap, so location.pathname is correct when the incoming init / player
  // route reads it. On a popstate the browser already moved history, so
  // opts.push is null.
  if (opts.push) history.pushState({ boost: true }, "", opts.push);

  // Tear down the outgoing operator page's scope before its DOM is removed.
  // The player path (below) is exempt: the module owns its own teardown.
  if (pageAbort) { pageAbort.abort(); pageAbort = null; }

  main.innerHTML = html;
  runInlineScripts(main);

  document.body.dataset.active = active;
  document.body.dataset.section = section;
  boostUpdateTopNav(active, section, resp.headers.get("X-Bridge-Player-Nav") || "");

  if (boostIsPlayerPath(location.pathname) && window.__player) {
    // The injected fragment is the player shell; the module wires its
    // sections + search and renders the current route (and does its own
    // focus / announce / scroll restore).
    window.__player.mountShell();
  } else {
    dispatchPageInit(active);
    boostFocusMain(main);
    // A same-document swap doesn't reset scroll, so without this the
    // operator lands already scrolled past the top after clicking a nav
    // entry from partway down a long page. A full load starts at the top;
    // match it. (Operator scroll isn't tracked, so popstate lands at the
    // top too — an acceptable default.)
    window.scrollTo({ top: 0 });
  }

  recycleEventStream();
  lastBoostPath = location.pathname;
  return true;
}

// boostNavigate is a forward navigation (a click). It fetches first and pushes
// history only on success, so a failed boost degrades to a clean hard load.
function boostNavigate(href) {
  boostSwap(href, { push: href }).then((ok) => {
    if (!ok) location.assign(href);
  });
}

function wireBoostRouter() {
  if (!BOOST_ENABLED) return;

  // Delegated click on the persistent nav. Player-internal links
  // (a[data-route]) are boot.js's job and are not matched here.
  document.addEventListener("click", (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest("#primary-nav a, .subnav a");
    if (!a) return;
    // Respect a link that explicitly opens elsewhere or downloads — a boost
    // swap would wrongly load it in place. None ship in the nav today; this
    // keeps the router correct if one is ever added.
    if (a.hasAttribute("download")) return;
    if (a.target && a.target !== "_self") return;
    const url = new URL(a.href, location.origin);
    if (url.origin !== location.origin) return;
    // A hash on the current page (Settings jump list) is not a navigation.
    if (url.pathname === location.pathname && url.hash) return;
    if (url.href === location.href) { e.preventDefault(); return; }
    e.preventDefault();
    boostNavigate(url.href);
  });

  // Single popstate owner for page changes. In-page state changes (same
  // pathname) and player-internal back/forward are left to their own
  // handlers; only a change of mounted page is swapped here.
  window.addEventListener("popstate", () => {
    if (location.pathname === lastBoostPath) return;
    if (boostIsPlayerPath(location.pathname) && boostIsPlayerPath(lastBoostPath)) {
      lastBoostPath = location.pathname;
      return; // player ↔ player: boot.js route() handles it
    }
    boostSwap(location.href, { push: null }).then((ok) => {
      if (!ok) location.assign(location.href);
    });
  });
}

document.addEventListener("DOMContentLoaded", () => {
  initMobileNav();
  initTheme();
  // Sidebar widget: every page, once. Cheap (one query + one statfs) and
  // self-hiding when neither a floor nor a quota is configured.
  refreshSpaceMeter();
  initLogout();
  wireBoostRouter();
  // First paint routes through the same dispatcher every boost swap uses, so
  // the page scope is established identically on load and on navigation.
  dispatchPageInit(document.body.dataset.active);
  // Start the SSE stream after page-init so the initial snapshot
  // can paint into a fully-bootstrapped DOM. The first snapshot
  // typically lands within a few ms; until it arrives, dashboard
  // tiles show their server-rendered first-paint values and the
  // pairing / endpoints panels show their template-default empty
  // state.
  startEventStream();

  // Visibility-change re-arm — covers the laptop-sleep-then-wake
  // case where the in-flight EventSource silently failed to
  // reconnect. Cheap no-op on every short tab-switch (<60 s), but
  // closes + recycles the stream on a real long-idle wake.
  document.addEventListener("visibilitychange", handleVisibilityRestore);
});

// ---- Roots: the transcoded cache panel ----
//
// These controls came from the Library Inspector's storage bar. They
// belong here because this page is about where library data lives, and
// because the whole-library clear needs a typed confirmation that has
// no place beside a per-album coverage bar in Browse.

function initVariantsPanel() {
  const panel = document.getElementById("variants-panel");
  if (!panel) return;
  refreshVariantsDir();
  wireVariantsDirChange();
  wireVariantsClear();
  wireVariantsRetry();
}

// Last snapshot from /api/upscale/variants-dir. Held so the two dialogs
// can quote real numbers without a second round-trip on open.
let variantsDirState = null;

async function refreshVariantsDir() {
  try {
    variantsDirState = await API.get("/api/upscale/variants-dir");
  } catch (err) {
    // The panel is server-rendered with the counts it had at page load,
    // so a failed refresh leaves a correct-if-stale readout rather than
    // a blank one. Say so quietly instead of replacing the numbers.
    setText("variants-status", "Could not refresh cache figures: " + err.message);
    return;
  }
  const s = variantsDirState;
  setText("variants-dir", s.current);
  setText("variants-free", formatBytes(s.freeBytes) + " free");
  // Sizes only. The file counts beside them are server-rendered and
  // stay put: this endpoint reports usedByKind, not a per-kind file
  // count, so writing the whole row would drop the count.
  const byKind = s.usedByKind || {};
  setText("variants-upscaled", formatBytes(byKind.upscale || 0));
  setText("variants-optimized", formatBytes(byKind.optimize || 0));

  const legacy = document.getElementById("variants-legacy");
  if (legacy) {
    legacy.hidden = !s.legacyCount;
    if (s.legacyCount) {
      setText("variants-legacy-count", String(s.legacyCount));
      setText("variants-legacy-plural", s.legacyCount === 1 ? "" : "s");
      setText("variants-legacy-bytes", formatBytes(s.legacyBytes));
    }
  }
  const clear = document.getElementById("variants-clear");
  if (clear) clear.disabled = !s.usedBytes;
}

function wireVariantsDirChange() {
  const dialog = document.getElementById("variants-dir-dialog");
  const open = document.getElementById("variants-change");
  const save = document.getElementById("variants-dir-save");
  if (!dialog || !open || !save) return;

  open.addEventListener("click", () => {
    const input = document.getElementById("variants-dir-input");
    if (input) input.value = variantsDirState?.current || "";
    setText("variants-dir-default", variantsDirState?.default || "—");
    hideEl("variants-dir-error");
    dialog.showModal();
  });
  // Enter in the field would otherwise submit the <form method="dialog">
  // — and Cancel is the submit button, so the dialog closed and threw
  // the typed path away. Route it to Save, which is what pressing Enter
  // in a single-field dialog means.
  submitOnEnter("variants-dir-input", save);

  save.addEventListener("click", async () => {
    const input = document.getElementById("variants-dir-input");
    save.disabled = true;
    try {
      // The server validates: absolute, writable, and NOT inside a
      // library root. Re-checking here would be a second copy of a rule
      // that has already bitten once by drifting — config.Load rejects
      // symlinked paths this endpoint used to accept, and the bridge
      // then refused to start over a value the UI called fine.
      variantsDirState = await API.post("/api/upscale/variants-dir",
        { path: (input?.value || "").trim() });
      dialog.close();
      await refreshVariantsDir();
      setText("variants-status", "Storage path updated. Existing variants stay where they are.");
    } catch (err) {
      showError("variants-dir-error", err.message);
    } finally {
      save.disabled = false;
    }
  });
}

function wireVariantsClear() {
  const dialog = document.getElementById("variants-clear-dialog");
  const open = document.getElementById("variants-clear");
  const go = document.getElementById("variants-clear-go");
  const phrase = document.getElementById("variants-clear-phrase");
  if (!dialog || !open || !go || !phrase) return;

  open.addEventListener("click", () => {
    phrase.value = "";
    go.disabled = true;
    hideEl("variants-clear-error");
    setText("variants-clear-dir", variantsDirState?.current || "the cache");
    setText("variants-clear-bytes", formatBytes(variantsDirState?.usedBytes || 0));
    dialog.showModal();
  });

  // The typed phrase is the guard, so the button follows it exactly —
  // no trimming, no case folding. A prefix match is what made the old
  // bare [y/N] uninstall prompt a fat-finger hazard.
  phrase.addEventListener("input", () => { go.disabled = phrase.value !== "CLEAR"; });
  // Same trap as the path dialog, with more at stake: Enter closed the
  // confirmation without clearing anything, which reads as the button
  // being broken. It routes to the action — still gated on the exact
  // phrase, since `go` refuses when the value is anything else.
  submitOnEnter("variants-clear-phrase", go);

  go.addEventListener("click", async () => {
    if (phrase.value !== "CLEAR") return;
    go.disabled = true;
    try {
      const res = await API.delete("/api/upscale/variants?confirm=true");
      dialog.close();
      await refreshVariantsDir();
      setText("variants-status",
        `Cleared ${res.deletedCount} variant${res.deletedCount === 1 ? "" : "s"}, ` +
        `freed ${formatBytes(res.freedBytes)}.`);
    } catch (err) {
      showError("variants-clear-error", err.message);
      go.disabled = false;
    }
  });
}

function wireVariantsRetry() {
  const btn = document.getElementById("variants-retry-failures");
  if (!btn) return;
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    setText("variants-status", "Clearing failure debounces…");
    try {
      // No body: the endpoint's unscoped form is the whole library,
      // which is what a global control should mean. Its `{path}` form
      // exists for a future folder-scoped version.
      const res = await API.post("/api/upscale/failures/retry");
      setText("variants-status", res.cleared > 0
        ? `Cleared ${res.cleared} failure record${res.cleared === 1 ? "" : "s"}. ` +
          `Those sources will be retried on the next run.`
        : "No failing sources were being held back.");
    } catch (err) {
      setText("variants-status", "Retry failed: " + err.message);
    } finally {
      btn.disabled = false;
    }
  });
}

// submitOnEnter makes Enter in a dialog field mean "do the thing"
// rather than "submit the form". A <form method="dialog"> treats Enter
// as a submit, which closes the dialog and discards the input — and
// since these dialogs put Cancel first, that is exactly the wrong
// button.
function submitOnEnter(inputID, button) {
  const input = document.getElementById(inputID);
  if (!input || !button) return;
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    if (!button.disabled) button.click();
  });
}

function showError(id, msg) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = msg;
  el.hidden = false;
}

function hideEl(id) {
  const el = document.getElementById(id);
  if (el) el.hidden = true;
}
