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
//   * `SyntaxError` — `r.json()` failing on an empty body when the
//     202 reaches the browser before the exit. The handler does
//     not write a JSON body for the 202.
//
// Anything else (notably a plain `Error` from `errorFromResponse`,
// which wraps real 4xx/5xx) is a genuine failure and the caller
// should surface it instead of claiming the restart was signalled.
// (CodeRabbit on PR #124.)
function isExpectedRestartDisconnect(err) {
  return err instanceof TypeError || err instanceof SyntaxError;
}

// --- dashboard ---

function initDashboard() {
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

  // Updates: "Roll back" swaps the previous binary back in. Guarded by
  // a typed-intent confirm rather than a bare one — this replaces the
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

  // Updates panel: "Check now" forces a fresh GitHub poll. The handler
  // returns the post-check status so the tile refreshes in one trip.
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
  //
  // Also wired in renderUpdateTile when the button is added mid-tick
  // (e.g. server-rendered first paint had no update available). The
  // helper is shared so both entry paths run the same flow.
  bindInstallButton(document.getElementById("update-install"));

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
  setText("tracks-indexed", s.tracksIndexed);
  setText("device-count", s.deviceCount);
  // Library composition tiles (dashboard only; no-op elsewhere).
  setText("comp-originals", s.tracksIndexed);
  setText("comp-upscaled", s.tracksWithUpscaled ?? 0);
  setText("comp-optimized", s.tracksWithOptimized ?? 0);
  setText("comp-variant-files", s.variantFiles ?? 0);
  setText("comp-variant-bytes", humanBytes(s.variantBytes ?? 0));
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
// derived in CSS from data-kind + the --i index (stable palette).
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
    const span = document.createElement("span");
    span.className = unknown ? "dist-seg is-unknown" : "dist-seg";
    span.style.width = pct.toFixed(2) + "%";
    span.style.setProperty("--i", String(i));
    span.dataset.kind = kind;
    span.title = `${seg.label}: ${seg.count} (${pct.toFixed(1)}%)`;
    bar.appendChild(span);

    const item = document.createElement("span");
    item.className = unknown ? "dist-legend-item is-unknown" : "dist-legend-item";
    item.dataset.kind = kind;
    item.style.setProperty("--i", String(i));
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
    // Fire restart and don't await — the server tears the listener
    // down before we can read the response body anyway. The 2.5 s
    // auto-reload below is the empirical sweet-spot for launchd /
    // systemd / SCM respawn; under those it lands the operator
    // back on a working admin page. (Qodo on PR #124.) For
    // unsupervised processes there's no respawn, so the reload
    // would race a listener that's never coming back — replace
    // it with an in-place message instructing manual restart.
    fetch("/api/restart", {
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

function initLibrary() {
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
  const editPayload = JSON.stringify({
    identity, name: s.name, udn, manualURL,
    pathPrefix: "", rootObjectID: "", skipTopLevelContainers: [],
    // ↑ PathPrefix/RootObjectID/SkipTopLevelContainers aren't in the
    // GET /api/upnp/servers response shape (they're internal to the
    // YAML row). The modal renders them blank and the PATCH only sends
    // the fields the operator actually edits, so leaving them blank
    // here doesn't accidentally clear them on save.
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

// showUpnpRestartBanner injects (or refreshes) a one-time "Restart
// required" banner above the configured panel so the operator knows
// their CRUD action won't take effect until the next bridge restart.
// Idempotent — calling it twice doesn't stack banners.
function showUpnpRestartBanner() {
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
      Use the <a href="/settings#danger">Restart</a> button on the
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

// Tabbed Settings sections (PR feat/admin-shell-tabs-and-theme).
// The Settings page used to scroll forever — General / Networking /
// Updates / Audio quality each as a section header on one long form.
// Tabs flatten to one section visible at a time. Single shared form
// is preserved so the bottom Save button still commits every pending
// edit at once; switching tabs doesn't lose work.
//
// Active tab is persisted in sessionStorage so a save+restart bounce
// returns the operator where they were. Validates the persisted id
// against the live tab set so a future template that drops a tab
// can't trap the user on a hidden pane.
function initSettingsTabs() {
  const tabs = document.querySelectorAll(".tab-btn[data-tab]");
  const panes = document.querySelectorAll(".tab-pane[data-tab]");
  if (tabs.length === 0 || panes.length === 0) return;
  // Mark the page as tabs-enabled BEFORE applying hidden — without
  // this, the template renders every pane visible (no-JS fallback)
  // and the CSS rule that visually hides the in-pane <h2> is gated
  // on this class so headings still appear in the fallback. (Qodo
  // on PR #129: don't lock operators out of Settings if JS fails
  // or is skipped by a future refactor.)
  const page = document.querySelector(".page.settings");
  if (page) page.classList.add("tabs-enabled");
  const STORAGE_KEY = "settings.activeTab";
  const validIds = new Set();
  tabs.forEach(t => validIds.add(t.dataset.tab));
  function activate(id) {
    if (!validIds.has(id)) return;
    tabs.forEach(t => {
      const on = t.dataset.tab === id;
      t.classList.toggle("active", on);
      t.setAttribute("aria-selected", on ? "true" : "false");
      // tabindex makes only the active tab keyboard-tabbable; arrow
      // keys move within the role=tablist group per WAI-ARIA.
      t.tabIndex = on ? 0 : -1;
    });
    panes.forEach(p => {
      p.hidden = p.dataset.tab !== id;
    });
    try { sessionStorage.setItem(STORAGE_KEY, id); } catch { /* private mode */ }
  }
  tabs.forEach(t => {
    t.addEventListener("click", () => activate(t.dataset.tab));
  });
  // Arrow-key navigation across the tab strip — standard WAI-ARIA
  // tablist convention. Operators who navigate by keyboard expect
  // Left/Right (and Home/End) on a focused tab to move within the
  // group rather than jump out into the form.
  const tabsArr = Array.from(tabs);
  tabsArr.forEach((t, i) => {
    t.addEventListener("keydown", (e) => {
      let nextIdx = -1;
      if (e.key === "ArrowRight") nextIdx = (i + 1) % tabsArr.length;
      else if (e.key === "ArrowLeft") nextIdx = (i - 1 + tabsArr.length) % tabsArr.length;
      else if (e.key === "Home") nextIdx = 0;
      else if (e.key === "End") nextIdx = tabsArr.length - 1;
      if (nextIdx >= 0) {
        e.preventDefault();
        const target = tabsArr[nextIdx];
        activate(target.dataset.tab);
        target.focus();
      }
    });
  });
  let saved = null;
  try { saved = sessionStorage.getItem(STORAGE_KEY); } catch { /* private mode */ }
  // ?tab=<id> deep-link (the Jobs page links straight to a section)
  // outranks the sessionStorage restore; both validate against the
  // rendered tab ids so a stale/typo'd value falls back cleanly.
  const urlTab = new URLSearchParams(window.location.search).get("tab");
  if (urlTab && validIds.has(urlTab)) {
    activate(urlTab);
  } else {
    activate(saved && validIds.has(saved) ? saved : tabsArr[0].dataset.tab);
  }
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
  autocertPollTimer = setInterval(refreshAutocertTile, 60_000);

  const form = document.getElementById("settings-form");
  const msg = document.getElementById("settings-msg");
  const restartBtn = document.getElementById("restart-btn");
  if (!form) return;

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
      // Enrich upstream base URLs, resolved from the source picker above
      // (blank = public MusicBrainz / Cover Art defaults; atlas = derived
      // <url>/ws/2 + <url>; custom = the raw Advanced fields). Server
      // validates + normalizes; restart-required.
      enrichMusicBrainzBaseURL: enrichBases.mb,
      enrichCoverArtBaseURL: enrichBases.ca,
      // Rich-tier Atlas metadata opt-in (bios/descriptions via the app
      // ferry). Restart-required; same checkbox-coerce pattern.
      atlasEnabled: fd.get("atlasEnabled") === "on",
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
      showMsg(msg, r.restartRequired ? "warn" : "ok",
        r.restartRequired
          ? "Saved. Some fields need a restart to take effect."
          : "Saved.");
      restartBtn.hidden = !r.restartRequired;
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
    try {
      await API.post("/api/restart");
      const post = supervised
        ? "Restart signalled. Reload the page in a few seconds."
        : "Stop signalled. Start the bridge again manually, then reload.";
      showMsg(msg, "warn", post);
    } catch (err) {
      if (isExpectedRestartDisconnect(err)) {
        // The bridge writes 202 then `os.Exit(0)` after a 100 ms
        // grace window — we may catch the response (`TypeError`
        // when the connection drops mid-read, or `SyntaxError`
        // when the empty body fails JSON.parse). Both mean
        // "request was honoured, server is on its way out."
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
        // `inspector-delete-variants-btn` 503 handler — same
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
function applyUpscale(r) {
  applyUpscaleStats(r); // Settings tile
  renderWorkerGrid(r); // Jobs page live pipeline
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
// Library Inspector (v1.4 folder-first navigation)
// =============================================================

const inspectorState = {
  path: "", // current navigation path; "" = library root
  selection: null, // {kind: "folder"|"track", row}
  lastBrowseData: null, // last successful browse response (used by toolbar action + search filter)
  // Multi-select state (PR feat/library-inspector-tiles). Map of
  // folder path → {trackCount, upscaledCount, optimizedCount}
  // snapshot, captured from the checkbox's data-* attrs at
  // toggle time. Using a Map (vs the original Set) means the
  // panel batch aggregator iterates O(M) over the map's values
  // — no document.querySelector calls per checkbox tick (Gemini
  // medium on PR #276: the prior implementation was O(M×N) where
  // M=selected and N=total tiles in the DOM). The action panel
  // auto-opens in batch mode when map.size > 0. Cleared on every
  // browse re-render.
  selectedPaths: new Map(),
  // Unified floating action panel state (replaces drawerClosedByUser).
  // `panelMode`: "single" (anchored to a tile / heading ⓘ),
  //              "batch"  (multi-select bottom-center),
  //               null    (closed).
  // `panelExpandedKind`: which card is expanded — preserved across
  //                       opens so the operator's last choice sticks.
  // The Escape + focus-trap listeners are stored here so removal at
  // close-time is symmetric with their addEventListener pair.
  panelMode: null,
  panelExpandedKind: "upscale",
  panelEscapeHandler: null,
  panelFocusHandler: null,
  // SoX availability snapshot — seeded once at init() time from
  // the page root's data-sox-available attribute. Used by the
  // selection bar + per-tile menu to gate generate actions.
  soxAvailable: true,
  // Atlas metadata layer (seeded from data-atlas-enabled /
  // data-booklets-enabled at init). atlasEnabled gates the About
  // card fetch; tile artwork refs are fetched regardless (covers
  // exist without Atlas). lastTileMeta memoizes the current path's
  // children-refs response so load-more pages decorate without a
  // refetch.
  atlasEnabled: false,
  bookletsEnabled: false,
  lastTileMeta: null,
  // Search state (PR B). `mode` is "browse" (default) or "search"
  // (flat-results view). `searchQuery` is the active query; the
  // debounced timer + in-flight controller live in module-level
  // vars below.
  mode: "browse",
  searchQuery: "",
  searchActiveIndex: -1, // keyboard navigation cursor in dropdown
  // Pagination state (PR C). The cursors track folder + track
  // pagination INDEPENDENTLY — each turns empty when its
  // collection is exhausted, which means subsequent Load-more
  // requests skip that collection on the server.
  nextFolderCursor: null,
  nextTrackCursor: null,
  // camelot is the active harmonic-key filter ("" = none). When set, the
  // inspector shows a flat, library-wide list of analyzed tracks in that
  // Camelot key (the coverage-wheel deep-link) instead of a folder browse.
  camelot: "",
  totalFolders: 0,
  totalTracks: 0,
  // renderedFolders / renderedTracks track the count of rows in
  // the tbody for the CURRENT navigation. Bumped per appended row
  // by inspectorAppendRows; used by Load-more sentinel math.
  // Maintaining counters here is O(1) vs the prior
  // `querySelectorAll('tr[data-kind=...]').length` walk which was
  // O(N) per pagination cycle (Gemini medium on PR C).
  renderedFolders: 0,
  renderedTracks: 0,
  loadingMore: false, // re-entrancy guard for Load-more clicks / observer fires
  // Generation counter for rAF-chunked rendering. Bumped on every
  // inspectorNavigate AND every replace-render. The pump() body
  // captures its expected generation at spawn time and bails out
  // if the live generation has moved on — prevents chunks from a
  // stale folder from interleaving into the new folder's table
  // (Gemini HIGH on PR C).
  renderGeneration: 0,
};

// IntersectionObserver instance for auto-load-more sentinel.
// Lazily constructed; same observer reused across folder navigates
// to avoid leaks.
let inspectorLoadMoreObserver = null;

// Search debounce + race-cancel state. Module-level so the search
// handler can clear the timer between keystrokes AND cancel an
// in-flight fetch when a newer query is typed. Mirrors the same
// pattern the browse handler's race-guards use.
let inspectorSearchDebounce = null;
let inspectorSearchController = null;

function initLibraryInspector() {
  // Initial view comes from the query so bookmarks / refresh / the
  // coverage-wheel deep-link land correctly: `?camelot=8A` opens the
  // harmonic-key filter (flat list), else `?path=` opens that folder,
  // else the library root.
  const params = new URLSearchParams(window.location.search);
  const initialCamelot = params.get("camelot") || "";
  const initialPath = initialCamelot ? "" : (params.get("path") || "");
  // Replace (not push) the initial entry so the user's first Back
  // press doesn't land them on a redundant query-string history slot.
  history.replaceState(
    { path: initialPath, camelot: initialCamelot, scrollY: 0 }, "",
    inspectorURLFor(initialPath, initialCamelot));
  inspectorNavigate(initialPath, { skipHistory: true, camelot: initialCamelot });

  // Breadcrumb clicks
  document.getElementById("inspector-breadcrumbs")
    .addEventListener("click", (e) => {
      const a = e.target.closest("a[data-path]");
      if (!a) return;
      e.preventDefault();
      inspectorNavigate(a.dataset.path);
    });

  // Toolbar navigation
  document.getElementById("inspector-nav-up")
    .addEventListener("click", inspectorNavigateUp);
  document.getElementById("inspector-nav-back")
    .addEventListener("click", () => history.back());
  document.getElementById("inspector-nav-home")
    .addEventListener("click", () => inspectorNavigate(""));

  // Current-folder ⓘ affordance: opens the unified action panel.
  document.getElementById("inspector-current-info")
    ?.addEventListener("click", () => {
      inspectorOpenProjectionForCurrent();
    });

  // Panel close + clear-selection. Generate / Delete / expand wiring
  // is handled per-card by event delegation below.
  document.getElementById("panel-close-btn")
    ?.addEventListener("click", inspectorClosePanel);
  document.getElementById("panel-clear-selection")
    ?.addEventListener("click", inspectorClearSelection);

  // Card expand/collapse via the summary trigger (click + keyboard).
  for (const trigger of document.querySelectorAll(".card-summary-trigger")) {
    trigger.addEventListener("click", inspectorOnCardTriggerActivate);
    trigger.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        inspectorOnCardTriggerActivate(e);
      }
    });
  }

  // Per-kind Generate / Delete buttons live inside the action panel
  // now. Single-tile and batch modes both route through these — the
  // button's `data-kind` carries the verb's kind.
  for (const btn of document.querySelectorAll(".panel-generate-btn")) {
    btn.addEventListener("click", inspectorPanelGenerateClick);
  }
  for (const btn of document.querySelectorAll(".panel-delete-btn")) {
    btn.addEventListener("click", inspectorPanelDeleteClick);
  }

  // Batch-confirmation overlay buttons.
  document.getElementById("panel-confirm-cancel")
    ?.addEventListener("click", inspectorPanelCancelConfirm);
  document.getElementById("panel-confirm-submit")
    ?.addEventListener("click", inspectorPanelConfirmSubmit);

  // Two independent Load-more buttons (folders + tracks). The shared
  // inspectorLoadMore reads both cursors and the server returns
  // pages for whichever collection is still in flight; the
  // IntersectionObserver in inspectorRefreshLoadMoreSentinel also
  // attaches to these buttons for auto-fire.
  for (const btn of document.querySelectorAll(".load-more")) {
    btn.addEventListener("click", inspectorLoadMore);
  }

  // Tile checkbox change → multi-select state update + panel
  // auto-open/close. ⓘ tile-button click → open panel in single
  // mode anchored to that tile. Both via event delegation from the
  // inspector content root so listeners survive chunked appends.
  const contentRoot = document.getElementById("inspector-content");
  if (contentRoot) {
    contentRoot.addEventListener("change", inspectorOnTileCheckboxChange);
    contentRoot.addEventListener("click", (ev) => {
      const infoBtn = ev.target.closest(".tile-info-btn");
      if (!infoBtn) return;
      ev.stopPropagation();
      ev.preventDefault();
      const tile = infoBtn.closest(".inspector-tile");
      const path = tile?.dataset.path;
      if (path == null) return;
      const folders = inspectorState.lastBrowseData?.folders || [];
      const folder = folders.find((f) => f.path === path);
      if (folder) inspectorOpenPanelSingle(folder);
    });
  }

  // SoX availability snapshot (set once at mount). The CSS gate
  // (`[data-sox-available="false"] [data-needs-sox]`) handles
  // visual state; JS uses this for the selection bar's
  // gate-disabled check.
  const page = document.querySelector(".library-inspector");
  inspectorState.soxAvailable = page?.dataset.soxAvailable !== "false";
  // Atlas metadata layer gates (set once at mount, mirroring the
  // template's server-side flags).
  inspectorState.atlasEnabled = page?.dataset.atlasEnabled === "true";
  inspectorState.bookletsEnabled = page?.dataset.bookletsEnabled === "true";
  const aboutRetryBtn = document.getElementById("panel-about-retry");
  if (aboutRetryBtn) aboutRetryBtn.addEventListener("click", inspectorAboutRetryClick);

  // Sticky-stack height tracker (from PR A): write actual measured
  // offsetHeight of the toolbar + storage bar into CSS custom
  // properties so the downstream sticky elements (storage bar's
  // top, table header's top) bind to the LIVE heights rather than
  // a hardcoded single-row assumption.
  updateInspectorStickyHeights();
  window.addEventListener("resize", updateInspectorStickyHeights);
  if (typeof ResizeObserver === "function") {
    const search = document.getElementById("inspector-search-slot");
    const toolbar = document.getElementById("inspector-toolbar");
    const storage = document.getElementById("inspector-storage-bar");
    if (search || toolbar || storage) {
      const ro = new ResizeObserver(updateInspectorStickyHeights);
      if (search) ro.observe(search);
      if (toolbar) ro.observe(toolbar);
      if (storage) ro.observe(storage);
    }
  }

  // Search wiring (PR B). Input is in the toolbar slot; results
  // render as a dropdown overlay below or as a flat-list view
  // when the operator clicks "View all results →".
  const searchInput = document.getElementById("inspector-search");
  if (searchInput) {
    searchInput.addEventListener("input", inspectorSearchInputChanged);
    searchInput.addEventListener("keydown", inspectorSearchKeyDown);
  }
  // Global `/` shortcut: focus the search input from anywhere on
  // the page unless the user is already typing into another input.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/") return;
    const tgt = e.target;
    if (tgt && (tgt.tagName === "INPUT" || tgt.tagName === "TEXTAREA" || tgt.isContentEditable)) {
      return;
    }
    e.preventDefault();
    searchInput?.focus();
  });
  // Click outside the dropdown to dismiss it.
  document.addEventListener("click", (e) => {
    const dropdown = document.getElementById("inspector-search-results");
    if (!dropdown || dropdown.hidden) return;
    const slot = document.getElementById("inspector-search-slot");
    if (slot && !slot.contains(e.target)) {
      inspectorSearchHideDropdown();
    }
  });

  // Browser history integration. popstate restores path + scroll
  // from the entry's stored state without re-pushing history.
  window.addEventListener("popstate", (ev) => {
    const sp = new URLSearchParams(window.location.search);
    const target = (ev.state && typeof ev.state.path === "string")
      ? ev.state.path
      : (sp.get("path") || "");
    const camelot = (ev.state && typeof ev.state.camelot === "string")
      ? ev.state.camelot
      : (sp.get("camelot") || "");
    inspectorNavigate(target, {
      skipHistory: true,
      camelot,
      restoreScroll: ev.state ? ev.state.scrollY : 0,
    });
  });

  // Variants-storage bar wiring (PR D2). Element-presence-guarded so
  // a bridge build without this template region (older custom
  // deployments) just no-ops cleanly.
  const storageBar = document.getElementById("inspector-storage-bar");
  if (storageBar) {
    inspectorStorageRefresh();
    const changeBtn = document.getElementById("inspector-storage-change");
    if (changeBtn) {
      changeBtn.addEventListener("click", inspectorStorageOpenModal);
    }
    const cancelBtn = document.getElementById("inspector-storage-modal-cancel");
    if (cancelBtn) {
      cancelBtn.addEventListener("click", inspectorStorageCloseModal);
    }
    const saveBtn = document.getElementById("inspector-storage-modal-save");
    if (saveBtn) {
      saveBtn.addEventListener("click", inspectorStorageSubmit);
    }
    const modalInput = document.getElementById("inspector-storage-modal-input");
    if (modalInput) {
      modalInput.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          inspectorStorageSubmit();
        } else if (e.key === "Escape") {
          inspectorStorageCloseModal();
        }
      });
    }
  }
}

// inspectorStorageRefresh hits GET /api/upscale/variants-dir and
// populates the storage bar with current path + usage stats +
// legacy-variant count. Called on inspector mount AND after every
// successful POST so the bar reflects the saved state without a
// page reload.
async function inspectorStorageRefresh() {
  try {
    const res = await fetch("/api/upscale/variants-dir");
    if (!res.ok) {
      // 404 (route not wired in older builds) / 500 — surface nothing
      // rather than breaking the inspector.
      return;
    }
    const data = await res.json();
    const pathEl = document.getElementById("inspector-storage-path");
    if (pathEl) pathEl.textContent = data.current || "—";
    const stats = document.getElementById("inspector-storage-stats");
    if (stats) {
      // Per-kind breakdown (Library Inspector tile-redesign PR).
      // When both kinds are present in `usedByKind`, surface the
      // parenthesised split alongside the total. Falls back
      // gracefully on pre-feature bridge responses that omit the
      // field.
      let usedHTML = humanBytes(data.usedBytes);
      const byKind = data.usedByKind;
      if (byKind && (byKind.upscale > 0 || byKind.optimize > 0)) {
        usedHTML += ` <span class="storage-used-breakdown">(upscale ${humanBytes(byKind.upscale || 0)} · optimize ${humanBytes(byKind.optimize || 0)})</span>`;
      }
      document.getElementById("inspector-storage-used").innerHTML = usedHTML;
      document.getElementById("inspector-storage-free").textContent = humanBytes(data.freeBytes);
      stats.hidden = false;
    }
    const legacy = document.getElementById("inspector-storage-legacy");
    if (legacy) {
      if ((data.legacyCount || 0) > 0) {
        document.getElementById("inspector-storage-legacy-count").textContent =
          String(data.legacyCount);
        document.getElementById("inspector-storage-legacy-bytes").textContent =
          humanBytes(data.legacyBytes);
        legacy.hidden = false;
      } else {
        legacy.hidden = true;
      }
    }
    // Stash the default + current values on the modal so the user
    // sees them when they open the Change dialog.
    const defaultEl = document.getElementById("inspector-storage-modal-default");
    if (defaultEl) defaultEl.textContent = data.default || "—";
    const input = document.getElementById("inspector-storage-modal-input");
    if (input) input.placeholder = data.default || "/mnt/external/variants";
  } catch {
    // Best-effort. The bar is informational; a transient blip
    // doesn't warrant a banner.
  }
}

function inspectorStorageOpenModal() {
  const modal = document.getElementById("inspector-storage-modal");
  if (!modal) return;
  const input = document.getElementById("inspector-storage-modal-input");
  const errEl = document.getElementById("inspector-storage-modal-error");
  if (errEl) errEl.hidden = true;
  // Pre-fill with the current path so the operator can edit in place
  // rather than retype from scratch.
  if (input) {
    const current = document.getElementById("inspector-storage-path")?.textContent || "";
    input.value = current;
    setTimeout(() => input.focus(), 0); // after the show transition
  }
  modal.hidden = false;
}

function inspectorStorageCloseModal() {
  const modal = document.getElementById("inspector-storage-modal");
  if (modal) modal.hidden = true;
}

async function inspectorStorageSubmit() {
  const input = document.getElementById("inspector-storage-modal-input");
  const errEl = document.getElementById("inspector-storage-modal-error");
  const saveBtn = document.getElementById("inspector-storage-modal-save");
  if (!input || !errEl || !saveBtn) return;
  errEl.hidden = true;
  saveBtn.disabled = true;
  try {
    const res = await fetch("/api/upscale/variants-dir", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: input.value.trim() }),
    });
    if (res.status === 400) {
      const data = await res.json();
      errEl.textContent = data.message || "Validation failed";
      errEl.hidden = false;
      saveBtn.disabled = false;
      return;
    }
    if (!res.ok) {
      const body = await res.text();
      throw new Error(body || `HTTP ${res.status}`);
    }
    // Server returned the refreshed snapshot — render it directly.
    const data = await res.json();
    const pathEl = document.getElementById("inspector-storage-path");
    if (pathEl) pathEl.textContent = data.current || "—";
    inspectorStorageRefresh(); // updates stats + legacy count
    inspectorStorageCloseModal();
  } catch (err) {
    errEl.textContent = `Couldn’t save: ${err.message}`;
    errEl.hidden = false;
  } finally {
    saveBtn.disabled = false;
  }
}

