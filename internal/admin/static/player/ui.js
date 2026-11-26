// Shared DOM helpers.
//
// Every node here is built with createElement/textContent, never
// innerHTML. That is not stylistic: album titles, artist names, genre
// labels and Atlas bios are third-party strings from tags and from a
// remote metadata service, and the console has no CSP to fall back on.

export function el(tag, opts = {}, ...children) {
  const n = document.createElement(tag);
  if (opts.class) n.className = opts.class;
  if (opts.text != null) n.textContent = String(opts.text);
  if (opts.attrs) for (const [k, v] of Object.entries(opts.attrs)) {
    if (v !== null && v !== undefined && v !== false) n.setAttribute(k, v === true ? "" : String(v));
  }
  if (opts.on) for (const [k, v] of Object.entries(opts.on)) n.addEventListener(k, v);
  for (const c of children.flat()) {
    if (c == null) continue;
    n.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return n;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/**
 * Disable or enable a control around a request, giving focus back.
 *
 * The implementation is app.js's setDisabled, reached through
 * window.BridgeControls because app.js is a classic script and this module
 * cannot import it, the same one-way handshake BridgeFeatureTray is. It is
 * the ONE implementation: a browser moves focus off a focused control that
 * becomes disabled and does not give it back when it is enabled again, so a
 * control that disabled itself around a request left a keyboard user on the
 * body, and setDisabled is what gives it back (see it for the rule).
 *
 * The fallback is a plain assignment: a page without app.js still disables
 * its controls and only loses the focus, the way the gear (BridgeFeatureTray)
 * costs a tray and not the grid. Every player control that disables itself
 * calls this and never sets `.disabled` to a literal
 * (TestNoConsoleControlDisablesItselfOutsideSetDisabled).
 */
export function setDisabled(control, disabled) {
  const shared = window.BridgeControls?.setDisabled;
  if (shared) shared(control, disabled);
  else control.disabled = disabled;
}

/**
 * An <svg><use> pointing at one of layout.html's transport sprite symbols.
 *
 * createElement cannot make SVG, so this goes through createElementNS and
 * setAttribute — which also means `cls` never reaches
 * TestPlayerEmittedClassesAreStyled's `class:` literal scrape, so a class
 * passed here needs a rule that nothing will check for. nowplaying.js's
 * iconButton documents the same gap for the same reason; it is left
 * alone rather than folded in here, because its OTHER classes are written
 * as per-branch literals precisely so that scrape can see them.
 *
 * Presentation belongs on this host element, never on the sprite's source
 * <g>: a rule matching the original does not cross into the shadow tree
 * that <use> builds from it.
 */
export function spriteIcon(name, cls) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", cls);
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `#i-np-${name}`);
  svg.appendChild(use);
  return svg;
}

/** An internal link that the router intercepts. */
export function link(href, opts = {}, ...children) {
  return el("a", { ...opts, attrs: { ...(opts.attrs || {}), href, "data-route": "" } }, ...children);
}

/**
 * A cover image with a placeholder that swaps on load.
 *
 * `loading="lazy"` matters at grid scale: a 25k-album library would
 * otherwise open thousands of connections at once. The error path drops
 * the <img> entirely rather than leaving a broken-image glyph, which is
 * the same fallback chain the Library Inspector's tiles use.
 */
export function cover(src, alt) {
  const box = el("div", { class: "cover" });
  if (!src) {
    box.classList.add("cover-empty");
    return box;
  }
  const img = el("img", {
    attrs: { src, alt: alt || "", loading: "lazy", decoding: "async" },
  });
  img.addEventListener("load", () => box.setAttribute("data-loaded", ""));
  img.addEventListener("error", () => {
    img.remove();
    box.classList.add("cover-empty");
  });
  box.appendChild(img);
  return box;
}

