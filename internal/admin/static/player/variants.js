// The variant panel: what an album or artist HAS in the way of cached
// hi-res and CarPlay copies, and the controls that change it.
//
// Coverage bars read against an ELIGIBLE denominator, not a track
// count. An album of sixteen CD-quality tracks is already at the
// CarPlay floor, so "0 / 0 — nothing to do" is the truth and
// "0 / 16" would be an accusation. The exempt remainder is a muted
// footnote for the same reason.

import { el, clear, setDisabled } from "./ui.js";
import { bytes } from "./format.js";
import { generateVariants, deleteVariants, isAborted } from "./api.js";

// Button labels are SPELLED OUT per kind rather than derived from the
// chip label. Deriving them meant lower-casing a proper noun, and
// "Generate carplay" is not a thing anyone calls it.
const KINDS = [
  {
    key: "upscale", title: "Hi-res upscale", action: "Generate hi-res",
    blurb: "Higher-rate copies for a capable DAC.",
  },
  {
    key: "optimize", title: "CarPlay-optimized", action: "Generate CarPlay",
    blurb: "16-bit copies for head units and cellular streaming.",
  },
];

// The CarPlay switch as a tray row. Two trays offer it: the panel-wide one
// when variant generation is off, and the CarPlay kind's own when only this
// switch is. One row, so the two cannot describe it differently.
const OPTIMIZE_SWITCH_ROW = {
  field: "optimizeEnabled", type: "switch", label: "CarPlay-optimized variants",
  hint: "16-bit downsamples for head units and cellular streaming. " +
    "Only active while PCM upscaling is on — they share a worker pool.",
};

/**
 * Build the panel.
 *
 * @param {object} summary - the response's `variants` block; absent on a
 *   bridge too old to send one, in which case nothing renders at all.
 * @param {object} scope   - `{albumIds}` or `{artistId}`, passed through
 *   to the endpoints verbatim.
 * @param {function} onChanged - called after a successful mutation so the
 *   caller can re-fetch. The panel deliberately does NOT re-fetch itself:
 *   generation is asynchronous, so the numbers that matter arrive later,
 *   from the live refresh rather than from the response to the click.
 *   Also what a tray save falls back to, when the panel was given no
 *   `refresh` to redraw itself with.
 * @param {object} [opts]
 * @param {boolean} [opts.plain=false] - drop the heading and the card
 *   chrome, for a container that already frames and labels the panel —
 *   i.e. a tab. The folder view, where this is one section among
 *   several on an unframed page, keeps both.
 * @param {function} [opts.refresh] - answers a fresh `variants` block. A
 *   tray save that changes what the panel draws (the panel's own switches,
 *   see paint) redraws the panel IN PLACE from it: the gears and their
 *   trays are built once per panel and stay in the document, so the tray's
 *   "Saved." and the reader's focus survive, as the Smart mixes page's do
 *   (drawMixes). Until 2026-09-29 the save called onChanged, which re-runs
 *   the whole route, and that took the tray, its "Saved." and the focus
 *   (moved to the page title) with it (backlog B68). Absent, a tray save
 *   calls onChanged, as before.
 * @param {function} [opts.alive] - whether the route this panel was drawn for
 *   is still the current one. A redraw whose answer lands after the reader
 *   has moved on paints nothing.
 */