// updateInspectorStickyHeights measures the toolbar + storage bar
// offsetHeights and writes them as CSS custom properties so the
// sticky-stack `top:` values flow through the cascade. Called on
// init, window resize, and ResizeObserver events. If the toolbar
// wraps to N lines (narrow viewport / large AX text), the JS-measured
// height is the AUTHORITATIVE one — the CSS fallback assumes a
// single-row toolbar (good default for the common case).
function updateInspectorStickyHeights() {
  const search = document.getElementById("inspector-search-slot");
  const toolbar = document.getElementById("inspector-toolbar");
  const storage = document.getElementById("inspector-storage-bar");
  const root = document.querySelector(".library-inspector");
  if (!root) return;
  if (search) {
    root.style.setProperty("--inspector-search-h", `${search.offsetHeight}px`);
  }
  if (toolbar) {
    root.style.setProperty("--inspector-toolbar-h", `${toolbar.offsetHeight}px`);
  }
  if (storage) {
    root.style.setProperty("--inspector-storage-h", `${storage.offsetHeight}px`);
  }
}

// inspectorURLFor builds the canonical URL for a given inspector
// path. Centralised so the pushState / replaceState / initial-load
// branches don't drift in encoding.
function inspectorURLFor(path, camelot) {
  if (camelot) return `/library/inspector?camelot=${encodeURIComponent(camelot)}`;
  return path
    ? `/library/inspector?path=${encodeURIComponent(path)}`
    : `/library/inspector`;
}

function inspectorNavigateUp() {
  // Parent folder = strip the last `/`-segment of the current path.
  // Disabled at the library root via the `aria-disabled` flag the
  // toolbar updater sets after each navigate.
  if (!inspectorState.path) return;
  const parts = inspectorState.path.split("/");
  parts.pop();
  inspectorNavigate(parts.join("/"));
}

// inspectorNavigate is the single state-mutation entrypoint for
// folder navigation. Options:
//   - skipHistory: don't push a new history entry (used by initial
//     load + popstate handler).
//   - restoreScroll: number of pixels to scrollTo after the table
//     body re-renders (popstate-driven restoration).
async function inspectorNavigate(path, opts = {}) {
  // The harmonic-key filter ("" = none) is part of the view identity.
  // Any navigate WITHOUT opts.camelot clears it (clicking a folder or the
  // breadcrumb root exits the key filter back to a normal browse).
  const camelot = opts.camelot || "";
  // Same-path refresh: don't add a duplicate history entry. This
  // covers callers like inspectorDeleteVariants that re-navigate
  // to the current folder to refresh row data after mutation, AND
  // a user clicking the active breadcrumb crumb. A path match with a
  // DIFFERENT camelot still pushes (it's a different view).
  if (!opts.skipHistory && history.state
    && history.state.path === path
    && (history.state.camelot || "") === camelot) {
    opts = { ...opts, skipHistory: true };
  }
  // Capture the OUTGOING entry's scroll position BEFORE we mutate
  // state — the user might pop back to this exact folder later.
  if (!opts.skipHistory && history.state) {
    history.replaceState(
      { ...history.state, scrollY: window.scrollY },
      "",
      window.location.href,
    );
  }

  inspectorState.path = path;
  inspectorState.camelot = camelot;
  // Invalidate any in-flight chunked-render pump from a previous
  // navigation or load-more BEFORE the new fetch starts. Pre-fix
  // the generation was only bumped INSIDE inspectorAppendRows on
  // the new render — which means between the navigation start and
  // that bump (could be hundreds of ms on a slow browse fetch), a
  // stale load-more pump would keep appending rows from the old
  // folder under the new path. CodeRabbit Major late on PR #246.
  inspectorState.renderGeneration++;
  inspectorState.lastBrowseData = null;
  inspectorState.lastTileMeta = null;
  // Any navigation exits search mode — covers the case where the
  // operator clicked a search-result folder/track and we land in
  // browse mode without an explicit Exit. Without this reset the
  // load-more sentinel's `mode === "search"` short-circuit kept
  // pagination permanently disabled in the post-search folder.
  // CodeRabbit Major on PR #246 round-2.
  inspectorState.mode = "browse";
  inspectorRenderBreadcrumbs(path);
  inspectorUpdateToolbarState(path);
  // Every navigation closes the panel. The new "explicit-open" model
  // (ⓘ icon / heading ⓘ / multi-select) replaces the prior
  // auto-open-on-render flow.
  inspectorClosePanel();
  document.getElementById("inspector-error").hidden = true;
  document.getElementById("inspector-current-heading").textContent =
    "Loading…";
  document.title = camelot
    ? `Library Inspector — Key ${camelot}`
    : `Library Inspector — ${path || "Root"}`;

  if (!opts.skipHistory) {
    history.pushState({ path, camelot, scrollY: 0 }, "", inspectorURLFor(path, camelot));
  }

  try {
    const browseQuery = camelot
      ? `camelot=${encodeURIComponent(camelot)}`
      : `path=${encodeURIComponent(path)}`;
    const res = await fetch(`/api/library/browse?${browseQuery}`);
    // Race guard: a slow response from an earlier navigation must
    // not overwrite the newer navigation's content. Compare against
    // the live `inspectorState.path` AND `.camelot` set synchronously
    // at the top of this call; subsequent navigations bump them before
    // their own fetch awaits. The camelot check is load-bearing: every
    // key view has path "", so without it a slow 8A response would
    // overwrite a newer 8B view. Per Gemini medium on PR #202 +
    // Gemini HIGH on PR #444.
    if (inspectorState.path !== path || inspectorState.camelot !== camelot) {
      return;
    }
    if (!res.ok) {
      throw new Error(`browse: HTTP ${res.status}`);
    }
    const data = await res.json();
    if (inspectorState.path !== path || inspectorState.camelot !== camelot) {
      return;
    }
    inspectorState.lastBrowseData = data;
    // Await inspectorRender → the chunked-render pump fully
    // populates the table body before we restore scroll.
    // Without this await, only the first 200-row chunk has
    // appended when scrollTo runs and the browser clamps a
    // deep scroll-target to the partial document height.
    // CodeRabbit Major on PR #246 round-2.
    await inspectorRender(data);
    if (inspectorState.path !== path || inspectorState.camelot !== camelot) return;
    // Atlas tile decoration — fire-and-forget AFTER the awaited
    // render so it can never block the render/scroll-restore chain;
    // its own path race-guard drops stale responses.
    if (!camelot) inspectorFetchTileMeta(path);
    // Restore scroll after the table body is fully realized.
    const targetY = typeof opts.restoreScroll === "number"
      ? opts.restoreScroll
      : 0;
    requestAnimationFrame(() => window.scrollTo(0, targetY));
  } catch (err) {
    if (inspectorState.path !== path || inspectorState.camelot !== camelot) {
      return;
    }
    document.getElementById("inspector-error").hidden = false;
    document.getElementById("inspector-error").textContent =
      `Couldn’t load this folder: ${err.message}`;
    document.getElementById("inspector-current-heading").textContent =
      pathLabel(path);
  }
}

// inspectorUpdateToolbarState reflects the current path into the
// toolbar control affordances: Up disabled at root; Upscale +
// Delete enabled iff the current folder has any tracks at all
// (toggled true when lastBrowseData lands).
function inspectorUpdateToolbarState(path) {
  const upBtn = document.getElementById("inspector-nav-up");
  const homeBtn = document.getElementById("inspector-nav-home");
  const atRoot = !path;
  upBtn.disabled = atRoot;
  upBtn.setAttribute("aria-disabled", atRoot ? "true" : "false");
  if (atRoot) {
    homeBtn.setAttribute("aria-current", "page");
  } else {
    homeBtn.removeAttribute("aria-current");
  }
  // Current-folder info button starts disabled; flipped on once
  // browse data lands AND the folder is non-empty.
  const infoBtn = document.getElementById("inspector-current-info");
  if (infoBtn) infoBtn.disabled = true;
}

// inspectorOpenProjectionForCurrent opens the action panel for the
// OPEN folder (rather than a child tile). Built from the cached
// browse rollup; the projection endpoint accepts the path as-is and
// walks server-side, so the synthetic numbers here are only used to
// pre-populate the panel's "Tracks / Source size" rows before the
// projection fetch lands. Root path is semantically valid; the
// projection endpoint handles empty path as "whole library."
function inspectorOpenProjectionForCurrent() {
  const data = inspectorState.lastBrowseData;
  if (!data) return; // toolbar button is disabled in this state, defensive
  // Prefer the server's recursive subtree rollup over a client-side sum of
  // the returned page — the latter under-counts whenever the node has more
  // folders/tracks than one page (the 647-folder root showed ~13k of 25k).
  // Fall back to the page sum only if an older bridge omits the field.
  const fallback = inspectorBrowseRollup(data);
  const folder = {
    name: inspectorState.path || "Library root",
    path: inspectorState.path,
    trackCount: data.subtreeTracks ?? fallback.trackCount,
    upscaledCount: data.subtreeUpscaled ?? fallback.upscaledCount,
    optimizedCount: data.subtreeOptimized ?? fallback.optimizedCount,
    totalSizeBytes: data.subtreeSizeBytes ?? fallback.totalSizeBytes,
    upscaleEligibleCount: data.subtreeUpscaleEligible ?? fallback.upscaleEligible,
    optimizeEligibleCount: data.subtreeOptimizeEligible ?? fallback.optimizeEligible,
  };
  inspectorOpenPanelSingle(folder);
}

// inspectorBrowseRollup aggregates a browse response's folders +
// tracks arrays into a single (trackCount, upscaledCount,
// optimizedCount, totalSizeBytes) rollup in one pass (vs the prior
// three-pass reduce shape). Used by inspectorOpenProjectionForCurrent
// and the toolbar-action enablement check in inspectorRender — both
// previously did the same math inline. Extracted per Gemini medium
// on PR A; extended to track per-kind variant coverage in the
// Library Inspector tile-redesign PR.
function inspectorBrowseRollup(data) {
  const folders = data.folders || [];
  const tracks = data.tracks || [];
  let trackCount = tracks.length;
  let upscaledCount = 0;
  let optimizedCount = 0;
  let totalSizeBytes = 0;
  // Page-sum eligible denominators (fallback only — the server's
  // subtree fields win). Loose tracks count as eligible here: their
  // per-track eligibility isn't in the browse row, and over-counting
  // in a degraded fallback beats under-counting (bar can only read
  // lower, never a false 100%).
  let upscaleEligible = tracks.length;
  let optimizeEligible = tracks.length;
  for (const t of tracks) {
    if (t.isUpscaled) upscaledCount++;
    if (t.isOptimized) optimizedCount++;
    totalSizeBytes += t.sizeBytes || 0;
  }
  for (const f of folders) {
    trackCount += f.trackCount || 0;
    upscaledCount += f.upscaledCount || 0;
    optimizedCount += f.optimizedCount || 0;
    totalSizeBytes += f.totalSizeBytes || 0;
    upscaleEligible += f.upscaleEligibleCount ?? f.trackCount ?? 0;
    optimizeEligible += f.optimizeEligibleCount ?? f.trackCount ?? 0;
  }
  return {
    trackCount, upscaledCount, optimizedCount, totalSizeBytes,
    upscaleEligible, optimizeEligible,
  };
}

// inspectorClosePanel hides the floating action panel and tears
// down the Escape + focus-trap listeners. Symmetric counterpart to
// inspectorOpenPanelSingle / inspectorOpenPanelBatch.
function inspectorClosePanel() {
  inspectorState.selection = null;
  inspectorState.panelMode = null;
  const panel = document.getElementById("inspector-action-panel");
  if (!panel) {
    inspectorA11yListeners("none");
    return;
  }
  delete panel.dataset.mode;
  delete panel.dataset.confirming;
  // Clear inline-positioned coordinates so the next open recomputes.
  panel.style.top = "";
  panel.style.left = "";
  panel.style.right = "";
  panel.style.bottom = "";
  const overlay = document.getElementById("panel-confirm-overlay");
  if (overlay) overlay.hidden = true;
  if (typeof panel.hidePopover === "function"
    && panel.matches(":popover-open")) {
    panel.hidePopover();
  }
  inspectorA11yListeners("none");
}

function inspectorRenderBreadcrumbs(path) {
  const crumbs = document.getElementById("inspector-breadcrumbs");
  crumbs.innerHTML = "";
  // Root crumb always present.
  const root = document.createElement("a");
  root.href = "#";
  root.dataset.path = "";
  root.textContent = "Library";
  crumbs.appendChild(root);
  // Harmonic-key filter view: show "Library › Key 8A" instead of a folder
  // trail. The root crumb (dataset.path="") is the CLEAR affordance — its
  // click handler calls inspectorNavigate("") with no camelot, exiting the
  // filter back to a normal browse.
  if (inspectorState.camelot) {
    crumbs.appendChild(document.createTextNode(" › "));
    const span = document.createElement("span");
    span.className = "inspector-key-crumb";
    span.textContent = `Key ${inspectorState.camelot}`;
    crumbs.appendChild(span);
    return;
  }
  if (!path) return;
  const parts = path.split("/");
  let acc = "";
  for (const part of parts) {
    crumbs.appendChild(document.createTextNode(" › "));
    acc = acc ? `${acc}/${part}` : part;
    const a = document.createElement("a");
    a.href = "#";
    a.dataset.path = acc;
    a.textContent = part;
    crumbs.appendChild(a);
  }
}

