# Session Boards and Remote herdr Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each herdr session its own board, send Slack asks to herdr's default session by default, and
make a server's boards, linked pages and dev servers reachable from the laptop through the tailnet.

**Architecture:** There is still one `herdr-tasks serve` per machine. It serves a board per session under
`/<session>/`, and serves a task's local pages itself at capability addresses, in a sandbox. With
`board_tailnet`, it also listens on a Unix socket that `tailscale serve` shares with the tailnet. That door
trusts Tailscale's identity header, and only on that door. Shared dev-server ports go through per-port
sockets and a proxy in the same server, which makes requests look local.

**Tech Stack:** Go 1.23 (stdlib only; `net/http`, `httputil.ReverseProxy`), vanilla JS (createElement and
textContent only), the `tailscale` CLI.

**Spec:** `docs/superpowers/specs/2026-11-01-session-boards-and-remote-design.md`

## Global Constraints

- Go 1.23 with `GOTOOLCHAIN=go1.23.4`, `CGO_ENABLED=0`, no new modules. Tailscale only through its CLI
  (`status --json`, `serve`, `serve status --json`).
- Never run any of these against the real HOME:
  - `make build`, `make link`, `sh scripts/build.sh`, `go build -o bin/...`;
  - `herdr-tasks setup`, `remove`, `serve`, `board`, `claude-hook` or `slack …`;
  - the real `tailscale`, `herdr` or `claude`.
- Never write to `~/.claude/settings.json`, `~/.config/herdr` or `~/.local/state/herdr`, and never send
  input to the user's panes.