export function variantPanel(summary, scope, onChanged, { plain = false, refresh = null, alive = null } = {}) {
  if (!summary) return null;

  const panel = {
    scope, onChanged, refresh, alive,
    summary,
    // The newest redraw: an answer that is not the newest paints nothing, so
    // two saves in quick succession end on the later one's state whichever
    // answer lands last.
    seq: 0,
    root: el("section", { class: plain ? "variants variants-plain" : "variants" }),
    head: plain ? null : el("h2", { class: "variants-head", text: "Variants" }),
    totals: el("p", { class: "muted small" }),
  };
  // The block that stops both kinds ("switched off for this bridge", sox
  // missing) and the CarPlay kind's own note are the same shape: a note, and
  // the gear that opens the switch behind it. The panel-wide one is offered
  // only for a SETTING; sox missing is a package to install on the host, and
  // a switch that cannot fix it would be the wrong offer.
  panel.gate = switchNote(() => window.BridgeFeatureTray?.build({
    title: "Variant generation",
    blurb: "Cached hi-res and CarPlay-optimized copies, generated offline by " +
      "sox. Nothing is transcoded on the fly and the originals are untouched.",
    rows: [
      { field: "upscaleEnabled", type: "switch", label: "PCM upscaling" },
      OPTIMIZE_SWITCH_ROW,
    ],
    link: { href: "/settings?tab=audio", text: "All audio settings →" },
    onSaved: (field) => {
      // The upscaling switch decides what the whole panel draws. The CarPlay
      // one decides the CarPlay row alone, and only once generation is on:
      // while it is off nothing here depends on it, and a redraw would
      // repaint the same panel. Generate does not redraw either: its work
      // has only been queued. Both are read from the panel as it is NOW: this tray
      // stays once built, so it can answer for the CarPlay switch after
      // generation has come on.
      if (field === "upscaleEnabled" || (field === "optimizeEnabled" && panel.summary.enabled)) {
        redraw(panel);
      }
    },
  }));
  panel.kinds = KINDS.map((kind) => kindRow(kind, panel));
  paint(panel, summary);
  return panel.root;
}

/**
 * Put a summary on the panel: update what is drawn in place, and reconcile
 * which nodes are in the document.
 *
 * Every node the panel owns is built once, so a paint changes text, disabled
 * states and attributes on the nodes already there, and `reconcile` adds and
 * removes only the ones a state does or does not need. A node a paint leaves
 * where it was is not detached, and detaching a node takes the focus with it:
 * the gears and their trays (with the switch the reader just toggled, and its
 * "Saved.") are among them.
 */
function paint(panel, summary) {
  panel.summary = summary;
  const totals = [];
  if (summary.sourceBytes) totals.push(`${bytes(summary.sourceBytes)} of originals`);
  if (summary.variantBytes) totals.push(`${bytes(summary.variantBytes)} of variants`);
  panel.totals.textContent = totals.join(" · ");

  const blocked = blockedReason(summary);
  // No "→ Settings" in the copy: the gear beside this line IS the fix now, and
  // a sentence sending the reader elsewhere would compete with the control
  // next to it. The gear is the exact "go to Settings → Audio" round trip the
  // trays exist to remove, on a panel showing coverage bars the reader cannot
  // act on.
  const gate = panel.gate.nodes(blocked, !summary.enabled);
  const nodes = [];
  if (panel.head) nodes.push(panel.head);
  if (totals.length) nodes.push(panel.totals);
  nodes.push(...gate);
  for (const row of panel.kinds) {
    // A block that stops both kinds is said once, above them. One that
    // stops a single kind is said in that kind's row, beside the button it
    // disables.
    const off = blocked ? "" : kindOffReason(row.kind.key, summary);
    row.update(summary[row.kind.key], !blocked && !off, off);
    nodes.push(row.node);
  }
  reconcile(panel.root, nodes);
}

/**
 * Make `parent`'s children exactly `desired`, in order, moving as little as
 * possible: what is not wanted goes, what is wanted and missing is inserted
 * before its successor, and a node that is already in place is never touched.
 * The relative order of the nodes that persist never changes, which is what
 * makes "never touched" true for them.
 */
function reconcile(parent, desired) {
  const wanted = new Set(desired);
  // A copy, because `children` is live and removing while walking it skips
  // the node after each one removed.
  const strangers = [...parent.children].filter((child) => !wanted.has(child));
  for (const child of strangers) child.remove();
  desired.forEach((node, i) => {
    const at = parent.children[i];
    if (at !== node) parent.insertBefore(node, at || null);
  });
}

/**
 * Redraw the panel after a tray saved a switch that changes what it draws.
 *
 * In place from a fresh summary when the panel was given a way to fetch one,
 * and through onChanged (the route's own re-render) when it was not, or when
 * the newest fetch fails: the route fetches again and says what went wrong in
 * its own error state. A redraw is refused once the route has moved on, and an
 * answer or a failure that a newer redraw has overtaken does nothing.
 */
