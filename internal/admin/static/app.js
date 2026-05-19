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
      alert("Re-mint failed: " + (err?.message ?? "unknown error"));
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
  // selection-bar aggregator iterates O(M) over the map's values
  // — no document.querySelector calls per checkbox tick (Gemini
  // medium on PR #276: the prior implementation was O(M×N) where
  // M=selected and N=total tiles in the DOM). Selection bar
  // floats when map.size > 0. Cleared on every browse re-render.
  selectedPaths: new Map(),
  // SoX availability snapshot — seeded once at init() time from
  // the page root's data-sox-available attribute. Used by the
  // selection bar + per-tile menu to gate generate actions.
  soxAvailable: true,
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
  // Initial path comes from `?path=` query so bookmarks / refresh
  // land on the right folder. Falls back to library root.
  const params = new URLSearchParams(window.location.search);
  const initialPath = params.get("path") || "";
  // Replace (not push) the initial entry so the user's first Back
  // press doesn't land them on a redundant `?path=` history slot.
  history.replaceState({ path: initialPath, scrollY: 0 }, "",
    inspectorURLFor(initialPath));
  inspectorNavigate(initialPath, { skipHistory: true });

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

  // Current-folder ⓘ affordance (v1.4 followup): replaces the
  // toolbar Upscale + Delete buttons that wrapped inconsistently.
  // Same drawer flow as the per-row ⓘ on child folders — the
  // operator picks Upscale / Delete inside the drawer.
  document.getElementById("inspector-current-info")
    ?.addEventListener("click", inspectorOpenProjectionForCurrent);

  // Per-kind drawer buttons (event delegation — N=2 sections per
  // page, 2 buttons each; the new tile redesign replaces the
  // legacy single Upscale + Delete pair with one Generate + one
  // Delete per kind).
  for (const btn of document.querySelectorAll(".drawer-generate-btn")) {
    btn.addEventListener("click", inspectorSubmitBatch);
  }
  for (const btn of document.querySelectorAll(".drawer-delete-btn")) {
    btn.addEventListener("click", inspectorDeleteVariants);
  }
  document.getElementById("inspector-drawer-close")
    .addEventListener("click", () => inspectorCloseDrawer({ userInitiated: true }));

  // Two independent Load-more buttons (folders + tracks). The shared
  // inspectorLoadMore reads both cursors and the server returns
  // pages for whichever collection is still in flight; the
  // IntersectionObserver in inspectorRefreshLoadMoreSentinel also
  // attaches to these buttons for auto-fire.
  for (const btn of document.querySelectorAll(".load-more")) {
    btn.addEventListener("click", inspectorLoadMore);
  }

  // Tile checkbox change → multi-select state update + bar refresh.
  // Event delegation from the inspector content root so the listener
  // survives chunked appends without re-wiring per tile.
  const contentRoot = document.getElementById("inspector-content");
  if (contentRoot) {
    contentRoot.addEventListener("change", inspectorOnTileCheckboxChange);
  }

  // Selection-bar buttons.
  document.getElementById("selection-clear")
    ?.addEventListener("click", () => {
      // Untick every visible tile checkbox to keep DOM state in sync.
      for (const cb of document.querySelectorAll(
        '.inspector-tile input[type="checkbox"][data-path]')) {
        if (cb.checked) cb.checked = false;
        const tile = cb.closest(".inspector-tile");
        if (tile) delete tile.dataset.selected;
      }
      inspectorState.selectedPaths.clear();
      inspectorUpdateSelectionBar();
    });
  for (const btn of document.querySelectorAll(".selection-submit-btn")) {
    btn.addEventListener("click", inspectorSelectionSubmit);
  }

  // SoX availability snapshot (set once at mount). The CSS gate
  // (`[data-sox-available="false"] [data-needs-sox]`) handles
  // visual state; JS uses this for the selection bar's
  // gate-disabled check.
  const page = document.querySelector(".library-inspector");
  inspectorState.soxAvailable = page?.dataset.soxAvailable !== "false";

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
    const target = (ev.state && typeof ev.state.path === "string")
      ? ev.state.path
      : (new URLSearchParams(window.location.search).get("path") || "");
    inspectorNavigate(target, {
      skipHistory: true,
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
function inspectorURLFor(path) {
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
  // Same-path refresh: don't add a duplicate history entry. This
  // covers callers like inspectorDeleteVariants that re-navigate
  // to the current folder to refresh row data after mutation, AND
  // a user clicking the active breadcrumb crumb.
  if (!opts.skipHistory && history.state
    && history.state.path === path) {
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
  // Invalidate any in-flight chunked-render pump from a previous
  // navigation or load-more BEFORE the new fetch starts. Pre-fix
  // the generation was only bumped INSIDE inspectorAppendRows on
  // the new render — which means between the navigation start and
  // that bump (could be hundreds of ms on a slow browse fetch), a
  // stale load-more pump would keep appending rows from the old
  // folder under the new path. CodeRabbit Major late on PR #246.
  inspectorState.renderGeneration++;
  inspectorState.lastBrowseData = null;
  // Any navigation exits search mode — covers the case where the
  // operator clicked a search-result folder/track and we land in
  // browse mode without an explicit Exit. Without this reset the
  // load-more sentinel's `mode === "search"` short-circuit kept
  // pagination permanently disabled in the post-search folder.
  // CodeRabbit Major on PR #246 round-2.
  inspectorState.mode = "browse";
  // Clear the "user-closed" latch on every navigation so a fresh
  // folder auto-opens the projection drawer in inspectorRender.
  inspectorState.drawerClosedByUser = false;
  inspectorRenderBreadcrumbs(path);
  inspectorUpdateToolbarState(path);
  inspectorCloseDrawer(); // transient close — userInitiated defaults to false
  document.getElementById("inspector-error").hidden = true;
  document.getElementById("inspector-current-heading").textContent =
    "Loading…";
  document.title = `Library Inspector — ${path || "Root"}`;

  if (!opts.skipHistory) {
    history.pushState({ path, scrollY: 0 }, "", inspectorURLFor(path));
  }

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
    inspectorState.lastBrowseData = data;
    // Await inspectorRender → the chunked-render pump fully
    // populates the table body before we restore scroll.
    // Without this await, only the first 200-row chunk has
    // appended when scrollTo runs and the browser clamps a
    // deep scroll-target to the partial document height.
    // CodeRabbit Major on PR #246 round-2.
    await inspectorRender(data);
    if (inspectorState.path !== path) return;
    // Restore scroll after the table body is fully realized.
    const targetY = typeof opts.restoreScroll === "number"
      ? opts.restoreScroll
      : 0;
    requestAnimationFrame(() => window.scrollTo(0, targetY));
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

// inspectorOpenProjectionForCurrent is the toolbar handler that
// acts on the OPEN folder rather than a child row. Builds a
// synthetic folder object from the cached browse rollup (or the
// library root semantics when path === "") and routes through the
// same drawer code path the per-row ⓘ icon uses.
function inspectorOpenProjectionForCurrent() {
  const data = inspectorState.lastBrowseData;
  if (!data) return; // toolbar button is disabled in this state, defensive
  // For non-root folders the rollup we want is THIS folder's data
  // — which the browse response describes via aggregate counts
  // across its `folders` + `tracks` arrays. The projection
  // endpoint accepts the path as-is and walks server-side, so the
  // synthetic numbers here are only used to pre-populate the
  // drawer's "Tracks / Source size" rows before the projection
  // fetch lands. Root path is semantically valid; the projection
  // endpoint already handles empty path as "whole library."
  const rollup = inspectorBrowseRollup(data);
  inspectorSelectFolder({
    name: inspectorState.path || "Library root",
    path: inspectorState.path,
    trackCount: rollup.trackCount,
    upscaledCount: rollup.upscaledCount,
    optimizedCount: rollup.optimizedCount,
    totalSizeBytes: rollup.totalSizeBytes,
  });
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
  }
  return { trackCount, upscaledCount, optimizedCount, totalSizeBytes };
}

// inspectorCloseDrawer hides the projection drawer. `userInitiated`
// distinguishes:
//   - true (user clicked × or pressed Esc): persists `drawerClosedByUser`
//     so the drawer doesn't auto-reopen on the current navigation's
//     re-render. Cleared on the next navigation.
//   - false (transient close from inspectorNavigate / inspectorRender
//     setup): doesn't latch the flag, so the navigation's downstream
//     auto-open path still fires.
function inspectorCloseDrawer({ userInitiated = false } = {}) {
  inspectorState.selection = null;
  const drawer = document.getElementById("inspector-drawer");
  if (drawer) drawer.hidden = true;
  if (userInitiated) {
    inspectorState.drawerClosedByUser = true;
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

// Returns a Promise resolved once the chunked-render pump has
// fully populated the table body (CodeRabbit Major on PR #246
// round-2). The caller (inspectorNavigate) awaits this so scroll
// restoration runs against the FINAL document height, not the
// partial height after just the first 200-row chunk.
async function inspectorRender(data) {
  document.getElementById("inspector-current-heading").textContent =
    pathLabel(data.path);

  // Capture pagination metadata so Load-more (manual click or
  // IntersectionObserver auto-fire) can advance against the right
  // cursors. Empty cursors = exhausted on the server side.
  inspectorState.nextFolderCursor = data.nextFolderCursor || "";
  inspectorState.nextTrackCursor = data.nextTrackCursor || "";
  inspectorState.totalFolders = data.totalFolders || 0;
  inspectorState.totalTracks = data.totalTracks || 0;

  // Multi-select is per-navigation — clear on every browse render
  // so a stale selection from a prior folder doesn't bleed into
  // the new view.
  inspectorState.selectedPaths.clear();
  inspectorUpdateSelectionBar();

  const folders = data.folders || [];
  const tracks = data.tracks || [];
  const hasAnyTracks = (data.totalTracks || 0) > 0
    || inspectorBrowseRollup(data).trackCount > 0;
  const currentInfoBtn = document.getElementById("inspector-current-info");
  if (currentInfoBtn) currentInfoBtn.disabled = !hasAnyTracks;

  // Auto-open the projection drawer for the CURRENT folder whenever
  // it has tracks. The drawer can still be closed manually via its
  // × button; closing sets `drawerClosedByUser` so the current
  // navigation doesn't re-open on a same-path refresh.
  if (hasAnyTracks && !inspectorState.drawerClosedByUser) {
    inspectorOpenProjectionForCurrent();
  }

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

function buildFolderTile(f) {
  const tile = document.createElement("article");
  tile.className = "inspector-tile";
  tile.dataset.kind = "folder";
  tile.dataset.path = f.path;
  tile.setAttribute("role", "link");
  tile.tabIndex = 0;
  tile.setAttribute("aria-label", `Open folder ${f.name}`);

  const upMax = Math.max(1, f.trackCount || 0);
  const upVal = Math.min(f.upscaledCount || 0, upMax);
  const opMax = Math.max(1, f.trackCount || 0);
  const opVal = Math.min(f.optimizedCount || 0, opMax);

  // The data-* attrs on the checkbox carry the client-side
  // aggregation inputs the selection bar reads — no fetch needed
  // on toolbar updates.
  tile.innerHTML = `
    <header class="tile-header">
      <label class="tile-select" title="Select this folder">
        <input type="checkbox"
          data-path="${escapeHTML(f.path)}"
          data-track-count="${f.trackCount || 0}"
          data-upscaled-count="${f.upscaledCount || 0}"
          data-optimized-count="${f.optimizedCount || 0}" />
      </label>
      <span class="tile-icon" aria-hidden="true">📁</span>
      <h3 class="tile-name" title="${escapeHTML(f.name)}">${escapeHTML(f.name)}</h3>
      <button class="tile-menu-btn" type="button"
        popovertarget="menu-${escapeHTML(f.pathHash || "")}"
        aria-label="Actions for ${escapeHTML(f.name)}">⋯</button>
    </header>
    <dl class="tile-meta">
      <div><dt>Tracks</dt><dd>${f.trackCount || 0}</dd></div>
      <div><dt>Size</dt><dd>${humanBytes(f.totalSizeBytes || 0)}</dd></div>
    </dl>
    <div class="tile-coverage">
      <div class="coverage-row" data-kind="upscale">
        <span class="coverage-label">Upscaled</span>
        <progress value="${upVal}" max="${upMax}"></progress>
        <span class="coverage-count">${f.upscaledCount || 0} / ${f.trackCount || 0}</span>
      </div>
      <div class="coverage-row" data-kind="optimize">
        <span class="coverage-label">CarPlay-optimized</span>
        <progress value="${opVal}" max="${opMax}"></progress>
        <span class="coverage-count">${f.optimizedCount || 0} / ${f.trackCount || 0}</span>
      </div>
    </div>
  `;
  attachTileMenu(tile, f);

  // Tile click → navigate INTO the folder. Excludes clicks on the
  // checkbox label (selection toggle) and the kebab menu button.
  tile.addEventListener("click", (e) => {
    if (e.target.closest(".tile-select")) return;
    if (e.target.closest(".tile-menu-btn")) return;
    if (e.target.closest(".tile-menu-popover")) return;
    inspectorNavigate(f.path);
  });
  tile.addEventListener("keydown", (e) => {
    if (e.target.closest(".tile-menu-btn")) return;
    if (e.target.closest("input[type=checkbox]")) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      inspectorNavigate(f.path);
    }
  });
  return tile;
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
      // Reuse the folder-selection path (also fires the drawer
      // auto-open) for folder tiles. For track tiles, fall back
      // to projection for the CURRENT folder — track tiles don't
      // carry per-folder rollup fields (upscaledCount /
      // optimizedCount are undefined), so a direct
      // inspectorSelectFolder(item) would render misleading "—"
      // values everywhere. The current-folder projection covers
      // the parent the operator is already viewing, which is the
      // user-meaningful scope for a track's "View projection"
      // tap. Per CodeRabbit major on PR #276 round 3.
      if (item.upscaledCount !== undefined) {
        inspectorSelectFolder(item);
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
  try {
    const params = new URLSearchParams({ path });
    // Include each cursor param IF the collection isn't exhausted;
    // OMIT the param to signal exhausted to the server.
    if (inspectorState.nextFolderCursor) {
      params.set("afterFolder", inspectorState.nextFolderCursor);
    }
    if (inspectorState.nextTrackCursor) {
      params.set("afterTrack", inspectorState.nextTrackCursor);
    }
    const res = await fetch(`/api/library/browse?${params.toString()}`);
    if (inspectorState.path !== path || inspectorState.mode === "search") return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (inspectorState.path !== path || inspectorState.mode === "search") return;
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

function inspectorSelectFolder(folder) {
  inspectorState.selection = { kind: "folder", row: folder };
  document.getElementById("inspector-drawer").hidden = false;
  document.getElementById("inspector-drawer-title").textContent =
    folder.name || "Library root";
  document.getElementById("inspector-drawer-content").hidden = false;

  // Populate both kind sections with the initial folder-rollup
  // snapshot. The projection fetches below fill in projected size /
  // required-with-margin / per-kind eligibility.
  setDrawerKindInitial("upscale", folder, folder.upscaledCount || 0);
  setDrawerKindInitial("optimize", folder, folder.optimizedCount || 0);

  // SoX-missing chip + Generate-button disabled state per section.
  const soxMissing = !inspectorState.soxAvailable;
  for (const kind of ["upscale", "optimize"]) {
    const section = document.querySelector(`.drawer-kind[data-kind="${kind}"]`);
    if (!section) continue;
    const chip = section.querySelector(".drawer-kind-soxchip");
    if (chip) chip.hidden = !soxMissing;
    const genBtn = section.querySelector(".drawer-generate-btn");
    if (genBtn) genBtn.disabled = true; // re-enabled after projection if eligible
  }
  inspectorFetchProjectionAllKinds(folder.path);
}

// setDrawerKindInitial pre-fills the per-kind drawer section with
// the rollup snapshot available before the projection lands. Source
// size + initial covered count + Delete button visibility come from
// the folder row's data-* (instant); projected / free / required
// come from the per-kind projection fetch (200–500 ms latency).
function setDrawerKindInitial(kind, folder, coveredCount) {
  const section = document.querySelector(`.drawer-kind[data-kind="${kind}"]`);
  if (!section) return;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  section.querySelector(`.drawer-tracks[data-kind="${kind}"]`).textContent =
    `${folder.trackCount} (${coveredCount} already ${lbl})`;
  section.querySelector(`.drawer-covered[data-kind="${kind}"]`).textContent =
    String(coveredCount);
  section.querySelector(`.drawer-source-size[data-kind="${kind}"]`).textContent =
    humanBytes(folder.totalSizeBytes || 0);
  section.querySelector(`.drawer-projected[data-kind="${kind}"]`).textContent = "—";
  section.querySelector(`.drawer-free[data-kind="${kind}"]`).textContent = "—";
  section.querySelector(`.drawer-required[data-kind="${kind}"]`).textContent = "—";
  const warn = section.querySelector(`.drawer-warning[data-kind="${kind}"]`);
  const unknown = section.querySelector(`.drawer-unknown[data-kind="${kind}"]`);
  const status = section.querySelector(`.drawer-submit-status[data-kind="${kind}"]`);
  if (warn) warn.hidden = true;
  if (unknown) unknown.hidden = true;
  if (status) status.textContent = "";
  const delBtn = section.querySelector(".drawer-delete-btn");
  if (delBtn) {
    delBtn.hidden = coveredCount === 0;
    delBtn.disabled = coveredCount === 0;
  }
}

// inspectorFetchProjectionAllKinds fires two parallel projection
// fetches and populates each drawer section independently. Both
// kinds are visible together (locked design choice — dual stacked
// bars on the tile, dual stacked sections in the drawer) so the
// operator never has to switch tabs to compare. Each fetch carries
// its own race-guard against `inspectorState.selection`.
async function inspectorFetchProjectionAllKinds(path) {
  await Promise.all([
    inspectorFetchProjection(path, "upscale"),
    inspectorFetchProjection(path, "optimize"),
  ]);
}

async function inspectorFetchProjection(path, kind) {
  // Race-guard: snapshot the path we were called with; bail out
  // on any branch below if the user has since selected a
  // different folder. Mirrors `inspectorNavigate`'s pattern.
  // Per CodeRabbit major on PR #205 round 2.
  const requested = path;
  const stillCurrent = () =>
    inspectorState.selection?.kind === "folder" &&
    inspectorState.selection.row.path === requested;

  const sel = `[data-kind="${kind}"]`;
  const lbl = kind === "upscale" ? "upscaled" : "optimized";
  const warnEl = document.querySelector(`.drawer-warning${sel}`);
  const unknownEl = document.querySelector(`.drawer-unknown${sel}`);
  const projectedEl = document.querySelector(`.drawer-projected${sel}`);
  const freeEl = document.querySelector(`.drawer-free${sel}`);
  const requiredEl = document.querySelector(`.drawer-required${sel}`);
  const targetEl = document.querySelector(`.drawer-target${sel}`);
  const genBtn = document.querySelector(
    `.drawer-generate-btn[data-kind="${kind}"]`);

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
    // Optimize target stays the static "16-bit / 44.1k or 48k
    // (family-preserved)" from the template — server returns
    // targetRate=0 to signal varies per-track.

    if (data.unknownFormatFiles > 0 && unknownEl) {
      unknownEl.hidden = false;
      const skipReason = kind === "optimize"
        ? "DSD / lossy / already at target / unknown format"
        : "DSD / lossy / unknown format";
      unknownEl.textContent =
        `${data.unknownFormatFiles} tracks here are ${skipReason} — they'll be skipped.`;
    }

    // Generate button visibility / enabled state.
    if (genBtn) {
      const soxMissing = !inspectorState.soxAvailable;
      if (data.projectedFiles === 0) {
        // Surface the "nothing to do" state inline instead of via
        // a hidden button. Different phrasing per state matrix.
        if (warnEl) {
          warnEl.hidden = false;
          const total = data.alreadyCoveredFiles + data.unknownFormatFiles;
          let msg;
          if (data.alreadyCoveredFiles > 0 && data.unknownFormatFiles === 0) {
            msg = `All eligible tracks already have a ${lbl} variant.`;
          } else if (data.alreadyCoveredFiles === 0 && data.unknownFormatFiles > 0) {
            msg = kind === "optimize"
              ? "No tracks here are eligible for CarPlay-optimize (already at target, lossy, DSD, or unknown source format)."
              : "No tracks here support upscaling (DSD or unknown source format).";
          } else if (data.alreadyCoveredFiles > 0 && data.unknownFormatFiles > 0) {
            msg = `${data.alreadyCoveredFiles} tracks already ${lbl}, ${data.unknownFormatFiles} not eligible — nothing left to generate.`;
          } else if (total === 0) {
            msg = "No tracks here.";
          } else {
            msg = "Nothing eligible.";
          }
          warnEl.textContent = msg;
        }
        genBtn.disabled = true;
        genBtn.hidden = true;
      } else if (data.wouldFit) {
        genBtn.hidden = false;
        genBtn.disabled = soxMissing; // SoX gate is the final word
      } else {
        genBtn.hidden = false;
        genBtn.disabled = true;
        if (warnEl) {
          warnEl.hidden = false;
          warnEl.textContent =
            `Not enough free space: needs ${humanBytes(data.requiredBytesWithMargin)} (incl. 10% safety margin), only ${humanBytes(data.availableBytes)} available on the bridge data volume.`;
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

// inspectorSubmitBatch is the drawer-button-driven submit. Reads
// the kind from the button's `data-kind` attr (event delegation
// in initLibraryInspector wires it) and dispatches to
// inspectorSubmitBatchForKind with a single-path array.
async function inspectorSubmitBatch(ev) {
  const sel = inspectorState.selection;
  if (sel?.kind !== "folder") return;
  const btn = ev?.currentTarget;
  const kind = btn?.dataset.kind || "upscale";
  await inspectorSubmitBatchForKind(kind, [sel.row.path]);
}

// inspectorSubmitBatchForKind fires N parallel POSTs against
// /api/upscale/batch (one per path) with the same `kind`. Aggregates
// enqueued / alreadyCovered counts into a single status line on the
// kind's drawer section (when paths.length === 1) or the selection
// bar (when paths.length > 1). The selection-bar callers route
// through inspectorSelectionSubmit, which delegates here.
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

// inspectorSelectionToast — chokepoint for the floating selection-bar
// toast. Lazy-creates the span on first use; idempotent on update.
// Used by both the multi-path submit aggregator and the no-op
// preflight branch — the latter needs a visible surface for the
// tile-menu case where the drawer is closed (CodeRabbit major on
// PR #278: drawer status text is invisible on tile-menu submits).
function inspectorSelectionToast(msg) {
  const bar = document.getElementById("inspector-selection-bar");
  if (!bar) return;
  let toast = bar.querySelector(".selection-toast");
  if (!toast) {
    toast = document.createElement("span");
    toast.className = "selection-toast hint";
    bar.appendChild(toast);
  }
  if (msg && msg.indexOf("<") >= 0) {
    toast.innerHTML = msg;
  } else {
    toast.textContent = msg || "";
  }
}

async function inspectorSubmitBatchForKind(kind, paths) {
  if (!Array.isArray(paths) || paths.length === 0) return;
  if (kind !== "upscale" && kind !== "optimize") return;

  const single = paths.length === 1;
  const status = single
    ? document.querySelector(`.drawer-submit-status[data-kind="${kind}"]`)
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
  if (single && paths.length === 1) {
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
      // Same: don't block on probe failure. The POST will run, and
      // its result message will be the operator's feedback.
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
    } catch (err) {
      results.push({ path, error: err.message });
    }
  }

  const ok = results.filter(r => r.ok);
  const failed = results.filter(r => r.error);
  const enqueued = ok.reduce((n, r) => n + (r.data?.enqueuedCount || 0), 0);
  const covered = ok.reduce((n, r) => n + (r.data?.alreadyCovered || 0), 0);

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
    // Multi-path: render a single toast-style message on the
    // selection bar. Fold into a small floating notice so the
    // operator sees the aggregated counts.
    if (failed.length > 0 && ok.length === 0) {
      inspectorSelectionToast(`Couldn't submit any: ${failed[0].error}`);
    } else if (failed.length > 0) {
      inspectorSelectionToast(
        `${enqueued} tracks queued across ${ok.length} folders · ${failed.length} folders failed`);
    } else {
      inspectorSelectionToast(
        `Batch enrolled · <strong>${enqueued}</strong> tracks queued across ${ok.length} folders. ` +
        `<a href="/jobs">View jobs →</a>`);
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
  return { ok: ok.length, failed: failed.length, enqueued };
}

// inspectorDeleteVariants is the drawer-button-driven delete.
// Reads kind from the button's `data-kind` attr (event delegation
// in initLibraryInspector wires it). Dispatches to
// inspectorDeleteVariantsForKind.
async function inspectorDeleteVariants(ev) {
  const sel = inspectorState.selection;
  if (sel?.kind !== "folder") return;
  const btn = ev?.currentTarget;
  const kind = btn?.dataset.kind || "upscale";
  await inspectorDeleteVariantsForKind(kind, sel.row.path || "");
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

  const btn = document.querySelector(`.drawer-delete-btn[data-kind="${kind}"]`);
  if (btn) btn.disabled = true;
  const status = document.querySelector(`.drawer-submit-status[data-kind="${kind}"]`);
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

// ===== Multi-select bar + tile-checkbox handling =====

// inspectorOnTileCheckboxChange is the delegated change handler
// for tile checkboxes. Adds/removes the tile's path from the
// selection set and refreshes the selection bar's aggregated
// counts client-side.
function inspectorOnTileCheckboxChange(ev) {
  const cb = ev.target.closest('input[type="checkbox"][data-path]');
  if (!cb) return;
  const tile = cb.closest(".inspector-tile");
  const path = cb.dataset.path;
  if (cb.checked) {
    // Capture the rollup snapshot at toggle time so the selection-
    // bar aggregator doesn't have to re-find the checkbox in the
    // DOM on every tick. Numeric coercion with `|| 0` defends
    // against any data-* attr that wasn't populated by the Go
    // template (defensive — server side fills all three fields).
    inspectorState.selectedPaths.set(path, {
      trackCount: Number.parseInt(cb.dataset.trackCount, 10) || 0,
      upscaledCount: Number.parseInt(cb.dataset.upscaledCount, 10) || 0,
      optimizedCount: Number.parseInt(cb.dataset.optimizedCount, 10) || 0,
    });
    if (tile) tile.dataset.selected = "true";
  } else {
    inspectorState.selectedPaths.delete(path);
    if (tile) delete tile.dataset.selected;
  }
  inspectorUpdateSelectionBar();
}

// inspectorUpdateSelectionBar iterates the cached rollup snapshots
// in `inspectorState.selectedPaths` and computes per-kind gap
// numbers. O(M) over selected count (M typically <50) with NO DOM
// queries — Gemini medium on PR #276 caught the prior O(M×N)
// `document.querySelector` per tick.
function inspectorUpdateSelectionBar() {
  const bar = document.getElementById("inspector-selection-bar");
  if (!bar) return;
  const sel = inspectorState.selectedPaths;
  if (sel.size === 0) {
    bar.hidden = true;
    return;
  }
  bar.hidden = false;
  let upscaleGap = 0;
  let optimizeGap = 0;
  for (const snap of sel.values()) {
    upscaleGap += Math.max(0, snap.trackCount - snap.upscaledCount);
    optimizeGap += Math.max(0, snap.trackCount - snap.optimizedCount);
  }
  document.getElementById("selection-count").textContent =
    `${sel.size} selected`;
  document.getElementById("selection-upscale-gap").textContent = String(upscaleGap);
  document.getElementById("selection-optimize-gap").textContent = String(optimizeGap);
  const soxMissing = !inspectorState.soxAvailable;
  for (const btn of bar.querySelectorAll(".selection-submit-btn")) {
    const kind = btn.dataset.kind;
    const gap = kind === "upscale" ? upscaleGap : optimizeGap;
    btn.disabled = soxMissing || gap === 0;
  }
}

// inspectorSelectionSubmit fires the multi-select bar's per-kind
// Submit button. Reads the selected paths and fans out N parallel
// POSTs via inspectorSubmitBatchForKind. Clears the selection on
// success.
async function inspectorSelectionSubmit(ev) {
  const btn = ev?.currentTarget;
  const kind = btn?.dataset.kind || "upscale";
  // `Array.from(map)` would yield [key, value] tuples; we want the
  // keys (paths) only. `map.keys()` is iterable; spread captures
  // them in insertion order.
  const paths = Array.from(inspectorState.selectedPaths.keys());
  if (paths.length === 0) return;
  btn.disabled = true;
  try {
    // Clear the selection ONLY when every submit landed cleanly.
    // On any partial / total failure, preserve the selection so
    // the operator can retry the failed folders without rebuilding
    // the entire selection from scratch (could be 20+ folders on
    // a deep multi-album batch). Per CodeRabbit major on PR #276.
    // The toast on the selection bar (rendered by
    // inspectorSubmitBatchForKind) carries the per-folder failure
    // detail so the operator knows what didn't enqueue.
    const result = await inspectorSubmitBatchForKind(kind, paths);
    if (result?.failed === 0) {
      inspectorState.selectedPaths.clear();
      // Mirror the DOM uncheck — keep checkbox state in sync with
      // the cleared internal map. Same shape as the Clear button
      // handler in initLibraryInspector.
      for (const cb of document.querySelectorAll(
        '.inspector-tile input[type="checkbox"][data-path]:checked')) {
        cb.checked = false;
        const tile = cb.closest(".inspector-tile");
        if (tile) delete tile.dataset.selected;
      }
      inspectorUpdateSelectionBar();
    }
  } finally {
    if (btn) btn.disabled = false;
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
