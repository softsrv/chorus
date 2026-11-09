// A minimal browser stand-in for the web test client's unit tests: just enough
// DOM for app.js's el()/replaceChildren rendering, storage, history and a
// routed fetch stub. Only fetch (the server) is substituted; app.js and api.js
// run as shipped.

class Node {
  constructor() { this.childNodes = []; this.parentNode = null; }
  append(...nodes) {
    for (const n of nodes) {
      const node = n instanceof Node ? n : new Text(String(n));
      if (node.parentNode) node.remove();
      node.parentNode = this;
      this.childNodes.push(node);
    }
  }
  prepend(...nodes) {
    const rest = this.childNodes;
    this.childNodes = [];
    this.append(...nodes);
    this.childNodes.push(...rest);
  }
  replaceChildren(...nodes) {
    for (const c of this.childNodes) c.parentNode = null;
    this.childNodes = [];
    this.append(...nodes);
  }
  remove() {
    if (!this.parentNode) return;
    const sibs = this.parentNode.childNodes;
    sibs.splice(sibs.indexOf(this), 1);
    this.parentNode = null;
  }
  replaceWith(node) {
    const p = this.parentNode;
    if (!p) return;
    const i = p.childNodes.indexOf(this);
    if (node.parentNode) node.remove();
    node.parentNode = p;
    p.childNodes.splice(i, 1, node);
    this.parentNode = null;
  }
  get children() { return this.childNodes.filter((c) => c instanceof Element); }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(v) { this.replaceChildren(new Text(String(v))); }
}

class Text extends Node {
  constructor(data) { super(); this.data = data; }
  get textContent() { return this.data; }
  set textContent(v) { this.data = String(v); }
}

class Element extends Node {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.attributes = {};
    this.listeners = {};
    this.dataset = {};
    this.style = {};
    this.className = '';
    this.value = '';
    this.checked = false;
    this.disabled = false;
    this.selected = false;
    this.required = false;
    this.type = '';
    this.name = '';
  }
  setAttribute(k, v) {
    this.attributes[k] = String(v);
    if (k === 'value' || k === 'type' || k === 'name') this[k] = String(v);
  }
  getAttribute(k) { return this.attributes[k] ?? null; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  reset() {}
}

export function installBrowser() {
  const store = () => {
    const m = new Map();
    return {
      getItem: (k) => (m.has(k) ? m.get(k) : null),
      setItem: (k, v) => m.set(k, String(v)),
      removeItem: (k) => m.delete(k),
      writes: m,
    };
  };
  const byId = {};
  const document = {
    createElement: (t) => new Element(t),
    createTextNode: (d) => new Text(String(d)),
    body: new Element('body'),
    getElementById: (id) => (byId[id] ||= new Element('div')),
  };
  const win = new EventTarget();
  Object.assign(globalThis, {
    Node, Text, Element, document, window: win,
    location: { pathname: '/', search: '' },
    history: { replaceState() {}, pushState() {} },
    localStorage: store(),
    sessionStorage: store(),
  });
  return { document, window: win, $app: document.getElementById('app') };
}

// The routed fetch stub. routes maps 'METHOD /path' (no /api/v1, no query) to
// a function (req) => [status, body]. Every request is recorded in calls.
export function installFetch(routes) {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    const full = url.replace(/^\/api\/v1/, '');
    const path = full.split('?')[0];
    const req = { method: init.method, path, url: full, headers: init.headers, body: init.body === undefined ? undefined : JSON.parse(init.body) };
    calls.push(req);
    const handler = routes[`${init.method} ${path}`];
    const [status, body] = handler ? handler(req) : [404, { error: { code: 'not_found', message: 'Not found.' } }];
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  };
  return calls;
}

// settle lets the click handlers' promise chains run to completion.
export async function settle() {
  for (let i = 0; i < 30; i++) await new Promise((r) => setImmediate(r));
}

export function walk(node, out = []) {
  for (const c of node.childNodes) {
    if (c instanceof Element) { out.push(c); walk(c, out); }
  }
  return out;
}

export const buttons = (root, text) => walk(root).filter((n) => n.tagName === 'BUTTON' && n.textContent === text);
export const buttonStarting = (root, prefix) => walk(root).find((n) => n.tagName === 'BUTTON' && n.textContent.startsWith(prefix));

export async function click(node) {
  for (const fn of node.listeners.click || []) await fn({ preventDefault() {}, currentTarget: node, target: node });
  await settle();
}

export async function submit(formNode) {
  for (const fn of formNode.listeners.submit || []) await fn({ preventDefault() {} });
  await settle();
}

// section returns the text of the list under an h4 heading (Available, Mine…).
export function section(root, title) {
  const h = walk(root).find((n) => n.tagName === 'H4' && n.textContent === title);
  return h ? h.parentNode.textContent : null;
}

// sectionNode returns the container of an h4 heading.
export function sectionNode(root, title) {
  const h = walk(root).find((n) => n.tagName === 'H4' && n.textContent === title);
  return h ? h.parentNode : null;
}

let importCount = 0;
// loadApp imports a fresh copy of app.js (its module state starts over); it
// boots on import.
export async function loadApp() {
  importCount += 1;
  await import(`../static/js/app.js?run=${importCount}`);
  await settle();
}