async function redraw(panel) {
  if (!panel.refresh) {
    if (panel.onChanged) panel.onChanged();
    return;
  }
  const seq = ++panel.seq;
  const gone = () => typeof panel.alive === "function" && !panel.alive();
  let next;
  try {
    next = await panel.refresh();
  } catch (e) {
    // A failure that a newer redraw has overtaken is that redraw's to answer:
    // handing over to the route here would re-render it away under the
    // newer one, and take the tray and the focus with it.
    if (isAborted(e) || gone() || seq !== panel.seq) return;
    if (panel.onChanged) panel.onChanged();
    return;
  }
  // No summary in the answer (the bridge logged and dropped it): keep what is
  // drawn rather than blank a panel the reader is using.
  if (seq !== panel.seq || gone() || !next) return;
  paint(panel, next);
}

/**
 * A note that a switch is off, with the gear beside it that opens that
 * switch: the panel-wide note and the CarPlay kind's own are this one shape.
 *
 * The gear and its tray are built once, the first time a note is offered one
 * (`offerGear`) and the app published a tray, and then they STAY: they are
 * where the reader's focus is after a save, and a redraw that took them away
 * would take it too. The note's text follows the state, and is left out while
 * there is nothing to say. Without a tray (app.js did not publish one) a
 * setting's note is followed by a link to Settings, the fallback ONLY: beside
 * a gear it would duplicate the tray's own footer. (CodeRabbit on PR #763.)
 *
 * `nodes` returns what belongs in the document now, in order.
 */
function switchNote(buildTray) {
  const note = el("p", { class: "variants-blocked small" });
  const row = el("div", { class: "variants-blocked-row" });
  const link = el("p", { class: "small" },
    el("a", { attrs: { href: "/settings?tab=audio" }, text: "Audio settings →" }));
  let tray = null;
  return {
    nodes(reason, offerGear) {
      if (!tray && offerGear) tray = buildTray() || null;
      note.textContent = reason;
      if (tray) {
        reconcile(row, reason ? [note, tray.button] : [tray.button]);
        return [row, tray.tray];
      }
      if (!reason) return [];
      return offerGear ? [note, link] : [note];
    },
  };
}

/**
 * Why the buttons are disabled, or "" when they are not.
 *
 * The two reasons are kept apart because they have different fixes: one
 * is a setting on this console, the other is a package to install on
 * the host. Collapsing them into "unavailable" tells an operator
 * nothing they can act on.
 */
function blockedReason(summary) {
  if (!summary.enabled) {
    return "Variant generation is switched off for this bridge.";
  }
  if (!summary.soxAvailable) {
    // soxAvailable is the gate's sox half (Server.soxUsable), so a sox
    // without FLAC lands here too, and the note is the one the Jobs card
    // gives for the same verdict.
    return "sox is not installed on the bridge host, or has no FLAC support, so no variants can be generated.";
  }
  return "";
}

/**
 * Why one kind's Generate is off while the panel is otherwise live, or "".
 *
 * Only the CarPlay kind has a switch of its own (upscale.optimizeEnabled),
 * and POST /api/upscale/batch refuses the kind while it is off.
 * `summary.optimizeActive` is that handler's own predicate, so the button is
 * off exactly where a click would be refused. Until 2026-09-28 the summary
 * did not carry it, and the button stayed live and answered with the 503.
 *
 * `=== false`, not a falsy test: an unknown must leave the button live and
 * let the endpoint answer, the folder view's rule for the feature state its
 * endpoint does not carry. A disabled button saying "switched off" about a
 * switch that is on would be the one outcome that lies.
 */
function kindOffReason(key, summary) {
  if (key === "optimize" && summary.optimizeActive === false) {
    return "CarPlay-optimized variants are switched off for this bridge.";
  }
  return "";
}

/**
 * One kind's row: built once, then updated by `update` as summaries arrive.
 *
 * A save of the CarPlay kind's switch redraws the panel, so the note, and the
 * disabled Generate, follow the switch; without it the row said "switched off"
 * beside the tray's "Saved." until the next render (CodeRabbit on #1068). The
 * row, its gear and its tray stay, for the reason `switchNote` gives.
 */
