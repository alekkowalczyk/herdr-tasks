# Session boards and remote herdr

Date: 2026-11-01. Builds on the web board and Slack asks (`2026-10-30-web-board-design.md`,
`2026-10-31-slack-asks-design.md`), which are on branch `web-board-and-slack-asks` (PR #1).

## What the user asked for

In the user's words, and from the conversation:

- **Slack in one place.** Slack asks are active in a single session on a single machine. By default that's
  herdr's default session, and a setting can name another one.
- **A board per session.** Each herdr session has its own board, even on the same machine.
- **Remote herdr.** When herdr runs on a server and the user attaches with `herdr --remote` over SSH, the
  board and the pages linked on tasks (HTML visualizations and the like) are hosted on the server and can be
  opened on the laptop "through some server or something".

The user's setup, as observed: a macOS laptop running several named herdr sessions, and a Linux server with no
display in the same tailnet. Both machines are registered to the user's own Tailscale login, not tagged. The
server already shares two services with `tailscale serve`, tailnet only. The user attaches to one of its
sessions with `herdr --remote ssh://… --session NAME --remote-keybindings server`.

The user agreed to these proposals:

- the server shares its board with the tailnet through `tailscale serve`;
- a browser signed in to Tailscale as the user is signed in to the board without a code;
- an SSH tunnel works on any local port, for people without Tailscale;
- the board serves the task's linked pages itself, each in a sandbox;
- a `localhost:` link on the remote board offers, on a click, to share that port with the tailnet.

The rest is marked **(my call)**.

**Why (my guess):** each session is a separate project, and mixing them on one page is noise. On a server the
laptop is only a screen, and today the board and the pages there can't be reached without a hand-made tunnel.

## Goals

1. Each running herdr session has its own board, with only its own tasks.
2. Slack asks go to herdr's default session unless `slack_session` names another.
3. With `board_tailnet = true`, a server's boards open from the laptop, tablet or phone at
   `https://<machine>.<tailnet>.ts.net:<board_port>/…`. The user's own Tailscale login needs no code.
4. **Open** on a task's local `.html` page shows it in the browser that's looking at the board, local or
   remote.
5. On a board opened through the tailnet, a `localhost:<port>` link can be shared with the tailnet on a click,
   and then opens from the laptop.
6. An SSH tunnel to the board works on any local port.

## Non-goals

- One board for several sessions or several machines.
- Slack asks in more than one session, or routing asks between sessions.
- Noticing that Slack is on in two machines. The README says to choose one.
- Moving tasks between sessions (herdr can't).
- Reaching the board from the internet (Funnel), through other VPNs, or over HTTPS without Tailscale.
- Links clicked in the strip or the overview, which are terminals: unchanged.
- Installing or updating herdr-tasks on the user's server. That's the user's step.
- `board_tailnet` on macOS beyond best effort. If `tailscale serve` fails there, the board says why.

## Anti-goals (a failure even if it works)

- The board answers anyone outside the tailnet, or another tailnet user without a login code.
- Tailscale's identity headers are trusted on a connection that didn't come through tailscaled.
- A page the board serves can use the board's API or read its browser key.
- A served page reaches a file outside its folder, a hidden file, or a file through a symlink that leads out.
- A localhost port gets shared without the user's click and confirmation, or with Funnel.
- herdr-tasks changes or removes a `tailscale serve` entry it didn't make.
- The local board on 127.0.0.1 stops working for people without Tailscale.

## How it works

### A board per session

- There's still one server per machine (`herdr-tasks serve` on `board_port`). Each running session gets its
  own board at its own address on that server, such as `http://127.0.0.1:7391/work/`. The board's own paths
  (API, static files, served pages) must not clash with any session name; the builder picks the layout.
- A board shows only its own session: no session switcher. The space filter, the stale marker, Linear chips,
  terminals and moves work as now, inside that session.
- The bare address `/` lists the machine's running sessions, each linking to its board, and says which one
  gets Slack asks. With exactly one session running, `/` goes straight to its board **(my call)**.
- `herdr-tasks board` and **Open task board** open the board of the session they run in. Outside herdr,
  `herdr-tasks board` opens the list, and `--session NAME` opens one board **(my call)**.
- A tab's view keeps its own address, so a reload keeps it. Old addresses such as
  `#tab=<session>/<tab>` that a bookmark may still hold open the right board.
- One sign-in covers every board on the machine, as now: the browser key is per server run, not per session.
- What the page remembers (space filter, open sections) is kept per session.

### The Slack session

- With `slack_session` unset, asks go to herdr's default session, the one plain `herdr` starts and
  `herdr session list` calls `default`. Today, unset means "the only running session".
- While that session isn't running, the watcher waits, as now. `herdr-tasks slack status` says "waiting for
  herdr session default". Every session's tab bar shows `Slack ⚠`, and every other session's board shows one
  line saying where asks go and that the session isn't running **(my call: otherwise nobody would notice)**.
- When the Slack session runs, only its board has the Slack line, Fetch and the skipped list. Other boards
  show nothing about Slack.
- `herdr-tasks slack fetch` and **Fetch Slack asks** still work from any session on that machine and deliver to
  the Slack session.
- Upgrade note: someone who relied on "the only running session" with a named session sets `slack_session`.
  The README's update note and the waiting message say so.

### Remote: the board through the tailnet

New setting `board_tailnet` (default `false`). When it's on and the `tailscale` CLI works:

- **A second door.** The server also listens on a Unix socket in its state folder: the folder is 0700, and
  the socket is 0600. It shares that socket with
  `tailscale serve --bg --https=<board_port> unix:<socket>`. That's tailnet only, never Funnel. The board's
  address is `https://<DNSName>:<board_port>/`, with `DNSName` from `tailscale status --json`.
- **Only its own entries.** herdr-tasks records the entry it made. Turning `board_tailnet` off, or
  `herdr-tasks remove`, takes the entry out with `tailscale serve --https=<port> off`, but only while it
  still points at herdr-tasks' socket. If the port is already served by something else, herdr-tasks leaves it
  alone and says why: in the board's header, in `herdr-tasks board`, and in board.log.
- **Requests through the socket.** `tailscale serve` sends `Host: localhost` to a `unix:` target, as
  Tailscale's source has it (`ipn/ipnlocal/serve.go`, checked 2026-11-01). So the socket door doesn't check the
  Host. On that door, DNS rebinding is stopped by Tailscale's TLS certificate, which only fits the machine's
  tailnet name. The Origin must be `https://<DNSName>` with `:<board_port>` unless that's 443. All other rules
  stay: CSP, no framing, the Sec-Fetch-Site check, the key.
- **Signing in by Tailscale login.** tailscaled first deletes any `Tailscale-User-*` headers the client sent,
  then sets `Tailscale-User-Login` on what it proxies. When that
  matches the login that owns this machine in the tailnet (from `tailscale status --json`), the page's
  sign-in call gets the browser key without a code. Anyone else gets the one-time code flow: another tailnet
  user, a tagged device, or a request without the header. On a tagged machine nobody is signed in
  automatically.
- **The 127.0.0.1 door ignores** every `Tailscale-*` header.
- **`herdr-tasks board` on the server.** With `board_tailnet` on, it shows the tailnet address of the session's
  board. On a machine without a display it can't open a browser, so the address goes to the overview's
  message line, as now. The owner needs no code; for anyone else the address carries one.
- **Requirements, which the board reports when they're missing:**
  - `tailscale serve` with `unix:` targets; the server observed runs 1.104.2.
  - The user is Tailscale's operator (`tailscale set --operator=$USER`), or else `serve` fails.
- The browser keeps a separate key for the tailnet address, since it's a different origin.

### Remote without Tailscale: an SSH tunnel on any port

- The 127.0.0.1 door accepts `127.0.0.1:<any port>` and `localhost:<any port>` as the Host. That still stops
  DNS rebinding. The Origin must be `http://` with the request's own host and port.
- So `ssh -L 8000:127.0.0.1:7391 server` works, without clashing with the laptop's own board on 7391.
- `herdr-tasks board --print` prints the sign-in address. Through a tunnel the user changes the port in it.
  The README shows how.

### Pages, served by the board

- **Open** on a task's local `.html` page loads it through the board, in the browser looking at the board. It
  no longer asks the server to run `open` or `xdg-open`. That holds for local boards too. **Copy path** stays.
- The address is a capability URL that a signed-in board gets from the API, such as `/page/<id>/name.html`.
  `<id>` is random per server run and per page folder. The id is the credential, since a navigation
  carries no key. It works until the server stops.
- **What gets served:**
  - the page, and files in its folder and subfolders (images, scripts, styles, data, fonts);
  - only files that, with symlinks followed, are still inside that folder;
  - no hidden file or folder (a name starting with `.`), nothing outside, and no folder listings.
  - When the page sits directly in the home folder or in `/`, only the page itself is served **(my call)**.
- **The sandbox.** Every served response gets `Content-Security-Policy: sandbox …` without
  `allow-same-origin`, so the page runs in an opaque origin. It can't read the board's storage or use its
  key.
- **The internet, and the files next to the page (my call): a page works on the board when it works opened
  as a file.**
  - A page may load scripts, styles, fonts and images from the internet, as Claude's pages do from CDNs.
  - The files in its folder can be shown (`<img>`, `<script src>`, `<link rel=stylesheet>`, a link), but the
    page can't read their contents. They're served without `Access-Control-Allow-Origin`, so a `fetch()`
    of them, a module script, or a canvas read fails, as it does for a `file://` page in Chrome today. A page
    that's been turned against the user can't send its neighbours anywhere.
  - Fonts are the exception: they get `Access-Control-Allow-Origin: *`, since `@font-face` needs it and fonts
    hold no secrets.
- **Never served (my call, with the research guard's list):** names that look like secrets, such as
  `.env*`, `*.pem`, `*.key` and `id_rsa*`, even when they're in the page's folder.
- The id is issued only while the link is on a task in a running session. Each request checks again that the
  file exists and is a regular file.
- Artifact links and hosted pages are unchanged.

### `localhost` links on a board opened through the tailnet

- This applies only through the tailnet. On 127.0.0.1, locally or through a tunnel, `localhost` links open as
  they do now.
- **Opening a link.** A click on a `localhost:<port>` or `127.0.0.1:<port>` link:
  - If herdr-tasks already shares that port, it opens `https://<DNSName>:<port><path>?<query>`.
  - If not, the page asks, in the page with no browser dialog: "Share localhost:4100 with your tailnet?
    Anyone in your tailnet can open it while it's shared." [Share] [Cancel].
  - **Share** records the share, makes the board's server listen on a socket for that port, runs
    `tailscale serve --bg --https=<port> unix:<socket>`, then opens the link.
- **Through the board's server, not straight to the port (my call).** `tailscale serve` passes the browser's
  Host (`<machine>.ts.net:<port>`) to an HTTP target unchanged. Current Vite and webpack-dev-server refuse such
  a Host, with "Blocked request" or "Invalid Host header".
  - So the board's server proxies each share itself, from the socket to `127.0.0.1:<port>`. Its requests,
    WebSockets included, carry `Host: localhost:<port>`, and an `Origin` of `http://localhost:<port>` when
    the browser sent one. The dev server sees a local request.
  - A redirect to `localhost:<port>` is rewritten to the tailnet address.
  - A share works while the board's server runs, which is whenever a herdr session does. When the server
    starts, it listens again for the shares it has recorded.
- **Refusals,** each with the reason: a port that `tailscale serve` already serves for anything else, the
  board's own port, and a port outside 1–65535.
- **Stopping.** The board's header lists the ports herdr-tasks shares, each with **Stop**. Turning
  `board_tailnet` off, and `herdr-tasks remove`, stop all of them, but only the entries herdr-tasks made.
- The `--bg` entries persist in Tailscale. The list of shares comes from herdr-tasks' record, checked against
  `tailscale serve status --json`.
- Every share and stop is logged in board.log.

### Settings

```toml
board_tailnet = false   # also share the boards with your tailnet, with tailscale serve
slack_session = ""      # the herdr session for Slack tabs; "" is herdr's default session
```

## Constraints

- Go 1.23 (`GOTOOLCHAIN=go1.23.4`), `CGO_ENABLED=0`, and no new modules. herdr-tasks talks to Tailscale
  through the `tailscale` CLI (`status --json`, `serve`, `serve status --json`), not its Go client.
- Tests use a fake `tailscale` on `PATH`, as they use a fake `claude` and a fake `herdr`. Nothing calls the
  real `tailscale`, touches the real HOME, or runs `setup`, `serve` or `board` against it.
- Test data is invented, and the personal-data grep stays empty. The real terms (session, machine and
  tailnet names, logins) are kept outside the repo, by whoever runs the build, and never written into it.
- The board's existing protections stay: CSP, no framing, the Origin and Sec-Fetch-Site checks, the token,
  the browser key.
- The README covers:
  - boards per session;
  - the new `slack_session` default, with an upgrade note;
  - a "Remote herdr" section: the tailnet, the SSH tunnel, pages, sharing localhost ports;
  - the settings.
- Version 0.11.0.

## Left to the builder

- The address layout for the boards, the list page, the API and served pages.
- How the API and the events stream are scoped to one session.
- How `herdr-tasks board` and the action find the session they run in.
- The exact sandbox flags, and size limits for served files.
- The look of the share confirmation and of the list of shared ports.
- Wording of messages and errors.

## Checks after the build (on the user's machines)

Tests run against a fake `tailscale`, so these are only proved on the real one.

- The server, with `board_tailnet` on, opened from the laptop:
  - sign-in without a code, which also confirms what Host and identity headers really arrive;
  - typing in a terminal;
  - opening a page with its images and a CDN script;
  - sharing a Vite dev server's port, with hot reload, then stopping it.
- The laptop with two sessions running: two boards, each showing only its own session.
