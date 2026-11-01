// Checks keymap.js against simulated key events, and every key name it
// makes against a port of herdr 0.8.2's key parser (parse_key_combo).
// Usage: node internal/board/web/keymap_test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const window = {};
vm.runInNewContext(fs.readFileSync(new URL('./keymap.js', import.meta.url), 'utf8'), {window});
const {mapKey} = window.herdrKeymap;

// herdrAccepts reports whether herdr takes a key name, as send_keys checks
// every name before it sends any.
function herdrAccepts(s) {
  s = s.trim();
  if (s === 'C-c' || s === 'c-c') s = 'ctrl+c';
  if (s === '+') s = 'plus';
  const mods = new Set(['ctrl', 'control', 'shift', 'alt', 'option', 'meta', 'cmd', 'command', 'super', 'hyper']);
  let key = null;
  for (const part of s.split('+')) {
    const t = part.trim();
    if (!t) return false;
    if (mods.has(t.toLowerCase())) continue;
    if (key !== null) return false;
    key = t;
  }
  if (key === null) return false;
  const names = ['space', 'enter', 'return', 'esc', 'escape', 'tab', 'backspace', 'bs', 'left', 'right', 'up', 'down', 'minus', 'comma',
    'period', 'slash', 'backslash', 'quote', 'double_quote', 'double-quote', 'semicolon', 'colon', 'percent', 'ampersand', 'backtick', 'plus'];
  const l = key.toLowerCase();
  if (names.includes(l)) return true;
  if ([...key].length === 1) return true;
  return /^f\d+$/.test(l) && Number(l.slice(1)) <= 255;
}

// key makes a key event: mods holds c (ctrl), a (alt), s (shift), m (meta).
function key(k, code, mods = '', type = 'keydown', more = {}) {
  return Object.assign({
    type,
    key: k,
    code,
    ctrlKey: mods.includes('c'),
    altKey: mods.includes('a'),
    shiftKey: mods.includes('s'),
    metaKey: mods.includes('m'),
    isComposing: false,
    getModifierState: () => false,
  }, more);
}

const mac = {mac: true};

