// Enough to reach the end of module evaluation — deliberately not a DOM.
// Values are chosen to TERMINATE: queries return a plain element-ish object
// (not a self-returning Proxy, which makes `while (node.parentNode)` loop
// forever), and collection-ish reads return empty arrays.
const noop = () => {};
const mkEl = () => ({
  style: { setProperty: noop, removeProperty: noop, getPropertyValue: () => '' }, dataset: {}, classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
  addEventListener: noop, removeEventListener: noop, setAttribute: noop, removeAttribute: noop,
  getAttribute: () => null, appendChild: noop, append: noop, remove: noop, insertBefore: noop,
  querySelector: () => null, querySelectorAll: () => [], closest: () => null, focus: noop, blur: noop,
  scrollTo: noop, replaceChildren: noop, children: [], childNodes: [], parentNode: null,
  textContent: '', innerHTML: '', value: '', hidden: false, checked: false,
});
const doc = mkEl();
Object.assign(doc, {
  getElementById: () => mkEl(), querySelector: () => mkEl(), querySelectorAll: () => [],
  createElement: () => mkEl(), createElementNS: () => mkEl(), createTextNode: () => mkEl(),
  body: mkEl(), documentElement: mkEl(), head: mkEl(), title: '', visibilityState: 'visible',
});
globalThis.document = doc;
globalThis.window = {
  document: doc, addEventListener: noop, removeEventListener: noop,
  location: { pathname: '/', search: '', href: 'http://x/', assign: noop },
  history: { pushState: noop, replaceState: noop }, matchMedia: () => ({ matches: false, addEventListener: noop }),
  setInterval: () => 0, setTimeout: () => 0, clearInterval: noop, clearTimeout: noop,
  requestAnimationFrame: () => 0, scrollTo: noop, getComputedStyle: () => ({}),
};
// Bare `location`, not just `window.location`: boot.js reads the global
// form at module scope.
globalThis.location = globalThis.window.location;
globalThis.history = globalThis.window.history;
globalThis.sessionStorage = { getItem: () => null, setItem: noop, removeItem: noop };
globalThis.localStorage = globalThis.sessionStorage;
globalThis.Audio = function () { return mkEl(); };
globalThis.EventSource = function () { return mkEl(); };