/**
 * A breadcrumb trail, outermost first, ending at the page you are on.
 *
 * `items` are the ancestors, each a link; `current` is the page itself,
 * rendered as plain non-link text carrying aria-current="page".
 *
 * The current page used to be left out, on the reasoning that the
 * heading below already named it. That was wrong in practice and was
 * reported as such: a trail reading "Artists › Buika" above an album
 * gives the reader no way to tell whether Buika is where they ARE or
 * where they came FROM, and on the two pages whose heading was a generic
 * category ("Album", "Artist") there was nothing naming the current page
 * at all. Ending the trail on a visibly non-link item removes the
 * question rather than asking the reader to infer the answer, and it is
 * what every file manager and repository browser does.
 *
 * Ancestors without an href are dropped: a crumb whose only job is to be
 * clickable is worse than absent when it is not, and a caller that could
 * not resolve one (an album with no artist row) has a shorter, still-true
 * trail to fall back on. `current` needs no href by construction.
 *
 * A trail with no ancestors is not a trail — it would be one non-link
 * word above a heading that already says it — so it returns null even
 * when `current` is given. That lets a top-level list pass its own label
 * unconditionally without special-casing.
 *
 * The title attribute is set only past truncateAt, because .crumb-link
 * and .crumb-current ellipsise long labels and a tooltip is then the only
 * way back to the full name. Setting it unconditionally would put a
 * tooltip identical to the visible text on every crumb, which some screen
 * readers announce twice.
 */
const crumbTruncateAt = 24;

export function crumbs(items, current) {
  const list = (items || []).filter((i) => i?.label && i?.href);
  if (!list.length) return null;
  const ol = el("ol", { class: "crumb-list" });
  const titleOf = (label) => (label.length > crumbTruncateAt ? { title: label } : {});
  for (const { label, href } of list) {
    ol.appendChild(el("li", { class: "crumb-item" },
      link(href, { class: "crumb-link", text: label, attrs: titleOf(label) })));
  }
  if (current) {
    ol.appendChild(el("li", { class: "crumb-item" },
      el("span", {
        class: "crumb-current", text: current,
        attrs: { "aria-current": "page", ...titleOf(current) },
      })));
  }
  return ol;
}

export function chip(text, cls = "") {
  return text ? el("span", { class: `chip ${cls}`.trim(), text }) : null;
}

export function spinner(label = "Loading…") {
  return el("p", { class: "player-status muted", text: label });
}

export function emptyState(title, detail, action) {
  const box = el("div", { class: "player-empty" },
    el("h2", { text: title }),
    detail ? el("p", { class: "muted", text: detail }) : null);
  // An empty library is the one empty state with something to DO about it, so
  // it gets a real control rather than a sentence describing one. `action` is
  // `{ href, label }`; every other caller passes nothing and is unchanged.
  if (action) {
    // NOT the player's link() helper: that stamps data-route, which routes the
    // click through the PLAYER, and an operator route like /upload is not a
    // player path. `data-boost` hands it to the admin boost router instead,
    // which swaps operator pages in place and is what keeps <audio> alive.
    box.appendChild(el("a", {
      class: "btn primary player-empty-action",
      attrs: { href: action.href, "data-boost": "" },
      text: action.label,
    }));
  }
  return box;
}

export function errorState(err, retry) {
  const box = el("div", { class: "player-empty" },
    el("h2", { text: "Something went wrong" }),
    el("p", { class: "muted", text: String(err?.message ? err.message : err) }));
  if (retry) box.appendChild(el("button", { class: "btn", text: "Try again", on: { click: retry } }));
  return box;
}

/**
 * Render a list in animation-frame-sized chunks, guarded by a
 * generation counter.
 *
 * The counter is the load-bearing part, and it is here because the
 * Library Inspector learned it the hard way: without it, chunks from a
 * view the user already navigated away from interleave into the new
 * one. Bump `gen()` on every navigation.
 */
