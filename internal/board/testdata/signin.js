// Runs the board page's script in a stand-in browser, to check how a tab
// follows a sign-in from another tab. Usage: node signin.js path/to/app.js
'use strict';

const fs = require('fs');
const vm = require('vm');

const KEY = 'herdr-tasks-board-key';
const src = fs.readFileSync(process.argv[2], 'utf8');

// fake is a stand-in for any DOM object: it takes any property and any call.
function fake() {
  const props = new Map();
  return new Proxy(function () {}, {
    get(_, p) {
      if (p === 'then') return undefined; // not a promise
      if (p === Symbol.toPrimitive) return () => '';
      if (p === Symbol.iterator) return function* () {};
      if (!props.has(p)) props.set(p, fake());
      return props.get(p);
    },
    set(_, p, v) {
      props.set(p, v);
      return true;
    },
    apply: () => fake(),
  });
}

// Timers that don't keep this script running.
const unref = t => (t && t.unref ? t.unref() : t);

// page runs the script as a tab opened at /work/, or at hash, with storage
// holding stored. The server takes only the key good. A stream it takes
// stays open. With https, the tab is opened through the tailnet; with
// owner, the server signs it in without a code, as the machine's owner.
function page(stored, good, hash, {https, owner} = {}) {
  const storage = new Map(stored ? [[KEY, stored]] : []);
  const listeners = {};
  const streams = []; // the key each connection's first request, for the board, was sent with
  const views = []; // each tab view the page opened, as session/tab
  const signIns = []; // the body of each sign-in
  const ok = {ok: true, status: 200, text: async () => '', body: {getReader: () => ({read: () => new Promise(() => {})})}};
  const denied = {ok: false, status: 401, json: async () => ({error: 'Sign in again'})};
  const window = {
    addEventListener: (type, fn) => (listeners[type] = listeners[type] || []).push(fn),
    herdrTerm: {
      mount: (box, o) => {
        views.push(o.session + '/' + o.tab);
        return {close() {}};
      },
    },
  };
  // A page that shows: one that doesn't waits to take up a stream.
  const document = fake();
  document.hidden = false;
  const ctx = {
    window,
    document,
    Element: class Element {},
    location: {search: '', hash: hash || '', pathname: '/work/', protocol: https ? 'https:' : 'http:'},
    history: {replaceState() {}},
    localStorage: {
      getItem: k => (storage.has(k) ? storage.get(k) : null),
      setItem: (k, v) => storage.set(k, String(v)),
    },
    fetch: async (url, opts) => {
      if (url === '/api/session') {
        signIns.push(opts.body);
        return owner ? {ok: true, status: 200, json: async () => ({key: good})} : denied;
      }
      const auth = ((opts && opts.headers) || {}).Authorization || '';
      if (url === '/api/board?session=work') streams.push(auth.replace(/^Bearer /, ''));
      return auth === 'Bearer ' + good ? ok : denied;
    },
    setTimeout: (fn, ms) => unref(setTimeout(fn, ms)),
    setInterval: (fn, ms) => unref(setInterval(fn, ms)),
    clearTimeout,
    requestAnimationFrame: fn => unref(setTimeout(fn, 0)),
    AbortController,
    TextDecoder,
    URL,
    URLSearchParams,
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  return {
    streams,
    views,
    signIns,
    // otherTab stores key, as a tab that signs in does, and tells this one.
    otherTab(key) {
      storage.set(KEY, key);
      for (const fn of listeners.storage || []) fn({key: KEY, newValue: key});
    },
  };
}

const settle = () => new Promise(r => setTimeout(r, 20));

function expect(what, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    throw new Error(what + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

async function main() {
  // A tab reloaded since it signed in has its key in storage only. The token
  // is replaced, so the server turns it away; another tab then signs in.
  let p = page('old-key', 'new-key');
  await settle();
  expect('streams before', p.streams, ['old-key']);
  p.otherTab('new-key');
  await settle();
  expect('streams after another tab signed in', p.streams, ['old-key', 'new-key']);

  // A tab with a stream open moves to the new key.
  p = page('old-key', 'old-key');
  await settle();
  p.otherTab('new-key');
  await settle();
  expect('streams of a signed-in tab', p.streams, ['old-key', 'new-key']);

  // The same key again changes nothing.
  p = page('old-key', 'old-key');
  await settle();
  p.otherTab('old-key');
  await settle();
  expect('streams after the same key', p.streams, ['old-key']);

  // A tab opened at a tab's view, with no key, opens the view once another
  // tab signs in.
  p = page(null, 'new-key', '#tab=api');
  await settle();
  expect('views before', p.views, []);
  p.otherTab('new-key');
  await settle();
  expect('views after another tab signed in', p.views, ['work/api']);

  // Opened through the tailnet with no code, the page asks to sign in
  // without one, and the machine's owner is signed in.
  p = page(null, 'new-key', '', {https: true, owner: true});
  await settle();
  expect('sign-ins of the owner', p.signIns, ['{}']);
  expect('streams of the owner', p.streams, ['new-key']);

  // Anyone else isn't, and on 127.0.0.1 the page doesn't ask.
  p = page(null, 'new-key', '', {https: true});
  await settle();
  expect('sign-ins of someone else', p.signIns, ['{}']);
  expect('streams of someone else', p.streams, []);
  p = page(null, 'new-key');
  await settle();
  expect('sign-ins on 127.0.0.1', p.signIns, []);
}

main().then(
  () => console.log('ok'),
  err => {
    console.error(err.stack || String(err));
    process.exit(1);
  },
);