// Returns a Promise resolved once the chunked-render pump has
// fully populated the table body (CodeRabbit Major on PR #246
// round-2). The caller (inspectorNavigate) awaits this so scroll
// restoration runs against the FINAL document height, not the
// partial height after just the first 200-row chunk.
async function inspectorRender(data) {
  document.getElementById("inspector-current-heading").textContent =
    data.keyFilter
      ? `Key ${data.keyFilter}${data.keyName ? " — " + data.keyName : ""}`
      : pathLabel(data.path);

  // Capture pagination metadata so Load-more (manual click or
  // IntersectionObserver auto-fire) can advance against the right
  // cursors. Empty cursors = exhausted on the server side.
  inspectorState.nextFolderCursor = data.nextFolderCursor || "";
  inspectorState.nextTrackCursor = data.nextTrackCursor || "";
  inspectorState.totalFolders = data.totalFolders || 0;
  inspectorState.totalTracks = data.totalTracks || 0;

  // Multi-select is per-navigation — clear on every browse render
  // so a stale selection from a prior folder doesn't bleed into
  // the new view. inspectorClosePanel was already called in
  // inspectorNavigate above; defensive close here too in case
  // inspectorRender is invoked via another path.
  inspectorState.selectedPaths.clear();
  inspectorClosePanel();

  const folders = data.folders || [];
  const tracks = data.tracks || [];
  const hasAnyTracks = (data.subtreeTracks ?? data.totalTracks ?? 0) > 0
    || inspectorBrowseRollup(data).trackCount > 0;
  const currentInfoBtn = document.getElementById("inspector-current-info");
  if (currentInfoBtn) currentInfoBtn.disabled = !hasAnyTracks;

  if (folders.length === 0 && tracks.length === 0) {
    document.getElementById("inspector-content").hidden = true;
    document.getElementById("inspector-empty").hidden = false;
    return;
  }
  document.getElementById("inspector-content").hidden = false;
  document.getElementById("inspector-empty").hidden = true;

  // Two independent sub-grids: folders + tracks paginate
  // independently, so each gets its own section/load-more
  // sentinel. Hide a section entirely if its collection is empty
  // to avoid a stray empty header.
  document.getElementById("folders-section").hidden = folders.length === 0;
  document.getElementById("tracks-section").hidden = tracks.length === 0;
  document.getElementById("folders-count").textContent =
    String(data.totalFolders || folders.length);
  document.getElementById("tracks-count").textContent =
    String(data.totalTracks || tracks.length);

  // Chunked append. Folder + track tiles render into separate
  // grids so a "Load more folders" click can append into
  // .folders-grid without disturbing the tracks-grid (or its
  // independent cursor).
  await inspectorAppendTiles(folders, tracks, /*replace=*/true);
}

// inspectorAppendRows is the shared chunked-render helper used by
// the initial render path AND the Load-more append path. Builds row
// elements into DocumentFragments and appends them in 200-row rAF
// chunks so the main thread yields between chunks. After all rows
// land, appends/refreshes the Load-more sentinel based on the
// current cursor state.
//
// Returns a Promise that resolves AFTER all chunks have been
// appended (or after the pump bails on a stale generation). Callers
// like `inspectorLoadMore` await this so the re-entrancy guard
// doesn't unlock mid-render — pre-fix the guard was released the
// moment this function returned (synchronously, before any rAF
// fired), letting a fast IntersectionObserver tick spawn an
// overlapping Load-more whose rows interleaved with the chunks
// still rendering from the prior batch. Gemini HIGH on PR C.
// inspectorAppendTiles renders folder + track tiles into the two
// dedicated sub-grids (.folders-grid + .tracks-grid). Replaces the
// legacy single-tbody chunker. Folders and tracks paginate
// independently — keeping them in separate DOM containers means a
// "Load more folders" click appends to .folders-grid without
// disturbing the track grid (which has its own independent cursor
// + Load-more button), eliminating the layout-jump regression
// flagged in the senior review.
//
// Same chunked-render machinery as the legacy table path:
// 200-tile rAF chunks with `renderGeneration` guard so an in-flight
// pump from a prior navigation bails as soon as a newer render
// bumps the generation.
function inspectorAppendTiles(folders, tracks, replace) {
  const foldersGrid = document.getElementById("folders-grid");
  const tracksGrid = document.getElementById("tracks-grid");
  if (!foldersGrid || !tracksGrid) return Promise.resolve();
  if (replace) {
    foldersGrid.innerHTML = "";
    tracksGrid.innerHTML = "";
    inspectorState.renderedFolders = 0;
    inspectorState.renderedTracks = 0;
    inspectorState.renderGeneration++;
  }

  const items = [];
  for (const f of folders) items.push({ kind: "folder", data: f });
  for (const t of tracks) items.push({ kind: "track", data: t });

  if (items.length === 0) {
    inspectorRefreshLoadMoreSentinel();
    return Promise.resolve();
  }

  return new Promise((resolve) => {
    const CHUNK = 200;
    let i = 0;
    const myGen = inspectorState.renderGeneration;
    function pump() {
      if (inspectorState.renderGeneration !== myGen) {
        resolve();
        return;
      }
      const folderFrag = document.createDocumentFragment();
      const trackFrag = document.createDocumentFragment();
      const end = Math.min(i + CHUNK, items.length);
      for (; i < end; i++) {
        const it = items[i];
        if (it.kind === "folder") {
          folderFrag.appendChild(buildFolderTile(it.data));
          inspectorState.renderedFolders++;
        } else {
          trackFrag.appendChild(buildTrackTile(it.data));
          inspectorState.renderedTracks++;
        }
      }
      if (folderFrag.childNodes.length) foldersGrid.appendChild(folderFrag);
      if (trackFrag.childNodes.length) tracksGrid.appendChild(trackFrag);
      if (i < items.length) {
        requestAnimationFrame(pump);
      } else {
        inspectorRefreshLoadMoreSentinel();
        // Load-more pages decorate from the memoized refs payload
        // (the response covers ALL children — no refetch needed).
        inspectorDecorateTiles();
        resolve();
      }
    }
    pump();
  });
}

// Keep the legacy entry point name as a shim so callers that grew
// up against `inspectorAppendRows` (notably inspectorLoadMore)
// don't break. Translates the (body, folders, tracks, replace)
// shape to the new (folders, tracks, replace) one.
function inspectorAppendRows(_body, folders, tracks, replace) {
  return inspectorAppendTiles(folders, tracks, replace);
}

// ===== Atlas metadata layer: tile artwork + booklet chips =====

// inspectorFetchTileMeta pulls the current folder's children refs
// (GET /api/library/enrichment) and decorates the rendered tiles.
// Fire-and-forget from inspectorNavigate AFTER the awaited render —
// it must never block the render/scroll-restore chain. One response
// covers ALL children (unpaginated server-side grouping), memoized in
// inspectorState.lastTileMeta so load-more pages decorate without a
// refetch. Race-guarded on inspectorState.path like the browse fetch.
// The guard covers mode as well as path: entering search does NOT
// change inspectorState.path, and search results render folder tiles
// into the same #folders-grid that inspectorDecorateTiles scans and
// matches by BASENAME — so a slow refs walk landing after the user
// searched would decorate a same-named hit from a different parent.
async function inspectorFetchTileMeta(path) {
  if (inspectorState.camelot) return; // key-filter view has no folder tiles
  const stale = () =>
    inspectorState.path !== path
    || inspectorState.camelot
    || inspectorState.mode !== "browse";
  try {
    const res = await fetch(`/api/library/enrichment?path=${encodeURIComponent(path)}`);
    if (stale()) return;
    if (!res.ok) return; // decoration is best-effort — tiles stay icon-only
    const data = await res.json();
    if (stale()) return;
    inspectorState.lastTileMeta = data;
    inspectorDecorateTiles();
  } catch {
    // Best-effort: a failed refs fetch leaves plain tiles.
  }
}

// inspectorDecorateTiles patches the rendered folder tiles from the
// memoized refs payload: cover / artist-portrait thumbnails (lazy
// <img> against the loopback byte routes) + a booklet indicator chip.
// Idempotent per tile via data-decorated.
function inspectorDecorateTiles() {
  const meta = inspectorState.lastTileMeta;
  if (!meta || !meta.children) return;
  const grid = document.getElementById("folders-grid");
  if (!grid) return;
  for (const tile of grid.querySelectorAll('.inspector-tile[data-kind="folder"]:not([data-decorated])')) {
    tile.dataset.decorated = "true";
    const tilePath = tile.dataset.path || "";
    const name = tilePath.slice(tilePath.lastIndexOf("/") + 1);
    const ref = meta.children[name];
    if (!ref) continue;

    const header = tile.querySelector(".tile-header");
    const icon = tile.querySelector(".tile-icon");
    if (header && (ref.artworkMBID || ref.artistMBID)) {
      const img = document.createElement("img");
      img.className = "tile-thumb";
      img.loading = "lazy";
      img.decoding = "async";
      img.alt = "";
      const coverURL = ref.artworkMBID
        ? `/api/library/artwork/${encodeURIComponent(ref.artworkMBID)}?size=500${ref.artworkVersion ? `&v=${encodeURIComponent(ref.artworkVersion)}` : ""}`
        : "";
      // Artist folders prefer the portrait, falling back to the
      // representative cover, then to no image (emoji icon stays).
      const sources = [];
      if (ref.kind === "artist" && ref.artistMBID) {
        sources.push(`/api/library/artist-image/${encodeURIComponent(ref.artistMBID)}`);
      }
      if (coverURL) sources.push(coverURL);
      if (sources.length > 0) {
        let attempt = 0;
        img.addEventListener("error", () => {
          attempt++;
          if (attempt < sources.length) {
            img.src = sources[attempt];
          } else {
            img.remove();
            delete tile.dataset.thumb;
          }
        });
        img.addEventListener("load", () => {
          img.dataset.loaded = "true";
          tile.dataset.thumb = "true";
        });
        img.src = sources[0];
        header.insertBefore(img, icon || header.firstChild);
      }
    }
    if (ref.hasBooklet && header) {
      const chip = document.createElement("span");
      chip.className = "tile-booklet";
      chip.textContent = "📖";
      chip.title = "PDF booklet available — open the ⓘ panel to view";
      chip.setAttribute("aria-label", "PDF booklet available");
      const nameEl = tile.querySelector(".tile-name");
      if (nameEl) nameEl.after(chip);
      else header.appendChild(chip);
    }
  }
}

// ===== Atlas metadata layer: About panel card =====

// safeExternalHref admits only parseable http(s) URLs — attribution
// links come from third-party metadata and must never become
// javascript:/data: vectors.
function safeExternalHref(raw) {
  try {
    const u = new URL(raw);
    if (u.protocol === "http:" || u.protocol === "https:") return u.href;
  } catch {
    /* fallthrough */
  }
  return null;
}

// aboutEl is a tiny createElement helper: tag, className, textContent.
// EVERYTHING in the About card renders through textContent — bios,
// descriptions, labels, and genres are remote third-party strings and
// must never touch innerHTML (Camelot-wheel SVG precedent).
function aboutEl(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text) el.textContent = text;
  return el;
}

// appendAttribution renders the mandatory "Read more on <source>"
// line (CC-BY-SA / ToS compliance — required whenever bio/description
// text renders; PROTOCOL.md attribution contract).
function appendAttribution(parent, source, sourceUrl) {
  if (!source) return;
  const p = aboutEl("p", "about-attribution");
  const href = sourceUrl ? safeExternalHref(sourceUrl) : null;
  if (href) {
    const a = document.createElement("a");
    a.href = href;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    a.textContent = `Read more on ${source}`;
    p.appendChild(a);
  } else {
    p.textContent = `Source: ${source}`;
  }
  parent.appendChild(p);
}

// appendClampedText renders a long text block with a 4-line CSS clamp
// and a "Show more" expander (swaps in the full text when longer).
function appendClampedText(parent, summaryText, fullText) {
  const display = summaryText || fullText;
  if (!display) return;
  const p = aboutEl("p", "about-clamp", display);
  p.dataset.expanded = "false";
  parent.appendChild(p);
  const hasMore = fullText && fullText.length > display.length;
  if (hasMore || display.length > 320) {
    const btn = aboutEl("button", "btn about-expand", "Show more");
    btn.type = "button";
    btn.addEventListener("click", () => {
      const expanded = p.dataset.expanded === "true";
      if (expanded) {
        p.textContent = display;
        p.dataset.expanded = "false";
        btn.textContent = "Show more";
      } else {
        p.textContent = fullText || display;
        p.dataset.expanded = "true";
        btn.textContent = "Show less";
      }
    });
    parent.appendChild(btn);
  }
}

// inspectorFetchPanelAbout loads the About card's detail for the open
// single-mode folder. Race-guarded like the projection fetch.
async function inspectorFetchPanelAbout(path) {
  const card = document.getElementById("panel-card-about");
  if (!card || !inspectorState.atlasEnabled) return;
  const content = document.getElementById("panel-about-content");
  const hint = document.getElementById("panel-hint-about");
  const retryBtn = document.getElementById("panel-about-retry");
  const statusEl = document.getElementById("panel-about-status");
  if (statusEl) statusEl.textContent = "";
  if (content) {
    content.textContent = "";
    content.appendChild(aboutEl("p", "hint", "Loading metadata…"));
  }
  const stillCurrent = () =>
    inspectorState.panelMode === "single"
    && inspectorState.selection?.kind === "folder"
    && inspectorState.selection.row.path === path;
  try {
    const res = await fetch(`/api/library/enrichment/detail?path=${encodeURIComponent(path)}`);
    if (!stillCurrent()) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (!stillCurrent()) return;
    renderAboutContent(content, hint, retryBtn, data);
  } catch (err) {
    if (!stillCurrent()) return;
    // Release the loaded-path marker so collapsing and re-expanding
    // retries. Committing it before the fetch is what dedupes a rapid
    // collapse/expand, but leaving it set on failure makes the error
    // card terminal for the panel's lifetime. Guarded by stillCurrent()
    // so a stale fetch can't clear the NEW selection's marker.
    delete card.dataset.loadedPath;
    if (!content) return;
    content.textContent = "";
    content.appendChild(aboutEl("p", "hint", `Couldn't load metadata: ${err.message}`));
  }
}

// renderAboutContent paints the About card from the detail payload.
function renderAboutContent(content, hint, retryBtn, data) {
  if (!content) return;
  content.textContent = "";
  let anyMissing = false;
  const presence = [];

  // Thumbnail row (cover + artist portrait when cached).
  const thumbs = aboutEl("div", "about-thumbs");
  if (data.coverMBID) {
    const img = document.createElement("img");
    img.className = "about-cover";
    img.loading = "lazy";
    img.alt = "Album cover";
    img.src = `/api/library/artwork/${encodeURIComponent(data.coverMBID)}?size=500${data.coverVersion ? `&v=${encodeURIComponent(data.coverVersion)}` : ""}`;
    img.addEventListener("error", () => img.remove());
    thumbs.appendChild(img);
  }
  if (data.hasArtistImage && data.artist?.mbid) {
    const img = document.createElement("img");
    img.className = "about-cover";
    img.loading = "lazy";
    img.alt = "Artist portrait";
    img.src = `/api/library/artist-image/${encodeURIComponent(data.artist.mbid)}`;
    img.addEventListener("error", () => img.remove());
    thumbs.appendChild(img);
  }
  if (thumbs.childNodes.length) content.appendChild(thumbs);

  // An empty payload is the state that MOST needs the retry, not least:
  // the enricher may have run and matched nothing, which leaves
  // enriched_at > 0 with empty MBIDs — outside the `WHERE enriched_at
  // = 0` worker queue, so it never revisits them on its own. The
  // folder-scoped retry re-queues exactly those rows. (If enrichment
  // simply hasn't reached them yet, every retry facet no-ops — safe
  // either way, so the copy must cover both readings.)
  const noIdentifiers = !data.artist && !data.release
    && !data.coverMBID && (!data.booklets || data.booklets.length === 0);
  if (noIdentifiers) {
    anyMissing = true;
    content.appendChild(aboutEl("p", "hint",
      "No metadata identifiers for this folder yet — either enrichment hasn't reached these tracks, or it ran and found no match. Retry re-queues them."));
  }

  if (data.artist) {
    const sec = aboutEl("section", "about-block");
    sec.appendChild(aboutEl("h4", "", "Artist"));
    switch (data.artist.state) {
      case "found":
        presence.push("bio");
        appendClampedText(sec, data.artist.bioSummary, data.artist.bio);
        if (data.artist.genres?.length) {
          sec.appendChild(aboutEl("p", "about-genres", data.artist.genres.join(" · ")));
        }
        appendAttribution(sec, data.artist.source, data.artist.sourceUrl);
        break;
      case "missing":
        anyMissing = true;
        sec.appendChild(aboutEl("p", "hint", "Atlas has no bio for this artist."));
        break;
      default:
        anyMissing = true;
        sec.appendChild(aboutEl("p", "hint",
          "Not checked yet — the harvest will fill this in on its next pass."));
    }
    if (!data.hasArtistImage && data.artist.mbid) {
      anyMissing = true;
      sec.appendChild(aboutEl("p", "hint", "No artist image cached."));
    }
    if (data.moreArtists > 0) {
      sec.appendChild(aboutEl("p", "hint",
        `Showing the dominant artist — ${data.moreArtists} more under this folder.`));
    }
    content.appendChild(sec);
  }

  if (data.release) {
    const sec = aboutEl("section", "about-block");
    sec.appendChild(aboutEl("h4", "", "Album"));
    switch (data.release.state) {
      case "found":
        presence.push("description");
        appendClampedText(sec, "", data.release.description);
        if (data.release.recordLabel) {
          sec.appendChild(aboutEl("p", "about-genres", `Label: ${data.release.recordLabel}`));
        }
        if (data.release.genres?.length) {
          sec.appendChild(aboutEl("p", "about-genres", data.release.genres.join(" · ")));
        }
        appendAttribution(sec, data.release.source, data.release.sourceUrl);
        break;
      case "missing":
        anyMissing = true;
        sec.appendChild(aboutEl("p", "hint", "Atlas has no description for this album."));
        break;
      default:
        anyMissing = true;
        sec.appendChild(aboutEl("p", "hint",
          "Not checked yet — the harvest will fill this in on its next pass."));
    }
    if (data.moreReleases > 0) {
      sec.appendChild(aboutEl("p", "hint",
        `Showing the dominant release — ${data.moreReleases} more under this folder.`));
    }
    content.appendChild(sec);
  }

  if (data.bookletsEnabled && data.booklets?.length) {
    presence.push("booklet");
    const sec = aboutEl("section", "about-block");
    sec.appendChild(aboutEl("h4", "", data.booklets.length === 1 ? "Booklet" : "Booklets"));
    for (const b of data.booklets) {
      const row = aboutEl("p", "about-booklet-row");
      if (b.state === "cached") {
        const a = document.createElement("a");
        a.href = `/api/library/booklet/${encodeURIComponent(b.mbid)}`;
        a.target = "_blank";
        a.rel = "noopener";
        a.textContent = "View booklet (PDF)";
        row.appendChild(a);
      } else {
        row.textContent = "Booklet available — download pending. ";
        const btn = aboutEl("button", "btn about-fetch-booklet", "Fetch now");
        btn.type = "button";
        btn.addEventListener("click", async () => {
          btn.disabled = true;
          btn.textContent = "Queued…";
          // The booklet route's 202 path nudges the fetch sweep
          // server-side; no response handling needed here.
          try { await fetch(`/api/library/booklet/${encodeURIComponent(b.mbid)}`); } catch { /* best-effort */ }
        });
        row.appendChild(btn);
      }
      sec.appendChild(row);
    }
    content.appendChild(sec);
  }

  if (hint) hint.textContent = presence.join(" · ");
  if (retryBtn) retryBtn.hidden = !anyMissing;
}