function kindRow(kind, panel) {
  // Title, ratio and both buttons share ONE line, with the bar under
  // it. Stacked — title / bar / note / buttons / status — each kind was
  // five rows tall and the pair filled a screen for two numbers and two
  // controls. Nothing was dropped; it is the same content on half the
  // lines.
  const status = el("p", { class: "small variant-status", attrs: { "aria-live": "polite" } });
  const ratio = el("span", { class: "variant-kind-ratio" });
  const fill = el("div", { class: "variant-bar-fill" });
  const bar = el("div", {
    class: "variant-bar",
    attrs: { role: "progressbar", "aria-label": `${kind.title} coverage`, "aria-valuemin": "0" },
  }, fill);
  const note = el("p", { class: "muted small variant-kind-note" });
  const stale = el("p", { class: "small variant-kind-stale" });
  const offSlot = el("div", { class: "variant-kind-off" });
  const off = switchNote(() => kind.key === "optimize" ? window.BridgeFeatureTray?.build({
    title: "CarPlay-optimized variants",
    blurb: "16-bit copies for head units and cellular streaming, generated offline " +
      "by sox. The originals are untouched.",
    rows: [OPTIMIZE_SWITCH_ROW],
    link: { href: "/settings?tab=audio", text: "All audio settings →" },
    onSaved: () => redraw(panel),
  }) : null);

  const row = { kind, cov: { covered: 0, eligible: 0, exempt: 0, stale: 0 } };
  const gen = el("button", { class: "btn btn-primary", text: kind.action });
  // No refresh callback on generate, deliberately. The work has only
  // been QUEUED when the response arrives, so re-rendering here would
  // repaint the same numbers and destroy the status line the operator
  // just read. The live refresh brings the real result when it lands.
  gen.addEventListener("click", () =>
    run(gen, status, () => generateVariants(panel.scope, kind.key),
      (r) => r.enqueuedCount > 0
        ? `Queued ${r.enqueuedCount}. They appear as they finish.`
        : "Nothing to queue — everything eligible is already covered."));
  const del = el("button", { class: "btn", text: "Delete" });
  del.addEventListener("click", () => {
    const c = row.cov;
    if (!confirm(
      `Delete ${c.covered} ${kind.title} variant${c.covered === 1 ? "" : "s"}?\n\n` +
      `The cached copies are removed from disk. Your original files are not touched.`)) return;
    run(del, status, () => deleteVariants(panel.scope, kind.key),
      (r) => `Deleted ${r.deletedCount}, freed ${bytes(r.freedBytes) || "0 B"}.`,
      panel.onChanged);
  });
  const head = el("div", { class: "variant-kind-head" },
    el("span", { class: "variant-kind-title", text: kind.title }),
    ratio,
    el("div", { class: "variant-actions" }, gen, del));
  row.node = el("div", { class: "variant-kind" });

  row.update = (cov, actionable, offReason) => {
    const c = cov || { covered: 0, eligible: 0, exempt: 0, stale: 0 };
    row.cov = c;
    ratio.textContent = `${c.covered} / ${c.eligible}`;
    const pct = c.eligible > 0 ? Math.round((c.covered / c.eligible) * 100) : 0;
    bar.setAttribute("aria-valuenow", String(c.covered));
    bar.setAttribute("aria-valuemax", String(c.eligible));
    fill.style.width = `${pct}%`;

    // Nothing eligible and nothing missing means there is no work to
    // request. A live button that answers "enqueued 0" reads as a
    // failure; a disabled one with the note beside it reads as done.
    const missing = Math.max(0, c.eligible - c.covered);
    setDisabled(gen, !actionable || missing === 0);
    // Delete stays available on a bridge with no sox: reclaiming disk is
    // exactly what an operator without a toolchain still needs to do.
    //
    // The exception is the WHOLE-LIBRARY folder scope, where "delete"
    // means every variant the bridge has ever made. That is a different
    // magnitude of action and it keeps its own typed-confirmation control
    // on the Roots page; a single browser confirm() beside a coverage bar
    // is not the guard it deserves.
    const wholeLibrary = panel.scope.path === "";
    setDisabled(del, c.covered === 0 || wholeLibrary);
    if (wholeLibrary && c.covered > 0) {
      del.title = "Clearing every variant is done from the Roots page.";
    } else {
      del.removeAttribute("title");
    }

    reconcile(offSlot, off.nodes(offReason, offReason !== ""));

    // One note, not a list. An empty denominator and a non-zero exempt
    // count are the SAME fact told twice — "2 need nothing · nothing here
    // can take this" reads like two problems.
    let text = kind.blurb;
    if (c.eligible === 0 && c.exempt > 0) {
      text = "Nothing here needs this.";
    } else if (c.exempt > 0) {
      text = `${c.exempt} of these need nothing.`;
    }
    note.textContent = text;

    // A stale copy exists and will not be served, and the bar counts it
    // as covered — which is the truth about what Generate will do, since
    // the batch skips any track that already has a variant of the kind.
    // So it needs saying out loud, WITH the remedy: Delete then Generate
    // is the only route back to a current copy.
    stale.textContent = staleNote(c.stale);

    reconcile(row.node, [
      head, bar,
      ...(offSlot.children.length ? [offSlot] : []),
      note,
      ...(c.stale > 0 ? [stale] : []),
      status,
    ]);
  };
  return row;
}