export function chunkAppend(container, items, make, gen, chunkSize = 40) {
  const mine = gen();
  let i = 0;
  function step() {
    if (mine !== gen()) return; // superseded — drop the rest
    const frag = document.createDocumentFragment();
    for (let n = 0; n < chunkSize && i < items.length; n++, i++) {
      const node = make(items[i], i);
      if (node) frag.appendChild(node);
    }
    container.appendChild(frag);
    if (i < items.length) requestAnimationFrame(step);
  }
  step();
}

/**
 * Call `onHit` when the sentinel scrolls into view — the paging
 * trigger. Returns a disposer.
 */
export function onVisible(sentinel, onHit) {
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) onHit();
  }, { rootMargin: "400px" });
  io.observe(sentinel);
  return () => io.disconnect();
}

/**
 * The jump rail: a list's index as its sort keys it (B1021).
 *
 * The server sends `buckets` on the FIRST page only, computed after the
 * filter and sort, with `rail`, the kind of index they are, so a bucket's
 * `offset` is an index into the CURRENT result set. Jumping therefore means
 * re-fetching at that offset, not scrolling: the target is usually far past
 * the pages loaded so far.
 *
 * Letters (a server before the kinds sent no others): # and A–Z, those with
 * nothing behind them disabled rather than omitted — a rail that changes
 * length as you filter is harder to hit than one that stays put. Years: the
 * phone's index, a band to each year where there are at most 16, else to each
 * decade (its years shared out top to bottom), "?" one of its own, every band
 * an equal share of the rail, and the century on every label unless every year
 * is in the ten decades ending with this one. Dates: an entry to each of
 * Recently Added's periods, its label short and its words whole.
 *
 * Real <button>s, so keyboard and screen-reader support come free. The
 * same reasoning that put a real <input type="range"> in the scrubber.
 */
const RAIL_LETTERS = "#ABCDEFGHIJKLMNOPQRSTUVWXYZ".split("");
const RAIL_NAMES = { letters: "Jump to letter", years: "Jump to year", dates: "Jump to date added" };
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const SCRUBS = new Set(["touch", "pen"]);
// How long a scrub's lift owes the click its tap makes to the scrub: WebKit
// fires a tap's click within milliseconds of its pointerup, and a lift the
// browser gives no click owes it no longer than this.
const OWED_MS = 1000;

// What the last pointer went down on, anywhere in the document, and its
// type, which one capture listener a document records for every rail (B1021):
// a pointer's click on a live entry jumps only where it went down outside the
// rail, or by a mouse on that entry, since WebKit hands a tap on an empty
// letter's click to the nearest live one and a touch's or a pen's jumped as
// the scrub took it, and takes the record; a key's click (detail 0) is its
// entry's own and leaves it. `owedUntil` is a scrub's click: its owner went up
// on the rail, and the click that tap makes, whatever pointer went down since
// elsewhere, is the scrub's, until the next pointerdown or keydown, or
// OWED_MS after the lift.
let pointerDown = null;
let owedUntil = 0;
let listening = null;

// railEntries is the rail's entries for its kind: each one's label, the words
// a screen reader and the callout say, and its buckets (none for an empty
// letter; a decade's years, newest first as the list sorts them).
function railEntries(kind, buckets, now) {
  if (kind === "dates") {
    const label = { "this-month": MONTHS[now.getMonth()], "this-year": String(now.getFullYear()) };
    const words = { "this-month": "This month", "this-year": "Earlier this year", "?": "Date unknown" };
    return buckets.map((b) => ({ label: label[b.key] || b.key, words: words[b.key] || b.key, buckets: [b] }));
  }
  if (kind === "years") {
    const dated = buckets.filter((b) => /^\d{4}$/.test(b.key));
    const decade = (b) => b.key.slice(0, 3) + "0";
    const here = Math.floor(now.getFullYear() / 10) * 10;
    const elide = dated.every((b) => Number(decade(b)) > here - 100 && Number(decade(b)) <= here);
    const name = (y) => (elide ? `’${y.slice(2)}` : y);
    const out = [];
    for (const b of buckets) {
      const d = decade(b);
      if (!dated.includes(b)) out.push({ label: "?", words: "Year unknown", buckets: [b] });
      else if (dated.length <= 16) out.push({ label: name(b.key), words: b.key, buckets: [b] });
      else if (out.at(-1)?.decade === d) out.at(-1).buckets.push(b);
      else out.push({ decade: d, label: name(d) + "s", words: d + "s", buckets: [b] });
    }
    return out;
  }
  const byKey = new Map(buckets.map((b) => [b.key, b]));
  return RAIL_LETTERS.map((l) => ({ label: l, words: l, buckets: byKey.has(l) ? [byKey.get(l)] : [] }));
}

