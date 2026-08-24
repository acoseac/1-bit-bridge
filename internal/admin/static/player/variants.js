// The variant panel: what an album or artist HAS in the way of cached
// hi-res and CarPlay copies, and the controls that change it.
//
// Coverage bars read against an ELIGIBLE denominator, not a track
// count. An album of sixteen CD-quality tracks is already at the
// CarPlay floor, so "0 / 0 — nothing to do" is the truth and
// "0 / 16" would be an accusation. The exempt remainder is a muted
// footnote for the same reason.

import { el, clear } from "./ui.js";
import { bytes } from "./format.js";
import { generateVariants, deleteVariants } from "./api.js";

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
 */
export function variantPanel(summary, scope, onChanged) {
  if (!summary) return null;

  const root = el("section", { class: "variants" });
  root.appendChild(el("h2", { class: "variants-head", text: "Variants" }));

  const totals = [];
  if (summary.sourceBytes) totals.push(`${bytes(summary.sourceBytes)} of originals`);
  if (summary.variantBytes) totals.push(`${bytes(summary.variantBytes)} of variants`);
  if (totals.length) {
    root.appendChild(el("p", { class: "muted small", text: totals.join(" · ") }));
  }

  const blocked = blockedReason(summary);
  if (blocked) root.appendChild(el("p", { class: "variants-blocked small", text: blocked }));

  for (const kind of KINDS) {
    root.appendChild(kindRow(kind, summary[kind.key], scope, !blocked, onChanged));
  }
  return root;
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
    return "Variant generation is switched off for this bridge — enable it in Settings → Audio.";
  }
  if (!summary.soxAvailable) {
    return "sox is not installed on the bridge host, so no variants can be generated.";
  }
  return "";
}

function kindRow(kind, cov, scope, actionable, onChanged) {
  const c = cov || { covered: 0, eligible: 0, exempt: 0, stale: 0 };
  const row = el("div", { class: "variant-kind" });

  const head = el("div", { class: "variant-kind-head" },
    el("span", { class: "variant-kind-title", text: kind.title }),
    el("span", { class: "variant-kind-ratio", text: `${c.covered} / ${c.eligible}` }));
  row.appendChild(head);
  row.appendChild(bar(c, kind.title));

  // One note, not a list. An empty denominator and a non-zero exempt
  // count are the SAME fact told twice — "2 need nothing · nothing here
  // can take this" reads like two problems.
  let note = kind.blurb;
  if (c.eligible === 0 && c.exempt > 0) {
    note = "Nothing here needs this.";
  } else if (c.exempt > 0) {
    note = `${c.exempt} of these need nothing.`;
  }
  row.appendChild(el("p", { class: "muted small variant-kind-note", text: note }));

  // A stale copy exists and will not be served, and the bar counts it
  // as covered — which is the truth about what Generate will do, since
  // the batch skips any track that already has a variant of the kind.
  // So it needs saying out loud, WITH the remedy: Delete then Generate
  // is the only route back to a current copy.
  if (c.stale > 0) {
    row.appendChild(el("p", {
      class: "small variant-kind-stale",
      text: c.stale === 1
        ? "1 copy is out of date — its source changed after it was made. " +
          "Delete, then generate again."
        : `${c.stale} copies are out of date — their sources changed after they ` +
          "were made. Delete, then generate again.",
    }));
  }

  const status = el("p", { class: "small variant-status", attrs: { "aria-live": "polite" } });
  const missing = Math.max(0, c.eligible - c.covered);

  const gen = el("button", { class: "btn btn-primary", text: kind.action });
  // Nothing eligible and nothing missing means there is no work to
  // request. A live button that answers "enqueued 0" reads as a
  // failure; a disabled one with the note beside it reads as done.
  gen.disabled = !actionable || missing === 0;
  // No refresh callback on generate, deliberately. The work has only
  // been QUEUED when the response arrives, so re-rendering here would
  // repaint the same numbers and destroy the status line the operator
  // just read. The live refresh brings the real result when it lands.
  gen.addEventListener("click", () =>
    run(gen, status, () => generateVariants(scope, kind.key),
      (r) => r.enqueuedCount > 0
        ? `Queued ${r.enqueuedCount}. They appear as they finish.`
        : "Nothing to queue — everything eligible is already covered."));

  const del = el("button", { class: "btn", text: "Delete" });
  // Delete stays available on a bridge with no sox: reclaiming disk is
  // exactly what an operator without a toolchain still needs to do.
  del.disabled = c.covered === 0;
  del.addEventListener("click", () => {
    if (!confirm(
      `Delete ${c.covered} ${kind.title} variant${c.covered === 1 ? "" : "s"}?\n\n` +
      `The cached copies are removed from disk. Your original files are not touched.`)) return;
    run(del, status, () => deleteVariants(scope, kind.key),
      (r) => `Deleted ${r.deletedCount}, freed ${bytes(r.freedBytes) || "0 B"}.`,
      onChanged);
  });

  row.appendChild(el("div", { class: "variant-actions" }, gen, del));
  row.appendChild(status);
  return row;
}

function bar(c, label) {
  const pct = c.eligible > 0 ? Math.round((c.covered / c.eligible) * 100) : 0;
  const outer = el("div", {
    class: "variant-bar",
    attrs: {
      role: "progressbar", "aria-label": `${label} coverage`,
      "aria-valuenow": String(c.covered), "aria-valuemin": "0",
      "aria-valuemax": String(c.eligible),
    },
  });
  const fill = el("div", { class: "variant-bar-fill" });
  fill.style.width = `${pct}%`;
  outer.appendChild(fill);
  return outer;
}

/**
 * Drive one mutation: disable, call, report, invite a refresh.
 *
 * The button is re-enabled on failure only. On success the caller
 * re-renders from fresh data, which replaces this node — re-enabling it
 * first would be a frame of the old state.
 */
async function run(button, status, call, describe, onChanged) {
  button.disabled = true;
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
    button.disabled = false;
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
    const settled = !!(ev.detail && ev.detail.settled);
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
