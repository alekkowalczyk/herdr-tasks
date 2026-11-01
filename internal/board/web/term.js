// The terminals of a task's tab: live from GET /api/tab/stream, and typed
// into with POST /api/pane/input. They use the page's own copies of
// @xterm/xterm 6.0.0 and @xterm/addon-unicode11 0.9.0, in vendor/.
//
// herdr has no stream of a pane's output, so the server sends each pane's
// screen as it changes: rows of text and colours, with no cursor. Each one
// replaces what the pane's terminal shows. Keys go back by herdr's names
// for them, so herdr encodes them in the pane's own modes; xterm only types
// text.
(() => {
  'use strict';

  const MONO = 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace';
  // The font size is shared by every pane, and chosen to fit the tab in.
  const MIN_FONT = 6;
  const MAX_FONT = 15;
  // What one input request may hold, as the server checks it.
  const MAX_OPS = 64;
  const MAX_INPUT = 64 << 10;
  // Text typed in a row is sent as one op, up to this many characters.
  const MAX_MERGE = 4096;
  // A screen is written as one frame: reset, home, clear, the screen, reset.
  const BEGIN = '\x1b[?2026h\x1b[0m\x1b[H\x1b[2J';
  const END = '\x1b[0m\x1b[?2026l';
  // The terminals are dark whatever the page is, as the panes' own colours
  // were picked for the terminal herdr runs in.
  const THEME = {
    background: '#0f1216',
    foreground: '#d6dbe1',
    cursor: '#0f1216',
    selectionBackground: 'rgba(127, 176, 255, 0.35)',
    black: '#1c2229',
    red: '#ff5f6d',
    green: '#7fd38f',
    yellow: '#e7b75a',
    blue: '#6fa6ff',
    magenta: '#c49cff',
    cyan: '#5ec9e8',
    white: '#c9cfd6',
    brightBlack: '#6f7884',
    brightRed: '#ff8a94',
    brightGreen: '#a6e6b2',
    brightYellow: '#f2d08f',
    brightBlue: '#9cc4ff',
    brightMagenta: '#dcc6ff',
    brightCyan: '#93ddf1',
    brightWhite: '#f4f6f8',
  };

  // Copy and paste are Cmd shortcuts on macOS, and ctrl+shift ones elsewhere.
  const mac = /Mac|iPhone|iPad/.test(navigator.userAgentData?.platform || navigator.platform || '');

  const utf8 = new TextEncoder();
  const size = op => utf8.encode(op.text || op.paste || (op.keys || []).join('')).length;

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  // copy puts a terminal's selected text on the clipboard. Without the
  // clipboard API, as on a page served over http to another computer, the
  // browser copies, and xterm gives it the selected text.
  function copy(term) {
    if (!term.hasSelection()) return;
    const text = term.getSelection();
    const byBrowser = () => document.execCommand('copy');
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).catch(byBrowser);
    else byBrowser();
  }

  // A terminal's size must be a whole number of cells, and at least one.
  const cells = n => Math.max(1, Math.min(2000, Math.floor(Number(n)) || 1));

  // cellAt is the size of a cell at a font size, in CSS pixels, as xterm's
  // renderer works it out: the width of W, and the font's height rounded up
  // to whole device pixels.
  const cellCache = new Map();
  let measureCtx = null;
  function cellAt(fontSize) {
    const dpr = window.devicePixelRatio || 1;
    const k = fontSize + '@' + dpr;
    let c = cellCache.get(k);
    if (c) return c;
    measureCtx = measureCtx || document.createElement('canvas').getContext('2d');
    measureCtx.font = fontSize + 'px ' + MONO;
    const m = measureCtx.measureText('W');
    const h = m.fontBoundingBoxAscent + m.fontBoundingBoxDescent || fontSize * 1.2;
    c = {w: m.width || fontSize * 0.6, h: Math.ceil(h * dpr) / dpr};
    cellCache.set(k, c);
    return c;
  }

  // fontFor is the largest font size, in half pixels, at which an area of
  // cols × rows cells fits in width × height pixels.
  function fontFor(cols, rows, width, height) {
    const base = cellAt(100);
    let f = Math.min(width / ((cols * base.w) / 100), height / ((rows * base.h) / 100));
    f = Math.max(MIN_FONT, Math.min(MAX_FONT, Math.floor(f * 2) / 2));
    while (f > MIN_FONT) {
      const c = cellAt(f);
      if (cols * c.w <= width && rows * c.h <= height) break;
      f -= 0.5;
    }
    return f;
  }

  // mount shows a tab's terminals in box until close is called. opts has:
  //   session, tab: the tab
  //   key(): the browser key
  //   parseEvent(block): reads one event-stream block
  //   toast(message)
  //   signOut(key): for a 401 to a request sent with key; it says whether
  //     the page signed out, which it doesn't when it has a new key since
  function mount(box, opts) {
    const status = el('p', 'term-status', 'Connecting…');
    const fit = el('div', 'term-fit');
    const stage = el('div', 'term-stage');
    fit.append(stage);
    box.replaceChildren(status, fit);

    const panes = new Map(); // pane ID → its terminal and what it waits for
    let layout = null; // the latest layout
    let font = 0;
    let closed = false;
    let mouseDown = false;
    let stream = null;
    let retryIn = 0;
    let retryTimer = 0;
    let placing = 0;

    function setStatus(text) {
      status.textContent = text;
      status.hidden = !text;
    }

    // ---- The stream

    function connect() {
      retryTimer = 0;
      if (closed) return;
      const ctl = new AbortController();
      stream = ctl;
      listen(ctl).then(again => {
        if (!again || closed || stream !== ctl) return;
        retryIn = Math.min(retryIn ? retryIn * 2 : 1000, 15000);
        retryTimer = setTimeout(connect, retryIn * (0.75 + Math.random() / 2));
      });
    }

    // listen reads the stream until it ends, and says whether to try again.
    async function listen(ctl) {
      let quiet = 0;
      let gone = ''; // why the session is gone, when the server says it is
      // The server pings every 25 s, so a stream silent for longer is gone.
      const wake = () => {
        clearTimeout(quiet);
        quiet = setTimeout(() => ctl.abort(), 70e3);
      };
      const url = '/api/tab/stream?session=' + encodeURIComponent(opts.session) + '&tab=' + encodeURIComponent(opts.tab);
      const k = opts.key();
      try {
        wake();
        const res = await fetch(url, {headers: {Authorization: 'Bearer ' + k}, signal: ctl.signal, cache: 'no-store'});
        // A 401 signs the page out, but for a key it has replaced since:
        // then the stream tries again with the new one.
        if (res.status === 401) return !opts.signOut(k);
        if (!res.ok || !res.body) {
          // The session or the tab is gone: its screens would only mislead.
          if (res.status === 404) dropPanes();
          setStatus((await message(res)) || 'The board server isn’t answering. Trying again…');
          return true;
        }
        if (!layout) setStatus('Reading the tab…');
        else setStatus('');
        const reader = res.body.getReader();
        const text = new TextDecoder();
        let buf = '';
        for (;;) {
          const {value, done} = await reader.read();
          if (done) break;
          wake();
          buf += text.decode(value, {stream: true});
          for (let end; (end = buf.indexOf('\n\n')) >= 0; ) {
            const ev = opts.parseEvent(buf.slice(0, end));
            buf = buf.slice(end + 2);
            gone = gotEvent(ev) || gone;
          }
        }
      } catch (e) {
        // The network failed, or the stream was stopped.
      } finally {
        clearTimeout(quiet);
      }
      // It still tries again, in case herdr starts again.
      if (!closed) setStatus(gone || 'Reconnecting…');
      return true;
    }

    // gotEvent handles an event. For a gone event, it returns why the
    // session is gone.
    function gotEvent(ev) {
      let data;
      try {
        data = JSON.parse(ev.data);
      } catch (e) {
        return '';
      }
      if (!data || closed) return '';
      retryIn = 0;
      if (ev.event === 'layout' && Array.isArray(data.panes)) gotLayout(data);
      else if (ev.event === 'screen') gotScreen(data);
      else if (ev.event === 'gone') {
        dropPanes();
        const why = data.error || 'This session ended.';
        setStatus(why);
        return why;
      }
      return '';
    }

    // ---- Panes

    function gotLayout(l) {
      layout = l;
      const want = new Set(l.panes.map(p => p.pane));
      for (const [id, p] of panes) {
        if (!want.has(id)) dropPane(p);
      }
      for (const info of l.panes) {
        let p = panes.get(info.pane);
        if (!p) {
          p = makePane(info);
          panes.set(info.pane, p);
        }
        p.info = info;
        p.agent.textContent = info.agent || '';
        p.agent.hidden = !info.agent;
        p.title.textContent = info.title || (info.agent ? '' : 'Shell');
        p.el.setAttribute('aria-label', [info.agent, info.title].filter(Boolean).join(': ') || 'Terminal');
      }
      setStatus(l.panes.length ? '' : 'This tab has no panes to show. It may have closed.');
      place();
    }

    // dropPane takes a pane's terminal away. Keys on their way to it still
    // go.
    function dropPane(p) {
      p.term.dispose();
      p.el.remove();
      panes.delete(p.id);
    }

    // dropPanes takes every pane away, as when the session is gone.
    function dropPanes() {
      for (const p of panes.values()) dropPane(p);
      layout = null;
    }

    function makePane(info) {
      const p = {id: info.pane, info, pending: null};
      p.el = el('section', 'term-pane');
      const head = el('div', 'term-head');
      p.agent = el('span', 'term-agent');
      p.title = el('span', 'term-title');
      p.badge = el('span', 'term-badge', 'scrolled back in herdr');
      p.badge.hidden = true;
      head.append(p.agent, p.title, p.badge);
      p.head = head;
      p.screen = el('div', 'term-screen');
      p.el.append(head, p.screen);
      stage.append(p.el);

      const term = new window.Terminal({
        cols: cells(info.cols),
        rows: cells(info.rows),
        scrollback: 0,
        allowProposedApi: true,
        cursorBlink: false,
        fontFamily: MONO,
        fontSize: font || 12,
        theme: THEME,
        // The mouse isn't sent. xterm would type arrow keys for an alt+click,
        // and a right click on macOS would select a word, which holds the
        // pane still.
        altClickMovesCursor: false,
        rightClickSelectsWord: false,
      });
      term.loadAddon(new window.Unicode11Addon.Unicode11Addon());
      term.unicode.activeVersion = '11';
      term.open(p.screen);
      // With no scrollback, xterm would type arrow keys for the wheel. Left
      // alone, the wheel scrolls the page.
      term.attachCustomWheelEventHandler(() => false);
      // No wrapping, so a row too wide is cut off rather than moving the
      // rows under it, and no cursor, as herdr doesn't say where it is.
      term.write('\x1b[?7l\x1b[?25l');
      p.term = term;

      term.attachCustomKeyEventHandler(ev => {
        const op = window.herdrKeymap.mapKey(ev, {mac});
        if (!op) return true;
        // Kept from xterm, and left to the browser: Cmd shortcuts, and its
        // paste, which the paste listener below sends.
        if (op.browser) return false;
        ev.preventDefault();
        ev.stopPropagation();
        if (op.copy) {
          copy(term);
        } else {
          // Typing ends a selection, as it does in xterm, which doesn't see
          // these keys. The screen held for it then shows.
          term.clearSelection();
          send(opts, p.id, op);
        }
        return false;
      });
      // Keys that make an escape are sent above, by name or as sequences, so
      // data that starts with ESC is xterm answering the terminal: a reply to
      // a query, or a mouse or focus report. None of it was typed.
      term.onData(data => {
        if (data.charCodeAt(0) !== 0x1b) send(opts, p.id, {text: data});
      });
      // Before xterm's own paste, which would type it.
      term.element.addEventListener(
        'paste',
        ev => {
          ev.preventDefault();
          ev.stopImmediatePropagation();
          const text = ev.clipboardData ? ev.clipboardData.getData('text/plain') : '';
          if (text) {
            term.clearSelection();
            send(opts, p.id, {paste: text.replace(/\r?\n/g, '\r')});
          }
        },
        true,
      );
      term.onSelectionChange(() => flush(p));
      // A left button selects text. A right one, or ctrl+click on macOS,
      // opens the context menu, which may not say when the button goes up.
      p.el.addEventListener('pointerdown', e => {
        if (e.button === 0 && !(mac && e.ctrlKey)) mouseDown = true;
      });
      p.el.addEventListener('click', e => {
        if (!p.screen.contains(e.target)) term.focus();
      });
      p.el.addEventListener('focusin', () => p.el.classList.add('focused'));
      p.el.addEventListener('focusout', () => p.el.classList.remove('focused'));
      return p;
    }

    function gotScreen(s) {
      const p = panes.get(s.pane);
      if (!p || typeof s.ansi !== 'string') return;
      p.pending = s;
      flush(p);
    }

    // flush shows a pane's latest screen, unless text is being selected in
    // it: the screen would move under the selection. Typing into the pane
    // ends the selection.
    function flush(p) {
      const s = p.pending;
      if (!s || mouseDown || p.term.hasSelection()) return;
      p.pending = null;
      const cols = cells(s.cols);
      const rows = cells(s.rows);
      if (cols !== p.term.cols || rows !== p.term.rows) p.term.resize(cols, rows);
      p.term.write(BEGIN + s.ansi + END);
      p.badge.hidden = !s.scrolled;
    }

    function mouseUp() {
      if (!mouseDown) return;
      mouseDown = false;
      for (const p of panes.values()) flush(p);
    }

    // place sizes the font so the whole tab fits, and puts each pane where
    // herdr has it. A pane's rect holds its border: the header goes in the
    // top row, and the terminal inside.
    function place() {
      if (!layout || !layout.panes.length) return;
      const area = layout.area || {};
      const aw = Math.max(1, area.width || 0);
      const ah = Math.max(1, area.height || 0);
      const f = fontFor(aw, ah, fit.clientWidth, fit.clientHeight);
      if (f !== font) {
        font = f;
        for (const p of panes.values()) p.term.options.fontSize = f;
      }
      const c = cellAt(f);
      stage.style.width = aw * c.w + 'px';
      stage.style.height = ah * c.h + 'px';
      for (const p of panes.values()) {
        const r = p.info;
        const insetX = r.width - r.cols >= 2 ? 1 : 0;
        const insetY = r.height - r.rows >= 2 ? 1 : 0;
        Object.assign(p.el.style, {
          left: (r.x - (area.x || 0)) * c.w + 'px',
          top: (r.y - (area.y || 0)) * c.h + 'px',
          width: r.width * c.w + 'px',
          height: r.height * c.h + 'px',
        });
        Object.assign(p.head.style, {
          height: c.h + 'px',
          lineHeight: c.h + 'px',
          fontSize: Math.max(7, Math.min(12, c.h * 0.75)) + 'px',
        });
        p.screen.style.left = insetX * c.w + 'px';
        p.screen.style.top = insetY * c.h + 'px';
      }
    }

    function placeSoon() {
      if (placing) return;
      placing = requestAnimationFrame(() => {
        placing = 0;
        if (!closed) place();
      });
    }

    // ---- Setting up

    const resized = new ResizeObserver(placeSoon);
    resized.observe(fit);
    // A button let go outside the window may not say so; leaving it does.
    // A context menu takes the button, and on macOS opens before it's let go.
    const ups = ['pointerup', 'pointercancel', 'blur', 'contextmenu'];
    for (const type of ups) window.addEventListener(type, mouseUp);
    connect();

    return {
      close() {
        if (closed) return;
        closed = true;
        clearTimeout(retryTimer);
        if (stream) stream.abort();
        cancelAnimationFrame(placing);
        resized.disconnect();
        for (const type of ups) window.removeEventListener(type, mouseUp);
        for (const p of panes.values()) p.term.dispose();
        panes.clear();
        box.replaceChildren();
      },
    };
  }

  // ---- Typing

  // What's queued to type into each pane, by session and pane: {id,
  // session, pane, opts, ops, busy}, where opts are the view's. A queue
  // outlives the view that typed into it, and the pane's place in the
  // layout, as when another pane is zoomed: keys on their way still go, and
  // keys typed in a view opened again go after them.
  const inputs = new Map();

  // send queues an op for a pane of the view with opts. Text typed in a row
  // goes as one op.
  function send(opts, pane, op) {
    const id = opts.session + '\n' + pane;
    let q = inputs.get(id);
    if (!q) {
      q = {id, session: opts.session, pane, opts, ops: [], busy: false};
      inputs.set(id, q);
    }
    q.opts = opts;
    const last = q.ops[q.ops.length - 1];
    if (op.text !== undefined && last && last.text !== undefined && last.text.length + op.text.length <= MAX_MERGE) {
      last.text += op.text;
    } else {
      q.ops.push(op);
    }
    pump(q);
  }

  // pump sends a pane's queued ops, one request at a time, and forgets the
  // queue once it's empty. When one fails, everything queued after it is
  // dropped: keys typed after a lost one would do something else. The board
  // says so, whether or not the view is still open.
  async function pump(q) {
    if (q.busy || !q.ops.length) return;
    let n = 0;
    let total = 0;
    while (n < q.ops.length && n < MAX_OPS) {
      const s = size(q.ops[n]);
      if (n && total + s > MAX_INPUT) break;
      total += s;
      n++;
    }
    const ops = q.ops.splice(0, n);
    const opts = q.opts;
    const k = opts.key();
    q.busy = true;
    let failed = '';
    try {
      const res = await fetch('/api/pane/input', {
        method: 'POST',
        headers: {Authorization: 'Bearer ' + k, 'Content-Type': 'application/json'},
        body: JSON.stringify({session: q.session, pane: q.pane, ops}),
      });
      if (res.status === 401) {
        // Signed out, nothing queued for any pane can go. Signed in again
        // since, with a new key, only these keys are lost.
        if (opts.signOut(k)) for (const x of inputs.values()) x.ops = [];
        else failed = 'Signed in again. Try that again.';
      }
      if (!res.ok && !failed) failed = (await message(res)) || 'That didn’t work (' + res.status + ').';
    } catch (e) {
      failed = 'The board server isn’t answering.';
    }
    q.busy = false;
    if (failed) {
      q.ops = [];
      opts.toast('Not typed: ' + failed);
    }
    if (q.ops.length) pump(q);
    else if (inputs.get(q.id) === q) inputs.delete(q.id);
  }

  // message is the error a reply carries, if any.
  async function message(res) {
    try {
      return (await res.json()).error || '';
    } catch (e) {
      return '';
    }
  }

  window.herdrTerm = {mount};
})();