export function indexRail(buckets, onJump, kind = "letters", { live = () => true, lift = null } = {}) {
  if (!Array.isArray(buckets) || buckets.length === 0) return null;
  if (listening !== document) {
    listening = document;
    document.addEventListener("pointerdown", (e) => { pointerDown = { target: e.target, type: e.pointerType }; owedUntil = 0; }, true);
    document.addEventListener("keydown", () => { owedUntil = 0; }, true);
  }
  const rail = el("nav", { class: "az-rail", attrs: { "aria-label": RAIL_NAMES[kind] || RAIL_NAMES.letters } });
  const entries = new Map();
  for (const entry of railEntries(kind, buckets, new Date())) {
    const n = entry.buckets.reduce((sum, b) => sum + b.count, 0);
    const btn = el("button", {
      class: "az-letter", text: entry.label,
      attrs: n
        ? { type: "button", title: `${n} under ${entry.words}`, "aria-label": entry.words === entry.label ? null : entry.words }
        : { type: "button", disabled: "", "aria-disabled": "true" },
    });
    if (n) {
      entries.set(btn, entry);
      btn.addEventListener("click", (e) => {
        if (e.detail > 0) {
          const from = pointerDown;
          const scrubbed = performance.now() < owedUntil;
          pointerDown = null;
          owedUntil = 0;
          if (scrubbed || (from && rail.contains(from.target) && (SCRUBS.has(from.type) || !btn.contains(from.target)))) return;
        }
        onJump(entry.buckets[0]);
      });
    }
    rail.appendChild(btn);
  }
  const callout = el("div", { class: "az-callout", attrs: { "aria-hidden": "true" } });
  callout.hidden = true;
  rail.appendChild(callout);

  // The scrub: the first touch's or pen's pointer to go down on the rail while
  // no scrub holds it, which the rail captures, owns it until its lift, its
  // cancel or the rail's lost capture; no other pointer, primary or not, starts
  // or takes over a scrub meanwhile (its own going down again, its end never
  // delivered, starts it anew). It takes the entry at the contact point
  // (the event's target, which WebKit gives), then at each move the one under
  // the finger, the point held inside the rail; a decade's band gives the year
  // at the finger's height in it. The list jumps at each new live bucket, told
  // that a finger holds the rail, never from an empty letter, and hears `lift`
  // at the end of a scrub that jumped; the callout says where it is, beside
  // the rail at the finger's height. While `live` says no (a filter's answer is
  // on its way), the rail takes no scrub, and ends one at its next move.
  let owner = null;
  const entryOf = (n) => {
    for (; n && n !== rail; n = n.parentNode) if (n.parentNode === rail) return n;
    return null;
  };
  const under = (x, y) => {
    const r = rail.getBoundingClientRect();
    const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);
    return entryOf(document.elementFromPoint(clamp(x, r.left + 1, r.right - 1), clamp(y, r.top + 1, r.bottom - 1)));
  };
  const take = (btn, y) => {
    const at = Number.isFinite(y);
    const entry = entries.get(btn);
    let b = null;
    if (entry) {
      b = entry.buckets[0];
      if (at && entry.buckets.length > 1) {
        // Held to the band's years at both ends; a band with no height is its first year's.
        const box = btn.getBoundingClientRect();
        const i = Math.floor(((y - box.top) / box.height) * entry.buckets.length);
        b = entry.buckets[Math.min(entry.buckets.length - 1, Math.max(0, i))] || b;
      }
      callout.textContent = entry.buckets.length > 1 ? b.key : entry.words;
      callout.hidden = false;
    }
    // Centred at the finger's height, and held inside the rail's height, so
    // what is pinned above the rail or the bar below it covers none of it;
    // placed before the jump, which empties the list and can move the rail.
    if (at && !callout.hidden) {
      const r = rail.getBoundingClientRect();
      const half = callout.offsetHeight / 2;
      callout.style.top = `${Math.min(Math.max(y - r.top, half), Math.max(half, r.height - half))}px`;
    }
    if (b && b !== owner.bucket) {
      owner.bucket = b;
      onJump(b, true);
    }
  };
  rail.addEventListener("pointerdown", (e) => {
    if (!SCRUBS.has(e.pointerType) || (owner && owner.id !== e.pointerId) || !live()) return;
    owner = { id: e.pointerId, bucket: null };
    rail.setPointerCapture(e.pointerId);
    take(entryOf(e.target) || under(e.clientX, e.clientY), e.clientY);
  });
  rail.addEventListener("pointermove", (e) => {
    if (owner?.id !== e.pointerId) return;
    if (live()) take(under(e.clientX, e.clientY), e.clientY);
    else {
      owner = null;
      callout.hidden = true;
    }
  });
  const end = (e) => {
    if (owner?.id !== e.pointerId || (e.type === "lostpointercapture" && e.target !== rail)) return;
    if (e.type === "pointerup") owedUntil = performance.now() + OWED_MS;
    const jumped = !!owner.bucket;
    owner = null;
    callout.hidden = true;
    if (jumped) lift?.();
  };
  for (const type of ["pointerup", "pointercancel", "lostpointercapture"]) rail.addEventListener(type, end);
  return rail;
}

