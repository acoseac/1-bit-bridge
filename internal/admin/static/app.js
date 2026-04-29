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

async function errorFromResponse(r) {
  try {
    const j = await r.json();
    return new Error(j.message || j.error || `${r.status} ${r.statusText}`);
  } catch {
    return new Error(`${r.status} ${r.statusText}`);
  }
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

  // Backups panel — list current snapshots + "Snapshot now" button.
  // The list is read on first tick; the button is opt-in for an
  // on-demand snapshot. The download/export button is intentionally
  // missing — backups contain the TLS private key and token hashes,
  // and a one-click web download would be a credential extraction
  // surface. Operators move snapshots offsite with scp/rsync.
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
  refreshCertInfo();

  // Live-refresh the top-line numbers every 3 s.
  const tick = async () => {
    try {
      const s = await API.get("/api/stats");
      setText("tracks-indexed", s.tracksIndexed);
      setText("device-count", s.deviceCount);
      const scanStatus = document.getElementById("scan-status");
      if (scanStatus) {
        scanStatus.innerHTML = s.isScanning
          ? `<span class="badge running">scanning</span><span>· ${s.scanProgress} tracks so far</span>`
          : `<span class="badge idle">idle</span>`;
      }
      // Refresh the update tile from the cached status — cheap, no
      // GitHub call. The "Check now" button is the only path that
      // forces a fresh poll.
      try {
        const u = await API.get("/api/updates");
        renderUpdateTile(u);
      } catch {
        // Updater not configured (older bridge, test harness): leave
        // the tile at its server-rendered first-paint state.
      }
    } catch (e) {
      // Admin listener went away — stop polling.
      clearInterval(handle);
    }
  };
  const handle = setInterval(tick, 3000);
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

// refreshCertInfo populates the dashboard's "Expires" line under the
// TLS fingerprint panel with the live cert's expiry. ≤7 days is
// rendered red, ≤30 days yellow, otherwise the plain count. Errors
// degrade silently — the panel just shows the placeholder dashes.
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

// bindInstallButton attaches the click handler to the Install &
// restart button. No-op when btn is null (server didn't render the
// button at first paint, e.g. the platform doesn't support install
// or no update was known). The renderUpdateTile path uses the same
// helper when it dynamically materialises the button mid-session.
function bindInstallButton(btn) {
  if (!btn) return;
  btn.addEventListener("click", async () => {
    if (!confirm("Install the new bridge release and restart?\n\nActive iOS downloads will be interrupted and will need to be retried.")) return;
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
  const oldText = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Installing…";
  try {
    const path = force ? "/api/updates/install?force=1" : "/api/updates/install";
    await API.post(path);
    btn.textContent = "Restarting…";
    // Fire restart and don't await — the server tears the listener
    // down before we can read the response body anyway. The page
    // reload below races the restart's port-rebind; 2.5 s is the
    // empirical sweet-spot for launchd respawn on macOS.
    fetch("/api/restart", {
      method: "POST",
      headers: { "content-type": "application/json" },
    }).catch(() => {});
    setTimeout(() => window.location.reload(), 2500);
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
      installBtn = document.createElement("button");
      installBtn.type = "button";
      installBtn.id = "update-install";
      installBtn.className = "btn btn-primary";
      installBtn.textContent = "Install & restart";
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
  } else {
    status.innerHTML = `<span class="badge idle">checking…</span>`;
  }

  if (lastCheck && u && u.lastCheck) {
    lastCheck.textContent = formatTimeAgo(new Date(u.lastCheck));
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

// --- devices ---

// renderEndpoints fetches /api/endpoints and paints the
// "Reachable endpoints" panel on the devices page. The bridge
// recomputes the list per-call from net.Interfaces() so a Tailscale
// tunnel coming up mid-session reflects on the next refresh; we
// poll every 30s to catch interface changes without forcing the
// operator to reload the page.
async function renderEndpoints() {
  const list = document.getElementById("endpoints-list");
  if (!list) return;
  let entries;
  try {
    entries = await API.get("/api/endpoints");
  } catch (e) {
    list.innerHTML = `<li class="endpoints-empty"><em>Couldn't load endpoints: ${escapeHTML(e.message)}</em></li>`;
    return;
  }
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

function initDevices() {
  const modal = document.getElementById("pair-modal");
  const openBtn = document.getElementById("pair-open");
  const form = document.getElementById("pair-form");
  const stepForm = document.getElementById("pair-step-form");
  const stepResult = document.getElementById("pair-step-result");

  // Endpoints panel — first paint + 30s poll. The interval handle
  // is intentionally not stored; the page lifecycle is tied to a
  // full reload on navigation, so the interval dies with the page.
  renderEndpoints();
  setInterval(renderEndpoints, 30_000);

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

function initSettings() {
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

  restartBtn?.addEventListener("click", async () => {
    if (!confirm("Restart the bridge now? The page will become unreachable until the service manager relaunches it.")) return;
    try {
      await API.post("/api/restart");
      showMsg(msg, "warn", "Restart signalled. Reload the page in a few seconds.");
    } catch (err) {
      // After the server exits, fetch rejects — that's expected.
      showMsg(msg, "warn", "Restart signalled (server went away).");
    }
  });
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
//   - Sort so reorder-only edits don't fire the dialog.
function normaliseCustomEndpointsText(s) {
  return String(s)
    .replace(/,/g, "\n")
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "")
    .sort()
    .join("\n");
}

// --- boot ---

document.addEventListener("DOMContentLoaded", () => {
  const active = document.body.dataset.active;
  switch (active) {
    case "dashboard": initDashboard(); break;
    case "library": initLibrary(); break;
    case "devices": initDevices(); break;
    case "settings": initSettings(); break;
  }
});