// inspectorAboutRetryClick fires the folder-scoped metadata retry.
// Optimistic in-card status, NO auto-refetch: enrichment runs at the
// MB/CAA/Deezer clients' pacing, so an immediate refetch would lose
// the race and read as failure — the card refreshes naturally on the
// next open.
async function inspectorAboutRetryClick() {
  const btn = document.getElementById("panel-about-retry");
  const statusEl = document.getElementById("panel-about-status");
  const sel = inspectorState.selection;
  if (inspectorState.panelMode !== "single" || sel?.kind !== "folder") return;
  if (btn) btn.disabled = true;
  try {
    const res = await fetch("/api/library/enrichment/retry", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ path: sel.row.path }),
    });
    if (res.status === 429) {
      if (statusEl) statusEl.textContent = "A retry for this folder ran less than a minute ago — give it a moment.";
      if (btn) btn.disabled = false;
      return;
    }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (statusEl) {
      const bits = [];
      if (data.resetTracks > 0) bits.push(`${data.resetTracks} covers re-queued`);
      if (data.artistImageResets > 0) bits.push(`${data.artistImageResets} artist images re-queued`);
      if (data.bookletChecksReset > 0) bits.push(`${data.bookletChecksReset} booklet checks re-armed`);
      if (data.harvestResubmitted) bits.push("bios/descriptions re-check queued (library-wide)");
      statusEl.textContent = bits.length
        ? `Retry queued — ${bits.join(", ")}. Metadata fills in in the background.`
        : "Nothing to retry — everything under this folder is already queued or resolved.";
    }
    // Keep the button disabled for the guard window; the card
    // refreshes on next open anyway.
  } catch (err) {
    if (statusEl) statusEl.textContent = `Retry failed: ${err.message}`;
    if (btn) btn.disabled = false;
  }
}

function buildFolderTile(f) {
  const tile = document.createElement("article");
  tile.className = "inspector-tile";
  tile.dataset.kind = "folder";
  tile.dataset.path = f.path;
  tile.setAttribute("role", "link");
  tile.tabIndex = 0;
  tile.setAttribute("aria-label", `Open folder ${f.name}`);

  // Eligible-denominator coverage: the server's *EligibleCount fields
  // count tracks that have a variant OR could get one — tracks that
  // need nothing (already at target / DSD / unknown) drop out, so a
  // fully-processed mixed folder reads 100% instead of pinning below
  // it forever. `??` (not `||`) is load-bearing: a present 0 is a
  // real denominator ("all set"), only ABSENT fields (degraded
  // server, older payloads) fall back to the all-tracks count.
  // trackCount is a Go int on the wire — a number, never null.
  const upDen = f.upscaleEligibleCount ?? f.trackCount ?? 0;
  const opDen = f.optimizeEligibleCount ?? f.trackCount ?? 0;
  const upVal = Math.min(f.upscaledCount || 0, upDen);
  const opVal = Math.min(f.optimizedCount || 0, opDen);

  const coverageRow = (kind, label, val, den) => {
    const exempt = Math.max(0, (f.trackCount || 0) - den);
    const exemptNote = exempt > 0
      ? `<span class="coverage-exempt" title="Not counted: already at target, DSD, lossy, or unknown format — nothing to generate">· ${exempt} need nothing</span>`
      : "";
    const count = den === 0
      ? `<span class="coverage-count" title="No tracks need ${escapeHTML(label)} variants">—</span>`
      : `<span class="coverage-count">${kind === "upscale" ? (f.upscaledCount || 0) : (f.optimizedCount || 0)} / ${den}</span>`;
    return `
      <div class="coverage-row" data-kind="${kind}"${den === 0 ? ` data-empty="true"` : ""}>
        <span class="coverage-label">${label}</span>
        <progress value="${val}" max="${Math.max(1, den)}"></progress>
        ${count}${exemptNote}
      </div>`;
  };

  // The data-* attrs on the checkbox carry the client-side
  // aggregation inputs the panel's batch summary reads — no fetch
  // needed on selection updates.
  tile.innerHTML = `
    <header class="tile-header">
      <label class="tile-select" title="Select this folder">
        <input type="checkbox"
          data-path="${escapeHTML(f.path)}"
          data-track-count="${f.trackCount || 0}"
          data-upscaled-count="${f.upscaledCount || 0}"
          data-optimized-count="${f.optimizedCount || 0}"
          data-upscale-eligible="${upDen}"
          data-optimize-eligible="${opDen}" />
      </label>
      <span class="tile-icon" aria-hidden="true">📁</span>
      <h3 class="tile-name" title="${escapeHTML(f.name)}">${escapeHTML(f.name)}</h3>
      <button class="tile-info-btn" type="button"
        aria-label="Open action panel for ${escapeHTML(f.name)}">ⓘ</button>
      <button class="tile-menu-btn" type="button"
        popovertarget="menu-${escapeHTML(f.pathHash || "")}"
        aria-label="Quick actions for ${escapeHTML(f.name)}">⋯</button>
    </header>
    <dl class="tile-meta">
      <div><dt>Tracks</dt><dd>${f.trackCount || 0}</dd></div>
      <div><dt>Size</dt><dd>${humanBytes(f.totalSizeBytes || 0)}</dd></div>
    </dl>
    <div class="tile-coverage">
      ${coverageRow("upscale", "Upscaled", upVal, upDen)}
      ${coverageRow("optimize", "CarPlay-optimized", opVal, opDen)}
    </div>
  `;
  attachTileMenu(tile, f);

  // Tile click → navigate INTO the folder. Excludes clicks on the
  // checkbox label (selection toggle), the ⓘ info button (opens
  // the action panel — handled by the delegated listener on
  // #inspector-content), and the kebab menu button.
  tile.addEventListener("click", (e) => {
    if (e.target.closest(".tile-select")) return;
    if (e.target.closest(".tile-info-btn")) return;
    if (e.target.closest(".tile-menu-btn")) return;
    if (e.target.closest(".tile-menu-popover")) return;
    inspectorNavigate(f.path);
  });
  tile.addEventListener("keydown", (e) => {
    if (e.target.closest(".tile-info-btn")) return;
    if (e.target.closest(".tile-menu-btn")) return;
    if (e.target.closest("input[type=checkbox]")) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      inspectorNavigate(f.path);
    }
  });
  return tile;
}

// SKIP_LABELS maps the server's kind-agnostic skipReason codes
// (browseTrackRow.skipReason) to a short tile badge + a fuller tooltip.
// Only these known codes render a badge — an unknown/empty reason is a
// no-op (the track is eligible, or only softly skipped by a per-kind
// projection, which the action panel's aggregate copy covers).
const SKIP_LABELS = {
  dsd_bitstream: { short: "DSD", full: "DSD bitstream — 1-bit, not PCM-resamplable" },
  lossy_source: { short: "Lossy", full: "Lossy source — upscaling adds no fidelity" },
  unknown_format: { short: "Unknown format", full: "Format unknown — the scanner couldn't read the sample rate / bit depth" },
};

function skipBadgeHTML(reason) {
  const m = SKIP_LABELS[reason];
  if (!m) return "";
  // reason is a fixed enum key (looked up above); short/full are constants.
  return `<div class="skip-badge" data-skip="${reason}" title="${escapeHTML(m.full)}">🚫 ${escapeHTML(m.short)}</div>`;
}

function buildTrackTile(t) {
  const tile = document.createElement("article");
  tile.className = "inspector-tile";
  tile.dataset.kind = "track";
  tile.dataset.path = t.path;

  tile.innerHTML = `
    <header class="tile-header">
      <span class="tile-icon" aria-hidden="true">🎵</span>
      <h3 class="tile-name" title="${escapeHTML(t.name)}">${escapeHTML(t.name)}</h3>
      <button class="tile-menu-btn" type="button"
        popovertarget="menu-${escapeHTML(t.pathHash || "")}"
        aria-label="Actions for ${escapeHTML(t.name)}">⋯</button>
    </header>
    ${skipBadgeHTML(t.skipReason)}
    <dl class="tile-meta">
      <div><dt>Quality</dt><dd>${formatTrackQuality(t)}</dd></div>
      <div><dt>Size</dt><dd>${humanBytes(t.sizeBytes || 0)}</dd></div>
    </dl>
    <div class="tile-track-dots" aria-label="Variant status">
      <span class="tile-track-dot" data-kind="upscale"
        data-present="${t.isUpscaled ? "true" : "false"}">Upscaled</span>
      <span class="tile-track-dot" data-kind="optimize"
        data-present="${t.isOptimized ? "true" : "false"}">CarPlay</span>
    </div>
  `;
  attachTileMenu(tile, t);
  return tile;
}

// attachTileMenu clones the popover template, customises menu-item
// disabled states based on the tile's current variant counts
// (delete actions disabled when count==0), and appends to the tile.
// Uses the native HTML `popover` API for outside-click / Escape
// dismissal — no JS wrapper needed.
//
// **SoX-gated generate actions** carry an actual `disabled`
// property (not just the CSS `data-needs-sox` visual gate). The
// CSS rule handles the visual treatment (opacity + cursor +
// pointer-events) but doesn't stop keyboard activation; setting
// `disabled` on the button is what blocks Enter/Space invocation.
// Per CodeRabbit major on PR #276 round 4 — defense in depth for
// accessibility.
function attachTileMenu(tile, item) {
  const tmpl = document.getElementById("tile-menu-template");
  if (!tmpl || !item.pathHash) return;
  const popover = tmpl.content.firstElementChild.cloneNode(true);
  popover.id = "menu-" + item.pathHash;
  const upCount = (item.upscaledCount || (item.isUpscaled ? 1 : 0)) || 0;
  const opCount = (item.optimizedCount || (item.isOptimized ? 1 : 0)) || 0;
  const soxMissing = !inspectorState.soxAvailable;
  // Disable generate actions when SoX is unavailable — keyboard
  // accessibility gate alongside the CSS [data-needs-sox] visual
  // dim. Without this `disabled` property, a Tab+Enter on a
  // SoX-less bridge would still queue a job that the backend
  // immediately rejects.
  const genUp = popover.querySelector('[data-action="upscale"]');
  const genOp = popover.querySelector('[data-action="optimize"]');
  if (genUp) genUp.disabled = soxMissing;
  if (genOp) genOp.disabled = soxMissing;
  // Disable delete actions when no variants of that kind exist
  // (operator can't "delete" what isn't there).
  const delUp = popover.querySelector('[data-action="delete-upscale"]');
  const delOp = popover.querySelector('[data-action="delete-optimize"]');
  if (delUp) delUp.disabled = upCount === 0;
  if (delOp) delOp.disabled = opCount === 0;
  // Wire click handler — routes via data-action.
  popover.addEventListener("click", (e) => {
    const btn = e.target.closest(".menu-item");
    if (!btn || btn.disabled) return;
    const action = btn.dataset.action;
    handleTileMenuAction(action, item);
    // Close the popover natively.
    if (typeof popover.hidePopover === "function") popover.hidePopover();
  });
  // Anchor the menu to its ⋯ button on each open. A `popovertarget` popover has
  // no native anchored positioning, so without this it lands at the viewport
  // top-left (the UA centering is deliberately the CSS fallback, not the primary
  // path). Done in JS — not CSS Anchor Positioning — so it works in Safari /
  // Firefox too. `beforetoggle` fires before paint, so there's no flash.
  popover.addEventListener("beforetoggle", (e) => {
    if (e.newState !== "open") return;
    const btn = tile.querySelector(".tile-menu-btn");
    if (!btn) return;
    const r = btn.getBoundingClientRect();
    const menuW = 240; // matches min-width
    const estH = 240; // ~5 items + dividers; only used to pick below-vs-above
    // Right-align the menu to the button, clamped inside the viewport.
    let left = Math.min(r.right, window.innerWidth - 8) - menuW;
    left = Math.max(8, left);
    popover.style.position = "fixed";
    popover.style.inset = "auto"; // clear the UA popover `inset: 0`
    popover.style.margin = "0";
    popover.style.left = left + "px";
    // Open downward when there's room, else flip up — choosing whichever side
    // actually has more room on a short viewport. Anchor the button-facing edge
    // (top when below, bottom when above) so the menu stays snug regardless of
    // its real height, and cap max-height to the available space so items scroll
    // (CSS overflow-y) instead of clipping off-screen on landscape / zoomed
    // viewports (Gemini on #503).
    const gap = 4;
    const vpMargin = 8;
    const spaceBelow = window.innerHeight - r.bottom;
    const spaceAbove = r.top;
    if (spaceBelow >= estH || spaceBelow >= spaceAbove) {
      popover.style.top = r.bottom + gap + "px";
      popover.style.bottom = "auto";
      popover.style.maxHeight = Math.max(96, spaceBelow - gap - vpMargin) + "px";
    } else {
      popover.style.bottom = window.innerHeight - r.top + gap + "px";
      popover.style.top = "auto";
      popover.style.maxHeight = Math.max(96, spaceAbove - gap - vpMargin) + "px";
    }
  });
  tile.appendChild(popover);
}

// handleTileMenuAction routes a per-tile kebab-menu action to the
// right submit/delete/projection handler. `item` is the folder or
// track row data (carries .path, .upscaledCount etc). Defense-in-
// depth SoX gate at the action sites — the button's `disabled`
// property already blocks user activation, but a future JS-direct
// caller (test, console-driven automation) bypassing the click
// handler would otherwise queue jobs the backend rejects.
function handleTileMenuAction(action, item) {
  const path = item.path;
  const soxMissing = !inspectorState.soxAvailable;
  switch (action) {
    case "upscale":
      if (soxMissing) break;
      inspectorSubmitBatchForKind("upscale", [path]);
      break;
    case "optimize":
      if (soxMissing) break;
      inspectorSubmitBatchForKind("optimize", [path]);
      break;
    case "delete-upscale":
      inspectorDeleteVariantsForKind("upscale", path);
      break;
    case "delete-optimize":
      inspectorDeleteVariantsForKind("optimize", path);
      break;
    case "projection":
      // Folder tiles open the panel for themselves; track tiles fall
      // back to the current-folder panel (no per-track rollup fields
      // on the row data).
      if (item.upscaledCount !== undefined) {
        inspectorOpenPanelSingle(item);
      } else {
        inspectorOpenProjectionForCurrent();
      }
      break;
  }
}

// inspectorRefreshLoadMoreSentinel rebuilds the "Load more (N
// remaining)" row at the bottom of the tbody based on the current
// nextCursor state. Called after every page render. Removes any
// pre-existing sentinel before checking — handles the "all done"
// case where Load-more's response had empty next cursors.
function inspectorRefreshLoadMoreSentinel() {
  // Two independent sentinels — folders + tracks paginate via
  // separate cursors. Show/hide each Load-more button based on
  // its collection's remaining count.
  const foldersBtn = document.getElementById("folders-load-more");
  const tracksBtn = document.getElementById("tracks-load-more");
  if (!foldersBtn || !tracksBtn) return;

  const foldersRemaining = Math.max(0,
    inspectorState.totalFolders - inspectorState.renderedFolders);
  const tracksRemaining = Math.max(0,
    inspectorState.totalTracks - inspectorState.renderedTracks);

  const hasFolderMore = !!inspectorState.nextFolderCursor && foldersRemaining > 0;
  const hasTrackMore = !!inspectorState.nextTrackCursor && tracksRemaining > 0;

  foldersBtn.hidden = !hasFolderMore;
  tracksBtn.hidden = !hasTrackMore;
  if (hasFolderMore) {
    foldersBtn.textContent = `Load more folders (${foldersRemaining} remaining)`;
  }
  if (hasTrackMore) {
    tracksBtn.textContent = `Load more tracks (${tracksRemaining} remaining)`;
  }

  // IntersectionObserver auto-fires Load-more when either sentinel
  // scrolls into view. One observer, two observed buttons; the
  // click handlers wired in initLibraryInspector dispatch to the
  // same inspectorLoadMore (which reads both cursors).
  if (!inspectorLoadMoreObserver) {
    inspectorLoadMoreObserver = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting && !e.target.hidden) inspectorLoadMore();
      }
    }, { root: null, rootMargin: "200px" });
    inspectorLoadMoreObserver.observe(foldersBtn);
    inspectorLoadMoreObserver.observe(tracksBtn);
  }
}

// inspectorLoadMore fetches the next page from the current cursors
// and appends results. Re-entrant calls (rapid scroll past sentinel
// before the prior fetch lands) are suppressed via `loadingMore` —
// the guard is held until BOTH the fetch lands AND the chunked
// rAF render completes (await inspectorAppendRows). Pre-fix the
// guard was released synchronously when inspectorAppendRows
// returned, letting an IntersectionObserver tick spawn an
// overlapping page whose rows interleaved with the still-rendering
// chunks. Gemini HIGH on PR C.
//
// Search-mode guard: a fetch that landed AFTER the user typed a
// global search query would otherwise overwrite the flat-list view
// with browse rows. Both the path AND mode are re-checked after
// each await.
async function inspectorLoadMore() {
  if (inspectorState.loadingMore) return;
  if (!inspectorState.nextFolderCursor && !inspectorState.nextTrackCursor) return;
  if (inspectorState.mode === "search") return;
  inspectorState.loadingMore = true;
  const path = inspectorState.path;
  // Capture camelot too — all key views share path "", so the race guards
  // below need it to drop a stale load-more from a previously-active key
  // (which would otherwise clobber the new key's pagination cursors).
  const camelot = inspectorState.camelot;
  try {
    const params = new URLSearchParams();
    // Harmonic-key filter view is a flat track list (no folders); the
    // folder cursor only applies to a normal path browse.
    if (inspectorState.camelot) {
      params.set("camelot", inspectorState.camelot);
    } else {
      params.set("path", path);
      // Include each cursor param IF the collection isn't exhausted;
      // OMIT the param to signal exhausted to the server.
      if (inspectorState.nextFolderCursor) {
        params.set("afterFolder", inspectorState.nextFolderCursor);
      }
    }
    if (inspectorState.nextTrackCursor) {
      params.set("afterTrack", inspectorState.nextTrackCursor);
    }
    const res = await fetch(`/api/library/browse?${params.toString()}`);
    if (inspectorState.path !== path || inspectorState.camelot !== camelot || inspectorState.mode === "search") return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (inspectorState.path !== path || inspectorState.camelot !== camelot || inspectorState.mode === "search") return;
    // Advance cursors based on the new page response.
    inspectorState.nextFolderCursor = data.nextFolderCursor || "";
    inspectorState.nextTrackCursor = data.nextTrackCursor || "";
    // Merge new totals — server may have refreshed counts.
    // Nullish-coalesce (??) instead of OR (||) so a server-reported
    // 0 (e.g. folder emptied between pages) updates state to 0
    // rather than retaining the stale non-zero count. Gemini medium
    // on PR C.
    inspectorState.totalFolders = data.totalFolders ?? inspectorState.totalFolders;
    inspectorState.totalTracks = data.totalTracks ?? inspectorState.totalTracks;
    // Update per-section count headers so they reflect the freshly
    // landed totals (folders / tracks paginate independently).
    const foldersCountEl = document.getElementById("folders-count");
    const tracksCountEl = document.getElementById("tracks-count");
    if (foldersCountEl) foldersCountEl.textContent = String(inspectorState.totalFolders);
    if (tracksCountEl) tracksCountEl.textContent = String(inspectorState.totalTracks);
    // Reveal each section whose collection just gained rows.
    if ((data.folders || []).length > 0) {
      document.getElementById("folders-section").hidden = false;
    }
    if ((data.tracks || []).length > 0) {
      document.getElementById("tracks-section").hidden = false;
    }
    // Await the chunked render — the loadingMore guard must stay
    // held until every rAF chunk has appended, otherwise a fast
    // IntersectionObserver tick spawns an overlapping page.
    await inspectorAppendTiles(data.folders || [], data.tracks || [], /*replace=*/false);
  } catch (err) {
    // Surface a non-blocking failure on whichever sentinel is
    // still showing. Both buttons fall back to a generic retry
    // label; next successful page restores the standard text.
    const foldersBtn = document.getElementById("folders-load-more");
    const tracksBtn = document.getElementById("tracks-load-more");
    if (foldersBtn && !foldersBtn.hidden) {
      foldersBtn.textContent = `Load failed: ${err.message} — retry`;
    }
    if (tracksBtn && !tracksBtn.hidden) {
      tracksBtn.textContent = `Load failed: ${err.message} — retry`;
    }
  } finally {
    inspectorState.loadingMore = false;
  }
}

