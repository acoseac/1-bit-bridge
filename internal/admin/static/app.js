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
  } catch (e) {
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
    if (!info || !info.notAfter) {
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
  let badgeClass = "idle", badgeText = "Detecting…", suffix = "";
  if (!s.cliAvailable && s.lastError) {
    badgeClass = "idle";
    badgeText = "Disabled";
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
      alert("Re-mint failed: " + (err && err.message ? err.message : "unknown error"));
    } finally {
      btn.textContent = oldText;
      btn.disabled = false;
    }
  });
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
  const actions = document.querySelector(".panel-head .panel-actions");
  let installBtn = document.getElementById("update-install");
  if (actions) {
    const should = u && u.updateAvailable && u.canInstall;
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

  if (u && u.updateAvailable && u.latestVersion) {
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
  } else if (u && u.latestVersion) {
    status.innerHTML = `<span class="badge idle">up to date</span><span>· latest <code>${escapeHTML(u.latestVersion)}</code></span>`;
  } else if (u && u.lastError) {
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
    if (u && u.lastError) {
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
  if (latest && u && u.latestVersion) {
    latest.textContent = u.latestVersion;
    latest.hidden = false;
  }

  // DeferredReason: the auto-installer's gate refused this cycle
  // (currently MinClientVersion compat). Surface as a yellow
  // "deferred" badge so the operator can see why an available
  // update isn't installing automatically.
  const deferred = document.getElementById("update-deferred");
  if (deferred) {
    if (u && u.deferredReason) {
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
  activate(saved && validIds.has(saved) ? saved : tabsArr[0].dataset.tab);
}

function initSettings() {
  initSettingsTabs();
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
    };
    try {
      const r = await API.patch("/api/settings", body);
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

  // v1.2 Audio quality stats poller. Refreshes the upscale tile
  // every 5 s while the Settings page is the active tab. Cheap
  // (single SQL COUNT + a mutex-protected pool snapshot in the
  // handler); the visibility check (the tile is hidden when the
  // feature is off) keeps the dashboard quiet for operators
  // who never enabled upscaling.
  startUpscaleStatsPoller();
  // Typed-phrase clear-all-variants modal wiring. Lives in
  // initSettings because the button + dialog are on this page.
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
        statusEl.textContent = "Upscale feature is disabled on this bridge.";
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
      // counters reflect the post-delete state. The poller would
      // catch up within 5 s anyway, but the immediate refresh
      // closes the visual loop on the operator's action.
      const tile = document.getElementById("upscale-stats");
      if (tile) refreshUpscaleStats(tile);
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

const upscaleStatsPollMs = 5000;
let upscaleStatsTimer = null;

function startUpscaleStatsPoller() {
  // Defensive: clear any prior timer when re-entering the
  // Settings page (single-page navigation reuses initSettings).
  if (upscaleStatsTimer) {
    clearInterval(upscaleStatsTimer);
    upscaleStatsTimer = null;
  }
  const tile = document.getElementById("upscale-stats");
  if (!tile) return; // not on the settings page
  refreshUpscaleStats(tile);
  upscaleStatsTimer = setInterval(() => refreshUpscaleStats(tile), upscaleStatsPollMs);
}

async function refreshUpscaleStats(tile) {
  try {
    const r = await API.get("/api/upscale/stats");
    // Hide the whole tile when the feature has never been used
    // (no cached variants AND feature is currently off). A
    // disabled feature with cached files keeps the tile up so
    // the operator sees historical state and disk usage.
    const hasHistory = r.cachedVariants > 0;
    if (!r.enabled && !hasHistory) {
      tile.hidden = true;
      return;
    }
    tile.hidden = false;
    setText("upscale-cached-count", r.cachedVariants ?? 0);
    setText("upscale-cached-bytes", formatBytes(r.cachedBytes ?? 0));
    if (r.pool) {
      setText("upscale-workers", r.pool.workers);
      setText("upscale-queue", r.pool.queueLen + " / " + r.pool.queueCap);
      setText("upscale-inflight", r.pool.inflight);
      setText("upscale-done", r.pool.done);
      setText("upscale-failed", r.pool.failed);
    } else {
      // Feature is off but we have cached variants — show the
      // historical fields, em-dash the live ones to communicate
      // "no live pool right now".
      setText("upscale-workers", "—");
      setText("upscale-queue", "—");
      setText("upscale-inflight", "—");
      setText("upscale-done", "—");
      setText("upscale-failed", "—");
    }
    // Toggle the "Clear all upscaled variants" button: nothing to
    // clear when the cache is empty, so disable to communicate
    // that visually. Re-enables on the next stats tick if a fresh
    // batch lands cached variants while this page is open.
    const clearBtn = document.getElementById("upscale-clear-all-btn");
    if (clearBtn) {
      clearBtn.disabled = !(r.cachedVariants > 0);
    }
  } catch (err) {
    // Stats endpoint failure isn't user-visible — log to
    // console so a developer debugging on the page can see it,
    // but don't disrupt the rest of the Settings UI.
    console.warn("upscale stats fetch failed:", err);
  }
}

function setText(id, value) {
  const el = document.getElementById(id);
  if (el) el.textContent = String(value);
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
  es.addEventListener("stats",     seen((e) => safeApply("stats",     e.data, applyStats)));
  es.addEventListener("endpoints", seen((e) => safeApply("endpoints", e.data, applyEndpoints)));
  es.addEventListener("pairing",   seen((e) => safeApply("pairing",   e.data, applyPairing)));
  es.addEventListener("updates",   seen((e) => safeApply("updates",   e.data, renderUpdateTile)));
  es.addEventListener("tailscale", seen((e) => safeApply("tailscale", e.data, renderTailscaleTile)));

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
    try { activeEventSource.close(); } catch (_) { /* ignore */ }
  }
  startEventStream();
  // Belt-and-braces: backfill the pairing snapshot directly so the
  // global badge updates immediately even if the first SSE frame
  // takes a moment to arrive.
  (async () => {
    try {
      applyPairing(await API.get("/api/pairing"));
    } catch (_) {
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
// Library Inspector (v1.3 operator-driven upscale)
// =============================================================

const inspectorState = {
  path: "", // current navigation path; "" = library root
  selection: null, // {kind: "folder"|"track", row}
};

function initLibraryInspector() {
  inspectorNavigate("");
  document.getElementById("inspector-breadcrumbs")
    .addEventListener("click", (e) => {
      const a = e.target.closest("a[data-path]");
      if (!a) return;
      e.preventDefault();
      inspectorNavigate(a.dataset.path);
    });
  document.getElementById("inspector-upscale-btn")
    .addEventListener("click", inspectorSubmitBatch);
  document.getElementById("inspector-delete-variants-btn")
    .addEventListener("click", inspectorDeleteVariants);
}

async function inspectorNavigate(path) {
  inspectorState.path = path;
  inspectorRenderBreadcrumbs(path);
  inspectorResetDrawer();
  document.getElementById("inspector-error").hidden = true;
  document.getElementById("inspector-current-heading").textContent =
    "Loading…";
  try {
    const res = await fetch(`/api/library/browse?path=${encodeURIComponent(path)}`);
    // Race guard: a slow response from an earlier navigation must
    // not overwrite the newer navigation's content. Compare against
    // the live `inspectorState.path` set synchronously at the top
    // of this call; subsequent navigations bump it before their
    // own fetch awaits. Per Gemini medium on PR #202.
    if (inspectorState.path !== path) {
      return;
    }
    if (!res.ok) {
      throw new Error(`browse: HTTP ${res.status}`);
    }
    const data = await res.json();
    if (inspectorState.path !== path) {
      return;
    }
    inspectorRender(data);
  } catch (err) {
    if (inspectorState.path !== path) {
      return;
    }
    document.getElementById("inspector-error").hidden = false;
    document.getElementById("inspector-error").textContent =
      `Couldn’t load this folder: ${err.message}`;
    document.getElementById("inspector-current-heading").textContent =
      pathLabel(path);
  }
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

function inspectorRender(data) {
  document.getElementById("inspector-current-heading").textContent =
    pathLabel(data.path);
  const body = document.getElementById("inspector-rows-body");
  body.innerHTML = "";
  const folders = data.folders || [];
  const tracks = data.tracks || [];
  if (folders.length === 0 && tracks.length === 0) {
    document.getElementById("inspector-rows-table").hidden = true;
    document.getElementById("inspector-empty").hidden = false;
    return;
  }
  document.getElementById("inspector-rows-table").hidden = false;
  document.getElementById("inspector-empty").hidden = true;
  for (const f of folders) {
    const tr = document.createElement("tr");
    tr.dataset.kind = "folder";
    tr.dataset.path = f.path;
    // Row split into three explicit affordances per the v1.3.1
    // UX review: (a) folder name + counts is the click-target
    // for "select to see upscale info" (the whole row before
    // the action cell); (b) an explicit "Inspect" button on
    // the right shows the action so operators don't have to
    // guess "tap empty space"; (c) the folder name is plain
    // text, not a blue link, so it doesn't compete visually
    // with the action button; navigation INTO the folder
    // happens via the chevron control alongside.
    tr.innerHTML = `
      <td class="folder-cell">
        <span class="folder-name">📁 ${escapeHTML(f.name)}</span>
      </td>
      <td class="num">${f.trackCount}</td>
      <td class="num">${f.upscaledCount}</td>
      <td class="num">${humanBytes(f.totalSizeBytes)}</td>
      <td class="folder-actions">
        <button type="button" class="btn folder-action-inspect"
          aria-label="Show upscale projection for ${escapeHTML(f.name)}">Inspect</button>
        <button type="button" class="btn folder-action-open"
          aria-label="Open ${escapeHTML(f.name)}">Open →</button>
      </td>
    `;
    tr.querySelector(".folder-action-inspect").addEventListener("click", (e) => {
      e.preventDefault();
      e.stopPropagation();
      inspectorSelectFolder(f);
    });
    tr.querySelector(".folder-action-open").addEventListener("click", (e) => {
      e.preventDefault();
      e.stopPropagation();
      inspectorNavigate(f.path);
    });
    // Row click anywhere else also selects (cheap discoverability
    // bonus — but the explicit buttons are the primary surface).
    tr.addEventListener("click", () => inspectorSelectFolder(f));
    body.appendChild(tr);
  }
  for (const t of tracks) {
    const tr = document.createElement("tr");
    tr.dataset.kind = "track";
    tr.dataset.path = t.path;
    const upscaled = t.isUpscaled ? "✓" : "";
    tr.innerHTML = `
      <td>🎵 ${escapeHTML(t.name)}</td>
      <td class="num">${formatTrackQuality(t)}</td>
      <td class="num">${upscaled}</td>
      <td class="num">${humanBytes(t.sizeBytes)}</td>
      <td class="folder-actions"></td>
    `;
    body.appendChild(tr);
  }
}

function inspectorSelectFolder(folder) {
  inspectorState.selection = { kind: "folder", row: folder };
  document.getElementById("inspector-drawer-title").textContent =
    folder.name;
  document.getElementById("inspector-drawer-hint").hidden = true;
  document.getElementById("inspector-drawer-content").hidden = false;
  document.getElementById("inspector-drawer-tracks").textContent =
    `${folder.trackCount} (${folder.upscaledCount} already upscaled)`;
  document.getElementById("inspector-drawer-covered").textContent =
    `${folder.upscaledCount}`;
  document.getElementById("inspector-drawer-source-size").textContent =
    humanBytes(folder.totalSizeBytes);
  document.getElementById("inspector-drawer-projected").textContent = "—";
  document.getElementById("inspector-drawer-free").textContent = "—";
  document.getElementById("inspector-drawer-required").textContent = "—";
  document.getElementById("inspector-drawer-warning").hidden = true;
  document.getElementById("inspector-drawer-unknown").hidden = true;
  document.getElementById("inspector-upscale-btn").disabled = true;
  // Delete-variants button: enabled only when this scope has at
  // least one already-upscaled track. Disabled-with-zero-count is
  // the right shape — there's literally nothing to delete on a
  // scope that's never been upscaled, and a click would surface as
  // a 0-deleted noop response that's just visual noise.
  document.getElementById("inspector-delete-variants-btn").disabled =
    !(folder.upscaledCount > 0);
  document.getElementById("inspector-submit-status").textContent = "";
  inspectorFetchProjection(folder.path);
}

function inspectorResetDrawer() {
  inspectorState.selection = null;
  document.getElementById("inspector-drawer-title").textContent =
    "Select a folder";
  document.getElementById("inspector-drawer-hint").hidden = false;
  document.getElementById("inspector-drawer-content").hidden = true;
}

async function inspectorFetchProjection(path) {
  // Race-guard: snapshot the path we were called with; bail out
  // on any branch below if the user has since selected a
  // different folder. Mirrors `inspectorNavigate`'s pattern.
  // Per CodeRabbit major on PR #205 round 2.
  const requested = path;
  const stillCurrent = () =>
    inspectorState.selection &&
    inspectorState.selection.kind === "folder" &&
    inspectorState.selection.row.path === requested;
  try {
    const res = await fetch(`/api/library/browse-projection?path=${encodeURIComponent(path)}`);
    if (!stillCurrent()) return;
    if (res.status === 503) {
      document.getElementById("inspector-drawer-warning").hidden = false;
      document.getElementById("inspector-drawer-warning").textContent =
        "Upscale feature is disabled on this bridge.";
      return;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = await res.json();
    if (!stillCurrent()) return;
    document.getElementById("inspector-drawer-projected").textContent =
      `${humanBytes(data.projectedSizeBytes)} (${data.projectedFiles} files)`;
    document.getElementById("inspector-drawer-free").textContent =
      humanBytes(data.availableBytes);
    document.getElementById("inspector-drawer-required").textContent =
      humanBytes(data.requiredBytesWithMargin);
    if (data.unknownFormatFiles > 0) {
      document.getElementById("inspector-drawer-unknown").hidden = false;
      document.getElementById("inspector-drawer-unknown").textContent =
        `${data.unknownFormatFiles} tracks here are in formats we can’t upscale (DSD, lossy, or unknown). They’ll be skipped.`;
    }
    if (data.wouldFit && data.projectedFiles > 0) {
      document.getElementById("inspector-upscale-btn").disabled = false;
    } else if (data.projectedFiles === 0) {
      // Differentiate three states for clearer operator UX
      // (per the screenshot review — original message
      // "every eligible track already has a variant"
      // misrepresents the unknown-format / no-eligible case).
      document.getElementById("inspector-drawer-warning").hidden = false;
      const total = data.alreadyCoveredFiles + data.unknownFormatFiles;
      let msg;
      if (data.alreadyCoveredFiles > 0 && data.unknownFormatFiles === 0) {
        msg = "All eligible tracks already have an upscaled variant.";
      } else if (data.alreadyCoveredFiles === 0 && data.unknownFormatFiles > 0) {
        msg = "No tracks here support upscaling (DSD or unknown source format).";
      } else if (data.alreadyCoveredFiles > 0 && data.unknownFormatFiles > 0) {
        msg = `${data.alreadyCoveredFiles} tracks already upscaled, ${data.unknownFormatFiles} can’t be upscaled — nothing eligible left.`;
      } else if (total === 0) {
        msg = "No tracks here.";
      } else {
        msg = "Nothing eligible to upscale.";
      }
      document.getElementById("inspector-drawer-warning").textContent = msg;
    } else {
      document.getElementById("inspector-drawer-warning").hidden = false;
      document.getElementById("inspector-drawer-warning").textContent =
        `Not enough free space: needs ${humanBytes(data.requiredBytesWithMargin)} (incl. 10% safety margin), only ${humanBytes(data.availableBytes)} available on the bridge data volume.`;
    }
  } catch (err) {
    if (!stillCurrent()) return;
    document.getElementById("inspector-drawer-warning").hidden = false;
    document.getElementById("inspector-drawer-warning").textContent =
      `Couldn’t fetch projection: ${err.message}`;
  }
}

async function inspectorSubmitBatch() {
  const sel = inspectorState.selection;
  if (!sel || sel.kind !== "folder") return;
  const btn = document.getElementById("inspector-upscale-btn");
  btn.disabled = true;
  const status = document.getElementById("inspector-submit-status");
  status.textContent = "Submitting…";
  try {
    const res = await fetch("/api/upscale/batch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: sel.row.path }),
    });
    if (res.status === 507) {
      const data = await res.json();
      status.textContent =
        `Refused: needs ${humanBytes(data.requiredBytes)}, only ${humanBytes(data.availableBytes)} available.`;
      btn.disabled = false;
      return;
    }
    if (res.status === 503) {
      // Bridge upscale feature disabled mid-flight (operator
      // toggled it off after the Inspector loaded). Surface a
      // clear message rather than the generic HTTP fall-through.
      // Per CodeRabbit minor on PR #205 round 2.
      status.textContent = "Upscale feature is disabled on this bridge.";
      btn.disabled = false;
      return;
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = await res.json();
    status.innerHTML =
      `Batch enrolled · <strong>${data.enqueuedCount}</strong> tracks queued ` +
      `(${data.alreadyCovered} already covered). ` +
      `<a href="/jobs">View jobs →</a>`;
  } catch (err) {
    status.textContent = `Couldn’t submit: ${err.message}`;
    btn.disabled = false;
  }
}

// inspectorDeleteVariants fires DELETE /api/upscale/variants?prefix=<scope>
// against the admin port. The handler forwards to api.RunVariantDelete
// which does the unlink + DB delete + SSE publish loop — paired iOS
// clients receive the same `upscale.deleted` event they'd get from a
// direct DELETE /v1/upscale/variants. Uses a native confirm() for the
// scope-bounded delete; the global "Clear all" path on the settings
// page has its own typed-phrase modal because the blast radius is wider.
//
// On success the inspector drawer's "Already upscaled" count is locally
// zeroed AND the underlying folder row's `upscaledCount` snapshot is
// updated, so subsequent selections (without navigation) reflect the
// post-delete state without waiting for the browse fetch to re-fire.
async function inspectorDeleteVariants() {
  const sel = inspectorState.selection;
  if (!sel || sel.kind !== "folder") return;
  const scope = sel.row.path || "";
  const scopeLabel = scope === "" ? "the library root" : scope;
  if (!confirm(
    `Delete every cached upscaled variant under “${scopeLabel}”?\n\n` +
    `This removes the FLAC sidecars on disk AND the matching DB rows.\n` +
    `Paired iOS devices will reconcile via the upscale.deleted SSE event.\n\n` +
    `Source files are untouched — re-upscale anytime.`
  )) return;

  const btn = document.getElementById("inspector-delete-variants-btn");
  btn.disabled = true;
  const status = document.getElementById("inspector-submit-status");
  status.textContent = "Deleting…";

  try {
    // Empty prefix is the library-root case → unscoped delete,
    // which requires `?confirm=true` per the shared parser's
    // safety gate. The native confirm() above is the operator's
    // explicit opt-in for that wider scope; relay it as the
    // ?confirm=true flag.
    let url;
    if (scope === "") {
      url = `/api/upscale/variants?confirm=true`;
    } else {
      url = `/api/upscale/variants?prefix=${encodeURIComponent(scope)}`;
    }
    const res = await fetch(url, {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
    });
    if (res.status === 503) {
      // Bridge upscale feature disabled mid-flight. Re-enable the
      // button so the operator can retry after toggling the
      // feature back on — matches `inspectorSubmitBatch`'s 503
      // handler. Without this, a single mid-session toggle-off
      // would force a navigation away + back to re-enable the
      // delete affordance (CodeRabbit Minor on PR #220).
      status.textContent = "Upscale feature is disabled on this bridge.";
      btn.disabled = false;
      return;
    }
    if (!res.ok) {
      const body = await res.text();
      throw new Error(body || `HTTP ${res.status}`);
    }
    const data = await res.json();
    status.innerHTML =
      `Deleted <strong>${data.deletedCount}</strong> variants · ` +
      `freed ${humanBytes(data.freedBytes ?? 0)}.`;
    // Re-fetch the authoritative folder state instead of deriving
    // a post-delete count locally. `data.deletedCount` is the
    // number of VARIANT ROWS removed; `folder.upscaledCount` is
    // the number of TRACKS that have at least one variant.
    // Subtracting variant-count from track-count is wrong-unit
    // arithmetic — multi-variant-per-track tracks (operator
    // generated several target rates over the lifetime of the
    // bridge) would silently underflow the displayed count.
    // CodeRabbit Major on PR #220 caught this. The re-navigation
    // also refreshes every other row in the folder list, so a
    // delete that affected sibling folders shows up immediately
    // (rare — prefix scope is typically the selected folder
    // itself, but possible if the selection is a parent and a
    // child has variants too).
    await inspectorNavigate(inspectorState.path);
  } catch (err) {
    status.textContent = `Couldn’t delete: ${err.message}`;
    btn.disabled = false;
  }
}

function pathLabel(path) {
  if (!path) return "Library root";
  return path;
}

function escapeHTML(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
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
// Jobs page (v1.3 upscale_batches history)
// =============================================================

function initJobs() {
  // setTimeout chain (not setInterval) so a slow response can't
  // build up overlapping in-flight requests when the bridge is
  // under load — the next poll is scheduled only AFTER the
  // current one resolves. Per CodeRabbit medium on PR #205
  // round 2.
  const tick = async () => {
    await jobsRefresh();
    setTimeout(tick, 5000);
  };
  tick();
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
      <td><span class="status status-${r.status}">${r.status}</span></td>
      <td><code>${escapeHTML(scopeLabel)}</code></td>
      <td>${target}</td>
      <td class="num">${r.processedFiles}</td>
      <td class="num">${r.failedFiles}</td>
      <td class="num">${r.totalFiles}</td>
      <td><time>${escapeHTML(updated)}</time></td>
      <td></td>
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

// --- boot ---

document.addEventListener("DOMContentLoaded", () => {
  const active = document.body.dataset.active;
  switch (active) {
    case "dashboard": initDashboard(); break;
    case "library": initLibrary(); break;
    case "library_inspector": initLibraryInspector(); break;
    case "jobs": initJobs(); break;
    case "devices": initDevices(); break;
    case "settings": initSettings(); break;
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
