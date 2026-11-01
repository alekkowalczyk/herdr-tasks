// Runs the board page's script in a stand-in browser, with a stand-in DOM
// and board server, to check what a card shows while its changes are on
// their way, and which 401s sign the page out. Usage: node board.js
// path/to/app.js
'use strict';

const assert = require('assert/strict');
const fs = require('fs');
const vm = require('vm');

const KEY = 'herdr-tasks-board-key';
const src = fs.readFileSync(process.argv[2], 'utf8');

// ---- A stand-in DOM, with what app.js uses

class Node {
  constructor() {
    this.parentNode = null;
  }
  remove() {
    const p = this.parentNode;
    if (!p) return;
    p.childNodes.splice(p.childNodes.indexOf(this), 1);
    this.parentNode = null;
  }
}

class Text extends Node {
  constructor(text) {
    super();
    this.textContent = String(text);
  }
}

// matcher reads the selectors app.js uses: a list of tags, .classes,
// [attributes], [attribute="values"] and :not([attributes]).
function matcher(sel) {
  const alts = sel.split(',').map(part => {
    part = part.trim();
    const m = /^([a-z][a-z0-9]*)?(.*)$/i.exec(part);
    const checks = [];
    if (m[1]) checks.push(e => e.tag === m[1].toLowerCase());
    const token = /\.([\w-]+)|\[([\w-]+)(?:="([^"]*)")?\]|:not\(\[([\w-]+)\]\)/g;
    let at = 0;
    for (let t; (t = token.exec(m[2])); ) {
      if (t.index !== at) break;
      at = token.lastIndex;
      const [, cls, attr, value, not] = t;
      if (cls) checks.push(e => e.classList.contains(cls));
      else if (attr) checks.push(e => e.hasAttribute(attr) && (value === undefined || e.getAttribute(attr) === value));
      else checks.push(e => !e.hasAttribute(not));
    }
    if (at !== m[2].length) throw new Error('the stand-in DOM can’t read ' + JSON.stringify(sel));
    return e => checks.every(c => c(e));
  });
  return e => alts.some(a => a(e));
}