/** What a kind says about copies whose source changed after they were made, or "". */
function staleNote(count) {
  if (count === 0) return "";
  if (count === 1) {
    return "1 copy is out of date — its source changed after it was made. " +
      "Delete, then generate again.";
  }
  return `${count} copies are out of date — their sources changed after they ` +
    "were made. Delete, then generate again.";
}

/**
 * Drive one mutation: disable, call, report, invite a refresh.
 *
 * The button is re-enabled on failure only. On success the caller
 * re-renders from fresh data, which replaces this node — re-enabling it
 * first would be a frame of the old state. It takes focus back when it is
 * re-enabled (setDisabled), so a failed request does not cost a keyboard
 * user their place.
 */
async function run(button, status, call, describe, onChanged) {
  setDisabled(button, true);
  clear(status);
  status.textContent = "Working…";
  status.classList.remove("variant-status-error");
  try {
    const res = await call();
    status.textContent = describe(res);
    if (onChanged) onChanged();
  } catch (e) {
    status.textContent = e?.message || "Request failed.";
    status.classList.add("variant-status-error");
    setDisabled(button, false);
  }
}

// ---- Live refresh ----
//
// Generation is an asynchronous queue, so a panel rendered once shows a
// snapshot of a moving target. app.js already runs the console's single
// SSE stream and re-broadcasts the pool's progress as `bridge:upscale`;
// this is the player's side of that.
//
// The registry lives HERE rather than in the router because views.js
// already imports this module — putting it in boot.js would have
// views.js and boot.js importing each other, and a cycle that happens to
// work today because nothing is used at evaluation time is a trap for
// whoever adds the first top-level use.

// The current view's hook, or null. Exactly one is live at a time:
// route() clears it before every dispatch, so a view the user has
// navigated away from can never be refreshed.
let variantRefresh = null;
let variantRefreshAt = 0;

// Minimum spacing between refreshes while work is still in flight. The
// stream ticks at 500 ms during a batch and each refresh costs a
// projection query, so following it frame-for-frame would put real
// database work on a 2 Hz timer for a bar that moves one track at a
// time. The queue-empty update bypasses this — that one is the answer
// the operator has been waiting for.
const VARIANT_REFRESH_MIN_MS = 8000;

/**
 * Called once per session by the shell.
 *
 * The event only fires when work has actually COMPLETED, so there is no
 * "is this news?" test here. `settled` means the queue is now empty:
 * that update bypasses the throttle, because it is the final state and
 * throttling it away would leave the panel permanently behind.
 */
export function wireVariantRefresh() {
  window.addEventListener("bridge:upscale", (ev) => {
    if (!variantRefresh) return;
    const settled = !!ev.detail?.settled;
    if (!settled && Date.now() - variantRefreshAt < VARIANT_REFRESH_MIN_MS) return;
    variantRefreshAt = Date.now();
    variantRefresh();
  });
}

/** Views call this to be told when generated variants may have landed. */
export function onVariantChange(fn) {
  variantRefresh = fn;
}

/** route() calls this up front, before dispatching the next view. */
export function clearVariantRefresh() {
  variantRefresh = null;
}