// Each case is an event, what it maps to, and the options: not macOS unless
// they say so.
const cases = [
  // Only keydown counts.
  [key('Enter', 'Enter', '', 'keyup'), null],
  [key('Enter', 'Enter', '', 'keypress'), null],
  [key('a', 'KeyA', '', 'keypress'), null],
  [key('c', 'KeyC', 'c', 'keyup'), null],

  [key('Enter', 'Enter'), {keys: ['enter']}],
  [key('Enter', 'Enter', 's'), {keys: ['shift+enter']}],
  [key('Enter', 'Enter', 'a'), {keys: ['alt+enter']}],
  [key('Escape', 'Escape'), {keys: ['esc']}],
  [key('Tab', 'Tab'), {keys: ['tab']}],
  [key('Tab', 'Tab', 's'), {keys: ['shift+tab']}],
  [key('Backspace', 'Backspace'), {keys: ['backspace']}],
  [key('Backspace', 'Backspace', 'a'), {keys: ['alt+backspace']}],
  [key('ArrowUp', 'ArrowUp'), {keys: ['up']}],
  [key('ArrowLeft', 'ArrowLeft', 'c'), {keys: ['ctrl+left']}],
  [key('F1', 'F1'), {keys: ['f1']}],
  [key('F12', 'F12', 's'), {keys: ['shift+f12']}],

  [key('c', 'KeyC', 'c'), {keys: ['ctrl+c']}],
  [key('c', 'KeyC', 'c'), {keys: ['ctrl+c']}, mac],
  // On macOS, where copy and paste are Cmd shortcuts, ctrl+shift goes to herdr.
  [key('C', 'KeyC', 'cs'), {keys: ['ctrl+shift+c']}, mac],
  [key('V', 'KeyV', 'cs'), {keys: ['ctrl+shift+v']}, mac],
  [key('Insert', 'Insert', 's'), {text: '\x1b[2;2~'}, mac],
  [key('Insert', 'Insert', 'c'), {text: '\x1b[2;5~'}, mac],
  [key('[', 'BracketLeft', 'c'), {keys: ['ctrl+[']}],
  [key(' ', 'Space', 'c'), {keys: ['ctrl+space']}],
  [key('@', 'Digit2', 'cs'), {keys: ['ctrl+@']}],
  [key('+', 'Equal', 'cs'), {keys: ['ctrl+plus']}],
  [key('x', 'KeyX', 'ca'), {keys: ['ctrl+alt+x']}],

  // Option on macOS: the key is what Option makes, the code is the key.
  [key('∫', 'KeyB', 'a'), {keys: ['alt+b']}],
  [key('ƒ', 'KeyF', 'a'), {keys: ['alt+f']}],
  [key('ı', 'KeyB', 'as'), {keys: ['alt+B']}],
  [key('≥', 'Period', 'a'), {keys: ['alt+.']}],
  // Alt elsewhere: the key is the key.
  [key('b', 'KeyB', 'a'), {keys: ['alt+b']}],
  // On macOS, the Option chords terminal programs use go as alt, and Option
  // types the letters it makes otherwise (Polish Pro: ą, ł, ź, Ą).
  [key('∫', 'KeyB', 'a'), {keys: ['alt+b']}, mac],
  [key('ƒ', 'KeyF', 'a'), {keys: ['alt+f']}, mac],
  [key('∂', 'KeyD', 'a'), {keys: ['alt+d']}, mac],
  [key('ı', 'KeyB', 'as'), {keys: ['alt+B']}, mac],
  [key('≥', 'Period', 'a'), {keys: ['alt+.']}, mac],
  [key('ą', 'KeyA', 'a'), null, mac],
  [key('ł', 'KeyL', 'a'), null, mac],
  [key('ź', 'KeyX', 'a'), null, mac],
  [key('Ą', 'KeyA', 'as'), null, mac],
  [key('å', 'KeyA', 'a'), null, mac],
  [key('Enter', 'Enter', 'a'), {keys: ['alt+enter']}, mac],
  [key('Backspace', 'Backspace', 'a'), {keys: ['alt+backspace']}, mac],
  [key('ArrowLeft', 'ArrowLeft', 'a'), {keys: ['alt+left']}, mac],
  [key('≈', 'KeyX', 'ca'), {keys: ['ctrl+alt+x']}, mac],

  [key('Home', 'Home'), {text: '\x1b[H'}],
  [key('End', 'End'), {text: '\x1b[F'}],
  [key('Home', 'Home', 's'), {text: '\x1b[1;2H'}],
  [key('PageUp', 'PageUp'), {text: '\x1b[5~'}],
  [key('PageDown', 'PageDown'), {text: '\x1b[6~'}],
  [key('PageDown', 'PageDown', 'c'), {text: '\x1b[6;5~'}],
  [key('Delete', 'Delete'), {text: '\x1b[3~'}],
  [key('Delete', 'Delete', 'a'), {text: '\x1b[3;3~'}],
  [key('Insert', 'Insert'), {text: '\x1b[2~'}],

  // Left to xterm, which types the text.
  [key('a', 'KeyA'), null],
  [key('A', 'KeyA', 's'), null],
  [key(' ', 'Space'), null],
  [key('!', 'Digit1', 's'), null],
  [key('é', 'KeyE'), null],
  [key('a', 'KeyA', '', 'keydown', {isComposing: true}), null],
  // Safari's key that ends a composition comes after compositionend, so
  // not composing, but with keyCode 229: xterm types the text it confirms.
  [key('Enter', 'Enter', '', 'keydown', {keyCode: 229}), null],
  [key('Enter', 'Enter', '', 'keydown', {keyCode: 229}), null, mac],
  [key('Backspace', 'Backspace', '', 'keydown', {keyCode: 229}), null, mac],
  [key('Enter', 'Enter', '', 'keydown', {keyCode: 13}), {keys: ['enter']}, mac],
  [key('Process', 'KeyA'), null],
  [key('Dead', 'KeyE', 'a'), null],
  [key('@', 'KeyQ', 'ca', 'keydown', {getModifierState: m => m === 'AltGraph'}), null],
  // Elsewhere, ctrl+shift+c and ctrl+insert copy, and ctrl+shift+v and
  // shift+insert are left to the browser, which pastes.
  [key('C', 'KeyC', 'cs'), {copy: true}],
  [key('V', 'KeyV', 'cs'), {browser: true}],
  [key('Insert', 'Insert', 'c'), {copy: true}],
  [key('Insert', 'Insert', 's'), {browser: true}],
  [key('С', 'KeyC', 'cs'), {copy: true}], // a Cyrillic layout
  [key('C', 'KeyI', 'cs'), {copy: true}], // Dvorak
  [key('v', 'KeyV', 'c'), {keys: ['ctrl+v']}],
  [key('C', 'KeyC', 'cs', 'keyup'), null],
  [key('C', 'KeyC', 'cas'), {keys: ['ctrl+alt+shift+c']}],
  [key('Insert', 'Insert', 'cs'), {text: '\x1b[2;6~'}],

  // Cmd shortcuts are left to the browser, and kept from xterm, which would
  // type \r for Cmd+Enter. Cmd+A is left to xterm, which selects all.
  [key('c', 'KeyC', 'm'), {browser: true}, mac],
  [key('v', 'KeyV', 'm'), {browser: true}, mac],
  [key('Enter', 'Enter', 'm'), {browser: true}, mac],
  [key('Backspace', 'Backspace', 'm'), {browser: true}, mac],
  [key('Escape', 'Escape', 'm'), {browser: true}, mac],
  [key('ArrowLeft', 'ArrowLeft', 'm'), {browser: true}, mac],
  [key('A', 'KeyA', 'ms'), {browser: true}, mac],
  [key('a', 'KeyA', 'm'), null, mac],
  [key('Enter', 'Enter', 'm', 'keyup'), null, mac],
  [key('Enter', 'Enter', 'm'), {browser: true}],
  [key('a', 'KeyA', 'm'), {browser: true}],
  // Left to the browser, or sending nothing.
  [key('Shift', 'ShiftLeft', 's'), null],
  [key('Control', 'ControlLeft', 'c'), null],
  [key('F13', 'F13'), null],
  [key('MediaPlayPause', 'MediaPlayPause'), null],
];

let failed = 0;
for (const [ev, want, opts = {mac: false}] of cases) {
  // The script runs in its own realm; JSON gives its answer this one's types.
  const got = JSON.parse(JSON.stringify(mapKey(ev, opts)));
  try {
    assert.deepEqual(got, want);
    for (const name of (got && got.keys) || []) assert.ok(herdrAccepts(name), 'herdr refuses ' + name);
  } catch (err) {
    failed++;
    const mods = ['ctrl', 'alt', 'shift', 'meta'].filter(m => ev[m + 'Key']).join('+');
    console.error(`${opts.mac ? 'macOS' : 'other'} ${ev.type} ${mods} ${JSON.stringify(ev.key)} ${ev.code}: ${err.message}`);
  }
}
if (failed) {
  console.error(`${failed} of ${cases.length} cases failed`);
  process.exit(1);
}
console.log(`ok: ${cases.length} cases`);