class Element extends Node {
  constructor(tag) {
    super();
    this.tag = tag.toLowerCase();
    this.childNodes = [];
    this.attrs = new Map();
    this.listeners = {};
    this.style = {};
    this.hidden = false;
    this.text = '';
    this.value = '';
    this.scrollTop = 0;
    this.offsetWidth = 0;
    this.offsetHeight = 0;
    const self = this;
    const classes = () => new Set(self.className.split(/\s+/).filter(Boolean));
    const setClasses = set => (self.className = [...set].join(' '));
    this.classList = {
      add: (...cs) => setClasses(new Set([...classes(), ...cs])),
      remove: (...cs) => setClasses(new Set([...classes()].filter(c => !cs.includes(c)))),
      contains: c => classes().has(c),
      toggle(c, force) {
        const on = force === undefined ? !classes().has(c) : Boolean(force);
        if (on) this.add(c);
        else this.remove(c);
        return on;
      },
    };
  }
  get className() {
    return this.getAttribute('class') || '';
  }
  set className(v) {
    this.setAttribute('class', v);
  }
  get textContent() {
    return this.text + this.childNodes.map(n => n.textContent).join('');
  }
  set textContent(v) {
    this.replaceChildren();
    this.text = String(v);
  }
  get children() {
    return this.childNodes.filter(n => n instanceof Element);
  }
  get firstElementChild() {
    return this.children[0] || null;
  }
  get nextElementSibling() {
    if (!this.parentNode) return null;
    const els = this.parentNode.children;
    return els[els.indexOf(this) + 1] || null;
  }
  get isConnected() {
    let e = this;
    while (e.parentNode) e = e.parentNode;
    return e === doc.body;
  }
  setAttribute(k, v) {
    this.attrs.set(k, String(v));
  }
  getAttribute(k) {
    return this.attrs.has(k) ? this.attrs.get(k) : null;
  }
  hasAttribute(k) {
    return this.attrs.has(k);
  }
  removeAttribute(k) {
    this.attrs.delete(k);
  }
  append(...nodes) {
    for (let n of nodes) {
      if (!(n instanceof Node)) n = new Text(n);
      n.remove();
      n.parentNode = this;
      this.childNodes.push(n);
    }
  }
  replaceChildren(...nodes) {
    for (const n of this.childNodes) n.parentNode = null;
    this.childNodes = [];
    this.text = '';
    this.append(...nodes);
  }
  insertBefore(n, ref) {
    if (!ref) return this.append(n);
    n.remove();
    n.parentNode = this;
    this.childNodes.splice(this.childNodes.indexOf(ref), 0, n);
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  matches(sel) {
    return matcher(sel)(this);
  }
  closest(sel) {
    const m = matcher(sel);
    for (let e = this; e instanceof Element; e = e.parentNode) if (m(e)) return e;
    return null;
  }
  querySelectorAll(sel) {
    const m = matcher(sel);
    const out = [];
    const walk = e => {
      for (const c of e.children) {
        if (m(c)) out.push(c);
        walk(c);
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter(f => f !== fn);
  }
  // dispatch sends an event from this element up to the document.
  dispatch(type, props = {}) {
    const ev = Object.assign({type, target: this, defaultPrevented: false, preventDefault: () => (ev.defaultPrevented = true)}, props);
    for (let e = this; e; e = e.parentNode) for (const fn of e.listeners[type] || []) fn(ev);
    for (const fn of doc.listeners[type] || []) fn(ev);
  }
  click() {
    this.dispatch('click');
  }
  focus() {
    doc.activeElement = this;
  }
  select() {}
  showModal() {
    this.setAttribute('open', '');
  }
  close() {
    this.removeAttribute('open');
  }
  getBoundingClientRect() {
    return {left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0};
  }
}

const doc = {body: null, activeElement: null, listeners: {}};

// ---- The page

const settle = () => new Promise(r => setTimeout(r, 5));

// card is a card on the board, as the server sends it.
function card(more) {
  return Object.assign(
    {
      session: 'work',
      tab: 'w1:t1',
      title: 'Fix the ABC-12 login loop',
      stage: 'active',
      column: 'yourturn',
      since: '2026-10-31T09:00:00Z',
      reason: '',
      priority: false,
      space: 's1',
      spaceLabel: 'acme',
      agentStatus: 'done',
      agents: [],
      links: [],
      note: '',
      log: [],
    },
    more,
  );
}

const board = c => ({version: '0.8.0', linearKey: false, sessions: [{name: 'work', stale: false, spaces: [{id: 's1', label: 'acme'}], cards: [c]}]});

// page runs the script as a tab opened at /, signed in with stored. The
// server takes only the key in pg.good.
function page(stored) {
  doc.body = new Element('body');
  doc.activeElement = null;
  doc.listeners = {};
  const pg = {good: stored, streams: [], posts: []};
  const ids = new Map();
  const document = {
    body: doc.body,
    get activeElement() {
      return doc.activeElement;
    },
    getElementById(id) {
      if (!ids.has(id)) {
        const e = new Element(id === 'board' ? 'main' : 'div');
        e.setAttribute('id', id);
        doc.body.append(e);
        ids.set(id, e);
      }
      return ids.get(id);
    },
    createElement: tag => new Element(tag),
    createTextNode: text => new Text(text),
    querySelector: sel => doc.body.querySelector(sel),
    querySelectorAll: sel => doc.body.querySelectorAll(sel),
    addEventListener(type, fn) {
      (doc.listeners[type] = doc.listeners[type] || []).push(fn);
    },
  };
  pg.$ = id => document.getElementById(id);

  const storage = new Map([[KEY, stored]]);
  const windowListeners = {};

  // openStream is a board stream the test sends boards on.
  function openStream(s) {
    const chunks = [];
    let wake = null;
    let ended = false;
    s.push = text => {
      chunks.push(new TextEncoder().encode(text));
      if (wake) wake();
    };
    s.signal.addEventListener('abort', () => {
      ended = true;
      if (wake) wake();
    });
    return {
      ok: true,
      status: 200,
      body: {
        getReader: () => ({
          async read() {
            while (!chunks.length && !ended) await new Promise(r => (wake = r));
            if (chunks.length) return {value: chunks.shift(), done: false};
            return {value: undefined, done: true};
          },
        }),
      },
    };
  }

  const reply = status =>
    status < 300 ? {ok: true, status} : {ok: false, status, json: async () => ({error: status === 401 ? 'Sign in again.' : 'No.'})};

  async function fetch(url, opts) {
    const k = ((opts.headers || {}).Authorization || '').replace(/^Bearer /, '');
    if (url === '/api/events') {
      const s = {key: k, signal: opts.signal};
      pg.streams.push(s);
      return k === pg.good ? openStream(s) : reply(401);
    }
    // A change is answered when the test says.
    return new Promise(resolve => pg.posts.push({url, key: k, body: JSON.parse(opts.body), answer: status => resolve(reply(status))}));
  }

  // Timers run when the test says, and the clock moves on with them.
  const timers = new Map();
  let timerID = 0;
  let later = 0;
  pg.runTimers = below => {
    later += below;
    for (const [id, t] of [...timers]) {
      if (t.ms < below) {
        timers.delete(id);
        t.fn();
      }
    }
  };
  const Clock = class extends Date {
    static now() {
      return Date.now() + later;
    }
  };

  const ctx = {
    window: {
      addEventListener: (type, fn) => (windowListeners[type] = windowListeners[type] || []).push(fn),
      getSelection: () => '',
    },
    document,
    Element,
    location: {search: '', hash: '', pathname: '/'},
    history: {state: null, replaceState() {}, pushState() {}, back() {}},
    localStorage: {
      getItem: k => (storage.has(k) ? storage.get(k) : null),
      setItem: (k, v) => storage.set(k, String(v)),
    },
    fetch,
    setTimeout: (fn, ms) => {
      timers.set(++timerID, {fn, ms});
      return timerID;
    },
    clearTimeout: id => timers.delete(id),
    setInterval: () => 0,
    Date: Clock,
    requestAnimationFrame: () => 0,
    innerWidth: 1200,
    innerHeight: 800,
    AbortController,
    TextEncoder,
    TextDecoder,
    URL,
    URLSearchParams,
    console,
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);

  // send sends a board on the newest stream.
  pg.send = b => pg.streams[pg.streams.length - 1].push('event: board\ndata: ' + JSON.stringify(b) + '\n\n');
  // otherTab stores key, as a tab that signs in does, and tells this one.
  pg.otherTab = key => {
    storage.set(KEY, key);
    for (const fn of windowListeners.storage || []) fn({key: KEY, newValue: key});
  };
  // button is the card's button for act, as it shows now.
  pg.button = act => pg.$('board').querySelector('.card').querySelector('[data-act="' + act + '"]');
  pg.priority = () => pg.button('priority').getAttribute('aria-pressed');
  pg.toasts = () => pg.$('toasts').children.map(t => t.textContent);
  return pg;
}

const tests = {
  // A card shows the last change made to it, though the board has the
  // one before it still: Priority clicked on and off again shows off once
  // both are answered, rather than the first, on, for 5 s.
  async 'a card shows the last change made to it'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.equal(pg.priority(), 'false');
    pg.button('priority').click();
    assert.equal(pg.priority(), 'true');
    pg.button('priority').click();
    assert.equal(pg.priority(), 'false');
    await settle();
    assert.deepEqual(pg.posts.map(p => p.body.on), [true, false]);
    pg.posts[0].answer(204);
    await settle();
    assert.equal(pg.priority(), 'false', 'the first change shows while the last is on its way');
    pg.posts[1].answer(204);
    await settle();
    assert.equal(pg.priority(), 'false', 'the change before the last shows');
    pg.send(board(card()));
    await settle();
    assert.equal(pg.priority(), 'false', 'the change before the last shows on a new board');

    // Moved to Parked and back to Active in a row, the same.
    pg.posts.length = 0;
    const stage = () => pg.$('board').querySelector('.card').closest('.column').getAttribute('data-column');
    assert.equal(stage(), 'yourturn');
    pg.button('move').click();
    pg.$('menu').querySelector('[data-stage="parked"]').click();
    assert.equal(stage(), 'parked');
    pg.button('move').click();
    pg.$('menu').querySelector('[data-stage="active"]').click();
    assert.equal(stage(), 'yourturn');
    await settle();
    assert.deepEqual(pg.posts.map(p => p.body.stage), ['parked', 'yourturn']);
    pg.posts[0].answer(204);
    pg.posts[1].answer(204);
    await settle();
    assert.equal(stage(), 'yourturn', 'the move before the last shows');
  },

  // A 401 for a key the page has replaced since, as when another tab signed
  // in after `board --new-token`, signs nothing out: the change goes back,
  // and a toast says to try again. A 401 for the key in use signs it out.
  async 'a 401 for an old key signs nothing out'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    pg.button('priority').click();
    await settle();
    pg.good = 'k2';
    pg.otherTab('k2');
    await settle();
    assert.deepEqual(pg.streams.map(s => s.key), ['k1', 'k2']);
    pg.send(board(card()));
    await settle();
    assert.equal(pg.posts[0].key, 'k1');
    pg.posts[0].answer(401);
    await settle();
    assert.ok(!pg.streams[1].signal.aborted, 'the new key’s stream was stopped');
    assert.equal(pg.$('board').hidden, false, 'the board was taken away');
    assert.equal(pg.priority(), 'false', 'the change stayed');
    assert.deepEqual(pg.toasts(), ['Signed in again. Try that again.']);

    pg.good = 'k3';
    pg.button('priority').click();
    await settle();
    assert.equal(pg.posts[1].key, 'k2');
    pg.posts[1].answer(401);
    await settle();
    assert.ok(pg.streams[1].signal.aborted, 'signed out, the stream is still open');
    assert.equal(pg.$('board').hidden, true, 'signed out, the board still shows');
    assert.match(pg.$('empty-text').textContent, /^Sign in again/);
  },

  // A change the board doesn't have yet shows for 5 s after it's answered.
  async 'a change shows until the board has it'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    pg.button('priority').click();
    await settle();
    pg.posts[0].answer(204);
    await settle();
    pg.send(board(card()));
    await settle();
    assert.equal(pg.priority(), 'true', 'a board from before the change undid it');
    pg.runTimers(6000);
    assert.equal(pg.priority(), 'false', 'a change the board never had stayed');
  },
};

(async () => {
  let failed = 0;
  for (const [name, test] of Object.entries(tests)) {
    try {
      await test();
    } catch (err) {
      failed++;
      console.error(name + ': ' + (err.stack || err));
    }
  }
  if (failed) {
    console.error(`${failed} of ${Object.keys(tests).length} failed`);
    process.exit(1);
  }
  console.log(`ok: ${Object.keys(tests).length} tests`);
  process.exit(0);
})();
