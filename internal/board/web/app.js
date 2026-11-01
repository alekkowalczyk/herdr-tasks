// The task board page. It signs in with the one-time code in its address.
// At /<session>/ it shows the board the server streams for that herdr
// session, and posts task changes back; opening a card shows its tab's
// terminals (term.js) next to the whole card. At / it lists the running
// sessions, each a link to its board.
//
// Titles, notes, recaps, prompts and links come from agents and other
// people. The page builds its DOM with createElement, textContent and
// setAttribute only, and makes links of http and https URLs only. A local
// file's link is a button that asks the server to open it. So is a
// localhost link on a board seen through the tailnet, which asks to share
// its port first.
(() => {
  'use strict';

  // What the page keeps in the browser.
  const KEY = 'herdr-tasks-board-key';
  const SPACES = 'herdr-tasks-board-spaces';
  const DONE_OPEN = 'herdr-tasks-board-done-open';
  const PRIO_ONLY = 'herdr-tasks-board-priority-only';

  const COLUMNS = [
    {id: 'asking', label: 'Asking', hint: 'The agent stopped mid-work, waiting for your answer'},
    {id: 'yourturn', label: 'Your turn', hint: 'The agent finished, or nothing is running'},
    {id: 'running', label: 'Working', hint: 'The agent is working'},
    {id: 'review', label: 'In review', hint: 'Your part is done; waiting on approval, merge, or testing'},
    {id: 'paused', label: 'Paused', hint: 'On hold until something happens'},
    {id: 'parked', label: 'Parked', hint: ''},
    {id: 'done', label: 'Done', hint: 'Close when ready'},
  ];

  // Where the Move to menu can send a card. Asking, Your turn and Working
  // are all Active: the agent decides which of them shows the card.
  const MOVES = [
    {stage: 'active', label: 'Active'},
    {stage: 'review', label: 'In review…'},
    {stage: 'paused', label: 'Paused…'},
    {stage: 'parked', label: 'Parked'},
    {stage: 'done', label: 'Done'},
  ];

  // The stages that record what they wait for, worded as the strip does.
  const REASONS = {
    review: {title: 'In review', label: 'Waiting for', lead: 'waiting for', placeholder: 'approval, merge, testing…'},
    paused: {title: 'Paused', label: 'Until', lead: 'until', placeholder: 'PR review, QA, your own decision…'},
  };

  // A card's links show in these groups, in this order, as in the strip.
  // The server gives each link's kind; a kind the page doesn't know goes
  // with the other links.
  const SECTIONS = [
    {id: 'linear', title: 'Linear', kinds: ['linear']},
    {id: 'slack', title: 'Slack', kinds: ['slack']},
    {id: 'pages', title: 'Pages', kinds: ['artifact', 'hosted', 'file', 'local']},
    {id: 'other', title: 'Links', kinds: []},
  ];

  // The marker on a page's chip: on the web, a file, or on this machine.
  const PAGE_MARKS = {artifact: 'web', hosted: 'web', file: 'file', local: 'local'};

  const isActive = column => column === 'asking' || column === 'yourturn' || column === 'running';
  const stageOf = column => (isActive(column) ? 'active' : column);

  // activeColumn is where an Active task shows, by what its tab's agent is doing.
  function activeColumn(agentStatus) {
    if (agentStatus === 'working') return 'running';
    if (agentStatus === 'blocked') return 'asking';
    return 'yourturn';
  }

  // Browser storage may be missing or throw. The page then keeps things in
  // memory for as long as it's open.
  const memory = new Map();
  function load(name) {
    if (memory.has(name)) return memory.get(name);
    try {
      return localStorage.getItem(name);
    } catch (e) {
      return null;
    }
  }
  function save(name, value) {
    memory.set(name, value);
    try {
      localStorage.setItem(name, value);
    } catch (e) {
      // kept in memory only
    }
  }
  const key = () => load(KEY) || '';
  // prioOnly is whether the board shows only the priority tasks.
  const prioOnly = () => load(PRIO_ONLY) === '1';

  let board = null; // the latest board the server sent
  let list = null; // at /, the latest list of sessions
  let index = new Map(); // card key → that card on the board
  let signedOut = false;
  // Changes posted but not on the board yet, by card key. Each changes how
  // the card shows until the board has it, the post fails, or 5 s pass.
  const overrides = new Map();
  const cardEls = new Map(); // card key → its element
  const columnEls = new Map(); // column id → {section, cards, count, toggle}
  let dragging = null; // {key, stage, el} while a card is dragged
  let held = false; // a render waits until the drag or the menu ends
  let menuFor = null; // {key, trigger} while the Move to menu is open
  let reasonFor = null; // {key, stage} while the reason dialog is open
  let detailKey = null; // the card the detail panel shows
  let detailSig = null;
  let viewing = null; // {session, tab, terms} while a tab's view is open
  // Cards with a move, a research start or a continue under way. Each can
  // take a while, so the card offers none of them again until it's answered.
  const working = new Set();
  // The Slack line: its buttons, the skipped asks, as the board has them, a
  // fetch and takes under way, and what the last of them said, for 15 s.
  let slackEls = null; // {fetch, said, skipped}
  let skipped = [];
  let fetching = false;
  const taking = new Set(); // message IDs
  let slackReply = null; // {text, problem}
  let replyTimer = 0;
  // Sharing localhost ports: the shares in the header and the dialog that
  // asks before a port is shared, and the link it asks about while it's
  // open.
  let shareEls = null; // {box, list, dialog, title}
  let shareFor = null; // {c, url}

  // The page's session is the first part of its path: /work/. At / there
  // is none, and the page is the list of sessions.
  const session = (() => {
    const m = /^\/([^/]+)\/$/.exec(location.pathname);
    try {
      return m ? decodeURIComponent(m[1]) : '';
    } catch (e) {
      return '';
    }
  })();
  const BOARD_URL = session ? '/api/board?session=' + encodeURIComponent(session) : '/api/board';
  const EVENTS_URL = session ? '/api/events?session=' + encodeURIComponent(session) : '/api/events';
  // pageOf is the address of a session's board.
  function pageOf(name) {
    return '/' + encodeURIComponent(name) + '/';
  }
  // The board serves no page for sessions called - or api, so the list
  // shows them without a link.
  const hasPage = name => name !== '-' && name !== 'api';

  const $ = id => document.getElementById(id);
  const keyOf = c => c.session + '\n' + c.tab;

  // el makes an element with a class and, as text, its content.
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null && text !== '') e.textContent = String(text);
    return e;
  }

  function attrs(e, map) {
    for (const [name, value] of Object.entries(map)) e.setAttribute(name, value);
    return e;
  }

  // age is a time since as task.Age writes it: now, 4m, 3h, 2d.
  function age(since) {
    const d = Date.now() - Date.parse(since);
    if (!(d >= 60e3)) return 'now';
    if (d < 3600e3) return Math.floor(d / 60e3) + 'm';
    if (d < 48 * 3600e3) return Math.floor(d / 3600e3) + 'h';
    return Math.floor(d / 86400e3) + 'd';
  }

  // clock is a time as the overview writes log entries: 14:03, Mon 14:03.
  function clock(at) {
    const t = new Date(at);
    if (isNaN(t)) return '';
    const hm = t.toLocaleTimeString([], {hour: 'numeric', minute: '2-digit'});
    const ago = Date.now() - t;
    if (ago > 6 * 86400e3) return t.toLocaleDateString([], {month: 'short', day: 'numeric'}) + ' ' + hm;
    if (ago > 20 * 3600e3) return t.toLocaleDateString([], {weekday: 'short'}) + ' ' + hm;
    return hm;
  }

  // ageEl shows how long ago since was, and keeps it current.
  function ageEl(since, title) {
    return attrs(el('time', 'age', age(since)), {
      'data-since': since,
      datetime: since,
      title: title + ' ' + new Date(since).toLocaleString(),
    });
  }

  function tickAges() {
    for (const t of document.querySelectorAll('time[data-since]')) t.textContent = age(t.getAttribute('data-since'));
  }

  function linkable(url) {
    try {
      const u = new URL(url);
      return u.protocol === 'http:' || u.protocol === 'https:';
    } catch (e) {
      return false;
    }
  }

  // localPort is the port of a localhost or 127.0.0.1 link on a board seen
  // through the tailnet, which is https: there localhost is the browser's
  // own machine, so the port has to be shared. It's 0 for any other link,
  // and on a board on 127.0.0.1, where the link opens as it is.
  function localPort(url) {
    if (location.protocol !== 'https:') return 0;
    try {
      const u = new URL(url);
      const local = u.hostname === 'localhost' || u.hostname === '127.0.0.1';
      return (u.protocol === 'http:' || u.protocol === 'https:') && local && u.port ? Number(u.port) : 0;
    } catch (e) {
      return 0;
    }
  }

  function sectionOf(l) {
    return SECTIONS.find(s => s.kinds.includes(l.kind)) || SECTIONS[SECTIONS.length - 1];
  }

  // linkGroups is the groups that have links, in order, each with its links.
  function linkGroups(links) {
    return SECTIONS.map(section => ({section, links: links.filter(l => sectionOf(l) === section)})).filter(g => g.links.length);
  }

  function reasonText(c) {
    const r = REASONS[c.stage];
    return r && c.reason ? r.lead + ' ' + c.reason : '';
  }

  function firstLine(text) {
    return (text || '').split('\n').map(l => l.trim()).find(l => l) || '';
  }

  // glyph is how the sidebar shows an agent's status.
  function glyph(status) {
    if (status === 'blocked') return {ch: '?', cls: 'ask', word: 'asking'};
    if (status === 'working') return {ch: '↻', cls: 'run', word: 'working'};
    if (status === 'done') return {ch: '●', cls: 'you', word: 'answered'};
    return {ch: '●', cls: '', word: status === 'idle' ? 'idle' : 'no status'};
  }

  // ---- Talking to the server

  // signIn swaps the code in the address for the browser key, and takes the
  // code out of the address. The page stays where it is. Opened through the
  // tailnet, which is https, it asks without a code: the machine's owner
  // needs none.
  async function signIn() {
    const code = new URLSearchParams(location.search).get('code');
    if (code === null && location.protocol !== 'https:') return;
    if (code === null) ownerAsked = true;
    try {
      await askKey(code === null ? {} : {code});
    } finally {
      history.replaceState(null, '', location.pathname + location.hash);
    }
  }

  // askKey swaps body, a code or {} for the owner, for the browser key, and
  // keeps the key. It says whether it got one.
  async function askKey(body) {
    try {
      const res = await fetch('/api/session', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(body),
      });
      if (!res.ok) return false;
      const reply = await res.json();
      if (reply && typeof reply.key === 'string' && reply.key) {
        save(KEY, reply.key);
        return true;
      }
    } catch (e) {
      // The server didn't answer. A key from before may still work.
    }
    return false;
  }

  let stream = null; // the AbortController of the open stream
  let streamKey = ''; // the key the stream signed in with
  let retryIn = 0;
  let retryTimer = 0;
  let misses = 0; // times in a row the server hadn't read this session
  // Through the tailnet: the page asked for a key without a code since the
  // last board came, and is asking now.
  let ownerAsked = false;
  let asking = false;

  function connect() {
    clearTimeout(retryTimer);
    retryTimer = 0;
    if (stream) stream.abort();
    stream = null;
    const k = key();
    if (!k) {
      signOut();
      return;
    }
    streamKey = k;
    signedOut = false;
    const ctl = new AbortController();
    stream = ctl;
    listen(ctl, k).then(again => {
      if (stream !== ctl) return;
      if (again === 'missing') wait();
      else if (again) retry();
    });
  }

  // listen loads the board, or the list of sessions, and reads its stream,
  // signed in with k, until it ends. It says whether to try again, or
  // 'missing' when the server has no such session.
  async function listen(ctl, k) {
    let quiet = 0;
    // The server pings every 25 s, so a stream silent for longer is gone.
    const wake = () => {
      clearTimeout(quiet);
      quiet = setTimeout(() => ctl.abort(), 70e3);
    };
    try {
      wake();
      const init = {headers: {Authorization: 'Bearer ' + k}, signal: ctl.signal, cache: 'no-store'};
      const first = await fetch(BOARD_URL, init);
      if (first.status === 401) {
        signOut(k);
        return false;
      }
      if (first.status === 404) return 'missing';
      if (!first.ok) return true;
      got(await first.text());
      const res = await fetch(EVENTS_URL, init);
      if (res.status === 401) {
        signOut(k);
        return false;
      }
      if (res.status === 404) return 'missing';
      if (!res.ok || !res.body) return true;
      const reader = res.body.getReader();
      const text = new TextDecoder();
      let buf = '';
      for (;;) {
        const {value, done} = await reader.read();
        if (done) return true;
        wake();
        buf += text.decode(value, {stream: true});
        for (let end; (end = buf.indexOf('\n\n')) >= 0; ) {
          const ev = parseEvent(buf.slice(0, end));
          buf = buf.slice(end + 2);
          if (ev.event === (session ? 'board' : 'sessions')) got(ev.data);
        }
      }
    } catch (e) {
      return true; // the network failed, or the stream was stopped
    } finally {
      clearTimeout(quiet);
    }
  }

  // parseEvent reads one event-stream block.
  function parseEvent(block) {
    const ev = {event: 'message', data: ''};
    const data = [];
    for (let line of block.split('\n')) {
      line = line.replace(/\r$/, '');
      if (!line || line[0] === ':') continue;
      const i = line.indexOf(':');
      const field = i < 0 ? line : line.slice(0, i);
      let value = i < 0 ? '' : line.slice(i + 1);
      if (value[0] === ' ') value = value.slice(1);
      if (field === 'event') ev.event = value;
      else if (field === 'data') data.push(value);
    }
    ev.data = data.join('\n');
    return ev;
  }

  function retry() {
    retryIn = Math.min(retryIn ? retryIn * 2 : 1000, 15000);
    const lost = retryIn >= 4000;
    const shown = board || list;
    $('conn').textContent = lost ? 'The board server isn’t answering' : shown ? 'Reconnecting…' : 'Connecting…';
    if (!shown && lost) {
      showMessage(
        document.createTextNode('The board server isn’t answering. Run '),
        el('code', '', 'herdr-tasks board'),
        document.createTextNode(' to start it.'),
      );
    }
    retryTimer = setTimeout(connect, retryIn * (0.75 + Math.random() / 2));
  }

  // wait asks for the session again in 2 s. The server may not have read it
  // yet, as when the board opens the page as it starts; after about 10 s the
  // page says it isn't running.
  function wait() {
    if (++misses > 5) {
      giveUp();
      return;
    }
    retryTimer = setTimeout(connect, 2000);
  }

  function giveUp() {
    board = null;
    $('conn').textContent = '';
    if (viewing) {
      history.replaceState(null, '', location.pathname + location.search);
      hideView();
    }
    showMessage(...goneText());
  }

  function goneText() {
    return [document.createTextNode('This session isn’t running. '), attrs(el('a', '', 'See the running sessions'), {href: '/'})];
  }

  // got reads what the stream sends: the session's board, or the list.
  function got(data) {
    let v;
    try {
      v = JSON.parse(data);
    } catch (e) {
      return;
    }
    if (!v || !Array.isArray(v.sessions)) return;
    misses = 0;
    ownerAsked = false;
    if (session) gotBoard(v);
    else gotList(v);
  }

  function gotList(l) {
    list = l;
    retryIn = 0;
    $('conn').textContent = '';
    $('version').textContent = l.version ? 'v' + l.version : '';
    renderTailnet(l.tailnet);
    const only = l.sessions.length === 1 ? l.sessions[0] : null;
    if (only && hasPage(only.name)) {
      location.replace(pageOf(only.name));
      return;
    }
    if (!l.sessions.length) {
      showMessage(document.createTextNode('No herdr session is running.'));
      return;
    }
    renderList(l);
  }

  function gotBoard(b) {
    board = b;
    index = new Map();
    for (const s of b.sessions) for (const c of s.cards) index.set(keyOf(c), c);
    retryIn = 0;
    $('conn').textContent = '';
    render();
  }

  // signOut shows the page signed out, and says whether it did. A 401 for a
  // request sent with a key the page has replaced since, as when another
  // tab signed in, is about that key only: sentWith, when given, is the key
  // the request used, and the page stays signed in with the new one.
  // Through the tailnet, where the machine's owner needs no code, it first
  // asks for a key without one, as the page does when it loads, so the owner
  // stays signed in when the server restarts. It asks once until a board
  // comes again, so a key that doesn't work ends there.
  function signOut(sentWith) {
    if (sentWith !== undefined && sentWith !== key()) return false;
    clearTimeout(retryTimer);
    retryTimer = 0;
    if (stream) stream.abort();
    stream = null;
    $('conn').textContent = '';
    if (location.protocol === 'https:' && !ownerAsked) {
      ownerAsked = asking = true;
      askKey({}).then(got => {
        asking = false;
        if (got) connect();
        else signOut();
      });
    }
    if (asking) return true;
    signedOut = true;
    if (viewing) {
      history.replaceState(null, '', location.pathname + location.search);
      hideView();
    }
    render();
    return true;
  }

  // post sends a task request, to /api/task unless path says otherwise. It
  // gives the server's answer, or null when there's none, and fails with
  // the server's message, the answer in its reply.
  async function post(body, path) {
    const k = key();
    let res;
    try {
      res = await fetch(path || '/api/task', {
        method: 'POST',
        headers: {Authorization: 'Bearer ' + k, 'Content-Type': 'application/json'},
        body: JSON.stringify(body),
      });
    } catch (e) {
      throw new Error('The board server isn’t answering.');
    }
    let reply = null;
    if (res.status !== 204) {
      try {
        reply = await res.json();
      } catch (e) {
        // not JSON
      }
    }
    if (res.ok) return reply;
    const msg = reply && typeof reply.error === 'string' ? reply.error : '';
    if (res.status === 401 && !signOut(k)) throw new Error('Signed in again. Try that again.');
    throw Object.assign(new Error(msg || 'That didn’t work (' + res.status + ').'), {reply});
  }

  // ---- Changing tasks

  // cardByKey is a card as the page shows it, with changes still on their
  // way, or null when it isn't on the board.
  function cardByKey(k) {
    const c = index.get(k);
    if (!c) return null;
    let shown = c;
    for (const o of overrides.get(k) || []) shown = o.apply(shown);
    return shown;
  }

  // prune forgets changes the board has, and ones it should have had by now.
  // A change the board has replaces every change made to the card before
  // it, as when Priority is clicked on and off again.
  function prune() {
    const now = Date.now();
    for (const [k, list] of overrides) {
      const c = index.get(k);
      let from = 0;
      if (c) {
        for (let i = list.length - 1; i >= 0; i--) {
          if (list[i].done && list[i].settled(c)) {
            from = i + 1;
            break;
          }
        }
      }
      const keep = list.slice(from).filter(o => !o.done || (c && !o.settled(c) && now - o.done < 5000));
      if (keep.length) overrides.set(k, keep);
      else overrides.delete(k);
    }
  }

  // change shows a change to a card at once and posts it. If the post
  // fails, the card goes back and a toast says why.
  function change(c, body, apply, settled) {
    const k = keyOf(c);
    const o = {apply, settled, done: 0};
    overrides.set(k, (overrides.get(k) || []).concat(o));
    render();
    post(Object.assign({session: c.session, tab: c.tab}, body)).then(
      () => {
        o.done = Date.now();
        setTimeout(render, 5100);
        render();
      },
      err => {
        const list = (overrides.get(k) || []).filter(x => x !== o);
        if (list.length) overrides.set(k, list);
        else overrides.delete(k);
        render();
        toast(err.message);
      },
    );
  }

  function changeStage(c, stage, reason) {
    const at = new Date().toISOString();
    change(
      c,
      {action: 'stage', stage: stage === 'active' ? activeColumn(c.agentStatus) : stage, reason},
      x => {
        const y = Object.assign({}, x, {stage, column: stage === 'active' ? activeColumn(x.agentStatus) : stage});
        if (x.stage !== stage) y.since = at;
        if (!REASONS[stage]) y.reason = '';
        else if (reason) y.reason = reason;
        if (stage === 'done') y.priority = false;
        return y;
      },
      x => x.stage === stage && (!reason || x.reason === reason),
    );
  }

  function setPriority(c, on) {
    change(c, {action: 'priority', on}, x => Object.assign({}, x, {priority: on}), x => x.priority === on);
  }

  function unlink(c, url) {
    change(
      c,
      {action: 'unlink', url},
      x => Object.assign({}, x, {links: x.links.filter(l => l.url !== url)}),
      x => !x.links.some(l => l.url === url),
    );
  }

  function showInHerdr(c) {
    post({session: c.session, tab: c.tab, action: 'focus'}).catch(err => toast(err.message));
  }

  // openFile opens a task's local page from the board, as browsers won't
  // open file links from a web page.
  function openFile(c, url) {
    openThrough('/api/open', c, url);
  }

  // openThrough opens a task's link at the address the board gives for it
  // at path. The window opens at the click, so popup blockers allow it, and
  // goes to the address once the board gives it.
  function openThrough(path, c, url) {
    const w = window.open('', '_blank');
    if (!w) {
      toast('Your browser blocked the page’s window. Allow pop-ups for the board.');
      return;
    }
    w.opener = null;
    post({session: c.session, tab: c.tab, url}, path).then(
      reply => {
        w.location = reply.href;
      },
      err => {
        w.close();
        toast(err.message);
      },
    );
  }

  // openLocal opens a task's localhost link on a board seen through the
  // tailnet. A port herdr-tasks shares opens at this machine's address on
  // the tailnet; any other is shared first, once you say so.
  function openLocal(c, url) {
    const port = localPort(url);
    if (!port) return;
    const shares = (board && board.tailnet && board.tailnet.shares) || [];
    if (!shares.some(s => s.port === port)) {
      askShare(c, url, port);
      return;
    }
    const u = new URL(url);
    const at = location.protocol + '//' + location.hostname + (port === 443 ? '' : ':' + port);
    window.open(at + u.pathname + u.search + u.hash, '_blank', 'noopener,noreferrer');
  }

  // askShare asks, in the page, before a port is shared with the tailnet.
  function askShare(c, url, port) {
    shareFor = {c, url};
    shareEls.title.textContent = 'Share localhost:' + port + ' with your tailnet?';
    shareEls.dialog.showModal();
  }

  // stopShare stops sharing a port. The header drops it once the board
  // has.
  function stopShare(b) {
    b.disabled = true;
    post({port: Number(b.getAttribute('data-port'))}, '/api/unshare').catch(err => {
      b.disabled = false;
      toast(err.message);
    });
  }

  function copyPath(c, url) {
    const l = c.links.find(x => x.url === url);
    if (!l || !l.path) return;
    const done = () => toast('Copied ' + l.path);
    const failed = () => toast('Couldn’t copy the path. It’s ' + l.path);
    if (navigator.clipboard) navigator.clipboard.writeText(l.path).then(done, failed);
    else failed();
  }

  // otherSpaces are the spaces of c's session that c can move to.
  function otherSpaces(c) {
    const s = board ? board.sessions.find(x => x.name === c.session) : null;
    return s ? s.spaces.filter(x => x.id !== c.space) : [];
  }

  // muteLabel names the mute of a's channel.
  const muteLabel = a => (/^#/.test(a.place || '') ? 'Mute ' + a.place : 'Mute this channel');

  // cardActions are what the Move to menu offers after the stages, and the
  // tab's view next to its buttons: a move to another workspace, and a
  // Slack task's research and mute, as its research and channel allow.
  function cardActions(c) {
    const busy = working.has(keyOf(c));
    const out = [];
    if (!busy && otherSpaces(c).length) out.push({act: 'spaces', label: 'Move to workspace…'});
    const a = c.slack;
    if (a) {
      const research = a.research || '';
      if (!busy && research === 'locked') out.push({act: 'continue', label: 'Continue with my tools'});
      if (!busy && (research === '' || research === 'held' || research === 'failed')) out.push({act: 'research', label: 'Start research'});
      if (!a.muted) out.push({act: 'mute', label: muteLabel(a)});
    }
    return out;
  }

  // work posts an action that can take a while, saying what it's doing
  // meanwhile. Moving a task's panes takes a moment; starting research waits
  // for Claude to be ready, which can take minutes.
  function work(c, body, doing) {
    const k = keyOf(c);
    working.add(k);
    toast(doing);
    render();
    const end = () => {
      working.delete(k);
      render();
    };
    return post(Object.assign({session: c.session, tab: c.tab}, body)).then(
      reply => {
        end();
        return reply;
      },
      err => {
        end();
        throw err;
      },
    );
  }

  // moveToSpace moves a card's task, with its panes, to another workspace.
  // An open view of its tab follows it to its new tab, also when the move
  // went wrong partway.
  function moveToSpace(k, id) {
    const c = cardByKey(k);
    if (!c || working.has(k)) return;
    const space = otherSpaces(c).find(x => x.id === id);
    const label = space ? space.label : id;
    const follow = reply => {
      const tab = reply && typeof reply.tab === 'string' ? reply.tab : '';
      if (!tab || !viewing || viewing.session !== c.session || viewing.tab !== c.tab) return;
      history.replaceState(history.state, '', viewHash(tab));
      route();
    };
    work(c, {action: 'move', workspace: id}, 'Moving to ' + label + '…').then(
      reply => {
        follow(reply);
        toast('Moved to ' + label);
      },
      err => {
        follow(err.reply);
        toast(err.message);
      },
    );
  }

  // slackAction starts a Slack task's research, continues it with your
  // tools, or mutes its channel.
  function slackAction(k, act) {
    const c = cardByKey(k);
    if (!c || !c.slack) return;
    if (act === 'mute') {
      const place = c.slack.place || c.slack.channel;
      post({session: c.session, tab: c.tab, action: 'mute'}).then(
        () => toast('Muted ' + place + '. Undo: herdr-tasks slack unmute ' + c.slack.channel),
        err => toast(err.message),
      );
      return;
    }
    if (working.has(k)) return;
    const [doing, done] =
      act === 'research' ? ['Starting research…', 'Research started'] : ['Continuing with your tools…', 'Continued with your tools'];
    work(c, {action: act}, doing).then(
      () => toast(done),
      err => toast(err.message),
    );
  }

  // moveTo moves a card to a stage. In review and Paused ask what they
  // wait for first.
  function moveTo(k, stage) {
    const c = cardByKey(k);
    if (!c) return;
    if (REASONS[stage]) askReason(c, stage);
    else if (c.stage !== stage) changeStage(c, stage, '');
  }

  function askReason(c, stage) {
    const r = REASONS[stage];
    reasonFor = {key: keyOf(c), stage};
    $('reason-title').textContent = r.title;
    $('reason-task').textContent = c.title;
    $('reason-label').textContent = r.label;
    const input = $('reason-text');
    input.value = c.reason || '';
    input.setAttribute('placeholder', r.placeholder);
    $('reason').showModal();
    input.select();
  }

  function toast(msg) {
    const box = $('toasts');
    const t = el('div', 'toast', msg);
    box.append(t);
    setTimeout(() => t.remove(), 5000);
    while (box.children.length > 3) box.firstElementChild.remove();
  }

  // ---- Drawing the board

  // sync makes box's children the elements for items, in order. An item's
  // element is kept while its key stays, so focus and scroll survive.
  function sync(box, items, keyFn, make, update) {
    const old = new Map();
    for (const e of box.children) old.set(e.getAttribute('data-key'), e);
    let at = box.firstElementChild;
    for (const item of items) {
      const k = keyFn(item);
      let e = old.get(k);
      if (e) old.delete(k);
      else e = attrs(make(item), {'data-key': k});
      update(e, item);
      if (e === at) at = at.nextElementSibling;
      else box.insertBefore(e, at);
    }
    for (const e of old.values()) e.remove();
  }

  function showMessage(...nodes) {
    $('board').hidden = true;
    $('session-list').hidden = true;
    $('empty').hidden = false;
    $('empty-text').replaceChildren(...nodes);
  }

  function render() {
    if (dragging || menuFor) {
      held = true;
      return;
    }
    held = false;
    if (signedOut) {
      for (const id of ['spaces', 'prio-only', 'slack-bar', 'stale', 'hint', 'tailnet']) $(id).hidden = true;
      hidePop();
      shareEls.box.hidden = true;
      showMessage(
        document.createTextNode('Sign in again: run '),
        el('code', '', 'herdr-tasks board'),
        document.createTextNode(', or use Open task board in herdr.'),
      );
      return;
    }
    if (!board) return;
    prune();
    $('version').textContent = board.version ? 'v' + board.version : '';
    renderSlack(board.slack, board.slackWaiting);
    renderTailnet(board.tailnet);
    const sessions = board.sessions;
    $('hint').hidden = board.linearKey || !sessions.some(s => s.cards.some(c => c.links.some(l => l.kind === 'linear')));
    const s = sessions.find(x => x.name === session);
    $('stale').hidden = !s || !s.stale;
    if (!s) {
      $('spaces').hidden = true;
      $('prio-only').hidden = true;
      hidePop();
      showMessage(...goneText());
    } else {
      const cards = s.cards.map(c => cardByKey(keyOf(c)));
      const shown = prioOnly() ? cards.filter(c => c.priority) : cards;
      const toggle = $('prio-only');
      toggle.hidden = false;
      toggle.setAttribute('aria-pressed', String(prioOnly()));
      const space = renderSpaces(s, cards, shown);
      renderColumns(space ? shown.filter(c => c.space === space) : shown);
      $('board').hidden = false;
      $('empty').hidden = true;
      pop.spaces = s.spaces;
      pop.cards = shown;
      if (pop.space) fillPop();
    }
    renderDetail();
  }

  // renderSlack shows the Slack watcher's line, while the board has one: its
  // status, cut short when it's long (its title has all of it), Fetch when
  // it offers one, and how many asks were skipped. On a session Slack asks
  // don't go to, it's the one line that says where they go, when they go to
  // a session that isn't running.
  function renderSlack(line, waiting) {
    if (!line && waiting) line = {text: waiting, problem: false};
    $('slack-bar').hidden = !line;
    const e = $('slack');
    e.textContent = line ? line.text : '';
    e.setAttribute('title', line ? line.text : '');
    e.classList.toggle('problem', Boolean(line && line.problem));
    skipped = line && Array.isArray(line.skipped) ? line.skipped : [];
    slackEls.fetch.hidden = !(line && line.fetch);
    slackEls.skipped.hidden = !skipped.length;
    slackEls.skipped.textContent = skipped.length + ' skipped';
    drawFetch();
  }

  // renderTailnet shows, while board_tailnet is on, why the board isn't on
  // the tailnet, or else its address there.
  function renderTailnet(line) {
    const e = $('tailnet');
    const problem = line && line.problem ? line.problem : '';
    const text = problem || (line && line.address) || '';
    e.hidden = !text;
    e.textContent = text;
    e.setAttribute('title', problem || (text ? 'The board on your tailnet' : ''));
    e.classList.toggle('problem', Boolean(problem));
    renderShares((line && line.shares) || []);
  }

  // renderShares lists the localhost ports herdr-tasks shares with the
  // tailnet, each with Stop.
  function renderShares(shares) {
    shareEls.box.hidden = !shares.length;
    sync(
      shareEls.list,
      shares,
      s => String(s.port),
      s => {
        const e = attrs(el('span', 'share'), {title: 'Shared with your tailnet since ' + new Date(s.since).toLocaleString()});
        const stop = attrs(el('button', 'share-stop', 'Stop'), {type: 'button', 'data-port': String(s.port), 'aria-label': 'Stop sharing localhost:' + s.port});
        e.append(el('span', 'share-port', 'localhost:' + s.port), stop);
        return e;
      },
      () => {},
    );
  }

  // drawFetch shows Fetch as the fetch is, and what the last fetch or take
  // said.
  function drawFetch() {
    const {fetch, said} = slackEls;
    fetch.disabled = fetching;
    fetch.textContent = fetching ? 'Fetching…' : 'Fetch';
    // With no reply to show, it says a take is under way while one is.
    const shown = slackReply || (taking.size ? {text: 'Taking it…', problem: false} : null);
    said.hidden = !shown;
    said.textContent = shown ? shown.text : '';
    said.setAttribute('title', shown ? shown.text : '');
    said.classList.toggle('problem', Boolean(shown && shown.problem));
  }

  // say shows a fetch's or take's reply next to Fetch, as a problem when it
  // failed, for 15 s. No text clears it.
  function say(text, problem) {
    clearTimeout(replyTimer);
    slackReply = text ? {text, problem: Boolean(problem)} : null;
    if (slackReply) {
      replyTimer = setTimeout(() => {
        slackReply = null;
        drawFetch();
      }, 15e3);
    }
    drawFetch();
  }

  // replyText is the text of a fetch's or take's answer, or else fallback.
  const replyText = (reply, fallback) => (reply && typeof reply.text === 'string' && reply.text ? reply.text : fallback);

  // fetchSlack has the board server check Slack for asks now, and says what
  // it found. It can take minutes: meanwhile Fetch reads Fetching…, and no
  // other fetch starts.
  function fetchSlack() {
    if (fetching) return;
    fetching = true;
    say('');
    const done = (text, problem) => {
      fetching = false;
      say(text, problem);
    };
    post({}, '/api/slack/fetch').then(
      reply => done(replyText(reply, 'Fetched.'), false),
      err => done(err.message, true),
    );
  }

  // takeAsk has the board server make the task for a skipped ask, by its
  // message ID, and says where it went. Until then, it says Taking it…
  function takeAsk(id) {
    if (!id || taking.has(id)) return;
    taking.add(id);
    say('');
    const done = (text, problem) => {
      taking.delete(id);
      say(text, problem);
    };
    post({id}, '/api/slack/take').then(
      reply => done(replyText(reply, 'Took it.'), false),
      err => done(err.message, true),
    );
  }

  // renderList shows the running sessions, each a link to its board with
  // how many cards it has. The one Slack asks go to says so.
  function renderList(l) {
    const ul = $('session-list');
    sync(
      ul,
      l.sessions,
      s => s.name,
      () => el('li', 'session-row'),
      (li, s) => {
        const n = s.cards === 1 ? '1 card' : s.cards + ' cards';
        const parts = [
          hasPage(s.name) ? attrs(el('a', 'session-link', s.name), {href: pageOf(s.name)}) : el('span', 'session-link', s.name),
          el('span', 'session-cards', n),
        ];
        if (s.stale) parts.push(attrs(el('span', 'badge stale', 'stale'), {title: $('stale').getAttribute('title')}));
        if (s.name === l.slack) parts.push(el('span', 'session-slack', 'Slack asks come here'));
        li.replaceChildren(...parts);
      },
    );
    ul.hidden = false;
    $('empty').hidden = true;
  }

  function spaceChoices() {
    try {
      const m = JSON.parse(load(SPACES) || '{}');
      return m && typeof m === 'object' ? m : {};
    } catch (e) {
      return {};
    }
  }

  // renderSpaces shows the space filter, and returns the space chosen, or
  // '' for all of them. Each space of cards has a button, with how many of
  // the cards shown in it are open.
  function renderSpaces(s, cards, shown) {
    const open = new Map();
    const used = new Set(cards.map(c => c.space));
    for (const c of shown) {
      if (c.column !== 'done') open.set(c.space, (open.get(c.space) || 0) + 1);
    }
    let chosen = spaceChoices()[s.name] || '';
    if (!s.spaces.some(x => x.id === chosen)) chosen = '';
    const spaces = s.spaces.filter(x => used.has(x.id) || x.id === chosen);
    const bar = $('spaces');
    bar.hidden = spaces.length < 2 && !chosen;
    sync(
      bar,
      bar.hidden ? [] : [{id: '', label: 'All'}].concat(spaces),
      x => x.id || '*',
      () => attrs(el('button', 'space-filter'), {type: 'button'}),
      (b, x) => {
        b.setAttribute('aria-pressed', String(x.id === chosen));
        const parts = [el('span', '', x.label)];
        if (x.id && open.get(x.id)) parts.push(el('span', 'n', open.get(x.id)));
        b.replaceChildren(...parts);
      },
    );
    return chosen;
  }

  // ---- The tasks of a space, shown while its button is hovered or focused

  // pop is what the list shows: the space, its button, and the spaces and
  // cards the board shows now.
  const pop = {space: null, anchor: null, timer: 0, spaces: [], cards: []};

  // order puts a space's open tasks as the list shows them: priority first,
  // then by column, newest first.
  const columnIndex = c => COLUMNS.findIndex(x => x.id === c.column);
  const popOrder = (a, b) => b.priority - a.priority || columnIndex(a) - columnIndex(b) || Date.parse(b.since) - Date.parse(a.since);

  function showPop(b) {
    clearTimeout(pop.timer);
    const id = b.getAttribute('data-key');
    if (id === '*') return hidePop();
    pop.space = id;
    pop.anchor = b;
    fillPop();
  }

  function hidePop() {
    clearTimeout(pop.timer);
    pop.space = null;
    pop.anchor = null;
    $('space-pop').hidden = true;
  }

  // hidePopSoon gives the pointer time to cross from the button to the list.
  function hidePopSoon() {
    clearTimeout(pop.timer);
    pop.timer = setTimeout(hidePop, 200);
  }

  function fillPop() {
    const box = $('space-pop');
    const space = pop.spaces.find(x => x.id === pop.space);
    if (!space || !pop.anchor.isConnected || viewing) return hidePop();
    const tasks = pop.cards.filter(c => c.space === pop.space && c.column !== 'done').sort(popOrder);
    const n = tasks.length;
    let count = n + ' open';
    if (prioOnly()) count = n ? n + (n === 1 ? ' priority task' : ' priority tasks') : 'no priority tasks';
    else if (!n) count = 'nothing open';
    const rows = tasks.map(c => {
      const row = attrs(el('button', 'pop-row'), {type: 'button', 'data-key': keyOf(c)});
      if (c.priority) row.append(attrs(el('span', 'prio', '!'), {role: 'img', 'aria-label': 'Priority', title: 'Priority'}));
      else row.append(el('span', 'pop-gap'));
      const column = COLUMNS[columnIndex(c)];
      row.append(attrs(el('span', 'pop-dot'), {'data-column': c.column}), el('span', 'pop-title', c.title), el('span', 'pop-col', column ? column.label : ''));
      return row;
    });
    box.replaceChildren(el('div', 'pop-head', space.label + ' · ' + count), ...rows);
    box.setAttribute('aria-label', 'Tasks in ' + space.label);
    box.hidden = false;
    const r = pop.anchor.getBoundingClientRect();
    box.style.left = Math.max(8, Math.min(r.left, window.innerWidth - 336)) + 'px';
    box.style.top = r.bottom + 6 + 'px';
  }

  function chooseSpace(id) {
    const m = spaceChoices();
    if (!id || m[session] === id) delete m[session];
    else m[session] = id;
    save(SPACES, JSON.stringify(m));
    render();
  }

  // newer orders a column's cards as the overview does: priority first,
  // then the newest stage. Ties keep the board's order.
  const newer = (a, b) => b.priority - a.priority || Date.parse(b.since) - Date.parse(a.since);

  function renderColumns(cards) {
    const lists = new Map(COLUMNS.map(col => [col.id, []]));
    for (const c of cards) if (lists.has(c.column)) lists.get(c.column).push(c);
    const want = new Set();
    for (const list of lists.values()) for (const c of list) want.add(keyOf(c));

    // Moving an element blurs it, so focus goes back afterwards.
    const active = document.activeElement;
    const activeCard = active && active.closest ? active.closest('.card') : null;
    const act = activeCard && active.getAttribute('data-act');

    for (const col of COLUMNS) {
      const list = lists.get(col.id).sort(newer);
      const box = columnEls.get(col.id);
      box.count.textContent = list.length ? String(list.length) : '';
      let at = box.cards.firstElementChild;
      for (const c of list) {
        const k = keyOf(c);
        let e = cardEls.get(k);
        if (!e) {
          e = attrs(el('article', 'card'), {draggable: 'true', 'data-key': k});
          cardEls.set(k, e);
        }
        const sig = JSON.stringify(c);
        if (e.sig !== sig) {
          fillCard(e, c);
          e.sig = sig;
        }
        if (e === at) at = at.nextElementSibling;
        else box.cards.insertBefore(e, at);
      }
      // What's left goes off the board, or to a column drawn later.
      while (at) {
        const next = at.nextElementSibling;
        if (!want.has(at.getAttribute('data-key'))) at.remove();
        at = next;
      }
    }
    for (const [k, e] of cardEls) {
      if (!want.has(k)) {
        e.remove();
        cardEls.delete(k);
      }
    }

    if (activeCard && document.activeElement !== active) {
      const e = cardEls.get(activeCard.getAttribute('data-key'));
      let target = active.isConnected ? active : e && act ? e.querySelector('[data-act="' + act + '"]') : null;
      if (target) target.focus({preventScroll: true});
      // A card moved into the folded Done column can't hold focus.
      if (e && document.activeElement !== target && e.closest('.collapsed')) {
        columnEls.get('done').toggle.focus({preventScroll: true});
      }
    }
  }

  function fillCard(e, c) {
    e.setAttribute('data-column', c.column);
    const parts = [];

    const top = el('div', 'card-top');
    if (c.priority) top.append(attrs(el('span', 'prio', '!'), {role: 'img', 'aria-label': 'Priority', title: 'Priority'}));
    top.append(el('h3', 'card-title', c.title));
    parts.push(top);

    const meta = el('div', 'meta');
    if (c.spaceLabel) meta.append(el('span', 'space', c.spaceLabel));
    meta.append(ageEl(c.since, 'In this stage since'));
    const reason = reasonText(c);
    if (reason) meta.append(el('span', 'reason', reason));
    parts.push(meta);

    if (c.agents.length) {
      const list = el('ul', 'agents');
      for (const a of c.agents) list.append(agentRow(a));
      parts.push(list);
    }
    if (c.links.length) {
      const links = el('div', 'links');
      for (const g of linkGroups(c.links)) {
        const row = attrs(el('div', 'link-row'), {role: 'group', 'aria-label': g.section.title});
        for (const l of g.links) row.append(linkChip(l));
        links.append(row);
      }
      parts.push(links);
    }
    const note = firstLine(c.note);
    if (note) parts.push(el('p', 'note', note));

    const bar = el('div', 'actions');
    bar.append(button('act', 'Move to…', 'move', {'aria-haspopup': 'menu', 'aria-expanded': 'false'}));
    if (c.column !== 'done') {
      bar.append(
        button('act prio-toggle end', '!', 'priority', {
          'aria-pressed': String(Boolean(c.priority)),
          'aria-label': 'Priority',
          title: c.priority ? 'Priority: on' : 'Priority: off',
        }),
      );
    }
    bar.append(button(c.column === 'done' ? 'act end' : 'act', 'Show in herdr', 'focus'));
    bar.append(button('act', 'Open', 'open'));
    parts.push(bar);

    e.replaceChildren(...parts);
  }

  function button(cls, text, act, more) {
    return attrs(el('button', cls, text), Object.assign({type: 'button', 'data-act': act}, more || {}));
  }

  function glyphEl(a) {
    const g = glyph(a.status);
    return attrs(el('span', 'glyph ' + g.cls, g.ch), {role: 'img', 'aria-label': g.word, title: g.word});
  }

  // agentName is what heads an agent: its pane's name in herdr, or else its
  // title. agentSub is the title under the pane's name, when it has one
  // that says more.
  const agentName = a => a.name || a.title || a.agent;
  const agentSub = a => (a.name && a.title && a.title !== a.name ? el('p', 'agent-sub', a.title) : null);

  // agentHead is the agent's name, and when its recap was written.
  function agentHead(a) {
    const head = el('div', 'agent-head');
    head.append(el('span', 'agent-title', agentName(a)));
    if (a.recap && (a.recapAt || a.recapStale)) {
      const when = el('span', 'agent-when');
      if (a.recapAt) when.append(ageEl(a.recapAt, 'Recap written'));
      if (a.recapStale) when.append(document.createTextNode(a.recapAt ? ' · earlier' : 'earlier'));
      head.append(when);
    }
    return head;
  }

  function agentRow(a) {
    const li = el('li', 'agent');
    const body = el('div', 'agent-body');
    body.append(agentHead(a));
    const sub = agentSub(a);
    if (sub) body.append(sub);
    if (a.recap) body.append(el('p', a.recapStale ? 'recap stale' : 'recap', a.recap));
    if (a.lastPrompt && (!a.recap || a.recapStale)) body.append(el('p', 'prompt', a.lastPrompt));
    li.append(glyphEl(a), body);
    return li;
  }

  // linkChip is a link as a card shows it. A file is a button that opens it
  // through the board, and so is a localhost link on a board seen through
  // the tailnet. Other http and https links open in a new tab.
  function linkChip(l) {
    let chip;
    if (l.kind === 'file') chip = button('chip', '', 'openfile', {'data-url': l.url});
    else if (localPort(l.url)) chip = button('chip', '', 'openlocal', {'data-url': l.url});
    else if (linkable(l.url)) chip = attrs(el('a', 'chip'), {href: l.url, target: '_blank', rel: 'noopener noreferrer'});
    else chip = el('span', 'chip');
    const is = l.linear;
    if (l.kind === 'linear' && is) {
      chip.classList.add('linear');
      if (is.state) {
        const dot = el('span', 'dot');
        if (/^#[0-9a-f]{3,8}$/i.test(is.color || '')) dot.style.backgroundColor = is.color;
        chip.append(dot);
      }
      chip.append(el('span', 'id', is.id));
      if (is.state) chip.append(el('span', 'state', is.state));
      const tip = [is.title, is.assignee ? 'Assignee: ' + is.assignee : '', l.url];
      chip.setAttribute('title', tip.filter(Boolean).join('\n'));
    } else if (l.kind === 'slack') {
      chip.classList.add('slack');
      chip.append(el('span', 'kind', 'Slack'), el('span', 'label', l.at ? clock(l.at) : 'channel'));
      const tip = [l.label, l.at ? new Date(l.at).toLocaleString() : '', l.url];
      chip.setAttribute('title', tip.filter(Boolean).join('\n'));
    } else if (PAGE_MARKS[l.kind]) {
      chip.classList.add('page');
      chip.append(attrs(el('span', 'mark', PAGE_MARKS[l.kind]), {'aria-hidden': 'true'}), el('span', 'label', l.label || l.url));
      const tip = l.kind === 'file' ? ['Open in your browser', l.path] : [l.url];
      if (l.source) tip.push('Published from ' + l.source);
      chip.setAttribute('title', tip.filter(Boolean).join('\n'));
      chip.setAttribute('aria-label', PAGE_MARKS[l.kind] + ' page: ' + (l.label || l.url));
    } else {
      chip.append(el('span', 'label', l.label || l.url));
      chip.setAttribute('title', l.url);
    }
    return chip;
  }

  // ---- A tab's view: its terminals, and the whole card next to them

  // The view is at #tab=<tab>, so a reload keeps it.
  function viewHash(tab) {
    return '#tab=' + encodeURIComponent(tab);
  }

  // hashTab is the tab the address shows. An old address has the session
  // before the tab, as #tab=<session>/<tab>, and says which; a new one has
  // none ('' as the session).
  function hashTab() {
    const h = location.hash || '';
    if (!h.startsWith('#tab=')) return null;
    const rest = h.slice(5);
    const i = rest.indexOf('/');
    try {
      if (i < 0) return {session: '', tab: decodeURIComponent(rest)};
      return {session: decodeURIComponent(rest.slice(0, i)), tab: decodeURIComponent(rest.slice(i + 1))};
    } catch (e) {
      return null;
    }
  }

  // hashView is the tab of this session the address shows, or null for the
  // board.
  function hashView() {
    const v = session ? hashTab() : null;
    return v && (!v.session || v.session === session) ? {session, tab: v.tab} : null;
  }

  function openView(k) {
    const c = cardByKey(k);
    if (!c) return;
    closeMenu(false);
    history.pushState({view: true}, '', viewHash(c.tab));
    route();
  }

  // closeView goes back to the board.
  function closeView() {
    if (history.state && history.state.view) {
      history.back();
      return;
    }
    history.replaceState(null, '', location.pathname + location.search);
    route();
  }

  // route shows what the address asks for: a tab's view, or the board.
  function route() {
    const v = signedOut ? null : hashView();
    if (v && viewing && v.session === viewing.session && v.tab === viewing.tab) return;
    if (viewing) hideView();
    if (v) showView(v.session, v.tab);
  }

  function showView(session, tab) {
    closeMenu(false);
    detailKey = session + '\n' + tab;
    detailSig = null;
    document.body.classList.add('viewing');
    $('view').hidden = false;
    renderDetail();
    const box = $('terms');
    let terms = null;
    if (window.herdrTerm) {
      terms = window.herdrTerm.mount(box, {session, tab, key, parseEvent, toast, signOut});
    } else {
      box.replaceChildren(el('p', 'term-status', 'The terminals didn’t load. Reload the page to try again.'));
    }
    viewing = {session, tab, terms};
    $('view-back').focus();
  }

  function hideView() {
    const v = viewing;
    viewing = null;
    if (v.terms) v.terms.close();
    closeMenu(false);
    detailKey = null;
    detailSig = null;
    $('view').hidden = true;
    document.body.classList.remove('viewing');
    render();
    const card = cardEls.get(v.session + '\n' + v.tab);
    const open = card && card.querySelector('[data-act="open"]');
    if (open) open.focus();
  }

  function renderDetail() {
    if (!detailKey) return;
    const c = board ? cardByKey(detailKey) : null;
    const sig = !board ? 'loading' : c ? JSON.stringify(c) + (working.has(detailKey) ? ' working' : '') : '';
    if (sig === detailSig) return;
    detailSig = sig;

    const body = $('detail-body');
    const active = body.contains(document.activeElement) ? document.activeElement : null;
    const was = active && {act: active.getAttribute('data-act'), url: active.getAttribute('data-url')};
    const top = body.scrollTop;
    fillDetail(body, c);
    body.scrollTop = top;
    if (was) {
      const acts = [...body.querySelectorAll('[data-act]')];
      const same = acts.find(e => e.getAttribute('data-act') === was.act && e.getAttribute('data-url') === was.url);
      // A removed link's button is gone; the next one takes its place.
      const next = acts.find(e => e.getAttribute('data-act') === was.act);
      (same || next || $('view-back')).focus({preventScroll: true});
    }
  }

  function detailSection(title, ...content) {
    const s = el('section', 'section');
    s.append(el('h3', 'section-title', title), ...content);
    return s;
  }

  function field(label, content) {
    const f = el('div', 'd-field');
    f.append(label, content);
    return f;
  }

  function fillDetail(body, c) {
    if (!board) {
      $('detail-title').textContent = '';
      body.replaceChildren(el('p', 'empty-line', 'Loading…'));
      return;
    }
    $('detail-title').textContent = c ? c.title : 'This task isn’t on the board any more';
    if (!c) {
      body.replaceChildren(el('p', 'empty-line', 'Its tab was closed, or its space was hidden.'));
      return;
    }
    const parts = [];

    const meta = el('div', 'detail-meta');
    const col = COLUMNS.find(x => x.id === c.column);
    meta.append(attrs(el('span', 'stage-badge', col ? col.label : c.stage), {'data-column': c.column}));
    if (c.priority) meta.append(el('span', 'badge prio-badge', '! Priority'));
    if (c.spaceLabel) meta.append(el('span', 'space', c.spaceLabel));
    meta.append(ageEl(c.since, 'In this stage since'));
    parts.push(meta);
    const reason = reasonText(c);
    if (reason) parts.push(attrs(el('p', 'detail-reason', reason), {'data-column': c.column}));
    const acts = el('div', 'detail-actions');
    acts.append(button('button', 'Move to…', 'move', {'aria-haspopup': 'menu', 'aria-expanded': 'false'}));
    if (c.column !== 'done') {
      acts.append(
        button('button prio-button', '! Priority', 'priority', {
          'aria-pressed': String(Boolean(c.priority)),
          title: c.priority ? 'Priority: on' : 'Priority: off',
        }),
      );
    }
    acts.append(button('button', 'Show in herdr', 'focus'));
    for (const a of cardActions(c)) {
      const more = a.act === 'spaces' ? {'aria-haspopup': 'menu', 'aria-expanded': 'false'} : {};
      acts.append(button('button', a.label, a.act, more));
    }
    parts.push(acts);

    parts.push(detailSection('Note', c.note ? el('p', 'text', c.note) : el('p', 'empty-line', 'No note yet.')));

    if (c.log && c.log.length) {
      const log = el('ul', 'd-log');
      for (const entry of c.log) {
        const li = el('li', 'd-entry');
        li.append(attrs(el('span', 'who', entry.who + ' ' + clock(entry.at)), {title: new Date(entry.at).toLocaleString()}));
        li.append(el('span', 'what', entry.text));
        log.append(li);
      }
      parts.push(detailSection('Log', log));
    }

    if (c.agents.length) {
      const list = el('ul', 'd-agents');
      for (const a of c.agents) list.append(agentDetail(a));
      parts.push(detailSection('Agents', list));
    } else {
      parts.push(detailSection('Agents', el('p', 'empty-line', 'No agent is running in this tab.')));
    }

    const groups = linkGroups(c.links);
    for (const g of groups) {
      const list = el('ul', 'd-links');
      for (const l of g.links) list.append(linkRow(l));
      parts.push(detailSection(g.section.title, list));
    }
    if (!groups.length) parts.push(detailSection('Links', el('p', 'empty-line', 'No links yet.')));

    body.replaceChildren(...parts);
  }

  function agentDetail(a) {
    const li = el('li', 'd-agent');
    const head = el('div', 'agent-head');
    head.append(glyphEl(a), el('span', 'agent-title', agentName(a)), el('span', 'agent-when', glyph(a.status).word));
    li.append(head);
    const sub = agentSub(a);
    if (sub) li.append(sub);
    if (a.recap) {
      const label = el('div', 'd-label', 'Recap');
      if (a.recapAt) label.append(document.createTextNode(' · ' + clock(a.recapAt)));
      if (a.recapStale) label.append(document.createTextNode(' · earlier: a prompt came after it'));
      li.append(field(label, el('p', 'text', a.recap)));
    }
    if (a.lastPrompt) li.append(field(el('div', 'd-label', 'Last prompt'), el('p', 'text', a.lastPrompt)));
    if (a.lastReply) li.append(field(el('div', 'd-label', 'Last reply'), el('p', 'text long', a.lastReply)));
    if (!a.recap && !a.lastPrompt && !a.lastReply) {
      li.append(el('p', 'empty-line d-field', a.agent === 'claude' ? 'Nothing in its session log yet.' : 'Only Claude agents have a recap.'));
    }
    return li;
  }

  function linkRow(l) {
    const li = el('li', 'd-link');
    li.append(linkChip(l));
    const is = l.linear;
    let about = l.kind === 'file' ? l.path || l.url : l.url;
    if (l.kind === 'linear' && is && is.title) about = is.title + (is.assignee ? ' · ' + is.assignee : '');
    li.append(attrs(el('span', 'url', about), {title: l.source ? about + '\nPublished from ' + l.source : about}));
    if (l.by) li.append(el('span', 'by', 'via ' + l.by));
    if (l.kind === 'file') li.append(button('copy', 'Copy path', 'copypath', {'data-url': l.url, title: 'Copy ' + (l.path || '')}));
    li.append(
      button('remove', '×', 'unlink', {
        'data-url': l.url,
        'aria-label': 'Remove ' + (l.label || l.url),
        title: 'Remove the link. Claude won’t add it back.',
      }),
    );
    return li;
  }

  // ---- The Move to menu

  // openMenu opens the Move to menu of the card k under trigger: its stages
  // and actions, or, when list is 'spaces', the workspaces it can move to.
  function openMenu(trigger, k, list) {
    const c = cardByKey(k);
    if (!c) return;
    menuFor = {key: k, trigger};
    fillMenu(c, list);
    trigger.setAttribute('aria-expanded', 'true');
  }

  function fillMenu(c, list) {
    const spaces = list === 'spaces';
    showMenu(spaces ? 'Move to workspace' : 'Move to', spaces ? spaceItems(c) : stageItems(c));
  }

  // openSkipped opens the list of the skipped asks under trigger, the Slack
  // line's "n skipped".
  function openSkipped(trigger) {
    menuFor = {key: '', trigger};
    showMenu('Skipped asks', skippedItems());
    trigger.setAttribute('aria-expanded', 'true');
  }

  // showMenu shows items in the menu, under the trigger it's open for. A
  // long list scrolls, in the room below the trigger or above it, whichever
  // is more.
  function showMenu(label, items) {
    const menu = $('menu');
    menu.setAttribute('aria-label', label);
    menu.replaceChildren(...items);
    menu.hidden = false;
    const r = menuFor.trigger.getBoundingClientRect();
    menu.style.maxHeight = Math.max(innerHeight - r.bottom, r.top) - 12 + 'px';
    const left = Math.max(8, Math.min(r.left, innerWidth - menu.offsetWidth - 8));
    let top = r.bottom + 4;
    if (top + menu.offsetHeight > innerHeight - 8) top = Math.max(8, r.top - menu.offsetHeight - 4);
    menu.style.left = left + 'px';
    menu.style.top = top + 'px';
    const first = menu.querySelector('.menu-item:not([aria-disabled])');
    if (first) first.focus();
  }

  function menuItem(cls, label, more) {
    const item = attrs(el('button', cls), Object.assign({type: 'button', role: 'menuitem', tabindex: '-1'}, more));
    item.append(el('span', '', label));
    return item;
  }

  // skippedItems are the skipped asks, newest first, each with its label,
  // why it was skipped, Take, and Open in Slack, which opens as a card's
  // links do. Their words come from Slack, so they're text.
  function skippedItems() {
    const items = [attrs(el('div', 'menu-label', 'Skipped asks'), {'aria-hidden': 'true'})];
    for (const a of skipped) {
      const row = attrs(el('div', 'menu-ask'), {role: 'group', 'aria-label': String(a.label)});
      const acts = el('div', 'ask-acts');
      const take = menuItem('menu-item slack-act', 'Take', {'data-act': 'take', 'data-id': String(a.id)});
      if (taking.has(a.id)) take.setAttribute('aria-disabled', 'true');
      acts.append(take);
      if (linkable(a.link)) {
        const open = attrs(el('a', 'menu-item slack-act'), {
          role: 'menuitem',
          tabindex: '-1',
          href: a.link,
          target: '_blank',
          rel: 'noopener noreferrer',
          'data-act': 'openslack',
        });
        open.append(el('span', '', 'Open in Slack'));
        acts.append(open);
      }
      row.append(el('div', 'ask-label', a.label), el('div', 'ask-why', a.why), acts);
      items.push(row);
    }
    return items;
  }

  // spaceItems are the workspaces c can move to.
  function spaceItems(c) {
    const items = [attrs(el('div', 'menu-label', 'Move to workspace'), {'aria-hidden': 'true'})];
    for (const s of otherSpaces(c)) items.push(menuItem('menu-item', s.label, {'data-space': s.id}));
    return items;
  }

  // stageItems are the stages c can move to, then its actions.
  function stageItems(c) {
    const items = [attrs(el('div', 'menu-label', 'Move to'), {'aria-hidden': 'true'})];
    for (const m of MOVES) {
      const current = c.stage === m.stage;
      const item = attrs(el('button', 'menu-item'), {
        type: 'button',
        role: 'menuitemradio',
        tabindex: '-1',
        'aria-checked': String(current),
        'data-stage': m.stage,
        'data-column': m.stage === 'active' ? activeColumn(c.agentStatus) : m.stage,
      });
      item.append(el('span', '', m.label));
      if (current) item.append(attrs(el('span', 'check', '✓'), {'aria-hidden': 'true'}));
      // In review and Paused stay open, to change what they wait for.
      if (current && !REASONS[m.stage]) item.setAttribute('aria-disabled', 'true');
      items.push(item);
    }
    const acts = cardActions(c);
    if (acts.length) items.push(attrs(el('div', 'menu-sep'), {role: 'separator'}));
    for (const a of acts) {
      const more = {'data-act': a.act};
      if (a.act === 'spaces') more['aria-haspopup'] = 'menu';
      items.push(menuItem(a.act === 'spaces' ? 'menu-item' : 'menu-item slack-act', a.label, more));
    }
    return items;
  }

  function closeMenu(refocus) {
    if (!menuFor) return;
    const {trigger} = menuFor;
    menuFor = null;
    $('menu').hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    if (refocus) trigger.focus();
    if (held) render();
  }

  // ---- Dragging

  function accepts(column, d) {
    return Boolean(d) && stageOf(column) !== d.stage;
  }

  function endDrag() {
    if (!dragging) return;
    dragging.el.classList.remove('dragging');
    dragging = null;
    document.body.classList.remove('dragging');
    for (const {section} of columnEls.values()) section.classList.remove('drop-ok', 'drop-over');
    if (held) render();
  }

  function columnAt(target) {
    const section = target instanceof Element ? target.closest('.column') : null;
    return section ? {section, id: section.getAttribute('data-column')} : null;
  }

  // ---- Setting up

  // buildSlack makes the Slack line's parts after its status: Fetch, "n
  // skipped", which opens the list, and what the last fetch or take said.
  function buildSlack() {
    slackEls = {
      fetch: attrs(el('button', 'slack-button slack-fetch', 'Fetch'), {type: 'button', title: 'Check Slack for asks now'}),
      said: attrs(el('span', 'slack-said'), {role: 'status'}),
      skipped: attrs(el('button', 'slack-button slack-skipped'), {type: 'button', 'aria-haspopup': 'menu', 'aria-expanded': 'false'}),
    };
    for (const e of Object.values(slackEls)) e.hidden = true;
    $('slack-bar').append(slackEls.fetch, slackEls.skipped, slackEls.said);
  }

  // buildShare makes the header's list of shared ports, and the dialog that
  // asks before a port is shared.
  function buildShare() {
    const box = attrs(el('span', 'shares'), {role: 'group', 'aria-labelledby': 'shares-label'});
    const list = el('span', 'share-list');
    box.hidden = true;
    box.append(attrs(el('span', 'shares-label', 'Sharing'), {id: 'shares-label'}), list);
    const tailnet = $('tailnet');
    tailnet.parentNode.insertBefore(box, tailnet.nextElementSibling);
    box.addEventListener('click', e => {
      const b = e.target instanceof Element ? e.target.closest('button[data-port]') : null;
      if (b) stopShare(b);
    });

    const dialog = attrs(el('dialog', 'sheet'), {'aria-labelledby': 'share-title', 'aria-describedby': 'share-note'});
    const title = attrs(el('h2', 'sheet-title'), {id: 'share-title'});
    const body = el('div', 'sheet-body');
    const cancel = attrs(el('button', 'button', 'Cancel'), {type: 'button'});
    const share = attrs(el('button', 'button primary', 'Share'), {type: 'button'});
    const buttons = el('div', 'sheet-buttons');
    buttons.append(cancel, share);
    body.append(title, attrs(el('p', 'sheet-note', 'Anyone in your tailnet can open it while it’s shared.'), {id: 'share-note'}), buttons);
    dialog.append(body);
    document.body.append(dialog);
    shareEls = {box, list, dialog, title};
    cancel.addEventListener('click', () => dialog.close());
    share.addEventListener('click', () => {
      const f = shareFor;
      dialog.close();
      if (f) openThrough('/api/share', f.c, f.url);
    });
    dialog.addEventListener('close', () => {
      shareFor = null;
    });
    dialog.addEventListener('click', e => {
      if (e.target === dialog) dialog.close();
    });
  }

  function buildColumns() {
    const doneOpen = load(DONE_OPEN + ' ' + session) === '1';
    for (const col of COLUMNS) {
      const section = attrs(el('section', 'column'), {'data-column': col.id, 'aria-labelledby': 'col-' + col.id});
      const name = attrs(el('span', 'col-name', col.label), {id: 'col-' + col.id});
      const count = el('span', 'col-count');
      const title = el('h2', 'col-title');
      let toggle = null;
      if (col.id === 'done') {
        toggle = attrs(el('button', 'col-head'), {type: 'button', 'aria-expanded': String(doneOpen), 'aria-controls': 'cards-done'});
        toggle.append(name, count, attrs(el('span', 'col-toggle', '▸'), {'aria-hidden': 'true'}));
        title.append(toggle);
        section.classList.toggle('collapsed', !doneOpen);
        toggle.addEventListener('click', () => {
          const open = section.classList.toggle('collapsed') === false;
          toggle.setAttribute('aria-expanded', String(open));
          save(DONE_OPEN + ' ' + session, open ? '1' : '0');
        });
      } else {
        title.classList.add('col-head');
        title.append(name, count);
      }
      if (col.hint) title.setAttribute('title', col.hint);
      const cards = attrs(el('div', 'col-cards'), {id: 'cards-' + col.id});
      section.append(title, cards);
      $('board').append(section);
      columnEls.set(col.id, {section, cards, count, toggle});
    }
  }

  function wire() {
    const boardEl = $('board');

    boardEl.addEventListener('click', e => {
      if (!(e.target instanceof Element)) return;
      const card = e.target.closest('.card');
      if (!card) return;
      const k = card.getAttribute('data-key');
      const btn = e.target.closest('button[data-act]');
      if (btn) {
        const c = cardByKey(k);
        const act = btn.getAttribute('data-act');
        if (!c) return;
        if (act === 'move') {
          if (menuFor && menuFor.trigger === btn) closeMenu(true);
          else openMenu(btn, k);
        } else if (act === 'priority') setPriority(c, !c.priority);
        else if (act === 'focus') showInHerdr(c);
        else if (act === 'open') openView(k);
        else if (act === 'openfile') openFile(c, btn.getAttribute('data-url'));
        else if (act === 'openlocal') openLocal(c, btn.getAttribute('data-url'));
        return;
      }
      if (e.target.closest('a, button') || String(window.getSelection())) return;
      openView(k);
    });

    boardEl.addEventListener('dragstart', e => {
      const card = e.target instanceof Element ? e.target.closest('.card') : null;
      // A link dragged out of a card is a link, not the card.
      if (!card || e.target.closest('a')) return;
      const c = cardByKey(card.getAttribute('data-key'));
      if (!c) {
        e.preventDefault();
        return;
      }
      closeMenu(false);
      dragging = {key: keyOf(c), stage: c.stage, el: card};
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', c.title);
      document.body.classList.add('dragging');
      for (const [id, {section}] of columnEls) section.classList.toggle('drop-ok', accepts(id, dragging));
      requestAnimationFrame(() => {
        if (dragging && dragging.el === card) card.classList.add('dragging');
      });
    });
    boardEl.addEventListener('dragover', e => {
      const at = columnAt(e.target);
      if (!at || !accepts(at.id, dragging)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      for (const {section} of columnEls.values()) section.classList.toggle('drop-over', section === at.section);
    });
    boardEl.addEventListener('dragleave', e => {
      const at = columnAt(e.target);
      if (at && !at.section.contains(e.relatedTarget)) at.section.classList.remove('drop-over');
    });
    boardEl.addEventListener('drop', e => {
      const at = columnAt(e.target);
      const d = dragging;
      if (!at || !accepts(at.id, d)) return;
      e.preventDefault();
      endDrag();
      moveTo(d.key, stageOf(at.id));
    });
    document.addEventListener('dragend', endDrag);

    const menu = $('menu');
    menu.addEventListener('click', e => {
      const item = e.target instanceof Element ? e.target.closest('.menu-item') : null;
      if (!item || item.getAttribute('aria-disabled') === 'true' || !menuFor) return;
      const k = menuFor.key;
      const act = item.getAttribute('data-act');
      if (act === 'spaces') {
        const c = cardByKey(k);
        if (c) fillMenu(c, 'spaces');
        return;
      }
      closeMenu(true);
      // Open in Slack is a link: the browser opens it.
      if (act === 'openslack') return;
      if (act === 'take') takeAsk(item.getAttribute('data-id'));
      else if (item.hasAttribute('data-space')) moveToSpace(k, item.getAttribute('data-space'));
      else if (act) slackAction(k, act);
      else moveTo(k, item.getAttribute('data-stage'));
    });
    menu.addEventListener('keydown', e => {
      const items = [...menu.querySelectorAll('.menu-item:not([aria-disabled])')];
      const i = items.indexOf(document.activeElement);
      let next = -1;
      if (e.key === 'ArrowDown') next = (i + 1) % items.length;
      else if (e.key === 'ArrowUp') next = (i - 1 + items.length) % items.length;
      else if (e.key === 'Home') next = 0;
      else if (e.key === 'End') next = items.length - 1;
      else if (e.key === 'Escape') {
        e.preventDefault();
        closeMenu(true);
      } else if (e.key === 'Tab') closeMenu(false);
      else if (e.key === ' ' && i >= 0 && items[i].matches('a')) {
        // A link opens on Enter only: as a menu item, Space opens it too.
        e.preventDefault();
        items[i].click();
      }
      if (next >= 0 && items[next]) {
        e.preventDefault();
        items[next].focus();
      }
    });
    document.addEventListener(
      'pointerdown',
      e => {
        if (menuFor && !menu.contains(e.target) && e.target !== menuFor.trigger) closeMenu(false);
      },
      true,
    );
    window.addEventListener('resize', () => closeMenu(false));
    document.addEventListener(
      'scroll',
      e => {
        if (menuFor && !menu.contains(e.target)) closeMenu(false);
      },
      true,
    );

    $('slack-bar').addEventListener('click', e => {
      const b = e.target instanceof Element ? e.target.closest('button') : null;
      if (!b) return;
      if (b === slackEls.fetch) fetchSlack();
      else if (b === slackEls.skipped && skipped.length) {
        if (menuFor && menuFor.trigger === b) closeMenu(true);
        else openSkipped(b);
      }
    });

    $('spaces').addEventListener('click', e => {
      const b = e.target instanceof Element ? e.target.closest('button') : null;
      if (b) chooseSpace(b.getAttribute('data-key') === '*' ? '' : b.getAttribute('data-key'));
    });

    // A space's tasks show while its button is hovered or focused, and while
    // the pointer or focus is in the list. Esc closes it.
    const spaceBox = $('spaces');
    const popBox = $('space-pop');
    const spaceButtonOf = e => (e.target instanceof Element ? e.target.closest('button') : null);
    const inPop = t => t instanceof Element && (spaceBox.contains(t) || popBox.contains(t));
    for (const type of ['mouseover', 'focusin']) {
      spaceBox.addEventListener(type, e => {
        const b = spaceButtonOf(e);
        if (b) showPop(b);
      });
      popBox.addEventListener(type, () => clearTimeout(pop.timer));
    }
    for (const box of [spaceBox, popBox]) {
      for (const type of ['mouseout', 'focusout']) {
        box.addEventListener(type, e => {
          if (pop.space && !inPop(e.relatedTarget)) hidePopSoon();
        });
      }
    }
    popBox.addEventListener('click', e => {
      const row = e.target instanceof Element ? e.target.closest('.pop-row') : null;
      if (!row) return;
      hidePop();
      openView(row.getAttribute('data-key'));
    });
    document.addEventListener('keydown', e => {
      if (e.key !== 'Escape' || !pop.space) return;
      const back = popBox.contains(document.activeElement) ? pop.anchor : null;
      hidePop();
      if (back) back.focus();
    });

    $('prio-only').addEventListener('click', () => {
      save(PRIO_ONLY, prioOnly() ? '' : '1');
      render();
    });

    const reason = $('reason');
    $('reason-form').addEventListener('submit', e => {
      e.preventDefault();
      const r = reasonFor;
      reasonFor = null;
      reason.close();
      const c = r && cardByKey(r.key);
      if (c) changeStage(c, r.stage, $('reason-text').value.trim());
    });
    $('reason-cancel').addEventListener('click', () => reason.close());
    reason.addEventListener('close', () => {
      reasonFor = null;
    });
    reason.addEventListener('click', e => {
      if (e.target === reason) reason.close();
    });

    $('detail-body').addEventListener('click', e => {
      const btn = e.target instanceof Element ? e.target.closest('button[data-act]') : null;
      const c = btn && detailKey ? cardByKey(detailKey) : null;
      if (!c) return;
      const act = btn.getAttribute('data-act');
      if (act === 'move' || act === 'spaces') {
        if (menuFor && menuFor.trigger === btn) closeMenu(true);
        else openMenu(btn, detailKey, act === 'spaces' ? 'spaces' : '');
      } else if (act === 'priority') setPriority(c, !c.priority);
      else if (act === 'focus') showInHerdr(c);
      else if (act === 'research' || act === 'continue' || act === 'mute') slackAction(detailKey, act);
      else if (act === 'unlink') unlink(c, btn.getAttribute('data-url'));
      else if (act === 'openfile') openFile(c, btn.getAttribute('data-url'));
      else if (act === 'openlocal') openLocal(c, btn.getAttribute('data-url'));
      else if (act === 'copypath') copyPath(c, btn.getAttribute('data-url'));
    });
    $('view-back').addEventListener('click', closeView);
    // Esc goes back to the board, unless it's for a terminal, the menu or a
    // dialog.
    document.addEventListener('keydown', e => {
      if (e.key !== 'Escape' || !viewing || e.defaultPrevented || menuFor || document.querySelector('dialog[open]')) return;
      if (e.target instanceof Element && e.target.closest('.term-pane')) return;
      e.preventDefault();
      closeView();
    });
    window.addEventListener('popstate', route);
    window.addEventListener('hashchange', route);

    // Another tab signed in, perhaps with a new token. Storage holds its
    // key already, so it's told apart by the key the stream signed in with.
    // A tab signed out tries it whatever it is, and opens the view its
    // address asks for.
    window.addEventListener('storage', e => {
      if (e.key !== KEY || !e.newValue) return;
      memory.set(KEY, e.newValue);
      if (signedOut || e.newValue !== streamKey) {
        connect();
        route();
      }
    });
    window.addEventListener('online', () => {
      if (retryTimer) connect();
    });
  }

  // moveOldHash sends an old address, /#tab=<session>/<tab>, to the page of
  // its session. It says whether it did.
  function moveOldHash() {
    const v = session ? null : hashTab();
    if (!v || !v.session || !hasPage(v.session)) return false;
    location.replace(pageOf(v.session) + viewHash(v.tab));
    return true;
  }

  if (session) {
    const name = $('session-name');
    name.textContent = session;
    name.hidden = false;
    document.title = session + ' · Tasks';
  }
  buildSlack();
  buildShare();
  buildColumns();
  wire();
  setInterval(tickAges, 30e3);
  signIn().then(() => {
    if (moveOldHash()) return;
    connect();
    route();
  });
})();