/** Announce a route change to assistive tech. */
export function announce(text) {
  const live = document.getElementById("player-live");
  if (live) live.textContent = text;
}

/**
 * Render an Atlas About block.
 *
 * The attribution is NOT optional: Source/SourceURL must render
 * whenever the text does (CC-BY-SA / ToS). The href is admitted only
 * when it parses as http(s), mirroring app.js's safeExternalHref — the
 * URL comes from a remote service and a javascript: scheme reaching an
 * anchor would be an XSS.
 */
export function aboutBlock(about, { title, plain } = {}) {
  if (!about || about.state !== "found") return null;
  const text = about.bio || about.bioSummary || about.description || "";
  if (!text.trim()) return null;
  // `plain` drops the heading and the card chrome, for the case where a
  // tab panel is already the boundary. A bordered card nested directly
  // inside a tabpanel reads as two frames around one paragraph, and the
  // tab is a better label than a heading repeating it.
  const box = el("section", { class: plain ? "about about-plain" : "about" },
    plain || !title ? null : el("h2", { text: title }));
  const body = el("p", { class: "about-text", text });
  box.appendChild(body);
  if (about.recordLabel) {
    box.appendChild(el("p", { class: "muted small", text: `Label: ${about.recordLabel}` }));
  }
  if (Array.isArray(about.genres) && about.genres.length) {
    box.appendChild(el("p", { class: "about-genres" },
      ...about.genres.slice(0, 8).map((g) => chip(g)).filter(Boolean)));
  }
  if (about.source) {
    const href = safeHref(about.sourceUrl);
    const attribution = el("p", { class: "muted small" });
    if (href) {
      attribution.appendChild(el("a", {
        text: `Read more on ${about.source}`,
        attrs: { href, target: "_blank", rel: "noopener noreferrer" },
      }));
    } else {
      attribution.textContent = `Source: ${about.source}`;
    }
    box.appendChild(attribution);
  }
  return box;
}

