// Checks search.js against the cases the overview's search passes too, in
// internal/view/testdata/search.json.
// Usage: node internal/board/web/search_test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const window = {};
vm.runInNewContext(fs.readFileSync(new URL('./search.js', import.meta.url), 'utf8'), {window});
const {words, search, MATCHES} = window.herdrSearch;

const cases = JSON.parse(fs.readFileSync(new URL('../../view/testdata/search.json', import.meta.url), 'utf8'));
assert.ok(cases.length > 0);
for (const c of cases) {
  const f = Object.assign({name: '', panes: [], recaps: [], content: []}, c.fields);
  const h = search(words(c.query), f);
  assert.equal(MATCHES[h.match], c.match, c.why);
  if (!h.match) continue;
  assert.equal(h.text, c.text, c.why);
  assert.equal(h.text.slice(h.at, h.at + h.len), c.found, c.why);
}
console.log('ok', cases.length, 'cases');
