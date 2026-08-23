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
export function aboutBlock(about, { title }) {
  if (!about || about.state !== "found") return null;
  const text = about.bio || about.bioSummary || about.description || "";
  if (!text.trim()) return null;
  const box = el("section", { class: "about" }, el("h2", { text: title }));
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
