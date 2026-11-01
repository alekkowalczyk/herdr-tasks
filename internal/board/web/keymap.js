// Which keys the board's terminals send to a herdr pane, and how.
//
// mapKey(ev, {mac}) takes a key event and says what to do with it. null
// leaves it to xterm. Otherwise it's one of:
//   - {keys: [name]}: an op for POST /api/pane/input that presses a key by
//     herdr's name for it. herdr encodes it in the pane's own modes (cursor
//     keys, the kitty keyboard protocol), which the browser's terminal never
//     learns.
//   - {text: bytes}: an op that types the sequence for a key herdr has no
//     name for: Home, End, Page Up, Page Down, Delete and Insert.
//   - {copy: true}: copy the selected text, and send nothing.
//   - {browser: true}: leave the key to the browser, and keep it from xterm,
//     which would type something for it.
// Plain printable keys, IME composition and dead keys are left to xterm,
// which types the text they make. Only keydown counts. Safari sends the key
// that ends a composition (Enter, or Backspace) after compositionend, so it
// isn't composing, but its keyCode is 229; it's left to xterm too, which
// types the confirmed text rather than an Enter ahead of it.
//
// On macOS, Option types the letters it makes (ą, ł and ś on a Polish
// keyboard, å or é elsewhere), which are left to xterm. Only the Option chords
// terminal programs use go to herdr as alt: Option with b, d, f, p, t, y or
// ".", and with the named keys (enter, backspace, the arrows).
//
// Cmd shortcuts stay with the browser; Cmd+A is left to xterm, which selects
// the pane's text. Elsewhere, copy and paste are terminals' usual keys:
// ctrl+shift+c or ctrl+insert copies, and ctrl+shift+v or shift+insert
// pastes. Paste isn't a key: the terminal view sends what the browser pastes.
(() => {
  'use strict';

  // herdr's names for keys that aren't characters.
  const NAMED = {
    Enter: 'enter',
    Escape: 'esc',
    Tab: 'tab',
    Backspace: 'backspace',
    ArrowUp: 'up',
    ArrowDown: 'down',
    ArrowLeft: 'left',
    ArrowRight: 'right',
  };

  // Keys herdr has no name for, as xterm's sequences: CSI n ~ or CSI final,
  // with the modifiers as CSI 1;m final or CSI n;m ~.
  const RAW = {
    Home: ['1', 'H'],
    End: ['1', 'F'],
    PageUp: ['5', '~'],
    PageDown: ['6', '~'],
    Delete: ['3', '~'],
    Insert: ['2', '~'],
  };

  const MODIFIERS = ['Shift', 'Control', 'Alt', 'Meta', 'CapsLock', 'AltGraph', 'NumLock', 'ScrollLock', 'Fn', 'OS', 'ContextMenu'];

  // The character on a US keyboard's key, for Option chords on macOS, where
  // the event's key is the character Option makes (Option+b is ∫).
  const CODE_CHAR = {
    Backquote: '`',
    Minus: '-',
    Equal: '=',
    BracketLeft: '[',
    BracketRight: ']',
    Backslash: '\\',
    Semicolon: ';',
    Quote: "'",
    Comma: ',',
    Period: '.',
    Slash: '/',
    Space: ' ',
  };

  // The keys whose Option chords go as alt on macOS: back, delete and
  // forward a word, and the readline and Claude Code chords next to them.
  const OPTION_AS_ALT = new Set(['b', 'd', 'f', 'p', 't', 'y', '.']);

  function codeChar(code) {
    if (/^Key[A-Z]$/.test(code)) return code[3].toLowerCase();
    if (/^Digit[0-9]$/.test(code)) return code[5];
    return CODE_CHAR[code];
  }

  // herdr's names for characters that can't stand in a chord: + joins
  // its parts, and spaces are trimmed away.
  const CHAR_NAME = {'+': 'plus', ' ': 'space'};

  // letterOf is the letter a key types, from the layout, or from the key's
  // place on a US keyboard when the layout's isn't a Latin letter.
  function letterOf(ev) {
    if (/^[a-z]$/i.test(ev.key)) return ev.key.toLowerCase();
    const c = codeChar(ev.code);
    return c && /^[a-z]$/.test(c) ? c : '';
  }

  function mapKey(ev, opts) {
    const mac = !!(opts && opts.mac);
    if (!ev || ev.type !== 'keydown') return null;
    if (ev.isComposing || ev.keyCode === 229 || ev.key === 'Process' || ev.key === 'Dead' || ev.key === 'Unidentified') return null;
    if (MODIFIERS.includes(ev.key)) return null;
    if (ev.metaKey) {
      // xterm types \r for Cmd+Enter, DEL for Cmd+Backspace and ESC for
      // Cmd+Escape, so it doesn't see Cmd keys, but for Cmd+A.
      const selectAll = mac && !ev.ctrlKey && !ev.altKey && !ev.shiftKey && letterOf(ev) === 'a';
      return selectAll ? null : {browser: true};
    }
    // AltGr arrives as ctrl and alt on Windows and Linux; its character is text.
    const altGr = typeof ev.getModifierState === 'function' && ev.getModifierState('AltGraph');
    const ctrl = ev.ctrlKey && !altGr;
    const alt = ev.altKey && !altGr;
    const shift = ev.shiftKey;

    // Copy and paste, but on macOS, where they're Cmd shortcuts. ctrl+shift+c
    // would otherwise be ctrl+c to herdr: an interrupt.
    if (!mac && !alt) {
      if (ctrl && shift && letterOf(ev) === 'c') return {copy: true};
      if (ctrl && shift && letterOf(ev) === 'v') return {browser: true};
      if (ev.key === 'Insert' && ctrl && !shift) return {copy: true};
      if (ev.key === 'Insert' && shift && !ctrl) return {browser: true};
    }

    if (Object.prototype.hasOwnProperty.call(RAW, ev.key)) {
      const [n, final] = RAW[ev.key];
      const m = 1 + (shift ? 1 : 0) + (alt ? 2 : 0) + (ctrl ? 4 : 0);
      if (final === '~') return {text: m > 1 ? '\x1b[' + n + ';' + m + '~' : '\x1b[' + n + '~'};
      return {text: m > 1 ? '\x1b[1;' + m + final : '\x1b[' + final};
    }

    const mods = [];
    if (ctrl) mods.push('ctrl');
    if (alt) mods.push('alt');
    if (Object.prototype.hasOwnProperty.call(NAMED, ev.key) || /^F([1-9]|1[0-2])$/.test(ev.key)) {
      if (shift) mods.push('shift');
      return {keys: [mods.concat(NAMED[ev.key] || ev.key.toLowerCase()).join('+')]};
    }
    // herdr takes F13 and up, but sends nothing for them.
    if (/^F\d+$/.test(ev.key)) return null;
    // Other named keys, such as media keys.
    if ([...ev.key].length !== 1) return null;
    if (!ctrl && !alt) return null;
    if (mac && alt && !ctrl && !OPTION_AS_ALT.has(codeChar(ev.code))) return null;

    let ch = ev.key;
    if (alt && ch.charCodeAt(0) > 0x7e) {
      const base = codeChar(ev.code);
      if (base) ch = shift && /^[a-z]$/.test(base) ? base.toUpperCase() : base;
    }
    if (ctrl && /^[a-zA-Z]$/.test(ch)) {
      if (shift) mods.push('shift');
      ch = ch.toLowerCase();
    }
    // Other shifted characters are in the key already (@, !). herdr would
    // send 1 for shift+1, so shift isn't added for them.
    return {keys: [mods.concat(CHAR_NAME[ch] || ch).join('+')]};
  }

  window.herdrKeymap = {mapKey};
})();
