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

// page runs the script as a tab opened at path (/work/ unless given) with the
// hash and search, signed in with stored. The server takes only the key in pg.good, and
// answers a request for the board or the list with pg.answers[url], a
// status, or 200 with nothing to read before the stream's own board. With
// tailnet, it's the board's tailnet address, which is https.
function page(stored, path, hash, search, tailnet) {
  doc.body = new Element('body');
  doc.activeElement = null;
  doc.listeners = {};
  const pg = {good: stored, streams: [], posts: [], gets: [], answers: {}, replaced: [], addresses: []};
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
  const go = url => {
    pg.addresses.push(String(url));
    const i = String(url).indexOf('#');
    location.hash = i < 0 ? '' : String(url).slice(i);
  };

  const ctx = {
    window: {
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
  vm.runInContext(src, ctx);

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
  return pg;
}

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
    await offers(slackCard({research: ''}), null, ['Move to workspace…', 'Start research', 'Mute #billing']);
    await offers(slackCard({research: 'held'}), null, ['Move to workspace…', 'Start research', 'Mute #billing']);
    await offers(slackCard({research: 'failed'}), null, ['Move to workspace…', 'Start research', 'Mute #billing']);
    await offers(slackCard({research: 'locked'}), null, ['Move to workspace…', 'Continue with my tools', 'Mute #billing']);
    await offers(slackCard({research: 'open'}), null, ['Move to workspace…', 'Mute #billing']);
    await offers(slackCard({research: 'locked', muted: true}), null, ['Move to workspace…', 'Continue with my tools']);
    // A plain card: only the move, and nothing in a session of one space.
    await offers(card({space: 'w1', spaceLabel: 'billing'}), null, ['Move to workspace…']);
    await offers(card({space: 'w1', spaceLabel: 'billing'}), [SPACES[0]], []);

    // The tab's view has the same buttons.
    pg.send(spaced([slackCard({research: 'locked'})], SPACES));
    await settle();
    pg.$('board').querySelector('.card').click();
    const acts = pg
      .$('detail-body')
      .querySelector('.detail-actions')
      .querySelectorAll('button')
      .map(b => b.textContent);
    assert.deepEqual(acts.slice(-3), ['Move to workspace…', 'Continue with my tools', 'Mute #billing']);
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
    assert.deepEqual(pg.menu(), ['Mute #billing'], 'a second start was offered');
    pg.close();
    pg.posts[0].answer(409, {error: 'Research already started.'});
    await settle();
    assert.deepEqual(pg.toasts(), ['Starting research…', 'Research already started.']);
    assert.deepEqual(pg.menu(), ['Move to workspace…', 'Start research', 'Mute #billing']);
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
    pg.posts[1].answer(502, {error: 'can’t fetch: waiting for herdr session default'});
    await settle();
    assert.equal(said.textContent, 'can’t fetch: waiting for herdr session default');
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

    // With none, it says so.
    const none = page('k1', '/');
    await settle();
    none.send({version: '0.11.0', slack: 'default', sessions: []}, 'sessions');
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
    const waiting = 'Slack asks go to herdr session default, which isn’t running.';
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