// =============================================================
// Floating action panel (replaces the persistent right drawer +
// sticky bottom selection bar). Two operating modes:
//   - single: anchored to a tile / heading ⓘ; coverage + stats
//     reflect that single folder. Projection fetch fills the
//     bytes/required rows.
//   - batch:  bottom-center fixed; coverage rollups come from the
//     selectedPaths Map (client-side). Generate transitions to a
//     batch-confirm overlay with throttled per-path projection.
// =============================================================

function inspectorOpenPanelSingle(folder) {
  const panel = document.getElementById("inspector-action-panel");
  if (!panel) return;
  // Clicking ⓘ on a tile is a focused action that supersedes any
  // pending multi-select. Pre-fix, leaving `selectedPaths` populated
  // meant closing the single panel hid every visible affordance
  // (panel gone, no selection bar) — the operator's checkboxes were
  // still ticked but there was no UI to act on them (CodeRabbit
  // major on PR #279). Clearing here is the simplest fix and
  // matches the operator's mental model: "open the action panel
  // for THIS tile" is single-tile-scoped.
  if (inspectorState.selectedPaths.size > 0) {
    for (const cb of document.querySelectorAll(
      '.inspector-tile input[type="checkbox"][data-path]:checked')) {
      cb.checked = false;
      const tile = cb.closest(".inspector-tile");
      if (tile) delete tile.dataset.selected;
    }
    inspectorState.selectedPaths.clear();
  }
  inspectorState.selection = { kind: "folder", row: folder };
  inspectorState.panelMode = "single";

  panel.dataset.mode = "single";
  delete panel.dataset.confirming;
  const overlay = document.getElementById("panel-confirm-overlay");
  if (overlay) overlay.hidden = true;

  const titleEl = document.getElementById("panel-title");
  if (titleEl) titleEl.textContent = folder.name || "Library root";
  const clearBtn = document.getElementById("panel-clear-selection");
  if (clearBtn) clearBtn.hidden = true;

  setPanelKindInitial("upscale", folder, folder.upscaledCount || 0);
  setPanelKindInitial("optimize", folder, folder.optimizedCount || 0);

  // About card is single-mode only (batch hides it below).
  const aboutCard = document.getElementById("panel-card-about");
  if (aboutCard) aboutCard.hidden = false;

  // Restore the operator's last-expanded card; default to upscale.
  inspectorSetExpandedCard(inspectorState.panelExpandedKind || "upscale");

  if (typeof panel.showPopover === "function"
    && !panel.matches(":popover-open")) {
    panel.showPopover();
  }
  inspectorA11yListeners("single");

  // Async projection fetch fills in projected / free / required /
  // target and decides Generate button enablement.
  inspectorFetchPanelProjectionAllKinds(folder.path);
  // About card detail is LAZY — fetched when the card expands (see
  // inspectorOnCardTriggerActivate), or right away if About was the
  // remembered expanded card. The root folder's detail is a full-
  // table walk; don't pay it when the operator wanted the upscale
  // numbers. Mark the card stale so re-opens on a new path refetch.
  if (aboutCard) delete aboutCard.dataset.loadedPath;
  if (inspectorState.panelExpandedKind === "about") {
    inspectorMaybeFetchAbout();
  }
}

// inspectorMaybeFetchAbout fetches the About detail for the open
// single-mode folder unless the card already holds that path's data.
function inspectorMaybeFetchAbout() {
  const card = document.getElementById("panel-card-about");
  const sel = inspectorState.selection;
  if (!card || inspectorState.panelMode !== "single" || sel?.kind !== "folder") return;
  if (card.dataset.loadedPath === sel.row.path) return;
  card.dataset.loadedPath = sel.row.path;
  inspectorFetchPanelAbout(sel.row.path);
}

// setPanelKindInitial pre-fills a card with the rollup snapshot
// (instant from data-*) before the projection fetch lands. The
// per-kind detail rows + coverage bar both get a first paint here.
function setPanelKindInitial(kind, folder, coveredCount) {
  const card = document.getElementById(`panel-card-${kind}`);
  if (!card) return;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  const trackCount = folder.trackCount || 0;
  // Eligible denominator (covered + currently-eligible); `??` keeps a
  // genuine 0 ("nothing needs this kind") distinct from an absent
  // field (degraded server → fall back to all tracks).
  const den = (kind === "upscale"
    ? folder.upscaleEligibleCount
    : folder.optimizeEligibleCount) ?? trackCount;
  const needNothing = Math.max(0, trackCount - den);
  const remaining = Math.max(0, den - coveredCount);

  // Coverage bar (custom progressbar div + ARIA semantics).
  updateCoverageBar(kind, coveredCount, den, lbl);

  const ratioEl = document.getElementById(`panel-ratio-${kind}`);
  if (ratioEl) ratioEl.textContent = den === 0 ? "—" : `${coveredCount} / ${den}`;
  const hintEl = document.getElementById(`panel-hint-${kind}`);
  if (hintEl) {
    if (trackCount === 0) hintEl.textContent = "";
    else if (den === 0) hintEl.textContent = "nothing to do";
    else if (remaining === 0) hintEl.textContent = "All covered";
    else hintEl.textContent = needNothing > 0
      ? `${remaining} left · ${needNothing} need nothing`
      : `${remaining} left`;
  }

  setPanelDetailText(card, kind, ".panel-tracks",
    `${trackCount} (${coveredCount} already ${lbl}${needNothing > 0 ? `, ${needNothing} need nothing` : ""})`);
  setPanelDetailText(card, kind, ".panel-covered", String(coveredCount));
  setPanelDetailText(card, kind, ".panel-source-size",
    humanBytes(folder.totalSizeBytes || 0));
  setPanelDetailText(card, kind, ".panel-projected", "—");
  setPanelDetailText(card, kind, ".panel-free", "—");
  setPanelDetailText(card, kind, ".panel-required", "—");
  if (kind === "upscale") {
    setPanelDetailText(card, kind, ".panel-target", "—");
  }

  hidePanelEl(card, kind, ".panel-warning");
  hidePanelEl(card, kind, ".panel-unknown");
  hidePanelEl(card, kind, ".panel-attarget");
  setPanelDetailText(card, kind, ".panel-submit-status", "");

  // Buttons start disabled; the projection response decides Generate
  // enablement. Delete enabled iff there's coverage to delete.
  const genBtn = card.querySelector(`.panel-generate-btn[data-kind="${kind}"]`);
  if (genBtn) {
    genBtn.disabled = true;
    genBtn.hidden = false;
  }
  const delBtn = card.querySelector(`.panel-delete-btn[data-kind="${kind}"]`);
  if (delBtn) {
    delBtn.hidden = coveredCount === 0;
    delBtn.disabled = coveredCount === 0;
  }
}

function setPanelDetailText(card, kind, selector, text) {
  const el = card.querySelector(`${selector}[data-kind="${kind}"]`);
  if (el) el.textContent = text;
}
function hidePanelEl(card, kind, selector) {
  const el = card.querySelector(`${selector}[data-kind="${kind}"]`);
  if (el) el.hidden = true;
}

// updateCoverageBar — keeps the role="progressbar" ARIA attrs +
// CSS custom property in sync. Default `safeMax = max(1, total)`
// because aria-valuemax must not be 0 (would compute NaN%).
function updateCoverageBar(kind, value, total, lbl) {
  const bar = document.getElementById(`panel-coverage-bar-${kind}`);
  if (!bar) return;
  const safeMax = Math.max(1, total);
  const v = Math.min(Math.max(0, value), safeMax);
  bar.setAttribute("aria-valuenow", String(v));
  bar.setAttribute("aria-valuemin", "0");
  bar.setAttribute("aria-valuemax", String(safeMax));
  bar.setAttribute("aria-label",
    `${value} of ${total} tracks ${lbl}`);
  const fill = bar.querySelector(".coverage-bar-fill");
  if (fill) {
    const pct = total > 0 ? (v / safeMax) * 100 : 0;
    fill.style.setProperty("--cov", `${pct}%`);
  }
}

// Card expand / collapse. Click target is the .card-summary-trigger
// inside each panel-card. Only one card is expanded at a time; the
// other folds back to the summary row. `panelExpandedKind` is the
// remembered choice across panel opens.
function inspectorOnCardTriggerActivate(ev) {
  const trigger = ev.currentTarget;
  const card = trigger.closest(".panel-card");
  if (!card) return;
  const kind = card.dataset.kind;
  if (!kind) return;
  inspectorSetExpandedCard(kind);
  // The About card's detail loads lazily on first expand per folder.
  if (kind === "about") inspectorMaybeFetchAbout();
}

function inspectorSetExpandedCard(kind) {
  inspectorState.panelExpandedKind = kind;
  for (const k of ["upscale", "optimize", "about"]) {
    const card = document.getElementById(`panel-card-${k}`);
    if (!card) continue;
    const trigger = card.querySelector(".card-summary-trigger");
    const details = document.getElementById(`panel-details-${k}`);
    const expanded = k === kind;
    card.dataset.expanded = expanded ? "true" : "false";
    if (trigger) trigger.setAttribute("aria-expanded", expanded ? "true" : "false");
    if (details) details.hidden = !expanded;
  }
}

// Fire both projection fetches in parallel for single-tile mode.
async function inspectorFetchPanelProjectionAllKinds(path) {
  await Promise.all([
    inspectorFetchPanelProjection(path, "upscale"),
    inspectorFetchPanelProjection(path, "optimize"),
  ]);
}

async function inspectorFetchPanelProjection(path, kind) {
  // Race-guard: bail if the operator opened a different panel
  // mid-fetch. Mirrors the legacy drawer pattern.
  const requested = path;
  const stillCurrent = () =>
    inspectorState.panelMode === "single"
    && inspectorState.selection?.kind === "folder"
    && inspectorState.selection.row.path === requested;

  const card = document.getElementById(`panel-card-${kind}`);
  if (!card) return;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  const warnEl = card.querySelector(`.panel-warning[data-kind="${kind}"]`);
  const unknownEl = card.querySelector(`.panel-unknown[data-kind="${kind}"]`);
  const atTargetEl = card.querySelector(`.panel-attarget[data-kind="${kind}"]`);
  const projectedEl = card.querySelector(`.panel-projected[data-kind="${kind}"]`);
  const freeEl = card.querySelector(`.panel-free[data-kind="${kind}"]`);
  const requiredEl = card.querySelector(`.panel-required[data-kind="${kind}"]`);
  const targetEl = card.querySelector(`.panel-target[data-kind="${kind}"]`);
  const genBtn = card.querySelector(`.panel-generate-btn[data-kind="${kind}"]`);

  try {
    const res = await fetch(
      `/api/library/browse-projection?path=${encodeURIComponent(path)}&kind=${kind}`);
    if (!stillCurrent()) return;
    if (res.status === 503) {
      if (warnEl) {
        warnEl.hidden = false;
        warnEl.textContent = kind === "optimize"
          ? "CarPlay-optimize feature is disabled on this bridge."
          : "Upscale feature is disabled on this bridge.";
      }
      return;
    }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (!stillCurrent()) return;
    if (projectedEl) {
      projectedEl.textContent =
        `${humanBytes(data.projectedSizeBytes)} (${data.projectedFiles} files)`;
    }
    if (freeEl) freeEl.textContent = humanBytes(data.availableBytes);
    if (requiredEl) requiredEl.textContent = humanBytes(data.requiredBytesWithMargin);
    if (targetEl && kind === "upscale") {
      if (data.targetRate > 0 && data.targetBits > 0) {
        targetEl.textContent = `${data.targetBits}-bit / ${data.targetRate} Hz`;
      } else {
        targetEl.textContent = "—";
      }
    }

    // Genuinely-skipped tracks (DSD / lossy / unknown geometry) keep
    // the warning-adjacent hint; tracks that are simply ALREADY AT
    // the target format get their own neutral line — "already done"
    // must not read as a failure (pre-split, at-floor CD tracks
    // showed up as "skipped" under kind=optimize).
    if (data.unknownFormatFiles > 0 && unknownEl) {
      unknownEl.hidden = false;
      unknownEl.textContent =
        `${data.unknownFormatFiles} tracks here are DSD / lossy / unknown format — they'll be skipped.`;
    }
    if (data.alreadyAtTargetFiles > 0 && atTargetEl) {
      atTargetEl.hidden = false;
      atTargetEl.textContent =
        `${data.alreadyAtTargetFiles} tracks are already at the target format — nothing to do for them.`;
    }

    if (genBtn) {
      const soxMissing = !inspectorState.soxAvailable;
      if (data.projectedFiles === 0) {
        if (warnEl) {
          warnEl.hidden = false;
          const atTarget = data.alreadyAtTargetFiles || 0;
          const total = data.alreadyCoveredFiles + data.unknownFormatFiles + atTarget;
          const parts = [];
          if (data.alreadyCoveredFiles > 0) parts.push(`${data.alreadyCoveredFiles} already ${lbl}`);
          if (atTarget > 0) parts.push(`${atTarget} already at target`);
          if (data.unknownFormatFiles > 0) parts.push(`${data.unknownFormatFiles} not eligible`);
          let msg;
          if (total === 0) {
            msg = "No tracks here.";
          } else if (parts.length === 1 && data.alreadyCoveredFiles > 0) {
            msg = `All eligible tracks already have a ${lbl} variant.`;
          } else {
            msg = `${parts.join(", ")} — nothing to generate.`;
          }
          warnEl.textContent = msg;
        }
        genBtn.disabled = true;
      } else if (data.wouldFit) {
        genBtn.disabled = soxMissing;
      } else {
        genBtn.disabled = true;
        if (warnEl) {
          warnEl.hidden = false;
          warnEl.textContent =
            `Not enough free space: needs ${humanBytes(data.requiredBytesWithMargin)} (incl. 10% safety margin), only ${humanBytes(data.availableBytes)} free where variants are stored.`;
        }
      }
    }
  } catch (err) {
    if (!stillCurrent()) return;
    if (warnEl) {
      warnEl.hidden = false;
      warnEl.textContent = `Couldn't fetch projection: ${err.message}`;
    }
  }
}

// Generate-button click. Single mode → submit directly with the
// folder path. Batch mode → enter the confirmation overlay.
function inspectorPanelGenerateClick(ev) {
  const btn = ev?.currentTarget;
  const kind = btn?.dataset.kind || "upscale";
  if (inspectorState.panelMode === "batch") {
    inspectorPanelConfirmBatch(kind);
    return;
  }
  const sel = inspectorState.selection;
  if (sel?.kind !== "folder") return;
  inspectorSubmitBatchForKind(kind, [sel.row.path]);
}

// Delete-button click. Single mode only (matches today's UX — batch
// delete is deliberately not exposed).
function inspectorPanelDeleteClick(ev) {
  const btn = ev?.currentTarget;
  const kind = btn?.dataset.kind || "upscale";
  const sel = inspectorState.selection;
  if (sel?.kind !== "folder") return;
  inspectorDeleteVariantsForKind(kind, sel.row.path || "");
}

// inspectorEvictSelectedPath removes one folder from the batch selection
// after its submit succeeded: drop it from selectedPaths and uncheck its
// tile (clearing the selected outline). Matching is by data-path value
// (folder names can contain CSS-selector metacharacters, so we iterate
// the checkboxes rather than build an attribute selector). A no-op when
// the path isn't selected — e.g. a single-folder kebab submit on a tile
// the operator never checked.
function inspectorEvictSelectedPath(path) {
  inspectorState.selectedPaths.delete(path);
  for (const cb of document.querySelectorAll(
    '.inspector-tile input[type="checkbox"][data-path]')) {
    if (cb.dataset.path === path) {
      cb.checked = false;
      const tile = cb.closest(".inspector-tile");
      if (tile) delete tile.dataset.selected;
      break;
    }
  }
}

// inspectorSubmitBatchForKind fires N SEQUENTIAL POSTs against
// /api/upscale/batch (one per path) with the same `kind` — serial, not
// Promise.all, to avoid bursting concurrent connections at the SQLite
// single-writer envelope (see the loop comment + Gemini on PR #276).
// Aggregates enqueued / alreadyCovered counts into a single status line
// on the kind's drawer section (when paths.length === 1) or the
// selection bar (when paths.length > 1). The selection-bar callers route
// through inspectorSelectionSubmit, which delegates here.
//
// Each path is evicted from the selection (checkbox unchecked + dropped
// from selectedPaths) the moment its POST acks OK — so on a partial
// failure only the folders that actually failed stay selected, and a
// retry re-submits ONLY those rather than re-walking the whole original
// batch server-side (each re-POST costs a full per-folder SQL projection
// + disk pre-check). Pre-fix, any failure preserved the entire selection.
// inspectorPreflightNoOpReason — pure helper that maps a single
// projection response to a human-readable "nothing to do because…"
// string when `projectedFiles === 0`. Reuses the message matrix
// from inspectorFetchProjection so the pre-flight toast wording
// matches what the drawer would have shown. Returns null when
// there IS work to do (caller proceeds with submit).
function inspectorPreflightNoOpReason(data, kind) {
  if (!data || data.projectedFiles > 0) return null;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  const covered = data.alreadyCoveredFiles || 0;
  // Aggregate every "skipped at projection time" bucket the
  // bridge reports — unknown source format, DSD, lossy. Pre-fix
  // only `unknownFormatFiles` was consulted, so an all-DSD or
  // all-lossy folder produced the wrong "no tracks here" toast.
  // Per CodeRabbit minor on PR #278.
  const unknown = data.unknownFormatFiles || 0;
  const dsd = data.dsdFiles || 0;
  const lossy = data.lossyFiles || 0;
  const ineligible = unknown + dsd + lossy;
  if (covered > 0 && ineligible === 0) {
    return `All eligible tracks already have a ${lbl} variant.`;
  }
  if (covered === 0 && ineligible > 0) {
    return kind === "optimize"
      ? "No tracks here are eligible for CarPlay-optimize (already at target, lossy, DSD, or unknown source format)."
      : "No tracks here support upscaling (DSD or unknown source format).";
  }
  if (covered > 0 && ineligible > 0) {
    return `${covered} tracks already ${lbl}, ${ineligible} not eligible — nothing left to generate.`;
  }
  return "No tracks here.";
}

// inspectorSelectionToast — surface aggregate / preflight messages
// that previously landed on the (now removed) selection bar. Falls
// through to a transient page-level toast so tile-menu submits with
// the panel closed still get visible feedback (preserves the
// CodeRabbit major fix from PR #278). The panel's own confirm-status
// row OR the expanded card's submit-status row is preferred when
// available; otherwise lazy-creates a one-time toast at the bottom
// of the viewport.
//
// API contract:
//   - Plain string  → rendered via textContent (XSS-safe).
//   - { html: "…" } → rendered via innerHTML; ONLY callers that
//                     construct trusted HTML from server-numeric
//                     fields (e.g. enqueued/covered counts +
//                     hardcoded <strong> / <a>) may use this form.
//
// Pre-fix the heuristic `msg.includes("<")` was used to auto-detect
// HTML — that turned a server-supplied error string like
// "HTTP 502: <html>...</html>" into a live XSS surface
// (Gemini high on PR #279).
function inspectorSelectionToast(msg) {
  const trusted = (msg && typeof msg === "object" && typeof msg.html === "string")
    ? msg.html
    : null;
  const plain = trusted == null ? (msg || "") : null;
  const apply = (el) => {
    if (trusted != null) el.innerHTML = trusted;
    else el.textContent = plain;
  };

  const panel = document.getElementById("inspector-action-panel");
  if (panel && panel.matches(":popover-open")) {
    // Batch-confirm overlay open → status row sits there.
    if (panel.dataset.confirming) {
      const cs = panel.querySelector(".panel-confirm-status");
      if (cs) {
        apply(cs);
        return;
      }
    }
    // Otherwise drop it on the currently-expanded card's submit row.
    const kind = inspectorState.panelExpandedKind || "upscale";
    const status = panel.querySelector(
      `.panel-submit-status[data-kind="${kind}"]`);
    if (status) {
      apply(status);
      return;
    }
  }
  // Fallback: lazy floating toast so tile-menu submits with the
  // panel closed still get visible feedback.
  let toast = document.getElementById("inspector-floating-toast");
  if (!toast) {
    toast = document.createElement("div");
    toast.id = "inspector-floating-toast";
    toast.className = "inspector-floating-toast hint";
    toast.setAttribute("role", "status");
    toast.setAttribute("aria-live", "polite");
    document.body.appendChild(toast);
  }
  if (trusted == null && !plain) {
    toast.hidden = true;
    return;
  }
  toast.hidden = false;
  apply(toast);
  // Auto-dismiss after 6 s — long enough to read "tracks queued"
  // even on a slow read, short enough to disappear when the operator
  // moves on.
  if (toast._dismiss) clearTimeout(toast._dismiss);
  toast._dismiss = setTimeout(() => { toast.hidden = true; }, 6000);
}