- Tests use temporary HOMEs and fake `tailscale`, `herdr` and `claude`.
- Test data is invented:
  - sessions `work`, `side` and `default`;
  - tailnet machine `devbox.tail0000.ts.net`;
  - logins `dana@acme.test` (the owner) and `rui@acme.test`;
  - the dev server on `localhost:4100`, the board on port 7391;
  - Slack names as before (acme.slack.com, #billing, Dana Reed).
- The repo is public: no secrets or personal data. The controller runs the personal-data grep with the real
  terms, which aren't in the repo; implementers never write real names.
- The board keeps its protections: CSP, `X-Frame-Options: DENY`, the Origin and Sec-Fetch-Site checks,
  the admin token, and the browser key per server run.
- Every `tailscale` call has a timeout (10 s). With `board_tailnet = false` and nothing recorded, the server
  makes no `tailscale` call at all.
- Never `tailscale funnel`. Never change or remove a `tailscale serve` entry that herdr-tasks didn't record,
  or one whose target no longer matches the record.
- Writing: plain, short sentences that name things the way the user sees them, as in the README now.
- Version 0.11.0, in `cmd/herdr-tasks/main.go` and `herdr-plugin.toml`.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`. Commit
  on `session-boards`, never on main.
- Checks before each commit:
  - `GOTOOLCHAIN=go1.23.4 go vet ./...`;
  - `GOTOOLCHAIN=go1.23.4 go test -race ./...`;
  - `node internal/board/web/keymap_test.mjs`;
  - for the last task, the four cross-builds:
    `for os in darwin linux; do for arch in amd64 arm64; do CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOTOOLCHAIN=go1.23.4 go build -o /dev/null ./cmd/herdr-tasks || exit 1; done; done`.

## Review Focus

1. **A session name that needs escaping in a URL** (`my work`, `zażółć`). Its board, its list link and
   `herdr-tasks board --session` all work. (Task 2: `TestBoardPathEscapesSessionName`.)
2. **`tailscale` missing, not running, denied or hanging.** The board keeps working on 127.0.0.1, its
   header says why, and the server asks `tailscale` at most once a minute while the problem lasts. (Task 7:
   `TestTailnetProblemShownAndBacksOff`.)
3. **A killed server leaves socket files, and Tailscale keeps its entries.** The next server replaces the
   stale sockets, and the board and the shares work again. (Task 7: `TestStaleSocketReplaced`; Task 8:
   `TestSharesComeBackAfterRestart`.)
4. **Path tricks on a served page:** encoded `..`, symlinks that lead out, hidden folders, names that look
   like secrets, folders. Each gets a 404. (Task 4: `TestPageRefuses`.)
5. **The user's own `tailscale serve` entries.** Sharing a port they already serve is refused, and
   `remove` after they changed the board's entry leaves it alone. (Task 7: `TestTailnetLeavesOthersEntries`;
   Task 8: `TestShareRefusesTakenPort`; Task 7: `TestRemoveUnsharesOnlyOwnEntries`.)

---

### Task 1: Slack asks go to herdr's default session

**Files:**
- Modify:
  - `internal/herdr/client.go`: add the `DefaultSession` constant; `SessionKey` uses it;
  - `internal/reconcile/reconcile.go`: `SlackSessionName`;
  - `internal/slack/settings.go`, `internal/slack/watcher.go` (`sessionFor`, `WaitingFor`, the doc comment
    on `Watcher`);
  - `internal/slack/fetch.go`, if it relies on the old branch;
  - `cmd/herdr-tasks/slack.go` (`slackTrouble`);
  - `README.md`: Slack asks, Settings, and an upgrade note.
- Test: `internal/slack/watcher_test.go`, `internal/slack/settings_test.go`, `cmd/herdr-tasks/slack_test.go`

**Interfaces:**
- Produces:
  - `herdr.DefaultSession = "default"`;
  - `func (o reconcile.Options) SlackSessionName() string`: `SlackSession`, or `herdr.DefaultSession` when
    it's empty;
  - `func slack.WaitingFor(session string) string`, which returns `"waiting for herdr session " + session`.
    Task 2 uses it for the board's notice.

- [ ] **Step 1: Write the failing tests**
  - `TestSessionForWaitsForTheNamedSession`:
    - `sessionFor([work, default], "default")` returns `default` with `why == ""`;
    - `sessionFor([work, side], "default")` returns `why == "waiting for herdr session default"`;
    - no running sessions and the name `work` gives `why == "waiting for herdr session work"`.
  - `TestSettingsFromDefaultsSlackSession`:
    - `SettingsFrom(Options{SlackWatch: true}, home).Session == "default"`;
    - with `SlackSession: "work"` it's `"work"`.
  - `TestSlackTroubleShowsInOtherSessionsWhileWaiting`. With `SlackWatch` on and `SlackSession` unset:
    - a state whose Problem is `WaitingFor("default")` gives `slackTrouble(opts, "work", dir) == true` and
      `slackTrouble(opts, "default", dir) == true`;
    - a state whose Problem is `"Slack isn't signed in"` gives false for `"work"` and true for
      `"default"`;
    - with `SlackWatch` off, everything is false.
  - Update the tests that expect "set slack_session in config.toml; more than one herdr session runs".

- [ ] **Step 2: Run them and see them fail**

Run: `GOTOOLCHAIN=go1.23.4 go test ./internal/slack/ ./cmd/herdr-tasks/ -run 'SessionFor|SettingsFrom|SlackTrouble'`
Expected: FAIL. `WaitingFor` and `SlackSessionName` are undefined, and the old message is still used.

- [ ] **Step 3: Implement**
  - `sessionFor` looks only for the named session. The "only running session" and "more than one" branches
    go.
  - `slackTrouble`:
    - for this session being `opts.SlackSessionName()`, it's true when the Problem isn't empty;
    - for any other session, it's true only when the Problem is `WaitingFor(opts.SlackSessionName())`.
  - Fix the `Watcher` doc comment.
  - README:
    - the settings line becomes `slack_session = ""  # the herdr session for Slack tabs; "" is herdr's default session`;
    - "Turning it on" says asks go to herdr's default session unless `slack_session` names another;
    - the `Slack ⚠` sentence says every session shows it while the Slack session isn't running;
    - an upgrade note says that whoever relied on "the only running session" sets `slack_session`.

- [ ] **Step 4: Run the package tests, then all checks**

Run: `GOTOOLCHAIN=go1.23.4 go vet ./... && GOTOOLCHAIN=go1.23.4 go test -race ./...`
Expected: PASS

- [ ] **Step 5: Commit** with the message `Slack asks go to herdr's default session unless slack_session names another`.

---

### Task 2: A board per session: the server

**Files:**
- Modify:
  - `internal/board/server.go`: routes, `serveBoard`, `events`;
  - `internal/board/model.go`: `Board` fields, `For`, `List`;
  - `internal/board/hub.go`: `addSlack` sets `SlackSession`; per-session JSON for each published board;
  - `internal/board/daemon.go`: `LoginURL`;
  - `internal/board/web/index.html`: asset paths only, `/static/…` becomes `/-/static/…`;
  - `cmd/herdr-tasks/board.go`: `--session`, `currentSession`.
- Test: `internal/board/model_test.go`, `internal/board/server_test.go`, `internal/board/daemon_test.go`,
  `internal/board/web_test.go`, `cmd/herdr-tasks/board_test.go`

**Interfaces:**
- Consumes: Task 1's `reconcile.Options.SlackSessionName()`.
- Produces:
  - Routes:
    - `GET /{$}` and `GET /{session}/{$}` serve `index.html`;
    - `GET /{session}` redirects with 308 to `/{session}/`;
    - static files are at `GET /-/static/`; the old `/static/` route goes.
  - `GET /api/board?session=NAME` returns that session's board (`Board.For`). Without `session` it returns
    the list (`Board.List`). An unknown or stopped session is a 404 with
    `{"error":"Session NAME isn't running."}`.
  - `GET /api/events?session=NAME` streams `event: board` with that session's JSON. Without `session` it
    streams `event: sessions` with the list.
  - New `Board` fields:
    - `SlackSession string` (json `slackSession`), the Slack session's name;
    - `SlackWaiting string` (json `slackWaiting,omitempty`).
  - `func (b Board) For(session string) (Board, bool)`:
    - only that session is kept, in `Sessions`;
    - `Slack` is kept only when `session == b.SlackSession`;
    - while the watcher shows (`b.Slack != nil`) and no running session is named `b.SlackSession`,
      `SlackWaiting` is `"Slack asks go to herdr session " + b.SlackSession + ", which isn't running."`.
  - `type SessionList struct`:
    - `Version string` (json `version`);
    - `Slack string` (json `slack`), the Slack session's name;
    - `Sessions []SessionEntry` (json `sessions`).
  - `type SessionEntry struct { Name string; Cards int; Stale bool }`, with json names `name`, `cards` and
    `stale`.
  - `func (b Board) List() SessionList`.
  - `type Login struct { Local, Tailnet string }`. `Tailnet` stays "" until Task 7.
  - `func LoginURL(stateDir, session string) (Login, error)`: `Local` is
    `http://127.0.0.1:<port>/<url.PathEscape(session)>/?code=<code>`, or `http://127.0.0.1:<port>/?code=…`
    when `session == ""`.
  - `func currentSession(getenv func(string) string) string`:
    - with `HERDR_SOCKET_PATH` or `HERDR_SESSION` set, it's `herdr.SessionKey` of the socket that
      `herdr.SocketPath` resolves from them;
    - otherwise it's "".
  - `herdr-tasks board [--print] [--new-token] [--session NAME]`. Without `--session` it opens
    `currentSession(os.Getenv)`.

- [ ] **Step 1: Write the failing tests**
  - `TestBoardForKeepsOneSession`: `Build` with sessions `work` and `side`, then `For("work")` has one
    session named `work`, and `For("gone")` returns `ok == false`.
  - `TestBoardForSlackLineOnlyOnSlackSession`: with `SlackSession = "work"` and `Slack` set,
    `For("side").Slack == nil` and `For("work").Slack != nil`.
  - `TestBoardForSaysSlackSessionIsntRunning`: with `SlackSession = "default"`, `Slack` set, and sessions
    `work` and `side`, `For("work").SlackWaiting` is
    `"Slack asks go to herdr session default, which isn't running."`. With `default` running it's "".
  - `TestBoardListCountsCards`: `List()` gives the names in order, card counts, `Slack`, and `Stale`.
  - `TestServeBoardBySession`:
    - a signed-in `GET /api/board?session=side` has `sessions[0].name == "side"` and length 1;
    - `?session=gone` is a 404 with that error;
    - without the parameter you get `{"version":…,"slack":…,"sessions":[{"name":"side",…},{"name":"work",…}]}`.
  - `TestEventsBySession`: the first event of `/api/events?session=work` is `event: board`, and its data has
    only `work`. Without `session`, the first is `event: sessions`.
  - `TestSessionPagesServeIndex`:
    - `GET /work/` is 200 HTML;
    - `GET /work` is 308 with `Location: /work/`;
    - `GET /-/static/app.js` is 200;
    - `GET /static/app.js` is 404;
    - `GET /` is 200 HTML.
  - `TestBoardPathEscapesSessionName`:
    - with a session named `my work`, `GET /my%20work/` is 200;
    - `?session=my%20work` gives that board;
    - `LoginURL(dir, "my work").Local` contains `/my%20work/?code=`.
  - `TestLoginURLHasSession`: `Local` is `http://127.0.0.1:<port>/work/?code=<code>`, and with "" it's
    `…/?code=`. Use daemon_test's existing fake-server pattern.
  - `TestCurrentSession`:
    - `HERDR_SESSION=work` gives `work`;
    - `HERDR_SOCKET_PATH=<tmp>/.config/herdr/herdr.sock` gives `default`;
    - no variables give "".
  - `TestBoardSessionFlag`:
    - `board --session side` asks `loginURL` for `side`;
    - `board --bogus` errors with a message naming `--print`, `--new-token` and `--session`.
  - Update `web_test.go`'s asset tests for `/-/static/`.

- [ ] **Step 2: Run them and see them fail**

Run: `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ ./cmd/herdr-tasks/ -run 'BoardFor|BoardList|ServeBoardBySession|EventsBySession|SessionPages|PathEscapes|LoginURLHas|CurrentSession|BoardSessionFlag|IndexServesAssets'`
Expected: FAIL

- [ ] **Step 3: Implement**
  - `published` keeps the `Board`, and builds each session's JSON once per `seq`, when first asked for.
  - `addSlack` reads `s.cfg.Options` for `SlackSessionName`. If the options can't be read, it uses
    `herdr.DefaultSession`.
  - The page's asset links move to `/-/static/`.
  - `term.js` and `app.js` don't change here, except for absolute `/static/` paths, if they have any.

- [ ] **Step 4: Run all checks.** Expected: PASS.

- [ ] **Step 5: Commit** with the message `Board: a board per herdr session, at /<session>/, with the list at /`.

---

### Task 3: A board per session: the page

**Files:**
- Modify: `internal/board/web/app.js`, `internal/board/web/index.html`, `internal/board/web/board.css`
- Test: `internal/board/web_test.go`

**Interfaces:**
- Consumes: Task 2's routes and JSON (`Board.SlackSession`, `Board.SlackWaiting`, `SessionList`).
- Produces: the page's modes, by `location.pathname`.
  - **`/`, the list.** It loads `GET /api/board` and listens to `/api/events` (`event: sessions`):
    - It lists each session as a link to `'/' + encodeURIComponent(name) + '/'`, with its card count.
    - The Slack session is marked "Slack asks come here".
    - With exactly one session, `location.replace` goes to its board.
    - An old `#tab=<session>/<tab>` hash goes, with `location.replace`, to
      `/<session>/#tab=<encoded tab>`.
  - **`/<session>/`, the board.** It loads `GET /api/board?session=<name>` and listens to
    `/api/events?session=<name>`:
    - There's no session switcher: `#sessions` and the `herdr-tasks-board-session` storage key go.
    - The header shows the session's name.
    - `SlackWaiting` shows as one line where the Slack line goes.
    - The view hash is `#tab=<encoded tab>`. An old `#tab=<session>/<tab>` for this session works too.
    - Remembered state, except the browser key, is kept per session.
  - **Signing in** replaces the address with `location.pathname` plus the hash, not with `/`.
  - **A 404 for the session** shows "This session isn't running." and a link to `/`.

- [ ] **Step 1: Write the failing tests.** These are source checks, in `web_test.go`'s existing style.
  - `TestPageHasNoSessionSwitcher`: `index.html` has no `id="sessions"`, and `app.js` has no
    `herdr-tasks-board-session`.
  - `TestPageScopesRequestsBySession`: `app.js` contains `/api/board?session=` and
    `/api/events?session=`.
  - `TestSignInKeepsThePath`: the `history.replaceState` in `signIn` uses `location.pathname`.
  - `TestPageHasSessionList`: `app.js` handles `event: sessions` (`'sessions'`) and builds links with
    `encodeURIComponent`.
  - `TestPageShowsSlackWaiting`: `app.js` reads `slackWaiting`.

- [ ] **Step 2: Run them and see them fail.**
  Run: `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ -run 'Page'`. Expected: FAIL.

- [ ] **Step 3: Implement.** Use createElement and textContent only, as the page does now.
  `node internal/board/web/keymap_test.mjs` must still pass.

- [ ] **Step 4: Check by eye.** Use a throwaway `_test.go`, behind an env var, that serves a fake two-session
  board with invented data. Open `/`, `/work/` and `/side/` in headless Chrome, on a free CDP port (never
  9333), and confirm:
  - the list links to both boards;
  - each board shows only its own cards;
  - a tab view opens and reloads.

  Delete the throwaway file afterwards; `git status --short` must show only this task's files.

- [ ] **Step 5: Run all checks and commit** with the message
  `Board page: the list of sessions at /, and each board shows one session`.

---

### Task 4: Pages, served by the board

**Files:**
- Create: `internal/board/pages.go`
- Modify:
  - `internal/board/server.go`: `openLink` returns an address; the route; remove `opener`, `openPath`,
    `openWait` and `capped`;
  - `internal/board/web/app.js`: `openFile` opens the returned `href`;
  - `internal/slack/guard.go`: export the secret-name matcher.
- Test: `internal/board/pages_test.go`, `internal/board/server_test.go` (open tests),
  `internal/slack/guard_test.go`

**Interfaces:**
- Produces:
  - `func slack.LooksSecret(name string) bool`: a base name that matches `secretNames`, the guard's list.
    The guard uses it too.
  - `POST /api/open {session, tab, url}`:
    - The same checks as now: the link is on the task, a local `.html` page, and a regular file once
      symlinks are followed.
    - It replies `{"href":"/-/page/<id>/<url.PathEscape(base name)>"}` and doesn't open anything.
  - `GET /-/page/{id}/{path...}`, with no key: the id is the credential.
  - `id` is 32 hex characters, random per server run and per page folder. The same folder gets the same id
    while the server runs.
  - **Served:** `<page folder>/<path>`, when all of these hold:
    - no path segment starts with `.`;
    - `!slack.LooksSecret(base)`;
    - with symlinks followed, it's still inside the folder (with its symlinks followed);
    - it's a regular file of at most 64 MiB.
  - **Only the page:** when the page's folder is the home folder or `/`, only the page itself is served.
  - **Response headers** (they replace the board's CSP):
    - `Content-Security-Policy: sandbox allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox allow-downloads`;
    - `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`;
    - `Content-Type` by extension;
    - `Access-Control-Allow-Origin: *` only on `.woff`, `.woff2`, `.ttf` and `.otf`.
  - **Every refusal** is a 404. An unknown id answers
    `This page's address works until the board's server restarts. Open it from the board again.`

- [ ] **Step 1: Write the failing tests.** The fixtures go in a temporary HOME:
  `<home>/acme/report/report.html`, `img/chart.png`, `fonts/x.woff2`, `.git/config`, `certs/server.key`,
  `.env`, `notes/`, `link-out` (a symlink to `<home>/outside.txt`), and `<home>/top.html`.
  - `TestOpenGivesPageAddress`:
    - `POST /api/open` for the `file://…/report.html` link on a task replies 200
      `{"href":"/-/page/<32 hex>/report.html"}`;
    - asking twice gives the same id;
    - a link that isn't on the task is a 404, as now;
    - nothing is executed.
  - `TestPageServesFolderFiles`:
    - `GET` of the href is 200 `text/html`;
    - its CSP starts with `sandbox` and has no `allow-same-origin`;
    - there's no `Access-Control-Allow-Origin`;
    - `GET …/img/chart.png` is 200 `image/png`, with no ACAO.
  - `TestPageFontsAllowCORS`: `…/fonts/x.woff2` has `Access-Control-Allow-Origin: *`.
  - `TestPageRefuses`. Each of these is a 404: `%2e%2e/outside.txt`, `..%2foutside.txt`, `.git/config`,
    `certs/server.key`, `.env`, `notes/`, `link-out`, and an unknown id.
  - `TestPageInHomeServesOnlyItself`: for `<home>/top.html`, its href is 200, and `…/outside.txt` under the
    same id is a 404.
  - `TestPageGoneAfterDelete`: after the file is removed, its href is a 404.
  - `TestLooksSecret`: true for `.env`, `.env.local`, `server.key`, `id_rsa` and `.secrets.json`; false for
    `report.html` and `chart.png`.

- [ ] **Step 2: Run them and see them fail.** Run
  `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ ./internal/slack/ -run 'Open|Page|LooksSecret'`.
  Expected: FAIL.

- [ ] **Step 3: Implement.** In `app.js`, `openFile` posts as now and then calls
  `window.open(reply.href, '_blank', 'noopener')`. Open the window in the click handler before the request
  (`const w = window.open('', '_blank')`) and set `w.location` afterwards, so popup blockers allow it. Close
  `w` if the request fails.

- [ ] **Step 4: Run all checks.** Expected: PASS.

- [ ] **Step 5: Commit** with the message `Board: open a task's local pages through the board, in a sandbox`.

---

### Task 5: Two doors: any loopback port, and the tailnet door

**Files:**
- Modify: `internal/board/auth.go`, `internal/board/server.go` (`session`, `Config`),
  `cmd/herdr-tasks/board.go` (the `NewAuth` call)
- Test: `internal/board/auth_test.go`, `internal/board/server_test.go`

**Interfaces:**
- Produces:
  - `func NewAuth(token string) *Auth`. The port goes: the loopback door no longer pins it.
  - **The loopback door.** `func (a *Auth) Wrap(h http.Handler, adminOnly func(string) bool) http.Handler`:
    - The Host must match `^(127\.0\.0\.1|localhost):[0-9]{1,5}$`.
    - For `/api/`, every Origin must equal `"http://" + r.Host`.
    - `Tailscale-*` headers are ignored.
  - **The tailnet door.** `func (a *Auth) WrapTailnet(h http.Handler, adminOnly func(string) bool, origin func() string) http.Handler`:
    - It sets the same headers, with no Host check.
    - For `/api/`, every Origin must equal `origin()`. If `origin()` is "", every `/api/` request is
      refused.
    - The Sec-Fetch-Site check is the same as on the loopback door.
    - `adminOnly` paths are a 403: `Only herdr-tasks on this machine can do that.`
    - It puts `r.Header.Get("Tailscale-User-Login")` in the request's context.
  - `func loginOf(ctx context.Context) string`. It's "" on the loopback door.
  - `func (a *Auth) BrowserKey() string`.
  - `Config.TailnetOwner func() string`: the login that owns this machine, "" when unknown or tagged, nil
    for none. Task 7 sets it.
  - `POST /api/session`:
    - with a code, as now;
    - with `{}` or no code, when `loginOf(ctx) != ""` and it equals `TailnetOwner()`, it replies 200
      `{"key":…}`;
    - otherwise 401 `signInAgain`.

- [ ] **Step 1: Write the failing tests.** Every one uses invented hosts and logins.
  - `TestLoopbackAcceptsAnyPort`:
    - Host `127.0.0.1:8000` with Origin `http://127.0.0.1:8000` and the key on `/api/board` is 200;
    - Host `localhost:9` is 200;
    - Host `evil.test:7391` is 403;
    - Host `127.0.0.1:8000` with Origin `http://127.0.0.1:7391` on `/api/board` is 403;
    - Host `127.0.0.1` with no port is 403.
  - `TestTailnetDoorChecksOrigin`. With `origin` returning `"https://devbox.tail0000.ts.net:7391"` and Host
    `localhost` (as `tailscale serve` sends):
    - that Origin with the key is 200;
    - Origin `https://evil.test` is 403;
    - an `origin()` of "" makes every `/api/` request 403.
  - `TestTailnetDoorRefusesAdminPaths`: `/api/login-code` with the admin token is 403, with that message.
  - `TestSessionSignsInTailnetOwner`. On the tailnet door, with owner `dana@acme.test` and a body of `{}`:
    - the header `Tailscale-User-Login: dana@acme.test` gets 200 and the key;
    - `rui@acme.test` gets 401;
    - no header gets 401;
    - an owner of "" (a tagged machine) gets 401.
  - `TestLoopbackIgnoresTailscaleHeaders`: on the loopback door, with the header and owner both
    `dana@acme.test` and a body of `{}`, it's 401.
  - Update the existing auth tests that pinned `127.0.0.1:<port>` and `NewAuth(token, port)`.

- [ ] **Step 2: Run them and see them fail.** Run
  `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ -run 'Loopback|TailnetDoor|SignsInTailnet|Auth'`.
  Expected: FAIL.

- [ ] **Step 3: Implement.** Write the context key as an unexported type.

- [ ] **Step 4: Run all checks.** Expected: PASS.

- [ ] **Step 5: Commit** with the message
  `Board: any loopback port works, for SSH tunnels; a tailnet door that trusts Tailscale's login`.

---

### Task 6: `internal/tailnet`: the tailscale CLI

**Files:**
- Create: `internal/tailnet/tailnet.go`, `internal/tailnet/tailnet_test.go`

**Interfaces:**
- Produces:
  - `type CLI struct { Bin string; Timeout time.Duration }`:
    - `Bin` "" means `tailscale` on `PATH`, then `/Applications/Tailscale.app/Contents/MacOS/Tailscale` on
      darwin;
    - `Timeout` 0 means 10 s.
  - `var ErrNoCLI = errors.New("the tailscale command isn't installed")`.
  - `type Self struct { DNSName, Owner string }`:
    - `DNSName` has no trailing dot;
    - `Owner` is the login of `Self.UserID` in the status's `User` map, or "" when `Self.Tags` isn't empty.
  - `func (c CLI) Self(ctx context.Context) (Self, error)` reads `status --json`. When `BackendState` isn't
    `"Running"`, the error is `Tailscale isn't running (<state>)`.
  - `func (c CLI) Served(ctx context.Context) (map[int]string, error)` reads `serve status --json`. It maps
    each HTTPS port in `TCP` to the `Proxy` of its `Web["<host>:<port>"].Handlers["/"]`, or "" when the
    port isn't a proxy.
  - `func (c CLI) Serve(ctx context.Context, port int, target string) error` runs
    `serve --bg --https=<port> <target>`.
  - `func (c CLI) Off(ctx context.Context, port int) error` runs `serve --https=<port> off`.
  - **Errors** include the CLI's output, trimmed. Output with "Access denied" or "operator" gets ` (run:
    sudo tailscale set --operator=$USER)` added.

- [ ] **Step 1: Write the failing tests** with
  `fakeTailscale(t, status, serve string, exit int) (bin, log string)`. It writes a `#!/bin/sh` script
  that appends `"$*"` to `log`, then prints `status` for `status --json` and `serve` for
  `serve status --json`, and exits with `exit`.
  - `TestSelfReadsNameAndOwner`: `"DNSName":"devbox.tail0000.ts.net."` with `User` `dana@acme.test` gives
    `Self{"devbox.tail0000.ts.net","dana@acme.test"}`.
  - `TestSelfTaggedHasNoOwner`: `"Tags":["tag:server"]` gives `Owner == ""`.
  - `TestSelfNotRunning`: `"BackendState":"Stopped"` gives an error containing `Tailscale isn't running`.
  - `TestServedReadsTargets`: the shape below gives `{443:"http://127.0.0.1:8800", 7391:"unix:/s/board.sock"}`.

    ```json
    {"TCP":{"443":{"HTTPS":true},"7391":{"HTTPS":true}},"Web":{"devbox.tail0000.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8800"}}},"devbox.tail0000.ts.net:7391":{"Handlers":{"/":{"Proxy":"unix:/s/board.sock"}}}}}
    ```
  - `TestServeAndOffArgs`: the log lines are `serve --bg --https=7391 unix:/s/board.sock` and
    `serve --https=7391 off`.
  - `TestServeAccessDenied`: a fake that prints `Access denied: serve config denied` and exits 1 gives an
    error containing `tailscale set --operator`.
  - `TestCallTimesOut`: a fake that runs `sleep 5`, with `Timeout: 200ms`, errors in under 2 s.
  - `TestNoCLI`: `Bin` pointing at a missing file gives `ErrNoCLI`.

- [ ] **Step 2: Run them and see them fail.**
  `GOTOOLCHAIN=go1.23.4 go test ./internal/tailnet/`, and the package doesn't exist yet.

- [ ] **Step 3: Implement.** Use `exec.CommandContext` with `WaitDelay`, and parse into small structs.

- [ ] **Step 4: Run all checks.** Expected: PASS.

- [ ] **Step 5: Commit** with the message `tailnet: read Tailscale's status and its serve entries, and add or remove one`.

---

### Task 7: The board through the tailnet

**Files:**
- Create: `internal/board/tailnet.go`: the door, its record, and syncing with Tailscale.
- Modify:
  - `internal/reconcile/reconcile.go`: `BoardTailnet bool` with `toml:"board_tailnet"`, and its default
    config comment if the defaults file lists settings;
  - `internal/board/server.go`: `Config.Tailscale`, `Run` closes the tailnet listeners, the `loginCode`
    reply;
  - `internal/board/hub.go`: `check` calls the sync;
  - `internal/board/model.go`: `Board.Tailnet`;
  - `internal/board/daemon.go`: `LoginURL` fills `Login.Tailnet`;
  - `cmd/herdr-tasks/board.go`: `serve` passes `tailnet.CLI{}`; `run` shows the tailnet address;
  - `cmd/herdr-tasks/main.go`: `remove` calls `board.Unshare`;
  - `internal/board/web/app.js`: the header shows the tailnet problem, or the address.
- Test: `internal/board/tailnet_test.go`, `internal/board/daemon_test.go`, `cmd/herdr-tasks/board_test.go`,
  `cmd/herdr-tasks/remove_test.go`

**Interfaces:**
- Consumes:
  - Task 5's `WrapTailnet`, `TailnetOwner` and `NewAuth(token)`;
  - Task 6's `tailnet.Self`, `tailnet.CLI` and `tailnet.ErrNoCLI`;
  - Task 2's `Login`.
- Produces:
  - `type Tailscale interface`, in `internal/board`:
    - `Self(ctx) (tailnet.Self, error)`;
    - `Served(ctx) (map[int]string, error)`;
    - `Serve(ctx, port int, target string) error`;
    - `Off(ctx, port int) error`.

    `Config.Tailscale Tailscale`, where nil means never.
  - **The socket:** `<stateDir>/board/sockets/board.sock`. The folder is 0700 and the socket 0600. A stale
    file is removed before listening.
  - **The record:** `<stateDir>/board/tailnet.json`.
    - `type tailnetRecord struct { Board *sharedEntry; Shares []sharedEntry }`, with json `board,omitempty`
      and `shares,omitempty`.
    - `type sharedEntry struct { Port int; Target string; At time.Time }`, with json `port`, `target` and
      `at`.
    - It's written 0600 with `writePrivate`.
  - `func (s *Server) syncTailnet(ctx context.Context, opts reconcile.Options)`, called by `check` each
    tick:
    - **When `opts.Board && opts.BoardTailnet`:**
      - listen on the socket, served by `WrapTailnet`;
      - read `Self` and `Served`;
      - if `Served[port]` is our `"unix:"+socket`, it's shared;
      - if it's something else, the problem is
        `Port <port> is already shared with your tailnet for something else.`, and nothing is changed;
      - if it's missing, `Serve` it and record it.
    - **When it's off and the record has a board entry:**
      - `Off` only if `Served[port]` still equals the recorded target;
      - drop the entry;
      - stop listening.
    - **After a failure,** the next try is a minute later, not every tick.
    - **With it off and nothing recorded,** there are no calls.
  - `Board.Tailnet *TailnetLine` (json `tailnet,omitempty`):
    - `type TailnetLine struct { Address, Problem string; Shares []SharedPort }`, with json `address`,
      `problem,omitempty` and `shares,omitempty`.
    - `Address` is `https://<DNSName>:<port>`, without `:443`.
    - `type SharedPort struct { Port int; Since time.Time }` is filled in Task 8.
  - The tailnet door's `origin()` is `Board.Tailnet.Address` while it's shared, and "" otherwise.
    `TailnetOwner` returns `Self.Owner`.
  - `POST /api/login-code` replies `{"code":…,"tailnet":"<Address>"}`, with `tailnet` only while it's
    shared.
  - `LoginURL` sets `Login.Tailnet` to `<tailnet>/<escaped session>/?code=<code>`, or "".
  - `func Unshare(stateDir string, ts Tailscale) error`:
    - for each recorded entry (the board and the shares), `Off` only if `Served` still shows its target;
    - then it clears the record.
    - `remove` calls it after `board.Stop`, with `tailnet.CLI{}`, only when the record exists.
  - `herdr-tasks board`:
    - it opens `Local` as now;
    - when opening fails, as with no display, it shows
      `Open this in your browser: <Tailnet or Local> (the code works for a minute)`;
    - `--print` prints `Local`, and `Tailnet` on a second line when it isn't "".

- [ ] **Step 1: Write the failing tests** with an in-memory `fakeTS` that implements `Tailscale` and logs
  its calls.
  - `TestTailnetSharesBoardWhenOn`:
    - with `Board` and `BoardTailnet` on, `Self` returning `devbox.tail0000.ts.net`/`dana@acme.test`, and
      `Served` empty, one tick calls `Serve(7391, "unix:<dir>/board/sockets/board.sock")`;
    - the record has the board entry;
    - `Board.Tailnet.Address == "https://devbox.tail0000.ts.net:7391"`.
  - `TestTailnetLeavesOthersEntries`:
    - with `Served = {7391:"http://127.0.0.1:9999"}` there's no `Serve` or `Off`;
    - `Board.Tailnet.Problem` is `Port 7391 is already shared with your tailnet for something else.`;
    - `origin()` is "".
  - `TestTailnetOffRemovesOnlyOwnEntry`:
    - with the board entry recorded and `Served` showing our target, turning it off calls `Off(7391)`;
    - when `Served` shows a different target, there's no `Off`, and the record is still cleared.
  - `TestNoTailscaleCallsWhenOff`: with `BoardTailnet` false and no record, three ticks make zero calls.
  - `TestTailnetDoorServesBoard`:
    - over the real Unix socket (an `http.Client` with a unix `DialContext`), `POST /api/session {}` with
      Origin `https://devbox.tail0000.ts.net:7391` and `Tailscale-User-Login: dana@acme.test` gets the
      key;
    - with that key, `GET /api/board?session=work` is 200.
  - `TestTailnetProblemShownAndBacksOff`. With `Serve` returning `access denied (run: sudo tailscale set
    --operator=$USER)`:
    - `Problem` contains `tailscale set --operator`;
    - ticks 10 s apart within a minute make no more `Serve` calls;
    - the 127.0.0.1 board still answers.
  - `TestStaleSocketReplaced`: with a regular file at the socket path, the door still listens, and
    `/api/health` answers over it.
  - `TestLoginURLHasTailnet`: a fake server replying `tailnet` gives
    `Login.Tailnet == "https://devbox.tail0000.ts.net:7391/work/?code=<code>"`.
  - `TestBoardShowsTailnetWhenNoBrowser`:
    - with `open` failing, the message names the tailnet address;
    - `--print` prints two lines.
  - `TestRemoveUnsharesOnlyOwnEntries`:
    - the record has the board 7391 and a share 4100;
    - the fake's `Served` shows 7391 with our target and 4100 with someone else's;
    - `Off(7391)` only, and then the record is gone.

- [ ] **Step 2: Run them and see them fail.** Run
  `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ ./cmd/herdr-tasks/ -run 'Tailnet|LoginURLHasTailnet|BoardShowsTailnet|RemoveUnshares|StaleSocket'`.
  Expected: FAIL.

- [ ] **Step 3: Implement.** Keep `Run`'s TCP listener as is. The tailnet listener is started and stopped by
  `syncTailnet`, and closed when the server shuts down. The server doesn't `Off` its entry on shutdown:
  the entry is kept so that the next server works again.

- [ ] **Step 4: Run all checks.** Expected: PASS.

- [ ] **Step 5: Commit** with the message `Board: share the boards with your tailnet, with board_tailnet`.

---

### Task 8: Sharing localhost ports with the tailnet

**Files:**
- Create: `internal/board/shares.go`
- Modify:
  - `internal/board/tailnet.go`: shares in the sync and the record;
  - `internal/board/server.go`: routes;
  - `internal/board/model.go`: `TailnetLine.Shares`;
  - `internal/board/web/app.js` and `internal/board/web/board.css`: the confirmation, and the list with
    Stop.
- Test: `internal/board/shares_test.go`, `internal/board/web_test.go`

**Interfaces:**
- Consumes: Task 7's `Tailscale`, the record, `Board.Tailnet` and `origin()`, and Task 5's door.
- Produces:
  - **`POST /api/share {session, tab, url}`:**
    - **Refused on the loopback door** with a 403: `Share ports from the board's tailnet address.`
    - **The link** must be on the task, `http://localhost:<port>` or `http://127.0.0.1:<port>` (https too),
      with a port in 1–65535 that isn't `board_port`. Otherwise it's a 400 with the reason.
    - **When we already share the port,** it replies `{"href": "<Address with :port><path>?<query>"}`.
    - **When `Served` has the port for anything else,** it's a 409:
      `Port <port> is already shared with your tailnet for something else.`
    - **Otherwise** it listens on `<stateDir>/board/sockets/share-<port>.sock` with the proxy,
      `Serve(port, "unix:"+sock)`, records it, and replies with the href.
  - **`POST /api/unshare {port}`** on either door: only for a recorded share whose target still matches.
    It calls `Off`, stops listening, drops the record, and replies 204. Anything else is a 404:
    `herdr-tasks isn't sharing port <port>.`
  - `func shareProxy(port int, address func() string) http.Handler`, an `httputil.ReverseProxy`:
    - `Rewrite` sets the URL to `http://127.0.0.1:<port>`, `Out.Host = "localhost:<port>"`, and, when an
      Origin was sent, `Origin: http://localhost:<port>`;
    - `ModifyResponse` rewrites a `Location` that starts with `http://localhost:<port>` or
      `http://127.0.0.1:<port>` to the tailnet address with `:<port>`;
    - `ErrorHandler` answers 502 `Nothing answers at localhost:<port>.`
  - When the server starts, every recorded share listens on its socket again.
  - Turning `board_tailnet` off stops every recorded share as in Task 7, matching targets only.
  - `TailnetLine.Shares` lists the recorded shares, by port.
  - Each share and stop is logged: `shared localhost:<port> with the tailnet` and `stopped sharing
    localhost:<port>`.
  - **The page, on a board opened over `https:`:**
    - A click on a `localhost` or `127.0.0.1` link opens the shared address when its port is in
      `tailnet.shares`.
    - Otherwise it shows an in-page confirmation, with no browser dialog: "Share localhost:4100 with your
      tailnet? Anyone in your tailnet can open it while it's shared." with **Share** and **Cancel**.
    - **Share** opens the window first (as in Task 4), posts, then sets its location.
    - The header lists the shared ports, each with **Stop**.
    - Over `http:`, `localhost` links open as now.

- [ ] **Step 1: Write the failing tests**
  - `TestShareOnlyThroughTailnet`: a loopback `POST /api/share` is a 403, with that message.
  - `TestShareServesAndRecords`:
    - a task link `http://localhost:4100/app?x=1` makes the fake log
      `Serve(4100, "unix:<dir>/board/sockets/share-4100.sock")`;
    - the reply's href is `https://devbox.tail0000.ts.net:4100/app?x=1`;
    - the record has 4100, and `Board.Tailnet.Shares[0].Port == 4100`.
  - `TestShareRefusesTakenPort`: with `Served = {8443:"http://127.0.0.1:8801"}`, a link to
    `localhost:8443` is a 409 with that message, and there's no `Serve` call.
  - `TestShareRefusesBadLinks`: these are refused with no `Serve`:
    - a port of 0 or 70000, and the board's port 7391 (400);
    - a link that isn't on the task (404);
    - `https://example.test/` (400).
  - `TestShareProxyRewritesHost`:
    - a backend `httptest.Server` on 127.0.0.1 records Host and Origin, and redirects `/old` to
      `http://localhost:<p>/next`;
    - a request over the share's socket, with `Host: localhost` and
      `Origin: https://devbox.tail0000.ts.net:<p>`, reaches it with Host `localhost:<p>` and Origin
      `http://localhost:<p>`;
    - `/old` answers `Location: https://devbox.tail0000.ts.net:<p>/next`.
  - `TestShareProxyWebSocket`:
    - the backend hijacks a `Connection: Upgrade` request, replies 101, and echoes one line;
    - through the proxy the upgrade succeeds, and the line comes back;
    - use stdlib only.
  - `TestShareProxyNothingListening`: when the port has no backend, the answer is 502 with that message.
  - `TestUnshareOnlyOwn`:
    - unsharing 4100 calls `Off(4100)` and stops the socket;
    - unsharing 8443, which isn't recorded, is a 404 with no `Off`;
    - unsharing 4100 when `Served` shows another target is a 404 with no `Off`, and drops the record.
  - `TestSharesComeBackAfterRestart`: with a record holding share 4100, a new server's first tick listens
    on `share-4100.sock` again, and a request through it reaches the backend.
  - `TestTailnetOffStopsShares`: turning `board_tailnet` off calls `Off` for the board and for 4100.
  - `TestPageHasShareConfirmation` (source check): `app.js` contains the confirmation text and
    `/api/share` and `/api/unshare`, and calls no `confirm(`.

- [ ] **Step 2: Run them and see them fail.** Run
  `GOTOOLCHAIN=go1.23.4 go test ./internal/board/ -run 'Share|Unshare|SharesCome|TailnetOffStops'`.
  Expected: FAIL.

- [ ] **Step 3: Implement.** One lock guards the record and the listeners. A share whose `Serve` fails
  closes its socket and isn't recorded.

- [ ] **Step 4: Check by eye.** Use a throwaway demo test (the Task 3 pattern) that serves the board over
  a socket, with a fake `Tailscale` and an `https`-looking page, in headless Chrome. Click a localhost
  link, confirm, and see the share listed with Stop. Delete the throwaway file afterwards.

- [ ] **Step 5: Run all checks and commit** with the message
  `Board: share a task's localhost port with your tailnet, through the board's server`.

---

### Task 9: README and version 0.11.0

**Files:**
- Modify: `README.md`, `cmd/herdr-tasks/main.go` (`const version`), `herdr-plugin.toml` (`version`), and
  `herdr-tasks help`'s `board` line in `main.go`.

**Interfaces:** none new.

- [ ] **Step 1: README**
  - "Task board" in "What it adds": a board per session, the list at `/`, and **Open task board** opening
    this session's board.
  - "Terminals on the board": the address is `/<session>/#tab=<tab>`.
  - A new "Remote herdr" section after "herdr --remote":
    - `board_tailnet = true` and the operator step;
    - the address `https://<machine>.<tailnet>.ts.net:7391/<session>/`, and signing in with your
      Tailscale login;
    - an SSH tunnel on any port, such as `ssh -N -L 8000:127.0.0.1:7391 you@server`, then opening
      `http://127.0.0.1:8000/<session>/` with a code from `herdr-tasks board --print`, with its port
      changed to 8000;
    - pages, and what a page can and can't do: it may load from the internet, and it can show its folder's
      files but not read them;
    - sharing a `localhost` port, Stop, and the dev server seeing `localhost`;
    - Slack on one machine only.
  - "Settings": `board_tailnet = false  # also share the boards with your tailnet (tailscale serve)`.
  - "How it works":
    - file links are served by the board at a sandboxed address that works until the server restarts;
    - the Unix sockets and `board/tailnet.json`;
    - the tailnet door trusts `Tailscale-User-Login`, and only there;
    - `herdr-tasks remove` takes out only the `tailscale serve` entries it made.
  - Remove the sentence about the server running `open` or `xdg-open` for file links.
  - An update note for 0.11.0: boards per session, the new `slack_session` default, `board_tailnet`.

- [ ] **Step 2: The version.** It becomes 0.11.0 in both files. `make release`'s check compares them.

- [ ] **Step 3: All checks:** vet, `test -race`, the four cross-builds, and the node keymap test.
  Expected: all pass.

- [ ] **Step 4: Commit** with the message
  `README: boards per session, remote herdr through the tailnet, and the Slack session default (0.11.0)`.
