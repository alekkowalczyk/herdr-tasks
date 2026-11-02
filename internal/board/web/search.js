// How the board's search finds tasks: the overview's rules, from
// internal/view/search.go, which search_test.mjs checks it against.
//
// search(words, fields) finds every word in a task's fields, or returns
// match 0. Each word is found in the best place that has it, and the task
// where its worst word was: name (1), then a pane's name or an agent's
// title (2), then a recap (3), then the rest of the card (4). A word
// matches any part of a word, in any case. text is the text it was found
// in, on one line; text.slice(at, at + len) is the word that decided where,
// and len is 0 when the text's lower case is longer or shorter than it.
(() => {
  'use strict';

  const MATCHES = ['none', 'name', 'pane', 'recap', 'content'];

  const words = query => String(query).toLowerCase().split(/\s+/).filter(Boolean);

  const oneLine = text => String(text).split(/\s+/).filter(Boolean).join(' ');

  function search(ws, f) {
    if (!ws.length) return {match: 0};
    const places = [null, [f.name], f.panes, f.recaps, f.content];
    const lines = new Map();
    let hit = {match: 0};
    for (const w of ws) {
      let found = false;
      for (let m = 1; m < places.length && !found; m++) {
        for (const text of places[m] || []) {
          let l = lines.get(text);
          if (!l) {
            const one = oneLine(text || '');
            l = [one, one.toLowerCase()];
            lines.set(text, l);
          }
          const i = l[1].indexOf(w);
          if (i < 0) continue;
          found = true;
          if (m > hit.match) {
            hit = {match: m, text: l[0], at: 0, len: 0};
            if (l[0].length === l[1].length) Object.assign(hit, {at: i, len: w.length});
          }
          break;
        }
      }
      if (!found) return {match: 0};
    }
    return hit;
  }

  // cardFields is what a search reads of a card on the board.
  function cardFields(c) {
    const agents = c.agents || [];
    const some = list => list.filter(Boolean);
    return {
      name: c.title || '',
      panes: some([...(c.panes || []), ...agents.map(a => a.title)]),
      recaps: some(agents.map(a => a.recap)),
      content: some([
        c.reason,
        c.note,
        ...(c.log || []).map(e => e.text),
        ...(c.links || []).flatMap(l => [l.label, l.url, l.linear && l.linear.title]),
        ...agents.flatMap(a => [a.lastPrompt, a.lastReply]),
        c.slack && c.slack.place,
      ]),
    };
  }

  window.herdrSearch = {MATCHES, words, search, cardFields};
})();
