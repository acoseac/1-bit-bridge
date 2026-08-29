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
 * A breadcrumb trail of ANCESTORS, outermost first.
 *
 * The current page is deliberately NOT listed. It is named by the
 * heading directly below — route() sets a section title and the detail
 * views retitle it through setAxisTitle — so listing it here would
 * print the same string twice, which is exactly what
 * renderCollectionDetail dropped its own .detail-title to stop doing.
 * That gives one rule with no exceptions: the crumb says where this
 * page hangs, the heading says what it is.
 *
 * Items with no href are dropped rather than rendered as inert text: a
 * crumb whose only job is to be clickable is worse than absent when it
 * is not, and the caller that could not resolve an ancestor (an album
 * with no artist row) has a shorter, still-true trail to fall back on.
 *
 * The title attribute is set only past truncateAt, because .crumb-link
 * ellipsises long labels and a tooltip is then the only way back to the
 * full name. Setting it unconditionally would put a tooltip identical
 * to the visible text on every crumb, which some screen readers
 * announce twice.
 */
const crumbTruncateAt = 24;

export function crumbs(items) {
  const list = (items || []).filter((i) => i && i.label && i.href);
  if (!list.length) return null;
  const ol = el("ol", { class: "crumb-list" });
  for (const { label, href } of list) {
    ol.appendChild(el("li", { class: "crumb-item" },
      link(href, {
        class: "crumb-link", text: label,
        attrs: label.length > crumbTruncateAt ? { title: label } : {},
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

export function emptyState(title, detail) {
  return el("div", { class: "player-empty" },
    el("h2", { text: title }),
    detail ? el("p", { class: "muted", text: detail }) : null);
}

export function errorState(err, retry) {
  const box = el("div", { class: "player-empty" },
    el("h2", { text: "Something went wrong" }),
    el("p", { class: "muted", text: String(err && err.message ? err.message : err) }));
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
 * The A–Z jump rail.
 *
 * The server sends `buckets` on the FIRST page only, computed after the
 * filter and sort, so a letter's `offset` is an index into the CURRENT
 * result set. Jumping therefore means re-fetching at that offset, not
 * scrolling: the target is usually far past the pages loaded so far.
 *
 * Letters with nothing behind them render as disabled buttons rather
 * than being omitted — a rail that changes length as you filter is
 * harder to hit than one that stays put.
 *
 * Real <button>s, so keyboard and screen-reader support come free. The
 * same reasoning that put a real <input type="range"> in the scrubber.
 */
const RAIL_LETTERS = "#ABCDEFGHIJKLMNOPQRSTUVWXYZ".split("");

export function alphabetRail(buckets, onJump) {
  if (!Array.isArray(buckets) || buckets.length === 0) return null;
  const byKey = new Map(buckets.map((b) => [b.key, b]));
  const rail = el("nav", { class: "az-rail", attrs: { "aria-label": "Jump to letter" } });
  for (const letter of RAIL_LETTERS) {
    const b = byKey.get(letter);
    const btn = el("button", {
      class: "az-letter", text: letter,
      attrs: b
        ? { type: "button", title: `${b.count} under ${letter}` }
        : { type: "button", disabled: "", "aria-disabled": "true" },
    });
    if (b) btn.addEventListener("click", () => onJump(b));
    rail.appendChild(btn);
  }
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
