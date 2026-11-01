// Runs the terminal view's scripts in a stand-in browser, with a stand-in
// xterm and board server, to check what the view types into herdr's panes.
// Usage: node term.js path/to/keymap.js path/to/term.js
'use strict';

const assert = require('assert/strict');
const fs = require('fs');
const vm = require('vm');

const [keymapSrc, termSrc] = process.argv.slice(2, 4).map(f => fs.readFileSync(f, 'utf8'));

// Element is a stand-in for a DOM element.
class Element {
  constructor(tag) {
    this.tag = tag;
    this.parent = null;
    this.children = [];
    this.listeners = {};
    this.style = {};
    this.attrs = {};
    this.hidden = false;
    this.textContent = '';
    this.className = '';
    this.classList = {add() {}, remove() {}};
    this.clientWidth = 800;
    this.clientHeight = 600;
  }
  append(...els) {
    for (const e of els) {
      e.parent = this;
      this.children.push(e);
    }
  }
  replaceChildren(...els) {
    this.children = [];
    this.append(...els);
  }
  remove() {
    if (this.parent) this.parent.children = this.parent.children.filter(c => c !== this);
    this.parent = null;
  }
  contains(e) {
    for (; e; e = e.parent) if (e === this) return true;
    return false;
  }
  setAttribute(k, v) {
    this.attrs[k] = v;
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter(f => f !== fn);
  }
  fire(type, ev = {}) {
    for (const fn of this.listeners[type] || []) fn(ev);
  }
  getContext() {
    return {font: '', measureText: () => ({width: 7, fontBoundingBoxAscent: 11, fontBoundingBoxDescent: 3})};
  }
}

// event makes an event that records what's done with it.
function event(props) {
  return Object.assign(
    {
      defaultPrevented: false,
      preventDefault() {
        this.defaultPrevented = true;
      },
      stopPropagation() {},
      stopImmediatePropagation() {},
    },
    props,
  );
}

// key makes a key event: mods holds c (ctrl), a (alt), s (shift), m (meta).
function key(k, code, mods = '', type = 'keydown') {
  return event({
    type,
    key: k,
    code,
    ctrlKey: mods.includes('c'),
    altKey: mods.includes('a'),
    shiftKey: mods.includes('s'),
    metaKey: mods.includes('m'),
    isComposing: false,
    getModifierState: () => false,
  });
}

// parseEvent reads one event-stream block, as the page's does.
function parseEvent(block) {
  const ev = {event: 'message', data: ''};
  for (const line of block.split('\n')) {
    if (line.startsWith('event: ')) ev.event = line.slice(7);
    else if (line.startsWith('data: ')) ev.data += line.slice(6);
  }
  return ev;
}

const settle = () => new Promise(r => setTimeout(r, 5));

