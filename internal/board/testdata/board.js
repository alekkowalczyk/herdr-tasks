// Runs the board page's script in a stand-in browser, with a stand-in DOM
// and board server, to check what a card shows while its changes are on
// their way, and which 401s sign the page out. Usage: node board.js
// path/to/app.js [scripts it uses, such as path/to/search.js]
'use strict';

const assert = require('assert/strict');
const fs = require('fs');
const vm = require('vm');

const KEY = 'herdr-tasks-board-key';
const src = fs.readFileSync(process.argv[2], 'utf8');
// The page loads these before app.js.
const before = process.argv.slice(3).map(f => fs.readFileSync(f, 'utf8'));

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
  blur() {
    if (doc.activeElement === this) doc.activeElement = null;
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

// slacked is a board with a card and the Slack line.
const slacked = line => Object.assign(board(card()), {slack: line});

// slackPart is the part of the Slack line with class cls.
const slackPart = (pg, cls) => pg.$('slack-bar').querySelector('.' + cls);

// Two spaces, and a card from a message in #billing in the first.
const SPACES = [
  {id: 'w1', label: 'billing'},
  {id: 'w2', label: 'infra'},
];
const spaced = (cards, spaces) => ({version: '0.10.0', linearKey: false, sessions: [{name: 'work', stale: false, spaces, cards}]});
const slackCard = more =>
  card({space: 'w1', spaceLabel: 'billing', slack: Object.assign({place: '#billing', channel: 'C0123ABCD', research: '', muted: false}, more)});

// seeded is what the next page finds stored besides its key.
let seeded = {};

// page runs the script as a tab opened at path (/work/ unless given) with the
// hash and search, signed in with stored. The server takes only the key in pg.good, and
// answers a request for the board or the list with pg.answers[url], a
// status, or 200 with nothing to read before the stream's own board. With
// tailnet, it's the board's tailnet address, which is https.
function page(stored, path, hash, search, tailnet) {
  doc.body = new Element('body');
  doc.activeElement = null;
  doc.listeners = {};
  const pg = {good: stored, streams: [], posts: [], gets: [], answers: {}, replaced: [], addresses: [], pushed: []};
  const ids = new Map();
  const document = {
    body: doc.body,
    get hidden() {
      return Boolean(pg.hidden);
    },
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

  const storage = new Map([[KEY, stored], ...Object.entries(seeded)]);
  seeded = {};
  const windowListeners = {};
  pg.windows = [];

  // openStream is a board stream the test sends boards on.
  function openStream(s) {
    const chunks = [];
    let wake = null;
    let ended = false;
    s.push = text => {
      chunks.push(new TextEncoder().encode(text));
      if (wake) wake();
    };
    s.end = () => {
      ended = true;
      if (wake) wake();
    };
    s.signal.addEventListener('abort', s.end);
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

  // reply is an answer with status. A 204 has no body; others have body,
  // or an error that says no.
  const reply = (status, body) => {
    if (status === 204) return {ok: true, status};
    const json = async () => body || {error: status === 401 ? 'Sign in again.' : 'No.'};
    return {ok: status < 300, status, json};
  };

  async function fetch(url, opts) {
    const k = ((opts.headers || {}).Authorization || '').replace(/^Bearer /, '');
    if (url.startsWith('/api/board')) {
      pg.gets.push(url);
      const status = k === pg.good ? pg.answers[url] || 200 : 401;
      return {ok: status < 300, status, text: async () => pg.answers[url + ' body'] || ''};
    }
    if (url.startsWith('/api/events')) {
      const s = {url, key: k, signal: opts.signal};
      pg.streams.push(s);
      return k === pg.good ? openStream(s) : reply(401);
    }
    // A change is answered when the test says.
    return new Promise(resolve =>
      pg.posts.push({url, key: k, body: JSON.parse(opts.body), answer: (status, body) => resolve(reply(status, body))}),
    );
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

  // The address: pushState and replaceState change its hash.
  const location = {
    protocol: tailnet ? 'https:' : 'http:',
    hostname: tailnet ? 'devbox.tail0000.ts.net' : '127.0.0.1',
    search: search || '',
    hash: hash || '',
    pathname: path || '/work/',
    replace: url => pg.replaced.push(url),
  };
  pg.location = location;
  pg.history = () => ctx.history;
  const go = url => {
    pg.addresses.push(String(url));
    const i = String(url).indexOf('#');
    location.hash = i < 0 ? '' : String(url).slice(i);
  };

  // The terminals: what each mount of a tab was given, how many times it
  // was focused, and whether it was closed.
  pg.mounts = [];
  const herdrTerm = {
    mount(box, opts) {
      const m = {box, session: opts.session, tab: opts.tab, focused: 0, closed: false};
      pg.mounts.push(m);
      return {close: () => (m.closed = true), focus: () => m.focused++};
    },
  };

  const ctx = {
    window: {
      herdrTerm,
      addEventListener: (type, fn) => (windowListeners[type] = windowListeners[type] || []).push(fn),
      getSelection: () => '',
      open: (url, target, features) => {
        if (pg.blocked) return null;
        const w = {url, target, features, opener: 'the board', closed: false, close: () => (w.closed = true)};
        pg.windows.push(w);
        return w;
      },
    },
    document,
    Element,
    location,
    history: {
      state: null,
      replaceState(state, title, url) {
        this.state = state;
        go(url);
      },
      pushState(state, title, url) {
        this.state = state;
        pg.pushed.push(String(url));
        go(url);
      },
      back() {},
    },
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
  for (const b of before) vm.runInContext(b, ctx);
  vm.runInContext(src, ctx);

  // back goes back to the board, as the browser's Back does from a view.
  pg.back = () => {
    ctx.history.state = null;
    location.hash = '';
    for (const fn of windowListeners.popstate || []) fn({});
  };
  // hide hides the page, or shows it again, as a tab put behind another does.
  pg.hide = on => {
    pg.hidden = on;
    for (const fn of doc.listeners.visibilitychange || []) fn({});
  };
  // send sends a board on the newest stream.
  pg.send = (b, event) => pg.streams[pg.streams.length - 1].push('event: ' + (event || 'board') + '\ndata: ' + JSON.stringify(b) + '\n\n');
  // otherTab stores key, as a tab that signs in does, and tells this one.
  pg.otherTab = key => {
    storage.set(KEY, key);
    for (const fn of windowListeners.storage || []) fn({key: KEY, newValue: key});
  };
  // button is the card's button for act, as it shows now.
  pg.button = act => pg.$('board').querySelector('.card').querySelector('[data-act="' + act + '"]');
  pg.priority = () => pg.button('priority').getAttribute('aria-pressed');
  // menu opens the card's Move to menu, and returns what it offers after
  // the stages, by text. close closes it again.
  pg.menu = () => {
    pg.button('move').click();
    return pg
      .$('menu')
      .querySelectorAll('.menu-item')
      .filter(i => !i.hasAttribute('data-stage'))
      .map(i => i.textContent);
  };
  pg.close = () => pg.button('move').click();
  pg.item = act => pg.$('menu').querySelector('[data-act="' + act + '"]');
  pg.toasts = () => pg.$('toasts').children.map(t => t.textContent);
  pg.query = sel => document.querySelector(sel);
  pg.storage = storage;
  pg.body = doc.body;
  return pg;
}

// sideSpace is the side panel's button for the workspace labelled label.
const sideSpace = (pg, label) =>
  pg
    .$('side-spaces')
    .querySelectorAll('.side-space')
    .find(b => b.querySelector('.side-label').textContent === label);

// sideTask is the side panel's row for the task titled title.
const sideTask = (pg, title) =>
  pg
    .$('side-spaces')
    .querySelectorAll('.side-task')
    .find(r => r.querySelector('.side-title').textContent === title);

// sideRows is what the side panel shows, a line each: a workspace as its
// label and its circle's column ('idle' when hollow), after a * when it's
// picked; each of its tasks as its ! when it's a priority, its glyph and its
// title; and the line counting what the list leaves out.
const sideRows = pg =>
  pg
    .$('side-spaces')
    .children.flatMap(g => {
      const b = g.querySelector('.side-space');
      const dot = b.querySelector('.side-dot').getAttribute('data-column') || 'idle';
      const head = (b.getAttribute('aria-pressed') === 'true' ? '* ' : '') + b.querySelector('.side-label').textContent + ' ' + dot;
      const tasks = g
        .querySelectorAll('.side-task')
        .map(r => ['.side-prio', '.side-glyph', '.side-title'].map(sel => r.querySelector(sel).textContent).filter(Boolean).join(' '));
      const more = g.querySelector('.side-more');
      return [head, ...tasks, ...(more && !more.hidden ? [more.textContent] : [])];
    });

// marked is el's text, with what it marks in [ ].
const marked = el => el.text + el.childNodes.map(n => (n.tag === 'mark' ? '[' + n.textContent + ']' : n.textContent)).join('');

// found is what the search shows, a line each: a place's name, and each
// task found there as its title and, but for the name, where it was found.
const found = pg =>
  pg.$('results-groups').children.flatMap(g => [
    g.querySelector('.result-label').textContent,
    ...g.querySelectorAll('.result').map(r =>
      [r.querySelector('.result-title'), r.querySelector('.result-where')]
        .filter(e => e && !e.hidden)
        .map(marked)
        .join(' · '),
    ),
  ]);

// type types text in the search box, as it is after typing.
const type = (pg, text) => {
  pg.$('search').value = text;
  pg.$('search').dispatch('input');
};

// agentLines is each agent's heading on el, and the line under it, if any.
const agentLines = (el, sel) =>
  el.querySelectorAll(sel).map(a => {
    const sub = a.querySelector('.agent-sub');
    return sub ? [a.querySelector('.agent-title').textContent, sub.textContent] : [a.querySelector('.agent-title').textContent];
  });

// onTailnet is a page on the board's tailnet address, signed in as its
// owner, with a card that links to localhost:4100, and the board shared
// there with shares.
async function onTailnet(shares) {
  const pg = page('k1', '/work/', '', '', true);
  await settle();
  pg.posts.shift().answer(200, {key: 'k1'});
  await settle();
  const b = board(card({links: [{url: 'http://localhost:4100/app?x=1', kind: 'local', label: 'localhost:4100/app'}]}));
  b.tailnet = {address: 'https://devbox.tail0000.ts.net:7391', shares};
  pg.send(b);
  await settle();
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

  // The header's Assistant opens the assistant beside the board, starting
  // it when it isn't running, and shows its tab once the board has it. Esc
  // or × closes it, and the page remembers whether it's open.
  async 'the assistant opens beside the board'() {
    const pg = page('k1');
    await settle();
    const b = board(card());
    pg.send(b);
    await settle();
    const open = pg.$('assistant-open');
    assert.equal(open.hidden, false);
    assert.equal(pg.$('assistant').hidden, true);

    open.click();
    await settle();
    assert.equal(pg.$('assistant').hidden, false);
    assert.equal(open.getAttribute('aria-expanded'), 'true');
    assert.ok(pg.body.classList.contains('assistant-shown'));
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/assistant', {session: 'work'}]]);
    assert.equal(pg.$('assistant-terms').textContent, 'Starting the assistant…');
    // It toggles, and opened again while it starts, it doesn't start twice.
    open.click();
    assert.equal(pg.$('assistant').hidden, true);
    open.click();
    await settle();
    assert.equal(pg.posts.length, 1, 'it started the assistant twice');
    assert.equal(pg.$('assistant-terms').textContent, 'Starting the assistant…');

    pg.posts[0].answer(200, {tab: 'w9:t1'});
    await settle();
    assert.equal(pg.mounts.length, 0, 'it showed a tab the board doesn’t have yet');
    b.sessions[0].assistant = {tab: 'w9:t1', status: 'idle'};
    pg.send(b);
    await settle();
    assert.deepEqual(pg.mounts.map(m => [m.box, m.session, m.tab, m.focused]), [[pg.$('assistant-terms'), 'work', 'w9:t1', 1]]);
    assert.equal(pg.$('assistant-status').textContent, 'Ready');
    assert.equal(pg.storage.get('herdr-tasks-board-assistant-open'), '1');
    pg.send(b);
    await settle();
    assert.equal(pg.mounts.length, 1, 'a new board mounted it again');

    // Esc on the board closes it, and the page forgets it.
    pg.$('board').dispatch('keydown', {key: 'Escape'});
    assert.equal(pg.$('assistant').hidden, true);
    assert.equal(open.getAttribute('aria-expanded'), 'false');
    assert.ok(pg.mounts[0].closed, 'its terminal stayed open');
    assert.equal(pg.storage.get('herdr-tasks-board-assistant-open'), '');

    // Running, it opens without starting it again; × closes it.
    open.click();
    await settle();
    assert.equal(pg.posts.length, 1, 'it started a running assistant');
    assert.equal(pg.mounts.length, 2);
    pg.$('assistant-close').click();
    assert.equal(pg.$('assistant').hidden, true);
    assert.ok(pg.mounts[1].closed);

    // A key typed in its terminal is the terminal's.
    open.click();
    const pane = pg.$('assistant-terms');
    pane.setAttribute('class', 'term-pane');
    pane.dispatch('keydown', {key: 'Escape'});
    assert.equal(pg.$('assistant').hidden, false, 'Esc in its terminal closed it');
  },

  // Ask the assistant, on a card or in its tab's view, opens the assistant
  // with the start of a question about the task typed. What went wrong
  // shows, with the assistant still open, as Claude may ask something.
  async 'asking the assistant about a task'() {
    const pg = page('k1');
    await settle();
    const b = board(card());
    b.sessions[0].assistant = {tab: 'w9:t1', status: 'blocked'};
    pg.send(b);
    await settle();
    pg.menu();
    pg.item('ask').click();
    await settle();
    assert.deepEqual(pg.posts.map(p => p.body), [{session: 'work', about: 'w1:t1'}]);
    assert.equal(pg.$('assistant').hidden, false);
    assert.equal(pg.$('assistant-status').textContent, 'Asks you something');
    assert.equal(pg.mounts.length, 1);
    pg.posts[0].answer(409, {tab: 'w9:t1', error: 'The assistant is waiting for your answer. Answer it first'});
    await settle();
    assert.deepEqual(pg.toasts(), ['The assistant is waiting for your answer. Answer it first']);
    assert.equal(pg.$('assistant').hidden, false);
    assert.equal(pg.mounts.length, 1);
    assert.equal(pg.mounts[0].focused, 2, 'focus didn’t go to its terminal once it answered');

    pg.$('board').querySelector('.card').click();
    pg.$('detail-body').querySelector('[data-act="ask"]').click();
    await settle();
    assert.deepEqual(pg.posts[1].body, {session: 'work', about: 'w1:t1'});
  },

  // An assistant left open shows again when the page opens, but isn't
  // started until you say: Start starts it.
  async 'an assistant left open waits to be started'() {
    seeded = {'herdr-tasks-board-assistant-open': '1'};
    const pg = page('k1');
    await settle();
    const b = board(card());
    pg.send(b);
    await settle();
    assert.equal(pg.$('assistant').hidden, false);
    assert.equal(pg.$('assistant-terms').textContent, 'The assistant isn’t running.');
    assert.equal(pg.$('assistant-start').hidden, false);
    assert.equal(pg.posts.length, 0);
    pg.$('assistant-start').click();
    await settle();
    assert.deepEqual(pg.posts.map(p => p.body), [{session: 'work'}]);
    assert.equal(pg.$('assistant-start').hidden, true, 'Start shows while it starts');

    // Its tab with no Claude running: Start again.
    pg.posts[0].answer(500, {tab: 'w9:t1', error: 'Claude didn’t start in the assistant’s tab: no shell'});
    b.sessions[0].assistant = {tab: 'w9:t1', status: ''};
    pg.send(b);
    await settle();
    assert.equal(pg.mounts.length, 1, 'its tab didn’t show');
    assert.equal(pg.$('assistant-status').textContent, 'Not running');
    assert.equal(pg.$('assistant-start').hidden, false);

    // Wider gives it the page, and the page remembers.
    const wide = pg.$('assistant-wide');
    assert.equal(wide.getAttribute('aria-pressed'), 'false');
    wide.click();
    assert.ok(pg.body.classList.contains('assistant-wide'));
    assert.equal(wide.getAttribute('aria-pressed'), 'true');
    assert.equal(pg.storage.get('herdr-tasks-board-assistant-wide'), '1');
  },

  // Latest lists the tasks by Claude's latest reply instead of the columns:
  // those an agent works on first, then the newest reply, then those with
  // none. Each shows its last reply and how long ago it came. It keeps to
  // the space and to Priority only, a search takes its place, a row opens
  // its view, and the browser remembers it.
  async 'latest lists the tasks by Claude’s latest reply'() {
    const ago = m => new Date(Date.now() - m * 60e3).toISOString();
    const claude = (pane, status, m, reply) => ({pane, agent: 'claude', status, title: '', replyAt: ago(m), lastReply: reply});
    const cards = [
      card({tab: 'w1:t1', title: 'Old reply', column: 'review', space: 'w1', spaceLabel: 'billing', agents: [claude('w1:p1', 'idle', 90, 'Opened the PR.')]}),
      card({tab: 'w1:t2', title: 'No Claude', space: 'w1', spaceLabel: 'billing', agents: [{pane: 'w1:p3', agent: 'codex', status: 'idle', title: ''}]}),
      card({tab: 'w1:t3', title: 'Fresh reply', space: 'w1', spaceLabel: 'billing', agents: [claude('w1:p4', 'done', 20, 'Earlier.'), claude('w1:p5', 'idle', 1, 'The tests pass.')]}),
      card({tab: 'w2:t1', title: 'Busy', column: 'running', space: 'w2', spaceLabel: 'infra', agents: [claude('w2:p1', 'working', 30, 'Running the migration.')]}),
    ];
    const pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    const latest = pg.$('latest');
    assert.equal(latest.hidden, false);
    assert.equal(latest.getAttribute('aria-pressed'), 'false');
    assert.equal(pg.$('results').hidden, true);

    latest.click();
    assert.equal(latest.getAttribute('aria-pressed'), 'true');
    assert.equal(pg.storage.get('herdr-tasks-board-latest'), '1');
    assert.equal(pg.$('board').hidden, true, 'the columns still show');
    assert.equal(pg.$('results').hidden, false);
    assert.deepEqual(found(pg), [
      'Working',
      'Busy · Running the migration.',
      'Latest reply',
      'Fresh reply · The tests pass.',
      'Old reply · Opened the PR.',
      'No reply',
      'No Claude',
    ]);
    const row = title => pg.$('results-groups').querySelectorAll('.result').find(r => r.querySelector('.result-title').textContent === title);
    assert.equal(row('Fresh reply').querySelector('time').textContent, '1m');
    assert.equal(row('Old reply').querySelector('time').textContent, '1h');
    assert.equal(row('No Claude').querySelector('time'), null);
    assert.equal(row('Old reply').getAttribute('data-column'), 'review');

    sideSpace(pg, 'infra').click();
    assert.deepEqual(found(pg), ['Working', 'Busy · Running the migration.']);
    pg.$('side-all').click();
    cards[0].priority = true;
    pg.send(spaced(cards, SPACES));
    await settle();
    pg.$('prio-only').click();
    assert.deepEqual(found(pg), ['Latest reply', 'Old reply · Opened the PR.']);
    pg.$('prio-only').click();

    type(pg, 'busy');
    assert.deepEqual(found(pg), ['Name', '[Busy]']);
    type(pg, '');
    assert.equal(found(pg)[0], 'Working');

    row('Fresh reply').click();
    assert.equal(pg.location.hash, '#tab=w1%3At3');

    seeded = {'herdr-tasks-board-latest': '1'};
    const again = page('k1');
    await settle();
    again.send(spaced(cards, SPACES));
    await settle();
    assert.equal(again.$('board').hidden, true, 'the page forgot Latest');
    assert.equal(found(again)[0], 'Working');
    again.$('latest').click();
    assert.equal(again.$('board').hidden, false);
    assert.equal(again.$('results').hidden, true);
  },

  // Beside a task's view, Esc closes the view, and the assistant only from
  // its column. Wider, the assistant covers the view, so Esc closes it.
  async 'esc beside a view'() {
    seeded = {'herdr-tasks-board-assistant-open': '1', 'herdr-tasks-board-assistant-wide': '1'};
    const pg = page('k1', '/work/', '#tab=w1%3At1');
    await settle();
    const b = board(card());
    b.sessions[0].assistant = {tab: 'w9:t1', status: 'idle'};
    pg.send(b);
    await settle();
    assert.ok(pg.body.classList.contains('viewing'), 'no view opened');
    assert.ok(pg.body.classList.contains('assistant-wide'));
    pg.body.dispatch('keydown', {key: 'Escape'});
    assert.equal(pg.$('assistant').hidden, true, 'Esc left the wide assistant open');
    assert.ok(pg.body.classList.contains('viewing'), 'Esc closed the view behind it');

    pg.$('assistant-open').click();
    pg.$('assistant-wide').click();
    assert.ok(!pg.body.classList.contains('assistant-wide'));
    pg.body.dispatch('keydown', {key: 'Escape'});
    assert.ok(!pg.body.classList.contains('viewing'), 'Esc left the view open');
    assert.equal(pg.$('assistant').hidden, false, 'Esc closed the assistant beside the view');
  },

  // The session list has no assistant.
  async 'the session list has no assistant'() {
    seeded = {'herdr-tasks-board-assistant-open': '1'};
    const pg = page('k1', '/');
    await settle();
    pg.send({sessions: [{name: 'work', cards: 2, stale: false}, {name: 'side', cards: 1, stale: false}]}, 'sessions');
    await settle();
    assert.equal(pg.$('assistant-open').hidden, true);
    assert.equal(pg.$('assistant').hidden, true);
  },

  // After the stages, the menu offers a move to another workspace, and a
  // Slack task's research and mute, as its research and channel allow. So
  // does the tab's view.
  async 'a Slack card’s menu has its actions'() {
    const pg = page('k1');
    await settle();
    const offers = (c, spaces, want) => {
      pg.send(spaced([c], spaces || SPACES));
      return settle().then(() => {
        assert.deepEqual(pg.menu(), want, JSON.stringify(c.slack));
        const sep = pg.$('menu').querySelector('[role="separator"]');
        assert.equal(Boolean(sep), want.length > 0, 'a separator only before more items');
        pg.close();
        assert.equal(pg.$('menu').hidden, true);
      });
    };
    const ask = 'Ask the assistant';
    await offers(slackCard({research: ''}), null, ['Move to workspace…', 'Start research', 'Mute #billing', ask]);
    await offers(slackCard({research: 'held'}), null, ['Move to workspace…', 'Start research', 'Mute #billing', ask]);
    await offers(slackCard({research: 'failed'}), null, ['Move to workspace…', 'Start research', 'Mute #billing', ask]);
    await offers(slackCard({research: 'locked'}), null, ['Move to workspace…', 'Continue with my tools', 'Mute #billing', ask]);
    await offers(slackCard({research: 'open'}), null, ['Move to workspace…', 'Mute #billing', ask]);
    await offers(slackCard({research: 'locked', muted: true}), null, ['Move to workspace…', 'Continue with my tools', ask]);
    // A plain card: the move and the assistant, and only the assistant in a
    // session of one space.
    await offers(card({space: 'w1', spaceLabel: 'billing'}), null, ['Move to workspace…', ask]);
    await offers(card({space: 'w1', spaceLabel: 'billing'}), [SPACES[0]], [ask]);

    // The tab's view has the same buttons.
    pg.send(spaced([slackCard({research: 'locked'})], SPACES));
    await settle();
    pg.$('board').querySelector('.card').click();
    const acts = pg
      .$('detail-body')
      .querySelector('.detail-actions')
      .querySelectorAll('button')
      .map(b => b.textContent);
    assert.deepEqual(acts.slice(-4), ['Move to workspace…', 'Continue with my tools', 'Mute #billing', 'Ask the assistant']);
  },

  // Picking a workspace moves the card there. An open view follows it to
  // its new tab.
  async 'picking a workspace moves the card'() {
    const pg = page('k1');
    await settle();
    pg.send(spaced([card({space: 'w1', spaceLabel: 'billing'})], SPACES));
    await settle();
    pg.menu();
    pg.item('spaces').click();
    const items = pg.$('menu').querySelectorAll('.menu-item');
    assert.deepEqual(
      items.map(i => i.textContent),
      ['infra'],
    );
    items[0].click();
    await settle();
    assert.equal(pg.$('menu').hidden, true);
    assert.deepEqual(pg.posts.map(p => p.body), [{session: 'work', tab: 'w1:t1', action: 'move', workspace: 'w2'}]);
    assert.deepEqual(pg.toasts(), ['Moving to infra…']);
    pg.posts[0].answer(200, {tab: 'w2:t4'});
    await settle();
    assert.deepEqual(pg.toasts(), ['Moving to infra…', 'Moved to infra']);

    // From the tab's view, which then shows the new tab.
    pg.$('board').querySelector('.card').click();
    assert.equal(pg.location.hash, '#tab=w1%3At1');
    pg.$('detail-body').querySelector('[data-act="spaces"]').click();
    pg.$('menu').querySelector('.menu-item').click();
    await settle();
    assert.deepEqual(pg.posts[1].body, {session: 'work', tab: 'w1:t1', action: 'move', workspace: 'w2'});
    pg.posts[1].answer(200, {tab: 'w2:t5'});
    await settle();
    assert.equal(pg.location.hash, '#tab=w2%3At5', 'the view stayed on the old tab');

    // A move that went wrong partway is followed too, and says why.
    pg.send(spaced([card({tab: 'w2:t5', space: 'w2', spaceLabel: 'infra'})], SPACES));
    await settle();
    pg.$('detail-body').querySelector('[data-act="spaces"]').click();
    assert.deepEqual(
      pg.$('menu').querySelectorAll('.menu-item').map(i => i.textContent),
      ['billing'],
    );
    pg.$('menu').querySelector('.menu-item').click();
    await settle();
    pg.posts[2].answer(500, {tab: 'w1:t6', error: 'Moved 1 of 2 panes; the rest are still in the old tab'});
    await settle();
    assert.equal(pg.location.hash, '#tab=w1%3At6');
    assert.equal(pg.toasts().pop(), 'Moved 1 of 2 panes; the rest are still in the old tab');
  },

  // Research and continue say what they're doing, as they can take a
  // while; the card offers neither again until they're answered. Mute says
  // how to undo it.
  async 'Slack actions say how they went'() {
    const pg = page('k1');
    await settle();
    pg.send(spaced([slackCard({research: ''})], SPACES));
    await settle();
    pg.menu();
    pg.item('research').click();
    await settle();
    assert.deepEqual(pg.posts.map(p => p.body), [{session: 'work', tab: 'w1:t1', action: 'research'}]);
    assert.deepEqual(pg.toasts(), ['Starting research…']);
    assert.deepEqual(pg.menu(), ['Mute #billing'], 'a second start, or a question about a task on the move, was offered');
    pg.close();
    pg.posts[0].answer(409, {error: 'Research already started.'});
    await settle();
    assert.deepEqual(pg.toasts(), ['Starting research…', 'Research already started.']);
    assert.deepEqual(pg.menu(), ['Move to workspace…', 'Start research', 'Mute #billing', 'Ask the assistant']);
    pg.close();

    pg.send(spaced([slackCard({research: 'locked'})], SPACES));
    await settle();
    pg.menu();
    pg.item('continue').click();
    await settle();
    assert.deepEqual(pg.posts[1].body, {session: 'work', tab: 'w1:t1', action: 'continue'});
    pg.posts[1].answer(204);
    await settle();
    assert.deepEqual(pg.toasts().slice(-2), ['Continuing with your tools…', 'Continued with your tools']);

    pg.menu();
    pg.item('mute').click();
    await settle();
    assert.deepEqual(pg.posts[2].body, {session: 'work', tab: 'w1:t1', action: 'mute'});
    pg.posts[2].answer(204);
    await settle();
    assert.equal(pg.toasts().pop(), 'Muted #billing. Undo: herdr-tasks slack unmute C0123ABCD');
  },

  // The header shows the Slack watcher's line while the board has one.
  async 'the header shows the Slack line'() {
    const pg = page('k1');
    await settle();
    const bar = pg.$('slack-bar');
    const line = pg.$('slack');
    pg.send(Object.assign(board(card()), {slack: {text: 'Slack: can’t reach Slack since 14:20', problem: true}}));
    await settle();
    assert.equal(bar.hidden, false);
    assert.equal(line.textContent, 'Slack: can’t reach Slack since 14:20');
    assert.ok(line.classList.contains('problem'), 'a problem shows as one');
    assert.equal(slackPart(pg, 'slack-fetch').hidden, true, 'Fetch showed with none offered');
    assert.equal(slackPart(pg, 'slack-skipped').hidden, true, 'the skipped list showed with none');
    pg.send(Object.assign(board(card()), {slack: {text: 'Slack: last check 14:32', problem: false}}));
    await settle();
    assert.equal(line.textContent, 'Slack: last check 14:32');
    assert.ok(!line.classList.contains('problem'), 'the problem stayed');
    pg.send(board(card()));
    await settle();
    assert.equal(bar.hidden, true, 'the line stayed with the watcher off');
  },

  // Fetch fetches, one at a time, and the reply shows next to it for 15 s,
  // or the error, as a problem.
  async 'the Slack line fetches and says what it did'() {
    const pg = page('k1');
    await settle();
    const fetch = slackPart(pg, 'slack-fetch');
    const said = slackPart(pg, 'slack-said');
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true}));
    await settle();
    assert.equal(fetch.hidden, false, 'no Fetch with one offered');
    assert.equal(fetch.textContent, 'Fetch');
    fetch.click();
    fetch.click();
    await settle();
    assert.deepEqual(
      pg.posts.map(p => [p.url, p.body]),
      [['/api/slack/fetch', {}]],
      'a second fetch was posted while one ran',
    );
    assert.equal(fetch.textContent, 'Fetching…');
    assert.equal(fetch.disabled, true);
    // A new board meanwhile keeps it.
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true}));
    await settle();
    assert.equal(fetch.textContent, 'Fetching…', 'a new board forgot the fetch');
    pg.posts[0].answer(200, {text: 'Fetched from 08:30: 2 new tasks, 2 with research.'});
    await settle();
    assert.equal(fetch.textContent, 'Fetch');
    assert.equal(fetch.disabled, false);
    assert.equal(said.hidden, false);
    assert.equal(said.textContent, 'Fetched from 08:30: 2 new tasks, 2 with research.');
    assert.ok(!said.classList.contains('problem'));
    pg.runTimers(14000);
    assert.equal(said.hidden, false, 'the reply went before 15 s');
    pg.runTimers(16000);
    assert.equal(said.hidden, true, 'the reply stayed after 15 s');

    fetch.click();
    await settle();
    pg.posts[1].answer(502, {error: 'can’t fetch: waiting for herdr session default. Set slack_session in config.toml to use another'});
    await settle();
    assert.equal(said.textContent, 'can’t fetch: waiting for herdr session default. Set slack_session in config.toml to use another');
    assert.ok(said.classList.contains('problem'), 'the error isn’t a problem');
    fetch.click();
    await settle();
    assert.equal(said.hidden, true, 'the last reply showed during a new fetch');
    pg.posts[2].answer(409, {error: 'A fetch is already running.'});
    await settle();
    assert.equal(said.textContent, 'A fetch is already running.');

    // Not offered, there's no Fetch.
    pg.send(slacked({text: 'Slack: last check 14:32', problem: false, fetch: false}));
    await settle();
    assert.equal(fetch.hidden, true);
  },

  // "n skipped" opens the skipped asks, each with its label and why as
  // text, Take and Open in Slack.
  async 'the skipped list takes an ask'() {
    const pg = page('k1');
    await settle();
    const off = slackPart(pg, 'slack-skipped');
    const said = slackPart(pg, 'slack-said');
    const asks = [
      {id: 'D0RUI0001/1793370600.000100', label: 'Oct 30 14:30 DM · Rui Costa: <b>book club</b> tonight?', why: 'off-topic: a book club', link: 'https://acme.slack.com/archives/D0RUI0001/p1793370600000100'},
      {id: 'C0123ABCD/1793192340.000100', label: 'Oct 28 12:59 #billing · Dana Reed: lunch on Friday', why: '<img src=x>', link: 'javascript:alert(1)'},
    ];
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true, skipped: asks}));
    await settle();
    assert.equal(off.hidden, false);
    assert.equal(off.textContent, '2 skipped');
    off.click();
    const menu = pg.$('menu');
    assert.equal(menu.hidden, false, 'the list didn’t open');
    assert.equal(off.getAttribute('aria-expanded'), 'true');
    assert.equal(menu.getAttribute('aria-label'), 'Skipped asks');
    assert.equal(menu.querySelector('.menu-label').textContent, 'Skipped asks');
    const rows = menu.querySelectorAll('.menu-ask');
    assert.deepEqual(
      rows.map(r => [r.querySelector('.ask-label').textContent, r.querySelector('.ask-why').textContent]),
      asks.map(a => [a.label, a.why]),
    );
    assert.equal(rows[0].querySelector('.ask-label').children.length, 0, 'a label made elements');
    const open = rows[0].querySelector('a');
    assert.equal(open.textContent, 'Open in Slack');
    assert.equal(open.getAttribute('href'), asks[0].link);
    assert.equal(open.getAttribute('target'), '_blank');
    assert.equal(open.getAttribute('rel'), 'noopener noreferrer');
    assert.equal(rows[1].querySelector('a'), null, 'a link that isn’t http opens');
    assert.equal(doc.activeElement && doc.activeElement.textContent, 'Take', 'the first Take has no focus');

    // A new board waits while the list is open.
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true, skipped: asks.slice(1)}));
    await settle();
    assert.equal(menu.querySelectorAll('.menu-ask').length, 2);

    rows[0].querySelector('[data-act="take"]').click();
    await settle();
    assert.equal(menu.hidden, true, 'the list stayed open');
    assert.equal(off.textContent, '1 skipped', 'the board waiting was never drawn');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/slack/take', {id: asks[0].id}]]);
    assert.equal(said.textContent, 'Taking it…');
    pg.posts[0].answer(200, {text: 'Took it: new tab w2:t1, with research'});
    await settle();
    assert.equal(said.textContent, 'Took it: new tab w2:t1, with research');
    assert.ok(!said.classList.contains('problem'));

    off.click();
    pg.$('menu').querySelector('[data-act="take"]').click();
    await settle();
    pg.posts[1].answer(404, {error: 'That ask isn’t on the skipped list any more.'});
    await settle();
    assert.equal(said.textContent, 'That ask isn’t on the skipped list any more.');
    assert.ok(said.classList.contains('problem'));

    // Open in Slack closes the list, and leaves the link to the browser.
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true, skipped: asks}));
    await settle();
    off.click();
    let prevented = false;
    pg.$('menu')
      .querySelector('[data-act="openslack"]')
      .dispatch('click', {preventDefault: () => (prevented = true)});
    assert.equal(pg.$('menu').hidden, true, 'Open in Slack left the list open');
    assert.equal(prevented, false, 'Open in Slack didn’t leave the link to the browser');
    assert.equal(pg.posts.length, 2);

    // Space opens it too, as Enter does: a link opens on Enter only. Take
    // is a button, which Space clicks by itself.
    off.click();
    const link = pg.$('menu').querySelector('[data-act="openslack"]');
    let opened = 0;
    link.addEventListener('click', () => opened++);
    const take = pg.$('menu').querySelector('[data-act="take"]');
    let spaced = false;
    take.dispatch('keydown', {key: ' ', preventDefault: () => (spaced = true)});
    assert.equal(spaced, false, 'Space on Take was kept from the button');
    assert.equal(pg.posts.length, 2, 'Space on Take took it twice');
    link.focus();
    link.dispatch('keydown', {key: ' ', preventDefault: () => (spaced = true)});
    assert.equal(opened, 1, 'Space didn’t open the link');
    assert.equal(spaced, true, 'Space on the link scrolled the page');
    assert.equal(pg.$('menu').hidden, true, 'Space on the link left the list open');

    // With none, there's no list.
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true}));
    await settle();
    assert.equal(off.hidden, true, 'the list showed with none');
  },

  // A fetch while a take is under way leaves its Taking it…, and the
  // fetch's reply gives way to it again after 15 s.
  async 'a fetch leaves the take under way showing'() {
    const pg = page('k1');
    await settle();
    const fetch = slackPart(pg, 'slack-fetch');
    const said = slackPart(pg, 'slack-said');
    const ask = {id: 'D0RUI0001/1793370600.000100', label: 'Oct 30 14:30 DM · Rui Costa: book club tonight?', why: 'off-topic: a book club', link: 'https://acme.slack.com/archives/D0RUI0001/p1793370600000100'};
    pg.send(slacked({text: 'Slack: off', problem: false, fetch: true, skipped: [ask]}));
    await settle();
    slackPart(pg, 'slack-skipped').click();
    pg.$('menu').querySelector('[data-act="take"]').click();
    await settle();
    assert.equal(said.textContent, 'Taking it…');
    fetch.click();
    await settle();
    assert.deepEqual(pg.posts.map(p => p.url), ['/api/slack/take', '/api/slack/fetch']);
    assert.equal(said.hidden, false, 'the fetch hid the take under way');
    assert.equal(said.textContent, 'Taking it…', 'the fetch wiped the take under way');
    pg.posts[1].answer(200, {text: 'Fetched from 08:30: nothing new.'});
    await settle();
    assert.equal(said.textContent, 'Fetched from 08:30: nothing new.');
    pg.runTimers(16000);
    assert.equal(said.hidden, false, 'the take under way went with the fetch’s reply');
    assert.equal(said.textContent, 'Taking it…');
    pg.posts[0].answer(200, {text: 'Took it: new tab w2:t1, with research'});
    await settle();
    assert.equal(said.textContent, 'Took it: new tab w2:t1, with research');
    pg.runTimers(16000);
    assert.equal(said.hidden, true, 'the take’s reply stayed after 15 s');
  },

  // The board the GET answers with shows at once, before the stream sends
  // anything.
  async 'the board loaded first shows'() {
    const pg = page('k1');
    pg.answers['/api/board?session=work body'] = JSON.stringify(board(card({title: 'Loaded by the GET'})));
    await settle();
    assert.equal(pg.$('board').hidden, false);
    assert.deepEqual(pg.$('board').querySelectorAll('.card-title').map(e => e.textContent), ['Loaded by the GET']);
    assert.equal(pg.streams.length, 1);
  },

  // The page asks for its own session's board and events, by the session in
  // its path, encoded.
  async 'a board page asks for its own session'() {
    let pg = page('k1');
    await settle();
    assert.deepEqual(pg.gets, ['/api/board?session=work']);
    assert.equal(pg.streams[0].url, '/api/events?session=work');
    assert.equal(pg.$('session-name').textContent, 'work');
    assert.equal(pg.$('session-name').hidden, false);
    pg = page('k1', '/my%20work/');
    await settle();
    assert.deepEqual(pg.gets, ['/api/board?session=my%20work']);
    assert.equal(pg.streams[0].url, '/api/events?session=my%20work');
  },

  // Signing in takes the code out of the address and leaves the path and
  // the view where they are.
  async 'signing in keeps the path'() {
    const pg = page(null, '/work/', '#tab=w1%3At1', '?code=abc');
    await settle();
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/session', {code: 'abc'}]]);
    pg.good = 'k9';
    pg.posts[0].answer(200, {key: 'k9'});
    await settle();
    assert.deepEqual(pg.addresses, ['/work/#tab=w1%3At1']);
    assert.deepEqual(pg.streams.map(s => s.key), ['k9']);
  },

  // The server may not have read a session yet, as when the board opens its
  // page at once: the page asks again every 2 s, and says nothing, for 10 s.
  async 'a session not read yet is asked for again'() {
    const pg = page('k1');
    pg.answers['/api/board?session=work'] = 404;
    await settle();
    for (let i = 0; i < 5; i++) {
      assert.equal(pg.gets.length, i + 1);
      assert.doesNotMatch(pg.$('empty-text').textContent, /isn’t running/, 'the page gave up after ' + (i + 1));
      pg.runTimers(3000);
      await settle();
    }
    assert.equal(pg.gets.length, 6);
    assert.equal(pg.streams.length, 0);
    assert.match(pg.$('empty-text').textContent, /^This session isn’t running\./);
    assert.equal(pg.$('empty-text').querySelector('a').getAttribute('href'), '/');
    assert.equal(pg.$('board').hidden, true);

    // Read in time, it shows the board.
    const late = page('k1');
    late.answers['/api/board?session=work'] = 404;
    await settle();
    delete late.answers['/api/board?session=work'];
    late.runTimers(3000);
    await settle();
    late.send(board(card()));
    await settle();
    assert.equal(late.$('board').hidden, false);
    assert.equal(late.$('board').querySelectorAll('.card').length, 1);
  },

  // When the session stops, its stream ends, and the page, after trying
  // again for 10 s, says it isn't running.
  async 'a stopped session ends its page'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.equal(pg.$('board').hidden, false);
    pg.answers['/api/board?session=work'] = 404;
    pg.streams[0].end();
    await settle();
    for (let i = 0; i < 6; i++) {
      pg.runTimers(3000);
      await settle();
    }
    assert.match(pg.$('empty-text').textContent, /^This session isn’t running\./);
    assert.equal(pg.$('empty-text').querySelector('a').getAttribute('href'), '/');
    assert.equal(pg.$('board').hidden, true);
  },

  // At /, the page lists the running sessions, each a link to its board, with
  // a card count, and marks the one Slack asks go to. A session without a page
  // has none.
  async 'the list links to each session'() {
    const pg = page('k1', '/');
    await settle();
    assert.deepEqual(pg.gets, ['/api/board']);
    assert.equal(pg.streams[0].url, '/api/events');
    assert.equal(pg.$('session-name').textContent, '');
    const list = {
      version: '0.11.0',
      slack: 'work',
      sessions: [
        {name: 'work', cards: 2, stale: false},
        {name: 'a b', cards: 1, stale: true},
        {name: 'api', cards: 0, stale: false},
      ],
    };
    pg.send(list, 'sessions');
    await settle();
    const rows = pg.$('session-list').querySelectorAll('li');
    assert.deepEqual(
      rows.map(r => r.textContent),
      ['work2 cardsSlack asks come here', 'a b1 cardstale', 'api0 cards'],
    );
    assert.deepEqual(
      rows.map(r => (r.querySelector('a') ? r.querySelector('a').getAttribute('href') : null)),
      ['/work/', '/a%20b/', null],
    );
    assert.equal(pg.$('session-list').hidden, false);
    assert.equal(pg.$('empty').hidden, true);
    assert.deepEqual(pg.replaced, []);

    // A change updates the rows.
    list.sessions[0].cards = 3;
    pg.send(list, 'sessions');
    await settle();
    assert.equal(pg.$('session-list').querySelectorAll('li')[0].textContent, 'work3 cardsSlack asks come here');

    // With one session, the page goes to its board.
    list.sessions = [{name: 'side', cards: 1, stale: false}];
    pg.send(list, 'sessions');
    await settle();
    assert.deepEqual(pg.replaced, ['/side/']);

    // With Slack off, no session gets asks.
    delete list.slack;
    list.sessions = [
      {name: 'work', cards: 2, stale: false},
      {name: 'side', cards: 1, stale: false},
    ];
    pg.send(list, 'sessions');
    await settle();
    assert.deepEqual(
      pg.$('session-list').querySelectorAll('li').map(r => r.textContent),
      ['work2 cards', 'side1 card'],
    );

    // With none, it says so.
    const none = page('k1', '/');
    await settle();
    none.send({version: '0.11.0', sessions: []}, 'sessions');
    await settle();
    assert.equal(none.$('empty-text').textContent, 'No herdr session is running.');
    assert.equal(none.$('session-list').hidden, true);
  },

  // A view address from before, /#tab=<session>/<tab>, goes to the session's
  // page, with the tab as the new address has it. On the session's own page,
  // the old address opens the view too; another session's doesn't.
  async 'an old view address still works'() {
    let pg = page('k1', '/', '#tab=side/w1%3At1');
    await settle();
    assert.deepEqual(pg.replaced, ['/side/#tab=w1%3At1']);
    assert.deepEqual(pg.gets, [], 'the list loaded after the page moved on');

    pg = page('k1', '/work/', '#tab=work/w1%3At1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.ok(doc.body.classList.contains('viewing'), 'no view opened');
    assert.equal(pg.$('detail-title').textContent, 'Fix the ABC-12 login loop');

    pg = page('k1', '/work/', '#tab=side/w1%3At1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.ok(!doc.body.classList.contains('viewing'), 'a view opened');
    assert.deepEqual(pg.replaced, []);

    pg = page('k1', '/work/', '#tab=w1%3At1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.ok(doc.body.classList.contains('viewing'), 'no view opened');
  },

  // With Slack asks going to a session that isn't running, a session's page
  // says so where the Slack line goes, with no buttons.
  async 'a session shows why Slack asks aren’t on it'() {
    const pg = page('k1');
    await settle();
    const waiting = 'Slack asks go to herdr session default, which isn’t running. Set slack_session in config.toml to use another.';
    pg.send(Object.assign(board(card()), {slackWaiting: waiting}));
    await settle();
    assert.equal(pg.$('slack-bar').hidden, false);
    assert.equal(pg.$('slack').textContent, waiting);
    assert.equal(slackPart(pg, 'slack-fetch').hidden, true);
    pg.send(board(card()));
    await settle();
    assert.equal(pg.$('slack-bar').hidden, true);
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

  // A local page opens in a window made at the click, which goes to the
  // address the board gives, and closes if it can't.
  async 'a page opens in a window opened first'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card({links: [{url: 'file:///home/dana/acme/report/report.html', kind: 'file', label: 'report.html', path: '/home/dana/acme/report/report.html'}]})));
    await settle();
    const chip = () => pg.$('board').querySelector('[data-act="openfile"]');
    chip().click();
    assert.equal(pg.windows.length, 1, 'no window opened at the click');
    assert.deepEqual([pg.windows[0].url, pg.windows[0].target], ['', '_blank']);
    assert.equal(pg.windows[0].opener, null, 'the window can reach the board');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/open', {session: 'work', tab: 'w1:t1', url: 'file:///home/dana/acme/report/report.html'}]]);
    assert.equal(pg.windows[0].location, undefined, 'it went somewhere before the board answered');
    pg.posts[0].answer(200, {href: '/-/page/0123456789abcdef0123456789abcdef/report.html'});
    await settle();
    assert.equal(pg.windows[0].location, '/-/page/0123456789abcdef0123456789abcdef/report.html');
    assert.equal(pg.windows[0].closed, false);

    chip().click();
    await settle();
    pg.posts[1].answer(404, {error: 'That file is gone.'});
    await settle();
    assert.equal(pg.windows[1].closed, true, 'the window stayed after a failure');
    assert.equal(pg.windows[1].location, undefined);
    assert.deepEqual(pg.toasts(), ['That file is gone.']);
  },

  // On the tailnet, a localhost link asks in the page before its port is
  // shared. Share opens the window at the click, and sends it to the
  // address the board gives once it has shared the port.
  async 'a localhost link asks before it’s shared'() {
    const pg = await onTailnet([]);
    const chip = () => pg.$('board').querySelector('[data-act="openlocal"]');
    const dialog = pg.query('dialog');
    chip().click();
    assert.ok(dialog.hasAttribute('open'), 'nothing asked');
    assert.equal(dialog.querySelector('.sheet-title').textContent, 'Share localhost:4100 with your tailnet?');
    assert.equal(dialog.querySelector('.sheet-note').textContent, 'Anyone in your tailnet can open it while it’s shared.');
    assert.equal(pg.windows.length + pg.posts.length, 0, 'it went ahead before you said so');
    const [cancel, share] = dialog.querySelectorAll('button');
    cancel.click();
    assert.ok(!dialog.hasAttribute('open'));
    assert.equal(pg.windows.length + pg.posts.length, 0, 'Cancel shared it');

    chip().click();
    share.click();
    assert.ok(!dialog.hasAttribute('open'));
    assert.equal(pg.windows.length, 1, 'no window opened at the click');
    assert.equal(pg.windows[0].opener, null, 'the window can reach the board');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/share', {session: 'work', tab: 'w1:t1', url: 'http://localhost:4100/app?x=1'}]]);
    pg.posts[0].answer(200, {href: 'https://devbox.tail0000.ts.net:4100/app?x=1'});
    await settle();
    assert.equal(pg.windows[0].location, 'https://devbox.tail0000.ts.net:4100/app?x=1');

    chip().click();
    share.click();
    pg.posts[1].answer(409, {error: 'Port 4100 is already shared with your tailnet for something else.'});
    await settle();
    assert.equal(pg.windows[1].closed, true, 'the window stayed after a refusal');
    assert.deepEqual(pg.toasts(), ['Port 4100 is already shared with your tailnet for something else.']);
  },

  // A port herdr-tasks shares opens at once, at the tailnet address, and
  // the header lists it with Stop.
  async 'a shared port opens at once, and Stop stops it'() {
    const pg = await onTailnet([{port: 4100, since: '2026-11-01T09:00:00Z'}]);
    pg.$('board').querySelector('[data-act="openlocal"]').click();
    assert.ok(!pg.query('dialog').hasAttribute('open'), 'it asked again');
    assert.deepEqual(
      pg.windows.map(w => [w.url, w.target, w.features]),
      [['https://devbox.tail0000.ts.net:4100/app?x=1', '_blank', 'noopener,noreferrer']],
    );
    assert.equal(pg.posts.length, 0);

    const shares = pg.query('.shares');
    assert.ok(!shares.hidden, 'the header doesn’t list the share');
    assert.equal(shares.querySelector('.share-port').textContent, 'localhost:4100');
    const stop = shares.querySelector('.share-stop');
    stop.click();
    assert.ok(stop.disabled, 'Stop can be clicked again while it stops');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/unshare', {port: 4100}]]);
    pg.posts[0].answer(404, {error: 'herdr-tasks isn’t sharing port 4100.'});
    await settle();
    assert.ok(!stop.disabled);
    assert.deepEqual(pg.toasts(), ['herdr-tasks isn’t sharing port 4100.']);

    const b = board(card({links: [{url: 'http://localhost:4100/app?x=1', kind: 'local', label: 'localhost:4100/app'}]}));
    b.tailnet = {address: 'https://devbox.tail0000.ts.net:7391'};
    pg.send(b);
    await settle();
    assert.ok(shares.hidden, 'the header lists a share that stopped');
  },

  // Through the tailnet, the owner is signed in again after the server
  // restarts: the page asks for a key without a code, once, before it says
  // to sign in. A refusal, or a key that doesn't work either, ends it there.
  async 'the owner is signed in again after a restart'() {
    const signedOut = /^Sign in again/;
    let pg = await onTailnet([]);
    // restart has the server start again with key good, and the stream end.
    const restart = async good => {
      pg.good = good;
      pg.streams[pg.streams.length - 1].end();
      await settle();
      pg.runTimers(2000);
      await settle();
    };
    await restart('k2');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/session', {}]]);
    assert.doesNotMatch(pg.$('empty-text').textContent, signedOut);
    pg.posts.shift().answer(200, {key: 'k2'});
    await settle();
    assert.equal(pg.streams[pg.streams.length - 1].key, 'k2');
    pg.send(board(card()));
    await settle();
    assert.equal(pg.$('board').hidden, false);
    assert.doesNotMatch(pg.$('empty-text').textContent, signedOut);

    // Back on the board, the next restart asks again. Refused, it's signed out.
    await restart('k3');
    assert.deepEqual(pg.posts.map(p => [p.url, p.body]), [['/api/session', {}]]);
    pg.posts.shift().answer(401, {error: 'Sign in again.'});
    await settle();
    assert.match(pg.$('empty-text').textContent, signedOut);
    pg.runTimers(60000);
    await settle();
    assert.equal(pg.posts.length, 0, 'it asked again');

    // A key that doesn't work either: signed out, and no more asking.
    pg = await onTailnet([]);
    await restart('k2');
    pg.posts.shift().answer(200, {key: 'k9'});
    await settle();
    assert.match(pg.$('empty-text').textContent, signedOut);
    pg.runTimers(60000);
    await settle();
    assert.equal(pg.posts.length, 0, 'it asked again');
  },

  // On 127.0.0.1, a localhost link is a link, as it was.
  async 'a localhost link on 127.0.0.1 is a link'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card({links: [{url: 'http://localhost:4100/app?x=1', kind: 'local', label: 'localhost:4100/app'}]})));
    await settle();
    const chip = pg.$('board').querySelector('.chip');
    assert.equal(chip.tag, 'a');
    assert.equal(chip.getAttribute('href'), 'http://localhost:4100/app?x=1');
    assert.equal(pg.$('board').querySelector('[data-act="openlocal"]'), null);
  },

  // A GitHub pull request's chip says PR and its number, and the card shows
  // it under GitHub, after Linear and before the other links.
  async 'a pull request shows in its own group'() {
    const pg = page('k1');
    await settle();
    const pr = {url: 'https://github.com/acme/web/pull/12', kind: 'github-pr', label: 'PR #12', pr: {repo: 'acme/web', number: 12}};
    const other = {url: 'https://example.com/spec', kind: 'other', label: 'example.com'};
    const issue = {url: 'https://linear.app/acme/issue/ABC-12', kind: 'linear', label: 'ABC-12', linear: {id: 'ABC-12'}};
    pg.send(board(card({links: [other, pr, issue]})));
    await settle();
    const groups = pg.$('board').querySelectorAll('.link-row').map(r => r.getAttribute('aria-label'));
    assert.deepEqual(groups, ['Linear', 'GitHub', 'Links']);
    const chip = pg.$('board').querySelector('.chip.github');
    assert.ok(chip, 'no pull request chip');
    assert.equal(chip.tag, 'a');
    assert.equal(chip.getAttribute('href'), pr.url);
    assert.equal(chip.textContent, 'PR#12');
    assert.equal(chip.getAttribute('title'), 'acme/web#12\n' + pr.url);
    assert.equal(chip.getAttribute('aria-label'), 'Pull request acme/web#12');

    // The tab's view says which repository each is in, and the label you
    // gave one.
    const named = Object.assign({}, pr, {url: 'https://github.com/acme/api/pull/3', label: 'Retry fix', pr: {repo: 'acme/api', number: 3}});
    pg.send(board(card({links: [pr, named]})));
    await settle();
    pg.$('board').querySelector('.card').click();
    const rows = pg.$('detail-body').querySelectorAll('.d-link').map(r => r.querySelector('.url').textContent);
    assert.deepEqual(rows, ['acme/web', 'Retry fix · acme/api']);
    assert.equal(pg.$('board').querySelectorAll('.chip.github')[1].getAttribute('title'), 'Retry fix\nacme/api#3\n' + named.url);
  },

  // A blocked window says so, and nothing is asked.
  async 'a blocked page window says so'() {
    const pg = page('k1');
    await settle();
    pg.send(board(card({links: [{url: 'file:///home/dana/acme/report/report.html', kind: 'file', label: 'report.html', path: '/home/dana/acme/report/report.html'}]})));
    await settle();
    pg.blocked = true;
    pg.$('board').querySelector('[data-act="openfile"]').click();
    await settle();
    assert.equal(pg.posts.length, 0, 'asked the board anyway');
    assert.equal(pg.toasts().length, 1);
    assert.match(pg.toasts()[0], /blocked/);
  },

  // An agent's pane shows its name in herdr, with the agent's title under
  // it; a pane without a name, or named as its title, shows the title alone,
  // as before. The task's view shows the same.
  async 'a card shows the name of each agent pane'() {
    const pg = page('k1');
    await settle();
    const agents = [
      {pane: 'w1:p1', agent: 'claude', status: 'done', name: 'Card audit', title: 'Payments platform audit', recap: 'The plan.'},
      {pane: 'w1:p2', agent: 'claude', status: 'idle', name: 'Login', title: 'Login'},
      {pane: 'w1:p3', agent: 'codex', status: 'idle', title: 'Fix the build'},
      {pane: 'w1:p4', agent: 'claude', status: 'idle', name: 'Scratch', title: ''},
    ];
    pg.send(board(card({agents})));
    await settle();
    const want = [['Card audit', 'Payments platform audit'], ['Login'], ['Fix the build'], ['Scratch']];
    assert.deepEqual(agentLines(pg.$('board').querySelector('.card'), '.agent'), want);
    pg.$('board').querySelector('.card').click();
    assert.deepEqual(agentLines(pg.$('detail-body'), '.d-agent'), want);
  },

  // The side panel lists every workspace, as herdr's sidebar does: its
  // circle has the colour of its most urgent task (asking, then your turn,
  // then running) and is hollow when none needs you. Under it are its tasks,
  // priority first, then by column, newest first, each with its column's
  // glyph. Paused, parked and done tasks are left out unless they're a
  // priority, and counted on a line under the list. It follows new boards,
  // and goes when the session does.
  async 'the side panel lists each workspace and its tasks'() {
    const pg = page('k1');
    await settle();
    const billing = more => card(Object.assign({space: 'w1', spaceLabel: 'billing'}, more));
    const cards = [
      billing({tab: 'w1:t1', title: 'Older', since: '2026-10-30T09:00:00Z'}),
      billing({tab: 'w1:t2', title: 'Newer', since: '2026-10-31T09:00:00Z'}),
      billing({tab: 'w1:t3', title: 'Asks', column: 'asking', agentStatus: 'blocked'}),
      billing({tab: 'w1:t4', title: 'Urgent', column: 'review', stage: 'review', priority: true}),
      billing({tab: 'w1:t5', title: 'Shipped', column: 'done', stage: 'done'}),
      billing({tab: 'w1:t6', title: 'Shelved', column: 'parked', stage: 'parked'}),
      billing({tab: 'w1:t7', title: 'Waiting', column: 'paused', stage: 'paused', priority: true}),
      card({tab: 'w2:t1', title: 'Infra work', space: 'w2', spaceLabel: 'infra', column: 'running', agentStatus: 'working'}),
      card({tab: 'w3:t1', title: 'Old docs', space: 'w3', spaceLabel: 'docs', column: 'done', stage: 'done'}),
    ];
    const spaces = SPACES.concat([
      {id: 'w3', label: 'docs'},
      {id: 'w4', label: 'scratch'},
    ]);
    pg.send(spaced(cards, spaces));
    await settle();
    assert.equal(pg.$('side').hidden, false);
    assert.equal(pg.$('side-all').getAttribute('aria-pressed'), 'true');
    assert.deepEqual(sideRows(pg), [
      'billing asking',
      '! ◐ Urgent',
      '! ‖ Waiting',
      '? Asks',
      '● Newer',
      '● Older',
      '1 parked · 1 done',
      'infra running',
      '↻ Infra work',
      'docs idle',
      '1 done',
      'scratch idle',
    ]);
    assert.equal(sideTask(pg, 'Asks').getAttribute('data-column'), 'asking');

    const moved = cards.slice();
    moved[2] = billing({tab: 'w1:t3', title: 'Asks', column: 'done', stage: 'done'});
    pg.send(spaced(moved, spaces));
    await settle();
    assert.deepEqual(sideRows(pg).slice(0, 6), ['billing yourturn', '! ◐ Urgent', '! ‖ Waiting', '● Newer', '● Older', '1 parked · 2 done']);

    pg.send({version: '0.10.0', linearKey: false, sessions: []});
    await settle();
    assert.equal(pg.$('side').hidden, true, 'the panel stayed for a session that went');
  },

  // A workspace in the side panel shows only its tasks on the board; it
  // again, or All, shows every workspace. The browser keeps the choice. From
  // a task's view, a workspace goes back to the board, showing it.
  async 'a workspace in the side panel shows only its tasks'() {
    const cards = [
      card({tab: 'w1:t1', title: 'Billing fix', space: 'w1', spaceLabel: 'billing'}),
      card({tab: 'w2:t1', title: 'Infra work', space: 'w2', spaceLabel: 'infra'}),
    ];
    const titles = pg => pg.$('board').querySelectorAll('.card').map(c => c.querySelector('.card-title').textContent).sort();
    let pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.deepEqual(titles(pg), ['Billing fix', 'Infra work']);

    sideSpace(pg, 'billing').click();
    assert.deepEqual(titles(pg), ['Billing fix']);
    assert.equal(sideSpace(pg, 'billing').getAttribute('aria-pressed'), 'true');
    assert.equal(pg.$('side-all').getAttribute('aria-pressed'), 'false');
    assert.equal(pg.storage.get('herdr-tasks-board-spaces'), JSON.stringify({work: 'w1'}));
    sideSpace(pg, 'billing').click();
    assert.deepEqual(titles(pg), ['Billing fix', 'Infra work']);
    sideSpace(pg, 'infra').click();
    assert.deepEqual(titles(pg), ['Infra work']);
    pg.$('side-all').click();
    assert.deepEqual(titles(pg), ['Billing fix', 'Infra work']);
    assert.equal(pg.$('side-all').getAttribute('aria-pressed'), 'true');

    pg = page('k1', '/work/', '#tab=w1%3At1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.ok(pg.body.classList.contains('viewing'), 'no view opened');
    sideSpace(pg, 'infra').click();
    assert.ok(!pg.body.classList.contains('viewing'), 'the view stayed');
    assert.equal(pg.location.hash, '');
    assert.deepEqual(titles(pg), ['Infra work']);
  },

  // A task in the side panel opens its view. In a view, it switches the view
  // to that task in place: the details stay where they were, on the right or
  // below, and Back still goes to the board. The open task is marked.
  async 'a task in the side panel opens, or switches the view'() {
    const cards = [
      card({tab: 'w1:t1', title: 'Billing fix', space: 'w1', spaceLabel: 'billing'}),
      card({tab: 'w2:t1', title: 'Infra work', space: 'w2', spaceLabel: 'infra'}),
    ];
    const current = pg => pg.$('side-spaces').querySelectorAll('.side-task[aria-current]').map(r => r.querySelector('.side-title').textContent);
    const pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.deepEqual(current(pg), []);

    sideTask(pg, 'Billing fix').click();
    assert.equal(pg.location.hash, '#tab=w1%3At1');
    assert.deepEqual(pg.pushed, ['#tab=w1%3At1']);
    assert.ok(pg.body.classList.contains('viewing'), 'no view opened');
    assert.equal(pg.$('detail-title').textContent, 'Billing fix');
    assert.deepEqual(current(pg), ['Billing fix']);

    pg.$('view-layout').click();
    sideTask(pg, 'Infra work').click();
    assert.equal(pg.location.hash, '#tab=w2%3At1');
    assert.deepEqual(pg.pushed, ['#tab=w1%3At1'], 'switching added to the history');
    assert.equal(JSON.stringify(pg.history().state), '{"view":true}', 'Back stopped going to the board');
    assert.equal(pg.$('detail-title').textContent, 'Infra work');
    assert.ok(pg.body.classList.contains('details-below'), 'the details went back to the right');
    assert.deepEqual(current(pg), ['Infra work']);

    sideTask(pg, 'Infra work').click();
    assert.equal(pg.location.hash, '#tab=w2%3At1');
  },

  // The side panel folds down to its circles, and back. The browser keeps
  // it folded. On a phone, it opens over the page from the header, and a
  // task picked in it closes it.
  async 'the side panel folds away'() {
    const FOLDED = 'herdr-tasks-board-side-folded';
    const cards = [card({tab: 'w1:t1', title: 'Billing fix', space: 'w1', spaceLabel: 'billing'})];
    let pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    const fold = pg.$('side-fold');
    assert.equal(pg.body.classList.contains('side-folded'), false);
    assert.equal(fold.getAttribute('aria-expanded'), 'true');
    fold.click();
    assert.equal(pg.body.classList.contains('side-folded'), true);
    assert.equal(fold.getAttribute('aria-expanded'), 'false');
    assert.equal(pg.storage.get(FOLDED), '1');
    fold.click();
    assert.equal(pg.body.classList.contains('side-folded'), false);
    assert.equal(pg.storage.get(FOLDED), '');

    pg.$('side-open').click();
    assert.equal(pg.body.classList.contains('side-open'), true);
    sideTask(pg, 'Billing fix').click();
    assert.equal(pg.body.classList.contains('side-open'), false, 'the panel stayed over the view');
    assert.equal(doc.activeElement, pg.$('view-back'), 'focus went into the closed panel');

    seeded = {[FOLDED]: '1'};
    pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.equal(pg.body.classList.contains('side-folded'), true, "the next page isn't folded");
    assert.equal(pg.$('side-fold').getAttribute('aria-expanded'), 'false');
  },

  // Priority only shows the priority tasks alone, on the board and in the
  // side panel, where every workspace stays. The browser keeps the choice,
  // and a page opened with it starts with it.
  async 'Priority only shows only priority tasks'() {
    const PRIO = 'herdr-tasks-board-priority-only';
    const cards = [
      card({tab: 'w1:t1', title: 'Urgent fix', space: 'w1', spaceLabel: 'billing', priority: true}),
      card({tab: 'w1:t2', title: 'Later', space: 'w1', spaceLabel: 'billing'}),
      card({tab: 'w2:t1', title: 'Infra work', space: 'w2', spaceLabel: 'infra'}),
    ];
    const titles = pg => pg.$('board').querySelectorAll('.card').map(c => c.querySelector('.card-title').textContent).sort();

    let pg = page('k1');
    await settle();
    const toggle = pg.$('prio-only');
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.equal(toggle.hidden, false);
    assert.equal(toggle.getAttribute('aria-pressed'), 'false');
    assert.deepEqual(titles(pg), ['Infra work', 'Later', 'Urgent fix']);
    assert.deepEqual(sideRows(pg), ['billing yourturn', '! ● Urgent fix', '● Later', 'infra yourturn', '● Infra work']);

    toggle.click();
    assert.equal(toggle.getAttribute('aria-pressed'), 'true');
    assert.deepEqual(titles(pg), ['Urgent fix']);
    assert.deepEqual(sideRows(pg), ['billing yourturn', '! ● Urgent fix', 'infra idle']);
    assert.equal(pg.storage.get(PRIO), '1');

    toggle.click();
    assert.equal(toggle.getAttribute('aria-pressed'), 'false');
    assert.deepEqual(titles(pg), ['Infra work', 'Later', 'Urgent fix']);
    assert.equal(pg.storage.get(PRIO), '');

    pg = page('k1');
    pg.storage.set(PRIO, '1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.equal(pg.$('prio-only').getAttribute('aria-pressed'), 'true');
    assert.deepEqual(titles(pg), ['Urgent fix']);
  },

  // A view's details sit on the right of its terminals, or below them, side
  // by side, from the layout button. The browser keeps the choice for the
  // next view.
  async "a view's details go below its terminals, and back"() {
    const BELOW = 'herdr-tasks-board-details-below';
    let pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    const below = () => pg.body.classList.contains('details-below');
    pg.$('board').querySelector('.card').click();
    const button = pg.$('view-layout');
    assert.equal(below(), false);
    assert.equal(button.getAttribute('aria-pressed'), 'false');
    assert.equal(button.title, "Put the task's details below the terminals");
    button.click();
    assert.equal(below(), true);
    assert.equal(button.getAttribute('aria-pressed'), 'true');
    assert.equal(button.title, "Put the task's details back on the right");
    assert.equal(pg.storage.get(BELOW), '1');
    button.click();
    assert.equal(below(), false);
    assert.equal(button.getAttribute('aria-pressed'), 'false');
    assert.equal(pg.storage.get(BELOW), '');

    seeded = {[BELOW]: '1'};
    pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    pg.$('board').querySelector('.card').click();
    assert.equal(below(), true, "the next view's details aren't below");
    assert.equal(pg.$('view-layout').getAttribute('aria-pressed'), 'true');
    pg.$('view-back').click();
    assert.equal(below(), true, 'leaving the view forgot the layout');
  },

  // Below the terminals, the details are four columns: the card's head, its
  // note and log, its agents, its links. Each scrolls on its own, and keeps
  // its place when the card changes.
  async 'the details below keep their place through an update'() {
    const agents = status => [{pane: 'w1:p1', agent: 'claude', status, name: 'Card audit', title: 'Audit', recap: 'The plan.'}];
    seeded = {'herdr-tasks-board-details-below': '1'};
    const pg = page('k1');
    await settle();
    pg.send(board(card({agents: agents('working'), note: 'Check the limits.'})));
    await settle();
    pg.$('board').querySelector('.card').click();
    const body = pg.$('detail-body');
    assert.deepEqual(body.children.map(e => e.className), ['detail-head', 'detail-notes', 'detail-agents', 'detail-links']);
    body.querySelector('.detail-agents').scrollTop = 120;
    body.querySelector('.detail-notes').scrollTop = 40;
    pg.send(board(card({agents: agents('done'), note: 'Check the limits.'})));
    await settle();
    assert.equal(body.querySelector('.agent-when').textContent, 'answered', "the update didn't show");
    assert.equal(body.querySelector('.detail-agents').scrollTop, 120, 'the agents went back to the top');
    assert.equal(body.querySelector('.detail-notes').scrollTop, 40, 'the note went back to the top');
  },

  // A page you can't see lets go of its stream, as the browser opens only
  // six connections to the board at once, and a page waiting for one never
  // loads. Shown again, it loads the board again and takes up a new one.
  async 'a hidden page lets go of its stream'() {
    const titles = pg => pg.$('board').querySelectorAll('.card-title').map(t => t.textContent);
    const pg = page('k1');
    await settle();
    pg.send(board(card()));
    await settle();
    assert.equal(pg.streams.length, 1);
    pg.hide(true);
    await settle();
    assert.ok(pg.streams[0].signal.aborted, 'the stream stayed open while hidden');
    pg.runTimers(60e3);
    await settle();
    assert.equal(pg.streams.length, 1, 'it took up a stream while hidden');
    assert.deepEqual(titles(pg), ['Fix the ABC-12 login loop'], 'the board went while hidden');
    pg.hide(false);
    await settle();
    assert.equal(pg.streams.length, 2, 'shown again, it took up no stream');
    assert.equal(pg.gets.length, 2, "shown again, it didn't load the board");
    pg.send(board(card({title: 'Renamed'})));
    await settle();
    assert.deepEqual(titles(pg), ['Renamed']);
    pg.hide(false);
    await settle();
    assert.equal(pg.streams.length, 2, 'shown twice, it took up two streams');
  },

  // The search finds tasks: first those with the words in their name, then
  // in a pane's name, then in a recap, then anywhere else, each with where
  // it was found. It looks through every task, whatever the side panel and
  // Priority only show, and keeps up with the board.
  async 'the search finds tasks, by name first'() {
    const cards = [
      card({tab: 'w1:t1', title: 'Invoice export', space: 'w1', spaceLabel: 'billing', note: 'Blocked on the\nlogin provider'}),
      card({tab: 'w1:t2', title: 'Login loop', space: 'w1', spaceLabel: 'billing', column: 'paused', stage: 'paused'}),
      card({tab: 'w1:t3', title: 'Billing', space: 'w1', spaceLabel: 'billing'}),
      card({tab: 'w2:t1', title: 'Deploy', space: 'w2', spaceLabel: 'infra', panes: ['login server']}),
      card({tab: 'w2:t2', title: 'Docs', space: 'w2', spaceLabel: 'infra', agents: [{pane: 'w2:p2', agent: 'claude', status: 'idle', recap: 'You’re documenting the login flow.'}]}),
    ];
    const titles = pg => pg.$('board').querySelectorAll('.card').map(c => c.querySelector('.card-title').textContent).sort();
    const pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    const box = pg.$('search');
    assert.equal(box.hidden, false, 'no search box');
    assert.equal(pg.$('results').hidden, true);
    sideSpace(pg, 'infra').click();
    pg.$('prio-only').click();

    type(pg, 'login');
    assert.equal(pg.$('board').hidden, true, 'the columns stayed');
    assert.equal(pg.$('results').hidden, false, 'no results');
    assert.equal(pg.$('results-count').textContent, '4 of 5 tasks match');
    assert.deepEqual(found(pg), [
      'Name',
      '[Login] loop',
      'Pane',
      'Deploy · [login] server',
      'Recap',
      'Docs · You’re documenting the [login] flow.',
      'Content',
      'Invoice export · Blocked on the [login] provider',
    ]);
    const result = title => pg.$('results-groups').querySelectorAll('.result').find(r => r.querySelector('.result-title').textContent === title);
    assert.equal(result('Login loop').getAttribute('data-column'), 'paused');

    cards[2] = card({tab: 'w1:t3', title: 'Billing', space: 'w1', spaceLabel: 'billing', log: [{who: 'claude', at: '2026-10-31T09:00:00Z', text: 'Moved the login check.'}]});
    pg.send(spaced(cards, SPACES));
    await settle();
    assert.deepEqual(found(pg).slice(-2), ['Invoice export · Blocked on the [login] provider', 'Billing · Moved the [login] check.']);

    type(pg, 'zzz');
    assert.equal(pg.$('results-count').textContent, 'No task matches “zzz”.');
    assert.deepEqual(found(pg), []);

    type(pg, '');
    assert.equal(pg.$('results').hidden, true);
    assert.equal(pg.$('board').hidden, false);
    assert.deepEqual(titles(pg), [], 'the side panel and Priority only no longer show');
    pg.$('prio-only').click();
    assert.deepEqual(titles(pg), ['Deploy', 'Docs']);
  },

  // / goes to the search, but in a field. Enter opens the first task found,
  // and a click any of them; Back comes back to what was found, and the
  // search is gone while a view is open. Esc empties the search, then
  // leaves it.
  async 'the search opens what it found'() {
    const cards = [
      card({tab: 'w1:t1', title: 'Invoice export', space: 'w1', spaceLabel: 'billing', note: 'The login provider'}),
      card({tab: 'w1:t2', title: 'Login loop', space: 'w1', spaceLabel: 'billing'}),
    ];
    const pg = page('k1');
    await settle();
    pg.send(spaced(cards, SPACES));
    await settle();
    const box = pg.$('search');
    const field = new Element('input');
    pg.body.append(field);
    field.dispatch('keydown', {key: '/'});
    assert.notEqual(doc.activeElement, box, '/ in a field went to the search');
    pg.body.dispatch('keydown', {key: '/', ctrlKey: true});
    assert.notEqual(doc.activeElement, box, 'ctrl+/ went to the search');
    let slash = null;
    pg.body.addEventListener('keydown', e => (slash = e));
    pg.body.dispatch('keydown', {key: '/'});
    assert.equal(doc.activeElement, box, "/ didn't go to the search");
    assert.ok(slash.defaultPrevented, '/ was typed too');

    type(pg, 'login');
    box.dispatch('keydown', {key: 'Enter'});
    assert.equal(pg.location.hash, '#tab=w1%3At2', 'Enter opened something else');
    assert.equal(box.hidden, true, 'the search shows in the view');
    pg.back();
    assert.equal(box.hidden, false);
    assert.equal(pg.$('results').hidden, false, "Back didn't come back to the results");
    assert.equal(doc.activeElement && doc.activeElement.querySelector('.result-title').textContent, 'Login loop');

    pg.$('results-groups').querySelectorAll('.result')[1].click();
    assert.equal(pg.location.hash, '#tab=w1%3At1');
    pg.back();

    box.focus();
    box.dispatch('keydown', {key: 'ArrowDown'});
    assert.equal(doc.activeElement.querySelector('.result-title').textContent, 'Login loop');
    doc.activeElement.dispatch('keydown', {key: 'ArrowDown'});
    assert.equal(doc.activeElement.querySelector('.result-title').textContent, 'Invoice export');
    doc.activeElement.dispatch('keydown', {key: 'ArrowUp'});
    doc.activeElement.dispatch('keydown', {key: 'ArrowUp'});
    assert.equal(doc.activeElement, box, 'up from the first found didn’t go back to the search');

    box.dispatch('keydown', {key: 'Escape'});
    assert.equal(box.value, '');
    assert.equal(pg.$('results').hidden, true);
    assert.equal(doc.activeElement, box);
    box.dispatch('keydown', {key: 'Escape'});
    assert.notEqual(doc.activeElement, box, 'a second Esc stayed in the search');
  },

  // A page opened behind another waits until it shows to take up a stream.
  async 'a page opened hidden waits to be shown'() {
    const pg = page('k1');
    pg.hide(true);
    await settle();
    // The first stream may have started before the page knew; it goes.
    assert.ok(pg.streams.every(s => s.signal.aborted), 'a stream stayed open while hidden');
    const before = pg.streams.length;
    pg.runTimers(60e3);
    await settle();
    assert.equal(pg.streams.length, before, 'it took up a stream while hidden');
    pg.hide(false);
    await settle();
    assert.equal(pg.streams.length, before + 1);
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