export function safeHref(raw) {
  if (!raw) return null;
  try {
    const u = new URL(raw, location.origin);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

// ---- Detail tabs ----

// Panel ids have to be unique across the document for aria-controls to
// resolve, and a detail view can be re-rendered many times in a session.
let tabSeq = 0;

// The tab the reader last chose, and the subject it belonged to.
//
// This is what makes the Variants tab usable at all. The album and
// artist views re-render themselves whenever a variant job lands or a
// delete completes — which is precisely when the reader is looking at
// the Variants tab — and without a memory every one of those bounced
// them back to Tracks mid-operation.
//
// Keyed on the SUBJECT, not just the tab id: navigating to a different
// album opens on its default, because a remembered "variants" from the
// previous album says nothing about what the reader wants from this
// one. One slot rather than a Map: only the current view can be
// re-rendered, so a second entry could never be read.
const tabMemory = { key: null, id: null };

/**
 * A tab strip over a set of panels.
 *
 * @param {string} key - identifies the subject ("album:<id>"). The
 *   remembered tab is restored only for a matching key.
 * @param {Array<{id: string, label: string, panel: Node|null}>} specs -
 *   in display order. A spec with a null panel is DROPPED, which is how
 *   "no Atlas entry for this release" and "this bridge sent no variant
 *   summary" turn into an absent tab rather than an empty one.
 * @returns {Node|null} the strip plus its panels, the lone panel when
 *   only one survives, or null when none do.
 */
export function detailTabs(key, specs) {
  const live = (specs || []).filter((s) => s?.panel);
  if (!live.length) return null;
  // A single tab is chrome that says nothing: a lone "Tracks" button
  // above a track list is a control with no alternative to offer.
  if (live.length === 1) return live[0].panel;

  const seq = ++tabSeq;
  const wanted = tabMemory.key === key ? tabMemory.id : null;
  let active = live.findIndex((s) => s.id === wanted);
  if (active < 0) active = 0;

  const strip = el("div", { class: "detail-tabstrip", attrs: { role: "tablist" } });
  const panels = el("div", { class: "tabpanels" });
  const buttons = [];

  const show = (i) => {
    tabMemory.key = key;
    tabMemory.id = live[i].id;
    live.forEach((s, n) => {
      const on = n === i;
      buttons[n].classList.toggle("active", on);
      buttons[n].setAttribute("aria-selected", on ? "true" : "false");
      // Roving tabindex: the strip is ONE tab stop and the arrow keys
      // move within it, which is what the tablist pattern asks for and
      // what stops a three-tab strip costing three tabs to walk past.
      buttons[n].tabIndex = on ? 0 : -1;
      s.panel.hidden = !on;
    });
  };

  live.forEach((s, i) => {
    const panelID = `tabpanel-${seq}-${s.id}`;
    const tabID = `tab-${seq}-${s.id}`;
    const btn = el("button", {
      // .tab-btn is app.css's — the Settings page's tab idiom, reused
      // so the console has one look for one control.
      class: "tab-btn", text: s.label,
      attrs: { type: "button", role: "tab", id: tabID, "aria-controls": panelID },
    });
    btn.addEventListener("click", () => show(i));
    btn.addEventListener("keydown", (e) => {
      let step = 0;
      if (e.key === "ArrowRight") step = 1;
      else if (e.key === "ArrowLeft") step = -1;
      let next = -1;
      if (step) next = (i + step + live.length) % live.length;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = live.length - 1;
      if (next < 0) return;
      e.preventDefault();
      show(next);
      buttons[next].focus();
    });
    buttons.push(btn);
    strip.appendChild(btn);

    s.panel.id = panelID;
    s.panel.setAttribute("role", "tabpanel");
    s.panel.setAttribute("aria-labelledby", tabID);
    // Not tabbable itself: every panel here holds its own focusable
    // content (track buttons, links, the variant controls), so a
    // tabindex on the container would add a stop that lands nowhere.
    panels.appendChild(s.panel);
  });

  show(active);
  return el("div", { class: "detail-tabs" }, strip, panels);
}