async function inspectorSubmitBatchForKind(kind, paths) {
  if (!Array.isArray(paths) || paths.length === 0) return;
  if (kind !== "upscale" && kind !== "optimize") return;

  const single = paths.length === 1;
  const status = single
    ? document.querySelector(`.panel-submit-status[data-kind="${kind}"]`)
    : null;
  if (status) status.textContent = "Submitting…";

  // Pre-flight projection on single-path submits (the kebab-menu and
  // drawer "Generate" both hit this branch). Avoids the silent no-op
  // batch that lands on the Jobs page as "completed 0/0" when the
  // folder has zero eligible tracks — instead the operator gets an
  // honest "nothing to do because <X>" toast BEFORE the POST. Skips
  // pre-flight on multi-path (selection-bar) submits because each
  // path would cost a separate projection GET and the aggregated
  // sub-toast is less useful than the per-batch Jobs row.
  // Per the inspector skip-reason feedback to-do (PR feat/inspector-skip-feedback).
  if (single) {
    try {
      const probeURL = `/api/library/browse-projection?path=${encodeURIComponent(paths[0])}&kind=${kind}`;
      const probe = await fetch(probeURL);
      if (probe.ok) {
        const data = await probe.json();
        const reason = inspectorPreflightNoOpReason(data, kind);
        if (reason) {
          const msg = `Nothing to do — ${reason}`;
          if (status) status.textContent = msg;
          // Selection-bar toast for the tile-menu path (drawer
          // closed, drawer-status invisible). CodeRabbit major
          // on PR #278.
          inspectorSelectionToast(msg);
          return { ok: 0, failed: 0, enqueued: 0, skippedDueToPreflight: true };
        }
      }
      // Non-200 / non-JSON falls through to the regular POST path —
      // we don't block submits on a flaky projection probe.
    } catch (e) {
      // Don't block on probe failure — the POST will run, and its
      // result message will be the operator's feedback. Breadcrumb
      // for diagnosis (SonarCloud + CodeRabbit minor on PR #278).
      console.warn("preflight probe failed, falling through to submit:", e);
    }
  }
  // Capture the path the operator is currently viewing so the
  // post-success refresh (below) doesn't race a mid-flight
  // navigation to a different folder — Gemini medium on PR #276.
  const originPath = inspectorState.path;

  // Sequential submit (not Promise.all). Multi-select against
  // many folders can otherwise burst N concurrent connections at
  // the bridge — over browser per-host limits AND the SQLite
  // single-writer envelope. The serial loop keeps the admin
  // server responsive under a 20+ folder Optimize push. Per
  // Gemini medium on PR #276.
  const results = [];
  for (const path of paths) {
    try {
      const res = await fetch("/api/upscale/batch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path, kind }),
      });
      if (res.status === 507) {
        const data = await res.json();
        results.push({ path, error: `needs ${humanBytes(data.requiredBytes)}, only ${humanBytes(data.availableBytes)} available` });
        continue;
      }
      if (res.status === 503) {
        results.push({ path, error: `${kind} feature disabled on this bridge` });
        continue;
      }
      if (!res.ok) {
        results.push({ path, error: `HTTP ${res.status}` });
        continue;
      }
      const data = await res.json();
      results.push({ path, ok: true, data });
      // Evict the moment this folder acks OK so a partial-failure retry
      // re-submits only the still-failed folders (see function doc).
      inspectorEvictSelectedPath(path);
    } catch (err) {
      results.push({ path, error: err.message });
    }
  }

  const ok = results.filter(r => r.ok);
  const failed = results.filter(r => r.error);
  const enqueued = ok.reduce((n, r) => n + (r.data?.enqueuedCount || 0), 0);
  const covered = ok.reduce((n, r) => n + (r.data?.alreadyCovered || 0), 0);
  // Captured for the multi-path failure cases so inspectorPanelConfirmSubmit
  // can re-surface it on the redrawn batch summary after it hides the
  // confirm overlay (the toast below lands inside that overlay, so it would
  // otherwise vanish with the count change unexplained). CodeRabbit on #442.
  let failureMsg = "";

  if (single && status) {
    if (failed.length > 0) {
      status.textContent = `Couldn't submit: ${failed[0].error}`;
    } else if (ok.length > 0) {
      status.innerHTML =
        `Batch enrolled · <strong>${enqueued}</strong> tracks queued ` +
        `(${covered} already covered). ` +
        `<a href="/jobs">View jobs →</a>`;
    }
  } else {
    // Multi-path: render a single toast-style message on the panel
    // (or as a floating page-level toast when the panel is closed).
    // The trusted-HTML form is reserved for the success branch where
    // every interpolated value is a numeric count from the server.
    if (failed.length > 0 && ok.length === 0) {
      failureMsg = `Couldn't submit any: ${failed[0].error}`;
      inspectorSelectionToast(failureMsg);
    } else if (failed.length > 0) {
      failureMsg = `${enqueued} tracks queued across ${ok.length} folders · ${failed.length} folders failed`;
      inspectorSelectionToast(failureMsg);
    } else {
      // SAFE: enqueued + ok.length are server-supplied integers,
      // not user-controlled strings; rendered via the html sentinel
      // so the <strong> and <a> elements paint as markup.
      inspectorSelectionToast({
        html: `Batch enrolled · <strong>${enqueued}</strong> tracks queued across ${ok.length} folders. ` +
          `<a href="/jobs">View jobs →</a>`,
      });
    }
  }

  // Refresh the page after a CLEAN-SUCCESS submit so coverage
  // bars update on the tiles the operator is looking at. Gated
  // on `failed.length === 0` because `inspectorRender` clears
  // `selectedPaths` on every navigate — refreshing after a
  // partial failure would clobber the preserve-on-failure
  // behaviour that `inspectorSelectionSubmit` depends on for
  // the operator's retry workflow. The `originPath ===
  // inspectorState.path` check ensures we don't re-navigate if
  // the user moved to a different folder mid-submit. Per
  // CodeRabbit major on PR #276 round 4.
  if (ok.length > 0 && failed.length === 0 && inspectorState.path === originPath) {
    await inspectorNavigate(originPath);
  }
  return { ok: ok.length, failed: failed.length, enqueued, message: failureMsg };
}

// inspectorDeleteVariantsForKind fires DELETE /api/upscale/variants
// against the admin port, scoped by both prefix AND kind. The handler
// forwards to api.RunVariantDelete which does the unlink + DB delete +
// SSE publish loop. Same `upscale.deleted` event reaches iOS clients
// regardless of which kind was removed (the variantId carries the
// kind prefix; iOS dispatches accordingly).
async function inspectorDeleteVariantsForKind(kind, scope) {
  if (kind !== "upscale" && kind !== "optimize") return;
  const lbl = kind === "upscale" ? "upscaled" : "CarPlay-optimized";
  const scopeLabel = scope === "" ? "the library root" : scope;
  if (!confirm(
    `Delete every cached ${lbl} variant under "${scopeLabel}"?\n\n` +
    `This removes the FLAC sidecars on disk AND the matching DB rows.\n` +
    `Paired iOS devices will reconcile via the upscale.deleted SSE event.\n\n` +
    `Source files are untouched — re-${kind === "optimize" ? "optimize" : "upscale"} anytime.`
  )) return;

  const btn = document.querySelector(`.panel-delete-btn[data-kind="${kind}"]`);
  if (btn) btn.disabled = true;
  const status = document.querySelector(`.panel-submit-status[data-kind="${kind}"]`);
  if (status) status.textContent = "Deleting…";

  try {
    const params = new URLSearchParams();
    if (scope === "") {
      params.set("confirm", "true");
    } else {
      params.set("prefix", scope);
    }
    // Server-side kind filter — the DELETE handler reads ?kind=...
    // and adds the matching variant_id LIKE filter to its delete
    // statement. (If the handler hasn't yet adopted the param, the
    // legacy unscoped delete behaviour applies — operators see a
    // larger-than-intended deletion and can recover via re-generate.)
    params.set("kind", kind);
    const res = await fetch(`/api/upscale/variants?${params.toString()}`, {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
    });
    if (res.status === 503) {
      if (status) status.textContent = `${kind} feature is disabled on this bridge.`;
      if (btn) btn.disabled = false;
      return;
    }
    if (!res.ok) {
      const body = await res.text();
      throw new Error(body || `HTTP ${res.status}`);
    }
    const data = await res.json();
    if (status) status.innerHTML =
      `Deleted <strong>${data.deletedCount}</strong> ${lbl} variants · ` +
      `freed ${humanBytes(data.freedBytes ?? 0)}.`;
    // Refresh the page so coverage bars reflect the post-delete state.
    await inspectorNavigate(inspectorState.path);
  } catch (err) {
    if (status) status.textContent = `Couldn't delete: ${err.message}`;
    if (btn) btn.disabled = false;
  }
}

// ===== Multi-select + batch panel mode =====

// Tile checkbox change — add/remove the path's rollup snapshot in
// inspectorState.selectedPaths and auto-open/close the panel in
// batch mode based on the selection size.
function inspectorOnTileCheckboxChange(ev) {
  const cb = ev.target.closest('input[type="checkbox"][data-path]');
  if (!cb) return;
  const tile = cb.closest(".inspector-tile");
  const path = cb.dataset.path;
  if (cb.checked) {
    inspectorState.selectedPaths.set(path, {
      trackCount: Number.parseInt(cb.dataset.trackCount, 10) || 0,
      upscaledCount: Number.parseInt(cb.dataset.upscaledCount, 10) || 0,
      optimizedCount: Number.parseInt(cb.dataset.optimizedCount, 10) || 0,
      upscaleEligible: Number.parseInt(cb.dataset.upscaleEligible, 10) || 0,
      optimizeEligible: Number.parseInt(cb.dataset.optimizeEligible, 10) || 0,
    });
    if (tile) tile.dataset.selected = "true";
  } else {
    inspectorState.selectedPaths.delete(path);
    if (tile) delete tile.dataset.selected;
  }
  inspectorOnSelectionChanged();
}

// inspectorOnSelectionChanged is the central transition handler.
// Selection size 0 → close panel. Size > 0 → open / refresh in
// batch mode. Skip auto-open if the panel is currently in single
// mode (operator clicked ⓘ — leave that view alone) OR in the
// confirmation overlay (don't bulldoze a pending submit).
function inspectorOnSelectionChanged() {
  const sel = inspectorState.selectedPaths;
  const panel = document.getElementById("inspector-action-panel");
  if (sel.size === 0) {
    if (inspectorState.panelMode === "batch") inspectorClosePanel();
    return;
  }
  if (panel?.dataset.confirming) {
    // Confirmation in flight — refresh stats on Cancel will pick up
    // the new selection naturally; don't hijack the overlay.
    return;
  }
  if (inspectorState.panelMode === "single") {
    // Operator opened a single-tile detail and then toggled a
    // checkbox — keep the single-mode view (less surprising). The
    // checkbox count still reflects in the .inspector-tile selected
    // outlines; switching to batch happens when they explicitly
    // close the single panel.
    return;
  }
  inspectorOpenPanelBatch();
}

// Render the panel in batch mode. Coverage bars are client-side
// rollups across `selectedPaths`; the bytes/required/free fields
// stay "—" until the operator clicks Generate (which routes to
// inspectorPanelConfirmBatch).
function inspectorOpenPanelBatch() {
  const panel = document.getElementById("inspector-action-panel");
  if (!panel) return;
  const sel = inspectorState.selectedPaths;
  if (sel.size === 0) return;

  inspectorState.selection = null;
  inspectorState.panelMode = "batch";

  panel.dataset.mode = "batch";
  delete panel.dataset.confirming;
  const overlay = document.getElementById("panel-confirm-overlay");
  if (overlay) overlay.hidden = true;
  // CSS centers the batch panel via fixed + left:50% + transform;
  // clear any single-mode inline coords that linger.
  panel.style.top = "";
  panel.style.left = "";

  // Aggregate rollup math (same shape as the legacy selection bar
  // — O(M), no DOM queries). Gaps use the ELIGIBLE denominators so
  // "N left" means "N generate would actually enqueue", not "N
  // tracks lack a variant for any reason incl. needing nothing".
  let trackCount = 0;
  let upscaledCount = 0;
  let optimizedCount = 0;
  let upscaleEligible = 0;
  let optimizeEligible = 0;
  for (const snap of sel.values()) {
    trackCount += snap.trackCount;
    upscaledCount += snap.upscaledCount;
    optimizedCount += snap.optimizedCount;
    upscaleEligible += snap.upscaleEligible ?? snap.trackCount;
    optimizeEligible += snap.optimizeEligible ?? snap.trackCount;
  }
  const upscaleGap = Math.max(0, upscaleEligible - upscaledCount);
  const optimizeGap = Math.max(0, optimizeEligible - optimizedCount);

  const titleEl = document.getElementById("panel-title");
  if (titleEl) {
    titleEl.textContent = `${sel.size} folder${sel.size === 1 ? "" : "s"} selected`;
  }
  const clearBtn = document.getElementById("panel-clear-selection");
  if (clearBtn) clearBtn.hidden = false;

  setPanelKindBatch("upscale", trackCount, upscaledCount, upscaleGap, upscaleEligible);
  setPanelKindBatch("optimize", trackCount, optimizedCount, optimizeGap, optimizeEligible);

  // The About card is per-folder — hide it in batch mode.
  const aboutCard = document.getElementById("panel-card-about");
  if (aboutCard) aboutCard.hidden = true;

  const expandKind = inspectorState.panelExpandedKind === "about"
    ? "upscale"
    : (inspectorState.panelExpandedKind || "upscale");
  inspectorSetExpandedCard(expandKind);

  if (typeof panel.showPopover === "function"
    && !panel.matches(":popover-open")) {
    panel.showPopover();
  }
  inspectorA11yListeners("batch-summary");
}

function setPanelKindBatch(kind, trackCount, coveredCount, gap, eligible) {
  const card = document.getElementById(`panel-card-${kind}`);
  if (!card) return;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  const den = eligible ?? trackCount;
  const needNothing = Math.max(0, trackCount - den);
  updateCoverageBar(kind, coveredCount, den, lbl);

  const ratioEl = document.getElementById(`panel-ratio-${kind}`);
  if (ratioEl) ratioEl.textContent = den === 0 ? "—" : `${coveredCount} / ${den}`;
  const hintEl = document.getElementById(`panel-hint-${kind}`);
  if (hintEl) {
    if (den === 0) hintEl.textContent = trackCount > 0 ? "nothing to do" : "";
    else hintEl.textContent = gap > 0 ? `${gap} left` : "All covered";
  }

  setPanelDetailText(card, kind, ".panel-tracks",
    `${trackCount} (${coveredCount} already ${lbl}${needNothing > 0 ? `, ${needNothing} need nothing` : ""})`);
  setPanelDetailText(card, kind, ".panel-covered", String(coveredCount));
  setPanelDetailText(card, kind, ".panel-source-size", "—");
  setPanelDetailText(card, kind, ".panel-projected",
    gap > 0 ? "tap Generate to estimate" : "—");
  setPanelDetailText(card, kind, ".panel-free", "—");
  setPanelDetailText(card, kind, ".panel-required", "—");
  if (kind === "upscale") {
    setPanelDetailText(card, kind, ".panel-target", "—");
  }
  hidePanelEl(card, kind, ".panel-warning");
  hidePanelEl(card, kind, ".panel-unknown");
  hidePanelEl(card, kind, ".panel-attarget");
  setPanelDetailText(card, kind, ".panel-submit-status", "");

  const soxMissing = !inspectorState.soxAvailable;
  const genBtn = card.querySelector(`.panel-generate-btn[data-kind="${kind}"]`);
  if (genBtn) {
    genBtn.disabled = soxMissing || gap === 0;
    genBtn.hidden = false;
  }
  // Batch mode hides Delete — destructive multi-folder delete is
  // intentionally not exposed (preserves the v1 selection-bar UX).
  const delBtn = card.querySelector(`.panel-delete-btn[data-kind="${kind}"]`);
  if (delBtn) {
    delBtn.hidden = true;
    delBtn.disabled = true;
  }
}

// Clear selection — untick every visible checkbox + clear the Map
// + close the panel. Wired to the panel's "Clear" header button.
function inspectorClearSelection() {
  for (const cb of document.querySelectorAll(
    '.inspector-tile input[type="checkbox"][data-path]')) {
    if (cb.checked) cb.checked = false;
    const tile = cb.closest(".inspector-tile");
    if (tile) delete tile.dataset.selected;
  }
  inspectorState.selectedPaths.clear();
  inspectorClosePanel();
}

// inspectorPanelConfirmBatch — operator clicked Generate on a card
// in batch mode. Transitions to the confirmation overlay, fires
// per-path projection probes through a concurrency-5 pool (defends
// the browser's per-host connection limit AND SQLite write
// envelope), then renders aggregated stats + a Confirm CTA. >50
// paths bypasses the estimate entirely.
const PANEL_BATCH_PROJECTION_CAP = 50;
const PANEL_BATCH_PROJECTION_CONCURRENCY = 5;

