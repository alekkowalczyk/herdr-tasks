# Web board for herdr-tasks: design

Date: 2026-10-30. Status: draft for review.

## Why

herdr-tasks gives herdr tabs a task stage, a note, and links, and shows them in
herdr's sidebar, a popup overview, and a strip at the bottom of each tab. That
works inside the terminal, one space at a time. With a few dozen tasks across
many spaces, each with one or more Claude agents, there is no single place to
see every task at once together with what its agents are doing, and the Linear
issues behind the work aren't linked unless someone adds them by hand.

The user wants:

- a modern-looking kanban board in the browser with every herdr task on it;
- on each card, what the agents in that tab are doing, including the recap
  Claude writes;
- clicking a card shows the tab's actual terminals, and you can type into them
  from the browser;
- when Claude works on a Linear issue inside a herdr tab, the issue is linked to
  that tab's task automatically, and the board shows each linked issue's
  current Linear status.

## The three parts, in shipping order

The work splits into three parts that each ship on their own:

1. **Linear links.** A Claude Code hook that links Linear issues Claude works on
   to the task. Useful in the terminal right away (the strip and overview
   already list links).
2. **The board.** A web server in the plugin, the board page, card summaries,
   and Linear status chips.
3. **Terminals in the browser.** Clicking a card shows the tab's panes live and
   lets you type into them.

Each part should get its own implementation plan and its own merge.

## Goals

- One board with every task from every running herdr session on this machine.
- Cards that answer "what is this, what is the agent doing, does it need me"
  without opening the tab.
- Linear issues Claude touches end up on the task without anyone doing it.
- Live Linear status on the board.
- Read and type into a tab's terminals from the browser.

## Non-goals (for now)

- Access from a phone or another computer. It's local only for now. Later it
  can go through Tailscale or an SSH tunnel, but nothing in v1 is built for
  that.
- An AI-written recap. Cards use what Claude already writes to its session log.
- Showing Linear status in the terminal strip, overview, or sidebar.
- Suggesting a stage change when all linked Linear issues are done.
- Creating, closing, or renaming tabs or spaces from the board.
- Scrolling back through a terminal's history in the browser (v1 shows the
  visible screen only).
- Linking Linear issues from agents other than Claude Code, or from a Linear URL
  Claude only mentions without calling a Linear tool.
- Fixing the separate data-loss risk (tasks dropped when a tab vanishes). That
  is its own piece of work.

## Anti-goals (would make it a failure even if it works)

- Anything else on the machine or network can type into the terminals. The
  board must be unreachable without the secret token and must refuse requests
  from other web pages.
- Linking every issue Claude reads during a search. Cards fill up with noise.
- Slowing herdr down. Polling and reading must stay light, and a stuck or
  crashed board server must not affect herdr or the existing plugin features.
- A Node or npm build step. The plugin builds with `go build` (or downloads a
  release binary) and that must stay true.