// page mounts a tab's terminals in a browser on platform.
function page(platform) {
  const pg = {terms: [], toasts: [], copied: [], commands: [], requests: [], streamKeys: [], signOuts: [], key: 'k'};

  // The terminals, as xterm's: they record what the view does with them.
  class Terminal {
    constructor(options) {
      this.options = Object.assign({}, options);
      this.cols = options.cols;
      this.rows = options.rows;
      this.unicode = {activeVersion: ''};
      this.written = [];
      this.selection = '';
      this.disposed = false;
      pg.terms.push(this);
    }
    loadAddon() {}
    open(parent) {
      this.element = new Element('div');
      parent.append(this.element);
    }
    write(s) {
      this.written.push(s);
    }
    resize(cols, rows) {
      this.cols = cols;
      this.rows = rows;
    }
    attachCustomKeyEventHandler(fn) {
      this.keyHandler = fn;
    }
    attachCustomWheelEventHandler(fn) {
      this.wheelHandler = fn;
    }
    onData(fn) {
      this.dataHandler = fn;
    }
    onSelectionChange(fn) {
      this.selectionHandler = fn;
    }
    hasSelection() {
      return this.selection !== '';
    }
    getSelection() {
      return this.selection;
    }
    // clearSelection says the selection changed, as xterm's does.
    clearSelection() {
      this.selection = '';
      if (this.selectionHandler) this.selectionHandler();
    }
    focus() {}
    dispose() {
      this.disposed = true;
    }
    // press gives the view a key event, and returns whether xterm may
    // handle it after.
    press(ev) {
      return this.keyHandler(ev);
    }
    // type has xterm make data, as for text typed or a terminal's reply.
    type(data) {
      this.dataHandler(data);
    }
    shown() {
      return this.written.slice(1).join('');
    }
  }

  // The server's streams: each answers with the next of replies, or opens.
  const server = {
    replies: [],
    stream: null,
    hold: false, // input requests wait for release
    waiting: [],
    inputReplies: [], // how input requests fail, in turn, once released
    zooms: [], // each zoom request, as {session, pane, zoom, key}
    zoomReplies: [], // how zoom requests fail, in turn
    inFlight: {},
    mostInFlight: {},
    send(name, data) {
      this.stream.push('event: ' + name + '\ndata: ' + JSON.stringify(data) + '\n\n');
    },
    end() {
      this.stream.end();
    },
    release() {
      for (const done of this.waiting.splice(0)) done();
    },
    // typed is what was typed into a pane, in the order the requests came,
    // as the server's lock for the pane applies them.
    typed(pane) {
      const out = [];
      for (const r of pg.requests) {
        if (r.pane !== pane) continue;
        for (const op of r.ops) out.push(op.keys ? 'keys ' + op.keys.join(' ') : op.paste !== undefined ? 'paste ' + op.paste : 'text ' + op.text);
      }
      return out;
    },
  };
  pg.server = server;

  function openStream(signal) {
    const chunks = [];
    let wake = null;
    let ended = false;
    const s = {
      get ended() {
        return ended;
      },
      push(text) {
        chunks.push(new TextEncoder().encode(text));
        if (wake) wake();
      },
      end() {
        ended = true;
        if (wake) wake();
      },
    };
    signal.addEventListener('abort', () => s.end());
    server.stream = s;
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

  async function fetch(url, opts) {
    const k = opts.headers.Authorization.replace(/^Bearer /, '');
    if (url.startsWith('/api/tab/stream?')) {
      pg.streamKeys.push(k);
      const r = server.replies.shift();
      if (r && r.before) r.before();
      if (r) return {ok: false, status: r.status, body: null, json: async () => ({error: r.error})};
      return openStream(opts.signal);
    }
    if (url === '/api/pane/zoom') {
      server.zooms.push({...JSON.parse(opts.body), key: k});
      const r = server.zoomReplies.shift();
      if (r) return {ok: false, status: r.status, json: async () => ({error: r.error})};
      return {ok: true, status: 204};
    }
    assert.equal(url, '/api/pane/input');
    const req = JSON.parse(opts.body);
    req.key = k;
    pg.requests.push(req);
    server.inFlight[req.pane] = (server.inFlight[req.pane] || 0) + 1;
    server.mostInFlight[req.pane] = Math.max(server.mostInFlight[req.pane] || 0, server.inFlight[req.pane]);
    if (server.hold) await new Promise(r => server.waiting.push(r));
    server.inFlight[req.pane]--;
    const r = server.inputReplies.shift();
    if (r && r.before) r.before();
    if (r) return {ok: false, status: r.status, json: async () => ({error: r.error})};
    return {ok: true, status: 204};
  }

  // Timers run when the test says.
  const timers = new Map();
  let timerID = 0;
  pg.runTimers = below => {
    for (const [id, t] of [...timers]) {
      if (t.ms < below) {
        timers.delete(id);
        t.fn();
      }
    }
  };

  const windowListeners = new Element('window');
  const window = {
    Terminal,
    Unicode11Addon: {Unicode11Addon: class {}},
    devicePixelRatio: 1,
    addEventListener: (type, fn) => windowListeners.addEventListener(type, fn),
    removeEventListener: (type, fn) => windowListeners.removeEventListener(type, fn),
  };
  pg.window = windowListeners;
  // hide hides the page, or shows it again, as a tab put behind another does.
  const docListeners = new Element('document');
  pg.hidden = false;
  pg.hide = on => {
    pg.hidden = on;
    docListeners.fire('visibilitychange');
  };
  pg.navigator = {
    platform,
    clipboard: {
      writeText: async text => {
        if (pg.clipboardFails) throw new Error('not allowed');
        pg.copied.push(text);
      },
    },
  };
  const ctx = {
    window,
    navigator: pg.navigator,
    document: {
      get hidden() {
        return pg.hidden;
      },
      addEventListener: (type, fn) => docListeners.addEventListener(type, fn),
      removeEventListener: (type, fn) => docListeners.removeEventListener(type, fn),
      createElement: tag => new Element(tag),
      execCommand: cmd => {
        pg.commands.push(cmd);
        return true;
      },
    },
    fetch,
    setTimeout: (fn, ms) => {
      timers.set(++timerID, {fn, ms});
      return timerID;
    },
    clearTimeout: id => timers.delete(id),
    requestAnimationFrame: () => 0,
    cancelAnimationFrame() {},
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    AbortController,
    TextEncoder,
    TextDecoder,
    console,
  };
  vm.createContext(ctx);
  vm.runInContext(keymapSrc, ctx);
  vm.runInContext(termSrc, ctx);

  pg.box = new Element('div');
  // mount opens the tab's view, as the board does again after Back.
  pg.mount = () => {
    pg.view = window.herdrTerm.mount(pg.box, {
      session: 'work',
      tab: 'w1:t1',
      key: () => pg.key,
      parseEvent,
      toast: msg => pg.toasts.push(msg),
      // As the board's: a 401 for a key the page has replaced since signs
      // nothing out.
      signOut: k => {
        pg.signOuts.push(k);
        return k === pg.key;
      },
    });
  };
  pg.mount();
  pg.status = () => (pg.box.children[0].hidden ? '' : pg.box.children[0].textContent);
  pg.live = () => pg.terms.filter(t => !t.disposed);
  return pg;
}

const A = {pane: 'w1:p1', x: 0, y: 0, width: 42, height: 12, cols: 40, rows: 10, agent: 'claude', title: 'Fixing it', focused: true};
const B = {pane: 'w1:p2', x: 42, y: 0, width: 38, height: 12, cols: 36, rows: 10, agent: '', title: '', focused: false};
const area = {x: 0, y: 0, width: 80, height: 12};
const screen = (pane, ansi) => ({pane: pane.pane, cols: pane.cols, rows: pane.rows, ansi, scrolled: false});

// opened is a page showing panes A and B.
async function opened(platform = 'MacIntel') {
  const pg = page(platform);
  await settle();
  pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
  await settle();
  assert.equal(pg.live().length, 2);
  return pg;
}

// zoomButtons is each pane's zoom button, in order.
function zoomButtons(pg) {
  const out = [];
  const walk = e => {
    for (const c of e.children) {
      if (c.tag === 'button' && c.className === 'term-zoom') out.push(c);
      walk(c);
    }
  };
  walk(pg.box);
  return out;
}

// heads is what each pane's header shows, in order, and its label.
function heads(pg) {
  const out = [];
  const walk = e => {
    for (const c of e.children) {
      if (c.className === 'term-pane') {
        const head = c.children.find(h => h.className === 'term-head');
        out.push([...head.children.filter(h => !h.hidden && h.textContent && h.tag !== 'button').map(h => h.textContent), c.attrs['aria-label']]);
      }
      walk(c);
    }
  };
  walk(pg.box);
  return out;
}

const tests = {
  // A pane's name in herdr heads its terminal, before the agent and its
  // title, as in herdr's borders; a shell with a name isn't called Shell.
  async "a pane's name heads its terminal"() {
    const pg = page('MacIntel');
    await settle();
    pg.server.send('layout', {zoomed: false, area, panes: [{...A, name: 'Card audit'}, {...B, name: 'dev server'}]});
    await settle();
    assert.deepEqual(heads(pg), [
      ['Card audit', 'claude', 'Fixing it', 'Card audit: claude: Fixing it'],
      ['dev server', 'dev server'],
    ]);
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    assert.deepEqual(heads(pg), [
      ['claude', 'Fixing it', 'claude: Fixing it'],
      ['Shell', 'Terminal'],
    ]);
  },

  // Keys typed into a pane go in order, one request at a time, even when
  // the pane leaves the layout and comes back while they're on their way,
  // as when another pane is zoomed and then isn't.
  async 'a pane keeps its queue across a zoom'() {
    const pg = await opened();
    const [a] = pg.live();
    pg.server.hold = true;
    a.type('first');
    await settle();
    a.press(key('Enter', 'Enter'));
    await settle();
    pg.server.send('layout', {zoomed: true, area, panes: [{...B, x: 0, width: 80}]});
    await settle();
    assert.ok(a.disposed, "A's terminal is still there while B is zoomed");
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    const a2 = pg.terms[2];
    assert.ok(a2 && !a2.disposed, 'A came back');
    a2.type('second');
    await settle();
    for (let i = 0; i < 5; i++) {
      pg.server.release();
      await settle();
    }
    assert.equal(pg.server.mostInFlight['w1:p1'], 1, 'two requests for A at once');
    assert.deepEqual(pg.server.typed('w1:p1'), ['text first', 'keys enter', 'text second']);
  },

  // Keys on their way when the view closes, as on Back, still go, in order.
  async 'keys typed before Back still go'() {
    const pg = await opened();
    const [a] = pg.live();
    pg.server.hold = true;
    a.type('echo test');
    await settle();
    a.press(key('Enter', 'Enter'));
    await settle();
    pg.view.close();
    for (let i = 0; i < 3; i++) {
      pg.server.release();
      await settle();
    }
    assert.deepEqual(pg.server.typed('w1:p1'), ['text echo test', 'keys enter']);
    assert.equal(pg.server.mostInFlight['w1:p1'], 1, 'two requests for A at once');
  },

  // The tab's view opened again while keys from the last one are on their
  // way types after them, one request at a time.
  async 'a view opened again types after the last one'() {
    const pg = await opened();
    const [a] = pg.live();
    pg.server.hold = true;
    a.type('first');
    await settle();
    a.press(key('Enter', 'Enter'));
    await settle();
    pg.view.close();
    pg.mount();
    await settle();
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    const [a2] = pg.live();
    assert.ok(a2 && a2 !== a, 'A is not open again');
    a2.type('second');
    await settle();
    for (let i = 0; i < 5; i++) {
      pg.server.release();
      await settle();
    }
    assert.equal(pg.server.mostInFlight['w1:p1'], 1, 'two requests for A at once');
    assert.deepEqual(pg.server.typed('w1:p1'), ['text first', 'keys enter', 'text second']);
  },

  // A request that fails after Back still says so, and what was queued
  // behind it is dropped.
  async 'a failure after Back still says so'() {
    const pg = await opened();
    const [a] = pg.live();
    pg.server.hold = true;
    a.type('first');
    await settle();
    a.press(key('Enter', 'Enter'));
    await settle();
    pg.view.close();
    pg.server.inputReplies.push({status: 404, error: 'That pane isn’t in the session.'});
    for (let i = 0; i < 3; i++) {
      pg.server.release();
      await settle();
    }
    assert.deepEqual(pg.server.typed('w1:p1'), ['text first']);
    assert.deepEqual(pg.toasts, ['Not typed: That pane isn’t in the session.']);
  },

  // A 401 signs the page out, with the key the request was sent with, and
  // drops what's queued for every pane.
  async 'a 401 signs out and drops every queue'() {
    const pg = await opened();
    const [a, b] = pg.live();
    pg.server.hold = true;
    a.type('one');
    await settle();
    a.press(key('Enter', 'Enter'));
    b.type('two');
    await settle();
    b.press(key('Enter', 'Enter'));
    pg.server.inputReplies.push({status: 401, error: 'Sign in again.'});
    for (let i = 0; i < 3; i++) {
      pg.server.release();
      await settle();
    }
    assert.deepEqual(pg.signOuts, ['k']);
    assert.deepEqual(pg.server.typed('w1:p1'), ['text one']);
    assert.deepEqual(pg.server.typed('w1:p2'), ['text two']);
    assert.deepEqual(pg.toasts, ['Not typed: Sign in again.']);
  },

  // A 401 for a key the page has replaced since, as when another tab signed
  // in, signs nothing out: the stream tries again with the new key, and
  // keys sent with the old one are lost, which a toast says.
  async 'a 401 for an old key signs nothing out'() {
    const pg = await opened();
    pg.server.end();
    await settle();
    pg.server.replies.push({status: 401, error: 'Sign in again.', before: () => (pg.key = 'k2')});
    pg.runTimers(60e3);
    await settle();
    assert.deepEqual(pg.signOuts, ['k']);
    pg.runTimers(60e3);
    await settle();
    assert.deepEqual(pg.streamKeys, ['k', 'k', 'k2']);
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    assert.equal(pg.status(), '');

    const [a, b] = pg.live();
    pg.server.hold = true;
    a.type('one');
    await settle();
    a.press(key('Enter', 'Enter'));
    b.type('two');
    await settle();
    pg.server.inputReplies.push({status: 401, error: 'Sign in again.', before: () => (pg.key = 'k3')});
    for (let i = 0; i < 3; i++) {
      pg.server.release();
      await settle();
    }
    assert.deepEqual(pg.signOuts, ['k', 'k2']);
    assert.deepEqual(pg.requests.map(r => r.key), ['k2', 'k2']);
    assert.deepEqual(pg.server.typed('w1:p1'), ['text one']);
    assert.deepEqual(pg.toasts, ['Not typed: Signed in again. Try that again.']);
    b.type('three');
    await settle();
    pg.server.release();
    await settle();
    assert.deepEqual(pg.server.typed('w1:p2'), ['text two', 'text three']);
  },

  // Cmd shortcuts are left to the browser, and kept from xterm, which would
  // type \r for Cmd+Enter.
  async 'cmd shortcuts stay with the browser'() {
    const pg = await opened('MacIntel');
    const [a] = pg.live();
    for (const [k, code] of [['Enter', 'Enter'], ['Backspace', 'Backspace'], ['Escape', 'Escape'], ['c', 'KeyC'], ['v', 'KeyV']]) {
      const ev = key(k, code, 'm');
      assert.equal(a.press(ev), false, 'xterm handles cmd+' + k);
      assert.equal(ev.defaultPrevented, false, 'the browser is kept from cmd+' + k);
    }
    // Cmd+A is xterm's: it selects all.
    assert.equal(a.press(key('a', 'KeyA', 'm')), true);
    await settle();
    assert.deepEqual(pg.requests, []);

    // On macOS, ctrl+shift+c goes to herdr; cmd+c copies.
    const ev = key('C', 'KeyC', 'cs');
    assert.equal(a.press(ev), false);
    assert.ok(ev.defaultPrevented);
    // A paste is sent as one.
    a.element.fire('paste', event({clipboardData: {getData: () => 'ls\nx'}}));
    await settle();
    assert.deepEqual(pg.server.typed('w1:p1'), ['keys ctrl+shift+c', 'paste ls\rx']);
  },

  // Elsewhere, ctrl+shift+c and ctrl+insert copy, and ctrl+shift+v and
  // shift+insert paste. None sends a key: ctrl+shift+c would interrupt.
  async 'copy and paste keys elsewhere'() {
    const pg = await opened('Linux x86_64');
    const [a] = pg.live();
    a.selection = 'hello';
    let ev = key('C', 'KeyC', 'cs');
    assert.equal(a.press(ev), false);
    assert.ok(ev.defaultPrevented, 'the browser would open its inspector');
    await settle();
    assert.deepEqual(pg.copied, ['hello']);

    // With no clipboard API, as over http to another computer, or one that
    // says no, the browser copies.
    pg.navigator.clipboard = undefined;
    a.press(key('Insert', 'Insert', 'c'));
    pg.navigator.clipboard = {writeText: async () => Promise.reject(new Error('not allowed'))};
    a.press(key('C', 'KeyC', 'cs'));
    await settle();
    assert.deepEqual(pg.commands, ['copy', 'copy']);

    // With nothing selected, nothing.
    a.selection = '';
    a.press(key('C', 'KeyC', 'cs'));
    await settle();
    assert.deepEqual(pg.copied, ['hello']);
    assert.deepEqual(pg.commands, ['copy', 'copy']);

    // Paste keys are the browser's, which pastes; xterm doesn't see them.
    for (const ev of [key('V', 'KeyV', 'cs'), key('Insert', 'Insert', 's')]) {
      assert.equal(a.press(ev), false);
      assert.equal(ev.defaultPrevented, false);
    }
    await settle();
    assert.deepEqual(pg.requests, []);

    // ctrl+c is still ctrl+c.
    ev = key('c', 'KeyC', 'c');
    assert.equal(a.press(ev), false);
    await settle();
    assert.deepEqual(pg.server.typed('w1:p1'), ['keys ctrl+c']);
  },

  // The mouse isn't sent: xterm is kept from making keys of the wheel or of
  // an alt+click.
  async 'the mouse types nothing'() {
    const pg = await opened();
    for (const t of pg.live()) {
      assert.equal(t.options.altClickMovesCursor, false);
      assert.equal(t.options.rightClickSelectsWord, false);
      assert.ok(t.wheelHandler, 'no wheel handler');
      const ev = event({deltaY: -100});
      assert.equal(t.wheelHandler(ev), false);
      assert.equal(ev.defaultPrevented, false, 'the wheel is kept from scrolling the page');
    }
  },

  // xterm answers queries in what it's shown. Its answers start with ESC,
  // and aren't typed; keys that make an escape go by name.
  async 'terminal replies are not typed'() {
    const pg = await opened();
    const [a] = pg.live();
    for (const reply of ['\x1b[?1;2c', '\x1b[2;3R', '\x1bP1$r0m\x1b\\', '\x1b[A', '\x1b[I']) a.type(reply);
    await settle();
    assert.deepEqual(pg.requests, []);
    a.type('x');
    await settle();
    assert.deepEqual(pg.server.typed('w1:p1'), ['text x']);
  },

  // A screen waits while text is being selected, but not for a right click,
  // whose button the context menu may keep to itself.
  async 'a right click holds no screen'() {
    const pg = await opened('MacIntel');
    const [a] = pg.live();
    const el = pg.box.children[1].children[0].children[0]; // fit > stage > A's pane
    const shows = async text => {
      pg.server.send('screen', screen(A, text));
      await settle();
      return a.shown().includes(text);
    };
    el.fire('pointerdown', {button: 2});
    assert.ok(await shows('after a right click'));
    el.fire('pointerdown', {button: 0, ctrlKey: true});
    assert.ok(await shows('after ctrl+click'));
    el.fire('pointerdown', {button: 0});
    assert.ok(!(await shows('while selecting')), 'a screen came while selecting');
    pg.window.fire('contextmenu', {});
    assert.ok(a.shown().includes('while selecting'), 'the context menu let the screen go');
  },

  // A screen waits while text is selected. The keys and pastes the view
  // sends itself end the selection, as what xterm types does, so the pane
  // doesn't stay still under them. Copying, and keys left to the browser,
  // keep it.
  async 'typing ends a selection'() {
    const pg = await opened('Linux x86_64');
    const [a] = pg.live();
    const shows = async text => {
      pg.server.send('screen', screen(A, text));
      await settle();
      return a.shown().includes(text);
    };
    a.selection = 'hello';
    assert.ok(!(await shows('one')), 'a screen came over a selection');
    for (const ev of [key('C', 'KeyC', 'cs'), key('Insert', 'Insert', 'c'), key('V', 'KeyV', 'cs'), key('c', 'KeyC', 'm')]) a.press(ev);
    await settle();
    assert.equal(a.selection, 'hello', 'copying ended the selection');
    assert.ok(!a.shown().includes('one'), 'a screen came over a selection');

    const typed = [];
    for (const [ev, sent] of [
      [key('Enter', 'Enter'), 'keys enter'],
      [key('c', 'KeyC', 'c'), 'keys ctrl+c'],
      [key('ArrowUp', 'ArrowUp'), 'keys up'],
      [key('Home', 'Home'), 'text \x1b[H'],
      [null, 'paste ls'],
    ]) {
      a.selection = 'hello';
      const text = 'after ' + sent;
      assert.ok(!(await shows(text)), 'a screen came over a selection');
      if (ev) a.press(ev);
      else a.element.fire('paste', event({clipboardData: {getData: () => 'ls'}}));
      await settle();
      assert.equal(a.selection, '', sent + ' kept the selection');
      assert.ok(a.shown().includes(text), 'the screen waited after ' + sent);
      typed.push(sent);
    }
    assert.deepEqual(pg.server.typed('w1:p1'), typed);
  },

  // When the session goes, its screens go too, and the view says why.
  async 'a gone session clears the view'() {
    const pg = await opened();
    pg.server.send('gone', {error: 'Session work isn’t running.'});
    await settle();
    assert.equal(pg.live().length, 0);
    assert.equal(pg.status(), 'Session work isn’t running.');
    pg.server.end();
    await settle();
    assert.equal(pg.status(), 'Session work isn’t running.');
    // It tries again, in case herdr starts again.
    pg.server.replies.push({status: 404, error: 'Session work isn’t running.'});
    pg.runTimers(60e3);
    await settle();
    assert.equal(pg.status(), 'Session work isn’t running.');
    pg.runTimers(60e3);
    await settle();
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    assert.equal(pg.live().length, 2);
    assert.equal(pg.status(), '');
  },

  // A stream that drops keeps the screens while it reconnects, but not once
  // the server says the tab is gone.
  async 'a reconnect told the tab is gone clears the view'() {
    const pg = await opened();
    pg.server.end();
    await settle();
    assert.equal(pg.status(), 'Reconnecting…');
    assert.equal(pg.live().length, 2);
    pg.server.replies.push({status: 404, error: 'That tab isn’t on the board.'});
    pg.runTimers(60e3);
    await settle();
    assert.equal(pg.live().length, 0);
    assert.equal(pg.status(), 'That tab isn’t on the board.');
  },

  // A page you can't see lets go of its stream, as the browser opens only
  // six connections to the board at once. Shown again, it takes up a new
  // one, keeping the screens meanwhile. Closed, it listens no more.
  async 'a hidden view lets go of its stream'() {
    const pg = await opened();
    assert.equal(pg.streamKeys.length, 1);
    const first = pg.server.stream;
    pg.hide(true);
    await settle();
    assert.ok(first.ended, 'the stream stayed open while hidden');
    pg.runTimers(60e3);
    await settle();
    assert.equal(pg.streamKeys.length, 1, 'it took up a stream while hidden');
    assert.equal(pg.live().length, 2, 'the screens went');
    pg.hide(false);
    await settle();
    assert.equal(pg.streamKeys.length, 2, 'shown again, it took up no stream');
    pg.server.send('layout', {zoomed: false, area, panes: [A, B]});
    await settle();
    assert.equal(pg.status(), '');
    pg.hide(false);
    await settle();
    assert.equal(pg.streamKeys.length, 2, 'shown twice, it took up two streams');
    pg.view.close();
    pg.hide(true);
    pg.hide(false);
    await settle();
    assert.equal(pg.streamKeys.length, 2, 'a closed view took up a stream');
  },

  // A pane's header has a button that zooms it to the whole tab, in herdr
  // too, as do two clicks on the header; zoomed, it shows the tab's panes
  // again. A tab of one pane has nothing to zoom.
  async 'a pane zooms in herdr from its header'() {
    const pg = await opened();
    const zoomOf = i => zoomButtons(pg)[i];
    const a = zoomOf(0);
    assert.equal(a.hidden, false);
    assert.equal(a.textContent, '⤢');
    assert.equal(a.attrs.title, 'Zoom this pane to the whole tab, in herdr too');
    assert.equal(a.attrs['aria-label'], 'Zoom claude: Fixing it');
    a.fire('click', event({target: a}));
    await settle();
    assert.deepEqual(pg.server.zooms, [{session: 'work', pane: 'w1:p1', zoom: true, key: 'k'}]);

    pg.server.send('layout', {zoomed: true, area, panes: [{...A, width: 80}]});
    await settle();
    assert.equal(pg.live().length, 1);
    assert.equal(zoomButtons(pg).length, 1);
    assert.equal(zoomOf(0).textContent, '⤡');
    assert.equal(zoomOf(0).attrs.title, "Show all the tab's panes again, in herdr too");
    assert.equal(zoomOf(0).attrs['aria-label'], 'Unzoom claude: Fixing it');
    const head = zoomOf(0).parent.children.find(h => h.className === 'term-head');
    head.fire('dblclick', event({target: head}));
    await settle();
    assert.deepEqual(pg.server.zooms[1], {session: 'work', pane: 'w1:p1', zoom: false, key: 'k'});

    pg.server.zoomReplies.push({status: 502, error: 'herdr: no pane w1:p1'});
    zoomOf(0).fire('click', event({target: zoomOf(0)}));
    await settle();
    assert.deepEqual(pg.toasts, ['herdr: no pane w1:p1']);
    pg.server.zoomReplies.push({status: 401, error: 'Sign in again.'});
    zoomOf(0).fire('click', event({target: zoomOf(0)}));
    await settle();
    assert.deepEqual(pg.signOuts, ['k']);

    pg.server.send('layout', {zoomed: false, area, panes: [A]});
    await settle();
    assert.equal(zoomOf(0).hidden, true, 'a lone pane offers a zoom');
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