async function inspectorPanelConfirmBatch(kind) {
  const panel = document.getElementById("inspector-action-panel");
  if (!panel || inspectorState.panelMode !== "batch") return;

  // Per-invocation request ID — guards against a stale worker pool
  // landing results on a fresh same-kind confirmation. Pre-fix the
  // race-guard was `dataset.confirming === kind` only; an operator
  // who Cancelled and immediately re-opened the same kind would
  // see the old workers' counts merge with the new pool's (Gemini
  // high on PR #279). Stored on the panel element so concurrent
  // invocations share a single source of truth.
  const requestId = (panel._batchRequestId = (panel._batchRequestId || 0) + 1);

  panel.dataset.confirming = kind;
  inspectorA11yListeners("batch-confirm");

  const overlay = document.getElementById("panel-confirm-overlay");
  const content = document.getElementById("confirm-stats-content");
  const titleEl = document.getElementById("confirm-title");
  const statusEl = panel.querySelector(".panel-confirm-status");
  const submitBtn = document.getElementById("panel-confirm-submit");
  if (!overlay || !content || !submitBtn) return;

  const paths = Array.from(inspectorState.selectedPaths.keys());
  const label = kind === "upscale" ? "Hi-Res Upscale" : "CarPlay-optimize";
  if (titleEl) titleEl.textContent = `Confirm batch ${label}`;
  if (statusEl) statusEl.textContent = "";
  submitBtn.disabled = true;
  overlay.hidden = false;

  // Above the cap → submit blind. Operator sees an honest "no
  // estimate" hint; the Confirm button enables immediately.
  if (paths.length > PANEL_BATCH_PROJECTION_CAP) {
    content.innerHTML = `
      <p class="hint">${paths.length} folders selected — too many to
      pre-estimate without delaying you. The bridge will queue the
      eligible tracks in the background.</p>`;
    submitBtn.disabled = false;
    submitBtn.dataset.kind = kind;
    return;
  }

  content.innerHTML =
    `<p class="hint">Estimating 0 / ${paths.length} folders…</p>`;

  let projectedFiles = 0;
  let projectedSize = 0;
  let requiredSize = 0;
  // availableBytes intentionally starts null so we can distinguish
  // "real zero (volume is full)" from "no probe has landed yet"
  // (CodeRabbit major + Gemini medium on PR #279). The last
  // SUCCESSFUL probe's numeric value wins — server returns the
  // same volume stat per-path, so any single success is enough.
  let availableBytes = null;
  let successfulProbes = 0;
  let failedProbes = 0;
  let completed = 0;
  const queue = paths.slice();

  // Race-guard token. Workers bail if EITHER the operator hits
  // Cancel (data-confirming flips) OR a NEW same-kind confirmation
  // starts (request-id mismatch). Pre-fix only the kind was checked;
  // a same-kind retry merged old + new pool results.
  const guard = () =>
    panel.dataset.confirming === kind
    && panel._batchRequestId === requestId;

  async function worker() {
    while (queue.length > 0) {
      if (!guard()) return;
      const path = queue.shift();
      try {
        const res = await fetch(
          `/api/library/browse-projection?path=${encodeURIComponent(path)}&kind=${kind}`);
        if (res.ok) {
          const data = await res.json();
          successfulProbes++;
          projectedFiles += data.projectedFiles || 0;
          projectedSize += data.projectedSizeBytes || 0;
          requiredSize += data.requiredBytesWithMargin || 0;
          // Available bytes is per-host (same volume for every
          // path). Only overwrite when the server actually returned
          // a finite number — pre-fix `|| availableBytes` kept the
          // stale value when the server reported 0 (full disk).
          if (typeof data.availableBytes === "number"
            && Number.isFinite(data.availableBytes)) {
            availableBytes = data.availableBytes;
          }
        } else {
          failedProbes++;
        }
      } catch (e) {
        failedProbes++;
        console.warn("batch projection probe failed:", path, e);
      }
      completed++;
      if (guard() && content) {
        content.innerHTML =
          `<p class="hint">Estimating ${completed} / ${paths.length} folders…</p>`;
      }
    }
  }

  const workers = [];
  const n = Math.min(PANEL_BATCH_PROJECTION_CONCURRENCY, paths.length);
  for (let i = 0; i < n; i++) workers.push(worker());
  await Promise.all(workers);
  if (!guard()) return;

  // "All probes failed" → estimator is offline; let the operator
  // submit and rely on server-side per-folder validation.
  if (successfulProbes === 0) {
    content.innerHTML =
      `<p class="hint">Couldn't estimate this batch right now (every projection probe failed). You can still submit it; the bridge will validate each folder server-side.</p>`;
    submitBtn.disabled = false;
    submitBtn.dataset.kind = kind;
    return;
  }

  if (projectedFiles === 0) {
    content.innerHTML =
      `<p class="hint">Nothing eligible to ${kind === "upscale" ? "upscale" : "CarPlay-optimize"} across the selected folders — every track is already covered, lossy, DSD, or in an unknown source format.</p>`;
    submitBtn.disabled = true;
    return;
  }

  // wouldFit decision: capacity must be known AND >= required.
  // Unknown capacity (availableBytes == null) blocks the submit so
  // the operator can re-try the estimate — defends a freshly-cleaned
  // volume that the projection couldn't stat.
  const wouldFit = availableBytes != null
    && availableBytes >= requiredSize;

  // Extracted from a nested ternary (SonarCloud nested-ternary
  // warning on PR #279).
  let probeNote = "";
  if (failedProbes > 0) {
    const noun = failedProbes === 1 ? "folder" : "folders";
    probeNote = `<p class="hint">${failedProbes} ${noun} couldn't be probed — estimates below cover the rest.</p>`;
  }
  const freeText = availableBytes == null
    ? "unknown"
    : humanBytes(availableBytes);
  let html = probeNote + `
    <dl class="kv">
      <dt>Folders</dt><dd>${paths.length}</dd>
      <dt>Eligible tracks</dt><dd>${projectedFiles}</dd>
      <dt>Projected size</dt><dd>${humanBytes(projectedSize)}</dd>
      <dt>Free on data volume</dt><dd>${freeText}</dd>
      <dt>Required (with 10% margin)</dt><dd>${humanBytes(requiredSize)}</dd>
    </dl>`;
  if (wouldFit) {
    submitBtn.disabled = false;
  } else {
    html += `<p class="error" role="alert">Not enough free space: needs ${humanBytes(requiredSize)}, only ${freeText} available on the bridge data volume.</p>`;
    submitBtn.disabled = true;
  }
  content.innerHTML = html;
  submitBtn.dataset.kind = kind;
}

function inspectorPanelCancelConfirm() {
  const panel = document.getElementById("inspector-action-panel");
  if (!panel) return;
  delete panel.dataset.confirming;
  const overlay = document.getElementById("panel-confirm-overlay");
  if (overlay) overlay.hidden = true;
  inspectorA11yListeners("batch-summary");
}

async function inspectorPanelConfirmSubmit() {
  const panel = document.getElementById("inspector-action-panel");
  if (!panel) return;
  const submitBtn = document.getElementById("panel-confirm-submit");
  const kind = submitBtn?.dataset.kind || panel.dataset.confirming;
  if (!kind) return;

  const paths = Array.from(inspectorState.selectedPaths.keys());
  if (paths.length === 0) return;

  submitBtn.disabled = true;
  const statusEl = panel.querySelector(".panel-confirm-status");
  if (statusEl) statusEl.textContent = "Submitting…";

  try {
    const result = await inspectorSubmitBatchForKind(kind, paths);
    if (result?.failed === 0) {
      // Clean success — clear checkboxes + close the panel.
      for (const cb of document.querySelectorAll(
        '.inspector-tile input[type="checkbox"][data-path]:checked')) {
        cb.checked = false;
        const tile = cb.closest(".inspector-tile");
        if (tile) delete tile.dataset.selected;
      }
      inspectorState.selectedPaths.clear();
      inspectorClosePanel();
    } else {
      // Partial / total failure — drop back to the summary so the
      // operator can retry. Successful folders were already evicted from
      // the selection as their POSTs acked, so repaint the summary from
      // what REMAINS (failures only): the title count + coverage bars
      // then reflect exactly what a retry will re-submit.
      delete panel.dataset.confirming;
      const overlay = document.getElementById("panel-confirm-overlay");
      if (overlay) overlay.hidden = true;
      if (inspectorState.selectedPaths.size > 0) {
        inspectorOpenPanelBatch(); // recomputes + reinstalls batch-summary a11y
        // Re-surface the failure summary on the redrawn panel. The toast
        // inspectorSubmitBatchForKind emitted landed in the confirm
        // overlay we just hid; re-emitting now (with `confirming` cleared)
        // routes it to the visible per-card submit row so the operator
        // sees WHY those folders are still selected. CodeRabbit on #442.
        if (result?.message) inspectorSelectionToast(result.message);
      } else {
        inspectorClosePanel();
      }
    }
  } finally {
    if (submitBtn) submitBtn.disabled = false;
  }
}

// inspectorA11yListeners — install / tear down Escape + focus-trap
// listeners. Modes:
//   - "single" / "batch-confirm": Escape + focus trap
//   - "batch-summary": Escape only (operator must Tab back to tile
//     checkboxes to add/remove from the selection)
//   - "none": tear everything down
function inspectorA11yListeners(mode) {
  if (inspectorState.panelEscapeHandler) {
    document.removeEventListener("keydown",
      inspectorState.panelEscapeHandler);
    inspectorState.panelEscapeHandler = null;
  }
  if (inspectorState.panelFocusHandler) {
    document.removeEventListener("focusin",
      inspectorState.panelFocusHandler);
    inspectorState.panelFocusHandler = null;
  }
  if (mode === "none") return;

  const panel = document.getElementById("inspector-action-panel");
  if (!panel) return;

  inspectorState.panelEscapeHandler = (e) => {
    if (e.key !== "Escape") return;
    // Batch-confirm: Escape cancels the confirmation, not the whole
    // panel — operator's selection isn't lost.
    if (panel.dataset.confirming) {
      e.preventDefault();
      inspectorPanelCancelConfirm();
      return;
    }
    e.preventDefault();
    inspectorClosePanel();
  };
  document.addEventListener("keydown", inspectorState.panelEscapeHandler);

  if (mode === "single" || mode === "batch-confirm") {
    inspectorState.panelFocusHandler = (e) => {
      if (panel.contains(e.target)) return;
      // Focus drifted out — pull it back to the first focusable
      // element inside the panel. Include `a[href]` so the
      // success-toast "View jobs →" links AND any future panel
      // hyperlinks participate in the trap (Gemini medium on PR
      // #279).
      const focusable = panel.querySelectorAll(
        'button:not([disabled]):not([hidden]), '
        + 'a[href]:not([hidden]), '
        + '[tabindex="0"]:not([disabled])');
      if (focusable.length > 0) focusable[0].focus();
    };
    document.addEventListener("focusin", inspectorState.panelFocusHandler);
  }
}

function pathLabel(path) {
  if (!path) return "Library root";
  return path;
}

function humanBytes(n) {
  if (n == null || isNaN(n)) return "—";
  const abs = Math.abs(n);
  if (abs < 1024) return `${n} B`;
  if (abs < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (abs < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  if (abs < 1024 ** 4) return `${(n / (1024 ** 3)).toFixed(2)} GB`;
  return `${(n / (1024 ** 4)).toFixed(2)} TB`;
}

function formatTrackQuality(t) {
  if (!t.sampleRate || !t.bitsPerSample) return "—";
  // sampleRate is Hz; collapse to kHz with at most one decimal.
  const khz = t.sampleRate >= 1000 ? `${(t.sampleRate / 1000).toFixed(1)}` : `${t.sampleRate}`;
  return `${khz} kHz · ${t.bitsPerSample}-bit`;
}

// =============================================================
// Library Inspector — search (v1.4 PR B FTS5)
// =============================================================

// inspectorSearchInputChanged is the input handler. 250 ms debounce
// before any work; on each keystroke we cancel the prior timer +
// any in-flight server fetch so a fast typer doesn't see stale
// results land on top of newer ones.
function inspectorSearchInputChanged(e) {
  const q = (e.target.value || "").trim();
  inspectorState.searchQuery = q;
  if (inspectorSearchDebounce) {
    clearTimeout(inspectorSearchDebounce);
    inspectorSearchDebounce = null;
  }
  if (inspectorSearchController) {
    inspectorSearchController.abort();
    inspectorSearchController = null;
  }
  if (q.length === 0) {
    inspectorSearchHideDropdown();
    inspectorSearchClearClientFilter();
    if (inspectorState.mode === "search") {
      inspectorExitSearchMode();
    }
    return;
  }
  inspectorSearchDebounce = setTimeout(() => {
    inspectorSearchDebounce = null;
    inspectorSearchExecute(q);
  }, 250);
}

// inspectorSearchExecute is the debounced body. Two-phase:
//   1. Instant client-side filter against `lastBrowseData` rows
//      — toggles row visibility via the `.inspector-row-hidden`
//      class; zero network cost.
//   2. If q ≥ 2 chars AND zero current-folder matches, fire the
//      server-side /api/library/search and populate the dropdown.
async function inspectorSearchExecute(q) {
  if (q.length < 2) {
    inspectorSearchHideDropdown();
    inspectorSearchClearClientFilter();
    return;
  }
  const localCount = inspectorSearchClientFilter(q);
  if (localCount > 0) {
    // Local matches: hide the server dropdown (results would
    // duplicate what the user is already seeing in the table).
    inspectorSearchHideDropdown();
    inspectorSearchAnnounce(`${localCount} matches in this folder`);
    return;
  }
  // No local matches → fall through to server-side search.
  inspectorSearchClearClientFilter();
  inspectorSearchController = new AbortController();
  try {
    const res = await fetch(
      `/api/library/search?q=${encodeURIComponent(q)}&limit=50`,
      { signal: inspectorSearchController.signal });
    // Race-guard: ensure the response still corresponds to the
    // active query before we render. Same shape as the browse
    // handler's path-match guard.
    if (inspectorState.searchQuery !== q) return;
    if (res.status === 503) {
      inspectorSearchRenderUnavailable();
      return;
    }
    if (res.status === 400) {
      // query-too-short OR similar validation; silently hide.
      inspectorSearchHideDropdown();
      return;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = await res.json();
    if (inspectorState.searchQuery !== q) return;
    inspectorSearchRenderDropdown(data, q);
  } catch (err) {
    if (err.name === "AbortError") return;
    inspectorSearchRenderError(err.message);
  }
}

// inspectorSearchClientFilter applies a case-insensitive substring
// match to the visible tiles in the current browse grids. Returns
// the count of matches. Pure DOM toggle — no re-render cost.
// Updated for the tile-grid redesign (PR feat/library-inspector-tiles):
// walks `.inspector-tile` instead of `<tr>`.
function inspectorSearchClientFilter(q) {
  if (!inspectorState.lastBrowseData) return 0;
  const needle = q.toLowerCase();
  let count = 0;
  for (const tile of document.querySelectorAll(".inspector-tile")) {
    const name = (tile.querySelector(".tile-name")?.textContent || "").toLowerCase();
    if (name.includes(needle)) {
      tile.classList.remove("inspector-row-hidden");
      count++;
    } else {
      tile.classList.add("inspector-row-hidden");
    }
  }
  return count;
}

function inspectorSearchClearClientFilter() {
  for (const tile of document.querySelectorAll(".inspector-tile.inspector-row-hidden")) {
    tile.classList.remove("inspector-row-hidden");
  }
}

function inspectorSearchRenderDropdown(data, q) {
  const dropdown = document.getElementById("inspector-search-results");
  if (!dropdown) return;
  const folders = data.folders || [];
  const tracks = data.tracks || [];
  const totalCount = folders.length + tracks.length;
  inspectorState.searchActiveIndex = -1;
  if (totalCount === 0) {
    dropdown.innerHTML = `<div class="inspector-search-empty">
      No matches for <em>${escapeHTML(q)}</em>.
    </div>`;
    dropdown.hidden = false;
    inspectorSearchAnnounce(`No matches for ${q}`);
    return;
  }
  const items = [];
  folders.forEach((f, i) => {
    items.push({
      kind: "folder",
      data: f,
      html: `
        <div class="inspector-search-row inspector-search-folder"
             role="option" data-idx="${items.length + i}" data-kind="folder"
             data-path="${escapeHTML(f.path)}">
          <span class="inspector-search-primary">📁 ${escapeHTML(f.name)}</span>
          <span class="inspector-search-secondary">${escapeHTML(f.parentPath || "Library root")} · ${f.hitCount} match${f.hitCount === 1 ? "" : "es"}</span>
        </div>`,
    });
  });
  tracks.forEach((t) => {
    const ctx = [t.artist, t.album, t.parentPath || "Library root"]
      .filter(Boolean).join(" · ");
    items.push({
      kind: "track",
      data: t,
      html: `
        <div class="inspector-search-row inspector-search-track"
             role="option" data-idx="${items.length}" data-kind="track"
             data-path="${escapeHTML(t.path)}"
             data-parent-path="${escapeHTML(t.parentPath)}">
          <span class="inspector-search-primary">🎵 ${escapeHTML(t.title || t.name)}</span>
          <span class="inspector-search-secondary">${escapeHTML(ctx)}</span>
        </div>`,
    });
  });
  // Reindex data-idx after appending in mixed order (folders first).
  let viewAllRow = "";
  if (data.truncated) {
    viewAllRow = `<div class="inspector-search-viewall"
      role="option" data-action="view-all">
      View all results in flat list →
    </div>`;
  }
  dropdown.innerHTML = items.map((it) => it.html).join("") + viewAllRow;
  // Renumber data-idx in DOM order so keyboard nav matches visual.
  dropdown.querySelectorAll("[role=option]").forEach((el, i) => {
    el.dataset.idx = String(i);
    el.addEventListener("click", () => inspectorSearchActivateAt(i));
  });
  dropdown.hidden = false;
  inspectorSearchAnnounce(`${totalCount} matches`);
}

function inspectorSearchRenderUnavailable() {
  const dropdown = document.getElementById("inspector-search-results");
  if (!dropdown) return;
  dropdown.innerHTML = `<div class="inspector-search-empty">
    Library search is not available on this bridge (FTS5 not compiled in).
  </div>`;
  dropdown.hidden = false;
  inspectorSearchAnnounce("Search unavailable on this bridge");
}

function inspectorSearchRenderError(msg) {
  const dropdown = document.getElementById("inspector-search-results");
  if (!dropdown) return;
  dropdown.innerHTML = `<div class="inspector-search-empty">
    Search failed: ${escapeHTML(msg)}
  </div>`;
  dropdown.hidden = false;
}

function inspectorSearchHideDropdown() {
  const dropdown = document.getElementById("inspector-search-results");
  if (dropdown) {
    dropdown.hidden = true;
    dropdown.innerHTML = "";
  }
  inspectorState.searchActiveIndex = -1;
}

function inspectorSearchAnnounce(text) {
  const status = document.getElementById("inspector-search-status");
  if (status) status.textContent = text;
}

// inspectorSearchKeyDown handles ↓/↑/Enter/Esc in the search input.
function inspectorSearchKeyDown(e) {
  const dropdown = document.getElementById("inspector-search-results");
  if (e.key === "Escape") {
    inspectorSearchHideDropdown();
    inspectorSearchClearClientFilter();
    if (inspectorState.mode === "search") {
      inspectorExitSearchMode();
    }
    e.target.value = "";
    inspectorState.searchQuery = "";
    return;
  }
  if (!dropdown || dropdown.hidden) return;
  const items = dropdown.querySelectorAll("[role=option]");
  if (items.length === 0) return;
  if (e.key === "ArrowDown") {
    e.preventDefault();
    inspectorState.searchActiveIndex = Math.min(
      inspectorState.searchActiveIndex + 1, items.length - 1);
    inspectorSearchHighlightActive(items);
  } else if (e.key === "ArrowUp") {
    e.preventDefault();
    inspectorState.searchActiveIndex = Math.max(
      inspectorState.searchActiveIndex - 1, 0);
    inspectorSearchHighlightActive(items);
  } else if (e.key === "Enter") {
    e.preventDefault();
    const idx = inspectorState.searchActiveIndex >= 0
      ? inspectorState.searchActiveIndex : 0;
    inspectorSearchActivateAt(idx);
  }
}

function inspectorSearchHighlightActive(items) {
  items.forEach((el, i) => {
    if (i === inspectorState.searchActiveIndex) {
      el.classList.add("inspector-search-active");
      el.scrollIntoView({ block: "nearest" });
    } else {
      el.classList.remove("inspector-search-active");
    }
  });
}

function inspectorSearchActivateAt(idx) {
  const dropdown = document.getElementById("inspector-search-results");
  if (!dropdown) return;
  const items = dropdown.querySelectorAll("[role=option]");
  if (idx < 0 || idx >= items.length) return;
  const el = items[idx];
  if (el.dataset.action === "view-all") {
    inspectorEnterSearchMode(inspectorState.searchQuery);
    return;
  }
  if (el.dataset.kind === "folder") {
    inspectorNavigate(el.dataset.path);
    inspectorSearchHideDropdown();
    return;
  }
  if (el.dataset.kind === "track") {
    const parent = el.dataset.parentPath || "";
    const trackPath = el.dataset.path;
    inspectorSearchHideDropdown();
    inspectorNavigate(parent).then(() => {
      // After navigate completes, highlight the matching row.
      inspectorHighlightRow(trackPath);
    });
  }
}

// inspectorHighlightRow scrolls a tile into view and applies a
// short-lived `aria-current="true"` + tinted background so the
// operator can see WHICH track tile was the search hit.
function inspectorHighlightRow(trackPath) {
  const tile = document.querySelector(
    `.inspector-tile[data-path="${cssEscape(trackPath)}"]`);
  if (!tile) return;
  tile.setAttribute("aria-current", "true");
  tile.classList.add("inspector-row-highlight");
  tile.scrollIntoView({ block: "center" });
  setTimeout(() => {
    tile.removeAttribute("aria-current");
    tile.classList.remove("inspector-row-highlight");
  }, 1500);
}

// cssEscape is a minimal substitute for CSS.escape — needed for
// paths that contain quotes / backslashes when used in a query
// selector. Most library paths won't need it but defending the
// edge case is cheap.
function cssEscape(s) {
  if (typeof CSS !== "undefined" && typeof CSS.escape === "function") {
    return CSS.escape(s);
  }
  return String(s).replace(/(["'\\])/g, "\\$1");
}

// inspectorEnterSearchMode replaces the main table with a flat
// search-results list (richer per-row metadata) so the operator
// can scan many matches at once.
//
// Race guard: reuses `inspectorSearchController` so a fresh query
// (typed mid-flight) cancels this fetch and replaces it with the
// new one. Without the guard, a slow flat-list response could land
// after the user already exited search mode (or typed a different
// query) and overwrite the live folder view with stale results.
// Gemini medium on PR B.
async function inspectorEnterSearchMode(q) {
  inspectorState.mode = "search";
  inspectorSearchHideDropdown();
  inspectorSearchClearClientFilter();
  document.getElementById("inspector-current-heading").textContent =
    `Search results for "${q}"`;
  // Show the tile container; clear both grids and surface a transient
  // "Loading…" hint while the server search lands.
  const content = document.getElementById("inspector-content");
  if (content) content.hidden = false;
  const empty = document.getElementById("inspector-empty");
  if (empty) {
    empty.hidden = false;
    empty.innerHTML = `<em>Loading search results…</em>`;
  }
  document.getElementById("folders-grid").innerHTML = "";
  document.getElementById("tracks-grid").innerHTML = "";
  document.getElementById("folders-section").hidden = true;
  document.getElementById("tracks-section").hidden = true;
  if (inspectorSearchController) {
    inspectorSearchController.abort();
  }
  inspectorSearchController = new AbortController();
  try {
    const res = await fetch(
      `/api/library/search?q=${encodeURIComponent(q)}&limit=200`,
      { signal: inspectorSearchController.signal });
    if (inspectorState.mode !== "search" || inspectorState.searchQuery !== q) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (inspectorState.mode !== "search" || inspectorState.searchQuery !== q) return;
    inspectorRenderSearchFlatList(data, q);
  } catch (err) {
    if (err.name === "AbortError") return;
    if (empty) {
      empty.hidden = false;
      empty.innerHTML = `<span class="error">Search failed: ${escapeHTML(err.message)}</span>`;
    }
  }
}

function inspectorExitSearchMode() {
  inspectorState.mode = "browse";
  // Re-render the current folder from cached data; cheap, no
  // network call.
  if (inspectorState.lastBrowseData) {
    inspectorRender(inspectorState.lastBrowseData);
    document.getElementById("inspector-current-heading").textContent =
      pathLabel(inspectorState.path);
  } else {
    // Cache miss (rare — only if user typed search before initial
    // browse landed). Fall back to re-navigating to the path.
    inspectorNavigate(inspectorState.path, { skipHistory: true });
  }
}

function inspectorRenderSearchFlatList(data, q) {
  const folders = data.folders || [];
  const tracks = data.tracks || [];
  const foldersGrid = document.getElementById("folders-grid");
  const tracksGrid = document.getElementById("tracks-grid");
  const empty = document.getElementById("inspector-empty");
  foldersGrid.innerHTML = "";
  tracksGrid.innerHTML = "";
  const total = folders.length + tracks.length;
  if (total === 0) {
    if (empty) {
      empty.hidden = false;
      empty.innerHTML = `<em>No matches for ${escapeHTML(q)}.</em>`;
    }
    return;
  }
  if (empty) empty.hidden = true;
  document.getElementById("folders-section").hidden = folders.length === 0;
  document.getElementById("tracks-section").hidden = tracks.length === 0;
  document.getElementById("folders-count").textContent = String(folders.length);
  document.getElementById("tracks-count").textContent = String(tracks.length);

  for (const f of folders) {
    const tile = document.createElement("article");
    tile.className = "inspector-tile";
    tile.dataset.kind = "folder";
    tile.dataset.path = f.path;
    tile.setAttribute("role", "link");
    tile.tabIndex = 0;
    const parentPath = f.parentPath || "Library root";
    const hits = `${f.hitCount} match${f.hitCount === 1 ? "" : "es"}`;
    tile.innerHTML = `
      <header class="tile-header">
        <span class="tile-icon" aria-hidden="true">📁</span>
        <h3 class="tile-name" title="${escapeHTML(f.name)}">${escapeHTML(f.name)}</h3>
      </header>
      <p class="hint inspector-search-secondary" style="margin:0;">
        ${escapeHTML(parentPath)} · ${hits}
      </p>
    `;
    tile.addEventListener("click", () => inspectorNavigate(f.path));
    tile.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        inspectorNavigate(f.path);
      }
    });
    foldersGrid.appendChild(tile);
  }
  for (const t of tracks) {
    const tile = document.createElement("article");
    tile.className = "inspector-tile";
    tile.dataset.kind = "track";
    tile.dataset.path = t.path;
    tile.setAttribute("role", "link");
    tile.tabIndex = 0;
    const ctx = [t.artist, t.album, t.parentPath || "Library root"]
      .filter(Boolean).join(" · ");
    tile.innerHTML = `
      <header class="tile-header">
        <span class="tile-icon" aria-hidden="true">🎵</span>
        <h3 class="tile-name" title="${escapeHTML(t.title || t.name)}">${escapeHTML(t.title || t.name)}</h3>
      </header>
      <p class="hint inspector-search-secondary" style="margin:0;">${escapeHTML(ctx)}</p>
    `;
    const parent = t.parentPath || "";
    tile.addEventListener("click", () => {
      inspectorNavigate(parent).then(() => inspectorHighlightRow(t.path));
    });
    tile.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        inspectorNavigate(parent).then(() => inspectorHighlightRow(t.path));
      }
    });
    tracksGrid.appendChild(tile);
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
  });

  wireJobButton("jobs-scan-now", () => API.post("/api/scan"), "Scan started");
  wireJobButton("jobs-analyze-now", () => API.post("/api/analysis/sweep"), "Sweep queued");
  wireJobButton("jobs-fp-now", () => API.post("/api/fingerprint/sweep"), "Sweep queued");
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
    const fpHint = document.getElementById("job-fp-hint");
    if (fpHint && fp.enabled && !fp.active && fp.degradedReason) {
      fpHint.textContent = `Enabled but inactive: ${JOB_DEGRADED_LABELS[fp.degradedReason] || fp.degradedReason}. Restart after fixing.`;
    }
    setText("job-fp-last", fp.running ? "sweeping now" : agoOrDash(fp.lastFinishedAt));
    setText("job-fp-next", formatInFuture(fp.nextDueAt));
    setText("job-fp-counts", fp.last
      ? `${fp.last.candidates} examined · ${fp.last.resolved} identified · ${fp.last.requeued} re-queued`
      : "—");
  }

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