- Breaking or replacing the terminal UI. The overview, strip, and sidebar stay
  as they are; the board is an extra way in. (Part 4 changes the strip's link
  column, at the user's request.)

## Constraints

- Go, in this repo. The server is the existing `herdr-tasks` binary
  (a new `serve` command). Web assets are embedded with `go:embed`, including
  any vendored JS library (xterm.js for part 3). No CDN at runtime.
- Keep new Go dependencies minimal. A WebSocket library is acceptable if the
  builder wants WebSockets; server-sent events plus POST requests is also fine.
- herdr 0.8.2 socket API (`internal/herdr/client.go`). One request per
  connection for calls; `events.subscribe` keeps the connection open and
  streams events as JSON lines (confirmed live).
- Task state stays where it is (`store` package, one JSON file per herdr
  session under the plugin state dir, written under a file lock). The board
  reads and writes through `store.Update` and `reconcile.Run`, the same way the
  CLI and strip do, so the sidebar, tab markers, and strip update as usual.
- The plugin supports macOS and Linux (`platforms` in `herdr-plugin.toml`).
- Branch rules: never commit to `main`; work on feature branches.

## Part 1: Linear links from Claude

### What the user decided

- Link issues Claude **creates, updates, comments on, or opens individually**.
  Don't link every hit from a search or list.
- Do it with a Claude Code hook, so it doesn't depend on Claude remembering an
  instruction.
- Only inside herdr. Claude sessions outside herdr are untouched.

### How it works

- A `PostToolUse` hook in Claude Code runs a new command,
  `herdr-tasks claude-hook`, which reads the hook's JSON from stdin.
- It acts only when `HERDR_PANE_ID` is set. Otherwise it exits 0 silently.
- It acts on these Linear tools, from either Linear MCP server the user has
  (both are in use in their session logs): `mcp__linear__*` and
  `mcp__claude_ai_Linear__*`. Tool names that count:
  - `get_issue`, `save_issue` (create or update), `save_comment`,
    `create_attachment`.
  - Not `list_issues`, `list_comments`, `list_*`, or search tools.
- The issue URL comes from the tool result where there is one (`get_issue` and
  `save_issue` results include a `url`). For `save_comment` and
  `create_attachment` the input has the issue ID (for example `ABC-12`) but
  the result has no issue URL. Build the URL from the ID and the Linear
  workspace's URL key. The builder decides how to get the URL key: from a URL
  already seen, from the Linear API, or from a setting.
- It adds the link to the task, attributed to `claude`. The task is found from
  `HERDR_PANE_ID`. It writes only the state file and makes no herdr calls
  beyond finding the tab. A link changes nothing herdr shows, and a full sync
  could rearrange panes under Claude. (Revised after plan review; it was "the
  existing `link add` logic".)
- **Dedupe by issue ID, not URL.** Linear URLs for the same issue differ
  (`/issue/ABC-12` vs `/issue/ABC-12/some-slug`). `task.AddLink` dedupes on the
  exact URL today, so Linear links need ID-aware matching.
- The hook must be fast, print nothing to stdout, and always exit 0, so it never
  disturbs Claude. Errors go to the plugin's log, not to Claude.
- **Removing a link sticks (my call).** `get_issue` is Claude's most common
  Linear call, and Claude often uses it to read parent or related issues, so
  some auto-links will be noise. When you remove a Linear link from a task,
  the task remembers the issue ID as dismissed and the hook never adds it back.
  This applies however the link is removed: `link rm`, `L` in the overview or
  strip, or the board. Adding the link yourself clears the dismissal. An agent
  adding it (including Claude running `link add`) doesn't.

### Installing the hook

The hook is opt-in (user decided): off for anyone who installs the plugin,
turned on with `linear_hook = true` (renamed `claude_links` in part 4) in the plugin's settings file
(`~/.config/herdr/plugins/config/herdr-tasks/config.toml`, next to `markers`,
`sort_tabs`, and `strip`).

When it's on, `herdr-tasks setup` adds the hook to `~/.claude/settings.json`
(user-level Claude Code settings). The entry is marked so setup can take it out
again when the setting is turned off, and `herdr-tasks remove` takes it out too
(my call on the location). Setup must preserve everything else in that file and
must not add the hook twice. The builder may choose another mechanism (for
example, shipping a Claude Code plugin) if it installs and uninstalls just as
cleanly.

## Part 2: The board

### Starting and opening it (user decided: starts with herdr, a herdr action opens it, opt-in)

- The board is opt-in: off for anyone who installs the plugin, turned on with
  `board = true` in the plugin's settings file. With it off, nothing listens on
  any port, the startup hook starts nothing, and the action says how to turn
  it on.
- `herdr-tasks serve` runs the web server. One server per machine, serving
  every running herdr session: each one whose socket takes a connection, busy
  or not, all checked at once. A session that drops out of that list stays,
  marked stale, for a few checks, unless its socket is gone.
- The plugin's startup hook starts the server if it isn't running, detached so
  the hook returns at once. It must not start a second copy.
- A new herdr action, "Open task board", ensures the server is running and opens
  the board in the default browser (`open` on macOS, `xdg-open` on Linux).
- If the port is taken by something else, the server says so in its log and the
  action shows the error. It doesn't pick another port silently.
- The server keeps running while any herdr session answers. If none answers for
  a while it exits. The builder picks the timeout.
- A `herdr-tasks board` CLI command prints the URL (and opens it), for use
  outside herdr.

### Access and security (user decided: this Mac only, secret token)

- Listen on `127.0.0.1` only, on a fixed default port that can be changed in
  config. The builder picks the port.
- A random secret token, created once, stored with mode 0600 in the plugin
  state dir. The CLI uses it. The browser gets a key derived from it.
- Signing in:
  1. The action asks the server for a one-time code (single use, valid for a
     minute) and opens `http://127.0.0.1:<port>/?code=…`.
  2. The page swaps the code for the browser key.
  3. The page keeps the key in its own storage and sends it as an
     `Authorization` header on every API call and on the live connection.
- Revised after plan review: cookies were dropped. Cookies on `127.0.0.1` are
  shared with every other port, so another local service could read them, and
  a header can't be forged by another web page.
- Every API request and the live connection require the key or the token.
- Refuse requests whose `Host` isn't `127.0.0.1:<port>` or `localhost:<port>`
  (blocks DNS rebinding). Refuse any request whose `Origin` is present and
  isn't the board's own, and any marked cross-site.
- Agent output, notes, and titles are shown as text, never as HTML. Only
  http(s) links are clickable. A strict Content-Security-Policy backs this up.
- `herdr-tasks board --new-token` replaces the token. Old keys and links stop
  working.
- Revised in review: the browser key also belongs to one run of the server.
  Restarting the server, which includes its exit when no herdr session has run
  for a while and an upgrade, signs browsers out, and the page asks you to sign
  in again. A page left open keeps sending its key to the port, also to a
  program that takes the port while no server runs; that key is then no use to
  the next server.

### Columns

From left to right, matching the overview's groups:

| Column | What's in it |
|---|---|
| Asking | Active tasks whose agent is waiting on a question or permission |
| Your turn | Active tasks whose agent finished, or with nothing running |
| Working | Active tasks whose agent is working |
| In review | Stage "In review" |
| Paused | Stage "Paused" |
| Parked | Stage "Parked" |
| Done | Stage "Done", collapsed by default |

- Tool tabs aren't on the board. They aren't tasks.
- Within a column, priority tasks come first, then the order the overview uses.
- If several herdr sessions are running, a session switcher at the top shows
  one session's board at a time (my call).
- Each card shows its space name. A space filter at the top narrows the board.
  Spaces hidden in the overview are hidden here too (my call).

### Moving cards (user decided: dragging changes the stage)

- Dropping a card on Asking, Your turn, or Working sets the stage to Active. The
  card then shows in whichever of the three its agent's state dictates.
- Dropping on In review or Paused asks for an optional reason, like the strip
  does.
- Dropping on Parked or Done sets that stage. Moving to Done clears priority, as
  `SetStage` already does.
- Each card also has controls for priority on/off and "Show in herdr", which
  focuses the tab in herdr (my call; `tab.focus` already exists in the client).
- The detail view can remove a link, which then counts as dismissed (see part 1).
  Editing notes from the board is not in v1.
- Writes go through the same code path as the CLI (`reconcile.Run` with a change
  function), so tab markers, the sidebar, and strips update.

### What's on a card

Always:

- The task title, a red "!" if it's a priority, and its space.
- How long it has been in its stage, and the pause or review reason if any.
- Agent state, styled like the sidebar: "?" asking (red), "↻" working (blue).
- Links as chips. Linear links show their issue ID and live status (below).
- The note's first line.

For each agent pane in the tab (user decided: from Claude's own session log, no
AI-written recap):

- **Title.** What Claude is doing in a few words. Claude writes this both as the
  terminal title (`terminal_title_stripped` in `pane.list`) and as `ai-title`
  in its session log.
- **Recap.** The latest `away_summary` entry in the Claude session log. This is
  the recap Claude shows when you come back to a session ("You're moving X…
  Next: …"). Show when it was written. If the session has moved on since then,
  say so: there is a newer prompt than the recap.
- **Last prompt.** The latest `last-prompt` entry.
- The detail view (part 3) also shows the start of Claude's last reply.

Agents other than Claude (codex, a plain shell) show only the title and status.
A tab with several agent panes shows each one compactly. The builder chooses
the layout.

The todo list is left out. It was in the earlier summary, but the user's
session logs almost never contain one (1 session out of hundreds uses
TaskCreate/TaskUpdate, none use TodoWrite). This is my call; easy to add later.

### Where card data comes from

- Tasks: the store's state file for the session.
- Tabs, panes, agent state, terminal titles: `tab.list`, `pane.list`, and
  `workspace.list` over the herdr socket.
- Claude session: each Claude pane in `pane.list` has
  `agent_session: {agent: "claude", kind: "id", value: "<session id>"}`. The log
  is `~/.claude/projects/<project dir>/<session id>.jsonl`. Find it by globbing
  `~/.claude/projects/*/<session id>.jsonl` rather than recreating Claude's
  folder naming. Respect `CLAUDE_CONFIG_DIR` if set.
- Logs can be hundreds of MB (the largest the user has is about 375 MB). Read
  from the end and stop once the latest `ai-title`, `last-prompt`, and
  `away_summary` are found, with a cap on how far back to read. Cache by file
  size and mtime.
- `away_summary` entries have a timestamp but `last-prompt` entries don't. Judge
  "there's a newer prompt than the recap" by which comes later in the file.

### Keeping the board live

- Per session, the server holds an `events.subscribe` connection for tab,
  workspace, `pane.agent_status_changed`, `pane.updated`, and pane
  created/closed/moved events, and reconnects if it drops.
- It also notices changes to the state file (written by rename) and to the
  watched Claude logs.
- The browser gets updates over one live connection (WebSocket or server-sent
  events, builder's choice) and redraws only what changed. No full-page reloads.

### Linear status

- The server asks Linear's GraphQL API (`https://api.linear.app/graphql`) for
  each linked issue: identifier, title, status name, status color, status type,
  and assignee.
- A chip shows the issue ID in the status's own Linear color, with the status
  name. Hovering or the detail view shows the title and assignee.
- Refresh on a timer, about every minute, only for issues linked to tasks that
  aren't Done, and batch the requests. Linear's API-key rate limit is far above
  that.
- The API key is a personal Linear API key, read-only is enough. On macOS it is
  kept in the Keychain. On Linux it goes in a 0600 file in the plugin state dir.
  A CLI command sets it (`herdr-tasks linear-key`). It never goes in the repo or
  in herdr's config. The Claude Linear connector can't be reused, because it
  signs in through claude.ai.
- Without a key, chips show just the issue ID, and the board shows a one-line
  hint with the command to add one.

### Look

"Modern, nice looking." The builder chooses the visual design within these
lines. It should look polished, not like a default HTML table. It should be
legible at a glance, with dark and light themes following the system. Card
colors should echo the sidebar: red asking or priority, orange your turn, blue
working.

## Part 3: Terminals in the browser

### What the user decided

Clicking a card opens a detail view that shows **only that tab's panes**,
live, and you can type into them. This is option 1 from the earlier
discussion. It is not a full herdr client in the browser.

### How it works

- The detail view lays out the tab's panes as herdr does, using `pane.layout`
  rectangles. Each pane gets its own terminal view (xterm.js, vendored),
  sized to the pane's columns and rows in herdr.
- The plugin's own strip pane (labeled "Task strip") is left out, since the card
  already shows that information (my call).
- **Output.** herdr has no "output changed" event to subscribe to. `pane.read`
  works (`source: "visible"`, `format: "ansi"`, `strip_ansi: false`), so the
  server polls only the panes someone is viewing, several times a second. It
  sends a pane only when its content changed. Polling stops when no browser is
  viewing that tab.
- **Input.** Click a pane to type into it. Printable text goes through
  `pane.send_text`. Special keys and combinations (Enter, Esc, arrows, Tab,
  Backspace, ctrl+c, etc.) go through `pane.send_keys` with herdr's key names
  (`esc`, `enter`, `ctrl+c`, …). Pasting sends the text as one piece.
- The detail panel next to the terminals shows the full card: note, log
  entries, all links with Linear status, the recap, last prompt, and the start
  of the last reply. It also has stage, priority, and "Show in herdr"
  controls.

### Known limits of this approach (accepted)

- The browser can't make a pane bigger than it is in herdr. It shows the pane at
  herdr's size, scaled to fit.
- Output is snapshots several times a second, not a stream. Fast scrolling can
  look choppy.
- `pane.read` returns no cursor position, so the browser shows no exact cursor.
  If herdr exposes it somewhere the builder finds, use it.
- Mouse clicks inside terminal apps aren't forwarded in v1.

## Part 4: Slack links and pages from Claude

Added 2026-10-31, after parts 1–3 were planned. Revised after a plan review by Fable and Codex.

### What the user decided

- Besides Linear issues, a task gets:
  - the **Slack links** posted into its Claude sessions;
  - links to the **HTML artifacts, visualizations, explainers and showcases**
    created and used during those sessions, whether they are on the web,
    on localhost, or files on disk.
- The board shows these links, like the Linear links.
- The strip in each tab shows the links **grouped by kind**: Linear, Slack,
  and pages, each kind clearly distinct.

### What counts

All of it comes from Claude Code hooks, in Claude sessions running in a herdr
pane, while the setting is on.

**Slack**
- Slack links you paste into a prompt: message, thread, and channel links on
  `*.slack.com/archives/…`, including Enterprise Grid hosts such as
  `acme.enterprise.slack.com`.
- Messages Claude posts with a Slack MCP `slack_send_message` call, from the
  result's `message_link`.
- Not counted:
  - Slack searches, reads, or drafts.
  - Slack links in Claude's replies. Slack digest and summary skills reply
    with lists of permalinks, which would flood the task.

**Linear (an addition to part 1, my call)**
- A Linear issue URL you paste into a prompt is linked too, as if Claude had
  opened the issue. Linear URLs in replies and in tool results still don't
  count.

**Pages** (HTML artifacts, visualizations, explainers, showcases)

Two kinds of evidence count:

- **Explicit use.** The page is named in your prompt or Claude's reply, or
  Claude opens it in a browser (Chrome or Playwright `navigate` to a
  `file://` or localhost URL). That covers:
  - localhost URLs;
  - claude.ai artifact URLs;
  - pages on static hosts: `*.github.io`, `*.gitlab.io`, `*.vercel.app`,
    `*.netlify.app`, `*.pages.dev`, `*.web.app`, `*.firebaseapp.com` and
    `*.surge.sh` (my call: other web links stay out);
  - paths to `.html` files. A path counts only when it looks like a path
    (absolute, or starting `~/`, `./` or `../`, or containing `/`), the file
    exists, and the file is a full page (`<!doctype html` or `<html`).

  My call: localhost links you paste yourself count, as pages "used" in the
  session.
- **Implicit creation.** Claude writes or edits an `.html` file, and it is a
  full page. The bar is higher here, because ordinary coding also touches
  HTML. It must be:
  - the main session, not a subagent;
  - not a template, which means:
    - no `template`/`.tpl.` in the name;
    - no template syntax such as `{%`, `{{`, `<%` or `<?php`;
  - not in a source, build, or dependency folder (`src`, `templates`,
    `static`, `node_modules`, `dist`, `build`, `public`, and so on).
- **Artifacts Claude publishes** with the Artifact tool. The link is the
  claude.ai URL, labeled with the page's `<title>`. If it was published from
  a local file, the task shows the artifact instead of the file.
- Not counted: other web links (GitHub, Notion and the like).

**Rules**
- **One link per page.** The same page under another URL form is one link.
  For example:
  - a Slack message with or without `?thread_ts`;
  - an artifact with or without its share key;
  - localhost with or without a trailing `/`.

  Different hash routes on localhost (`#/a`, `#/b`) are different pages.
- **Removing any link sticks** (my call; this extends part 1's rule from
  Linear issues to every link). Claude doesn't add it back, and an agent
  running `link add` is told why. Removing a published artifact also keeps
  its source file off, and the other way round. Adding the link yourself
  brings it back.
- **Secrets stay out.** A localhost link is never saved when it has a user
  name or password, or a token-like parameter in its query or fragment
  (`token`, `code`, `key`, `secret`, `password`, `auth`, `sig`, `session`,
  `jwt`), or when it's an OAuth callback. A Linear issue link is saved without
  a user name, password, query or fragment, and an artifact link keeps only
  its share key.
- **Limits against noise.**
  - Each hook call adds at most 20 links.
  - Claude stops adding new links to a task once it has 60. Updating or
    replacing an existing link still works at 60.
  - Each hook call checks at most 200 files, with a time budget, so a pasted
    listing can't run out the hook's 5 seconds.
- **Labels and URLs are untrusted.** Control characters are refused in URLs
  and stripped from labels when they're stored. A refused URL is never
  reported as added: `link add` fails with an error, and the strip and the
  overview say it isn't a valid link (added in review). The strip checks both again
  before it prints anything, so neither can drive the terminal.
- **So is the rest of what agents write** (added in review): notes, log
  entries and their authors, task names and stage reasons. The CLI strips
  control characters from them when it stores them (a note keeps its line
  breaks), and the strip, the overview, `show`, `list` and the tab bar status
  strip them again before printing, which covers older and hand-edited state.

### How it works

- **The same `herdr-tasks claude-hook` command,** now installed for three
  events, all with `async: true`, so the hook never delays a prompt or a
  reply:
  - `PostToolUse`;
  - `UserPromptSubmit`, for your prompt;
  - `Stop`, for the reply, from `last_assistant_message`, which Claude Code
    2.1.316 provides.
- **The hook stays silent,** keeps its 5-second limit, and edits only the
  state file.
- **Prompts you write.** `UserPromptSubmit` counts only prompts with a
  `source` of `user` or `sdk`. It skips loop wake-ups and system-injected
  prompts.
- **Hooks can arrive out of order.** A reply that names an artifact can be
  saved before the publish call that gives the artifact its source file.
  The richer details are merged into the existing link.
- **One setting turns all of this on:** `claude_links = true` (my call). It
  replaces part 1's `linear_hook`, which never shipped, and is off by
  default. I turn it on for this user's install after the build. The
  implementers never touch the live config.

### In the terminal

- **The strip.** Its right column shows one row per kind: **Linear**,
  **Slack**, **Pages**, and **Links** for anything else.
  - Each row starts with its kind's name in its own color.
  - The items follow on the same row, each a clickable terminal hyperlink:
    an issue ID, a Slack message's date and time, or a page's title. Only
    http(s) links and local files are clickable; a link with another scheme,
    which the terminal would hand to whatever app handles it, shows as text
    (added in review, as on the board).
  - A row that doesn't fit ends in `+N`.
- **Removing a link (`L`)** makes the strip taller and numbers every link.
  You type the number and press Enter, so tasks with more than nine links
  work too.
- **The overview, sidebar, and tab bar** stay as they are.
- **`link add`** (CLI and strip) accepts a local file path and stores it as
  a `file://` link.

### On the board

- **Card links** are grouped the same way:
  - Linear chips with their status;
  - Slack chips with the message date;
  - Pages chips marked web, file, or local;
  - other links.
- **The detail view** lists every group, with a remove button on each link.
- **Web and localhost pages** (artifacts, hosted pages, localhost) open in a
  new tab.
- **Opening a file.** Browsers won't open `file://` links from a web page,
  so a file link is a button that asks the board server to open it on this
  machine (`open` on macOS, `xdg-open` on Linux), next to a copy-path
  button. The server opens a file only when all of these hold:
  - its URL is one of that task's links;
  - it is a local file, with no other host;
  - it is still a regular `.html`/`.htm` file after following symlinks;
  - the request carries the browser key.

  If the opener fails, the board says so.

## Check these first (risks)

1. **Terminal fidelity.** Before building part 3, spike it: render
   `pane.read` ANSI snapshots of a Claude Code pane in xterm.js and check the
   result is faithful (colors, box drawing, wide characters, line endings
   `\r\n`). If it isn't, come back before building part 3.
2. **A long-running process from the startup hook.** Confirm herdr lets a
   detached child outlive the startup command, and that it doesn't run every
   startup hook with a timeout that kills children.
3. **Key mapping.** Confirm the full set of key names `pane.send_keys` accepts
   (herdr validates all keys before writing any).
4. **Hook payload.** Confirm the exact `PostToolUse` stdin shape for MCP tools
   (tool name, input, and result content), using a real call to each counted
   Linear tool.

## Left to the builder

- Port number, the timeout after which the server exits, and the polling and
  refresh intervals.
- WebSocket vs server-sent events.
- The frontend structure (plain JS modules are fine). Visual design within the
  "Look" section.
- How to get the Linear workspace URL key for comment-only links.
- Card layout when a tab has several agents.
- How the server reports its own errors (log file location, a status line on
  the board).
- Tests: follow the repo's habits. Pure logic (column mapping, Linear URL and
  ID parsing, Claude log tail parsing, hook filtering) gets unit tests like
  `reconcile_test.go`.

## Decisions log

The user decided (confirmed 2026-10-30):

- Build all three parts, in the order above.
- Local only, with a secret token. Other devices later.
- Link Linear issues Claude creates, updates, comments on, or opens
  individually. Not search results.
- Card summary from Claude's own session log. No AI-written recap.
- Terminal view shows only the tab's panes, and you can type into them.
- The server starts with herdr, and a herdr action opens the board.
- Dragging a card changes its stage.
- Both the board and the Linear hook are opt-in settings, off by default,
  because the plugin is public. They are turned on for this user's install.
  The README documents both settings.

The user decided (2026-10-31, part 4):

- Tasks also get the Slack links posted into their Claude sessions, and the
  HTML artifacts, visualizations, explainers and showcases created and used in
  them (web, localhost, and files). The board shows them.
- The strip shows links grouped by kind: Linear, Slack, and pages.

My calls (marked above; easy to change):

- Install the Claude Code hook into `~/.claude/settings.json` via
  `herdr-tasks setup`.
- Use the `away_summary` log entries as "the recap". Drop the todo list.
- One server for all sessions, with a session switcher.
- Tool tabs are left off the board. Hidden spaces stay hidden.
- Leave the strip pane out of the terminal view.
- Add a "Show in herdr" button.
- A removed Linear link stays removed. The hook doesn't add it back.
- Store the Linear key in the macOS Keychain, or a 0600 file on Linux.
- Part 4: localhost links you paste count as pages, and so do pages on static
  hosts (github.io and the like). Linear URLs you paste are linked. Slack links
  in Claude's replies don't count. Removing any link sticks. One setting,
  `claude_links`, replaces `linear_hook`. Files open from the board through the
  server.