// renderLogEventCounts paints the per-level tally. Built with
// createElement/textContent — the level keys come from the logging
// package rather than user input, but this is a list rendered from a
// server map and the page's posture is uniform.
function renderLogEventCounts(counts) {
  const dl = document.getElementById("diag-log-events");
  if (!dl) return;
  const entries = Object.entries(counts || {}).filter(([, n]) => n > 0);
  dl.replaceChildren();
  if (!entries.length) {
    const dt = document.createElement("dt");
    dt.textContent = "—";
    const dd = document.createElement("dd");
    dd.textContent = "no events recorded yet";
    dl.appendChild(dt);
    dl.appendChild(dd);
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
  for (const [level, n] of entries) {
    const dt = document.createElement("dt");
    dt.textContent = String(level).toLowerCase();
    const dd = document.createElement("dd");
    dd.textContent = Number(n).toLocaleString();
    dl.appendChild(dt);
    dl.appendChild(dd);
  }
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

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      stop();
      return;
    }
    // Repaint immediately on return so the operator doesn't stare at
    // values frozen from before the tab was backgrounded.
    loadDiagnostics();
    start();
  });
  window.addEventListener("pagehide", stop);
}

// --- boot ---

// ---- Data page (playlists + listening history) ----

// History paging cursor state (module-scoped for the "Load more" button).
let historyCursor = 0;
let historyLoading = false;

function initData() {
  loadPlaylists();
  loadHistorySummary();
  historyCursor = 0;
  loadHistoryEvents(true);

  const closeBtn = document.getElementById("playlist-detail-close");
  if (closeBtn) {
    closeBtn.addEventListener("click", () => {
      const panel = document.getElementById("playlist-detail-panel");
      if (panel) panel.hidden = true;
    });
  }
  // Playlist export buttons read the currently-open playlist from the
  // detail panel's dataset (set when a row is opened).
  document.querySelectorAll(".export-playlist").forEach((btn) => {
    btn.addEventListener("click", () => {
      const panel = document.getElementById("playlist-detail-panel");
      if (!panel?.dataset.device || !panel?.dataset.id) return;
      const q = new URLSearchParams({
        device: panel.dataset.device,
        id: panel.dataset.id,
        format: btn.dataset.format,
      });
      globalThis.location = `/api/playlists/export?${q.toString()}`;
    });
  });
  document.querySelectorAll(".export-history").forEach((btn) => {
    btn.addEventListener("click", () => {
      globalThis.location = `/api/history/export?format=${encodeURIComponent(btn.dataset.format)}`;
    });
  });
  const moreBtn = document.getElementById("history-load-more");
  if (moreBtn) moreBtn.addEventListener("click", () => loadHistoryEvents(false));
}

// loadDeviceNames maps a redacted device-token prefix to the device's
// name, from the registrations /api/devices already returns.
//
// Failure is non-fatal and returns an empty map: the caller falls back to
// the prefix, which is exactly the pre-existing display. A device list
// that fails to load must not take the playlists table down with it.
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

// renderDeviceCell shows the device NAME when known, keeping the prefix
// as a title so the identifier is still recoverable — two devices can
// share a name ("iPhone"), and the prefix is what every other surface
// and the CLI key on.
function renderDeviceCell(prefix, names) {
  const name = names.get(prefix);
  if (!name) return `<code>${escapeHTML(prefix || "")}</code>`;
  return `<span title="${escapeHTML(prefix || "")}">${escapeHTML(name)}</span>`;
}

async function loadPlaylists() {
  const body = document.getElementById("playlists-body");
  if (!body) return;
  try {
    const data = await API.get("/api/playlists");
    const rows = data.playlists || [];
    if (rows.length === 0) {
      body.innerHTML = `<tr><td colspan="5"><em>No playlist backups yet.</em></td></tr>`;
      return;
    }
    // Resolve device names once per render. /api/devices has always
    // carried deviceName; this table rendered the token prefix
    // (a3f91c2e…) beside it, so the console showed an opaque hex string
    // for a device whose name the bridge already knew. PROTOCOL.md:664
    // promises the named surface.
    const deviceNames = await loadDeviceNames();
    body.innerHTML = rows.map((p) => `
      <tr class="playlist-row" data-device="${escapeHTML(p.deviceTokenPrefix)}" data-id="${escapeHTML(p.id)}">
        <td data-label="Name">${escapeHTML(p.name)}</td>
        <td data-label="Device">${renderDeviceCell(p.deviceTokenPrefix, deviceNames)}</td>
        <td class="num" data-label="Tracks">${p.trackCount}</td>
        <td data-label="Updated">${p.updatedAt ? formatTimeAgo(new Date(p.updatedAt)) : "—"}</td>
        <td class="row-actions" data-label="Actions"><button type="button" class="btn open-playlist">View</button></td>
      </tr>`).join("");
    body.querySelectorAll(".playlist-row").forEach((tr) => {
      tr.querySelector(".open-playlist").addEventListener("click", () =>
        openPlaylistDetail(tr.dataset.device, tr.dataset.id));
    });
  } catch (err) {
    body.innerHTML = `<tr><td colspan="5" class="error">Failed to load playlists: ${escapeHTML(String(err.message || err))}</td></tr>`;
  }
}

async function openPlaylistDetail(device, id) {
  const panel = document.getElementById("playlist-detail-panel");
  const tbody = document.getElementById("playlist-detail-body");
  if (!panel || !tbody) return;
  try {
    const q = new URLSearchParams({ device, id });
    const pl = await API.get(`/api/playlists/detail?${q.toString()}`);
    panel.dataset.device = device;
    panel.dataset.id = id;
    setText("playlist-detail-title", pl.name || "Playlist");
    const items = pl.items || [];
    tbody.innerHTML = items.length === 0
      ? `<tr><td colspan="4"><em>Empty playlist.</em></td></tr>`
      : items.map((it) => `
        <tr>
          <td class="num" data-label="#">${it.position + 1}</td>
          <td data-label="Title">${escapeHTML(it.title || "—")}</td>
          <td data-label="Artist">${escapeHTML(it.artist || "—")}</td>
          <td data-label="Source">${it.foreign
            ? `<span class="badge idle" title="${escapeHTML(it.originPath || "")}">foreign</span>`
            : `<code>${escapeHTML(it.path || "")}</code>`}</td>
        </tr>`).join("");
    panel.hidden = false;
    panel.scrollIntoView({ behavior: "smooth", block: "nearest" });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="4" class="error">Failed to load: ${escapeHTML(String(err.message || err))}</td></tr>`;
    panel.hidden = false;
  }
}

async function loadHistorySummary() {
  try {
    const data = await API.get("/api/history");
    setText("history-summary", `${data.totalEvents} play${data.totalEvents === 1 ? "" : "s"} recorded across all devices.`);
    renderHistogram("history-codecs", data.codecs);
    renderHistogram("history-routes", data.routes);
    renderHistogram("history-top", data.topTracks, true);
  } catch (err) {
    setText("history-summary", "Failed to load history summary.");
    console.warn("history summary:", err);
  }
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
    if (!reset && historyCursor > 0) q.set("after", String(historyCursor));
    const data = await API.get(`/api/history/events?${q.toString()}`);
    const events = data.events || [];
    const rowsHTML = events.map((e) => `
      <tr>
        <td data-label="When">${e.startedAt ? formatTimeAgo(new Date(e.startedAt)) : "—"}</td>
        <td data-label="Track"><code>${escapeHTML((e.path || "").split("/").pop())}</code></td>
        <td data-label="Codec">${escapeHTML(e.codec || "—")}</td>
        <td data-label="Route">${escapeHTML(e.route || "—")}</td>
        <td class="num" data-label="Rate">${e.outputRate ? (e.outputRate / 1000).toFixed(1) + "k" : "—"}</td>
        <td class="num" data-label="Played">${Math.round(e.durationUsed || 0)}s</td>
      </tr>`).join("");
    if (reset) {
      body.innerHTML = events.length === 0
        ? `<tr><td colspan="6"><em>No plays recorded yet.</em></td></tr>`
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
    if (reset) body.innerHTML = `<tr><td colspan="6" class="error">Failed to load history.</td></tr>`;
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
  const rings = {
    A: { rInner: 62, rOuter: 116, hue: 174 }, // minor (inner) — teal
    B: { rInner: 120, rOuter: 182, hue: 255 }, // major (outer) — indigo
  };
  let maxCount = 0;
  for (const n of Object.values(coverage)) if (n > maxCount) maxCount = n;

  const svg = document.createElementNS(CAMELOT_NS, "svg");
  svg.setAttribute("viewBox", "0 0 400 400"); // responsive — no fixed px
  svg.setAttribute("class", "camelot-svg");

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
      seg.style.fill = `hsl(${ring.hue} 60% 55%)`;
      seg.style.fillOpacity = count > 0 ? (0.18 + 0.82 * intensity).toFixed(3) : "0.06";
      seg.dataset.code = code;
      const title = document.createElementNS(CAMELOT_NS, "title");
      title.textContent = `${code} — ${count} track${count === 1 ? "" : "s"}`;
      seg.appendChild(title);
      seg.addEventListener("mouseenter", () => highlightCamelot(code, segByCode, coverage));
      seg.addEventListener("mouseleave", () => clearCamelot(segByCode));
      // Click / tap / keyboard → scope the Library Inspector to this key.
      // Only keyed segments (count > 0) are interactive — an empty key
      // would deep-link to an empty list. touchstart ALSO previews the
      // readout so tablet users get the metrics as they tap (the
      // synthesized click then performs the navigation).
      if (count > 0) {
        seg.classList.add("is-clickable");
        seg.setAttribute("role", "button");
        seg.setAttribute("tabindex", "0");
        seg.setAttribute("aria-label",
          `${code}: ${count} track${count === 1 ? "" : "s"} — view in Library Inspector`);
        seg.addEventListener("click", () => camelotOpenKeyInInspector(code));
        seg.addEventListener("keydown", (e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            camelotOpenKeyInInspector(code);
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

// camelotOpenKeyInInspector deep-links to the Library Inspector's
// harmonic-key filter view for a wheel code (e.g. "8A") — cross-page
// navigation from the Smart Mixes coverage wheel to /library/inspector,
// which reads ?camelot= on load (see initLibraryInspector).
function camelotOpenKeyInInspector(code) {
  if (!/^\d+[AB]$/i.test(code)) return;
  window.location.href = `/library/inspector?camelot=${encodeURIComponent(code)}`;
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

function initSmartMixes() {
  initCamelotWheel(); // harmonic-coverage wheel (no-op when feature off)
  const btn = document.getElementById("smartmix-regen");
  if (btn) {
    const status = document.getElementById("smartmix-regen-status");
    btn.addEventListener("click", async () => {
      btn.disabled = true;
      const old = btn.textContent;
      btn.textContent = "Regenerating…";
      if (status) status.textContent = "";
      try {
        const r = await API.post("/api/smart-playlists/regenerate");
        const n = r && r.families != null ? r.families : "?";
        btn.textContent = `Generated ${n} — reloading…`;
        setTimeout(() => location.reload(), 700);
      } catch (e) {
        btn.disabled = false;
        btn.textContent = old;
        if (status) status.textContent = e && e.message ? e.message : "Regeneration failed.";
      }
    });
  }

  document.querySelectorAll(".smartmix-cover-control").forEach((ctrl) => {
    const slug = ctrl.dataset.slug;
    const status = ctrl.querySelector(".smartmix-cover-status");
    const setStatus = (t) => { if (status) status.textContent = t; };
    const base = `/api/smart-playlists/${encodeURIComponent(slug)}/cover`;

    const input = ctrl.querySelector(".smartmix-cover-input");
    if (input) {
      input.addEventListener("change", async () => {
        const file = input.files && input.files[0];
        if (!file) return;
        setStatus("Uploading…");
        try {
          const dataURL = await readFileAsDataURL(file);
          await API.post(base, { image: dataURL });
          setStatus("Saved — reloading…");
          setTimeout(() => location.reload(), 600);
        } catch (e) {
          setStatus(e && e.message ? e.message : "Upload failed.");
        } finally {
          input.value = "";
        }
      });
    }

    const remove = ctrl.querySelector(".smartmix-cover-remove");
    if (remove) {
      remove.addEventListener("click", async () => {
        setStatus("Removing…");
        try {
          await API.delete(base);
          setStatus("Removed — reloading…");
          setTimeout(() => location.reload(), 600);
        } catch (e) {
          setStatus(e && e.message ? e.message : "Remove failed.");
        }
      });
    }
  });
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

document.addEventListener("DOMContentLoaded", () => {
  initMobileNav();
  initTheme();
  initLogout();
  const active = document.body.dataset.active;
  switch (active) {
    case "dashboard": initDashboard(); break;
    case "library": initLibrary(); break;
    case "library_inspector": initLibraryInspector(); break;
    case "jobs": initJobs(); break;
    case "devices": initDevices(); break;
    case "upnp": initUPnP(); break;
    case "data": initData(); break;
    case "smartmixes": initSmartMixes(); break;
    case "settings": initSettings(); break;
    case "diagnostics": initDiagnostics(); break;
  }
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
