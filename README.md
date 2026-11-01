# herdr-tasks

A [herdr](https://herdr.dev) plugin that treats every tab as a task. Each tab gets a stage, a note, and links, and one overview shows every task across all spaces.

Any task that isn't done can also be a **priority** (`f`): it gets a red `!` and stays in the sidebar list whatever its stage.

You set the stage. Agents never get a status from you: for active tasks, herdr's own agent detection decides whether it's your turn or the agent is still running.

| Stage | Marker | Use it for |
|---|---|---|
| Active | `●` | Work in progress. The overview splits it by what the agent is doing: **Agent asking** (stopped mid-work for your answer or approval, the most urgent), **Your turn** (finished, or nothing running), and **Agent running**. |
| In review | `◐` | Your part is done; waiting on approval, merge, or testing. Takes what it's waiting for. |
| Paused | `‖` | On hold until something happens: a PR review, QA, or your own decision. Takes a reason. |
| Parked | `○` | Started, but no time to follow up. |
| Done | `✓` | Finished. Close the tab from the overview when you're ready. |
| Tool | `◇` | Not a task: a tab that stays open for a tool like lazygit, or a running app. Sorted last, left out of the counts, listed under TOOLS at the bottom of the overview, and has no strip unless you turn one on. |

![The task overview: every tab across four spaces, grouped by stage, with the selected task's note and links](docs/screenshots/overview.png)

## What it adds to herdr

- **Task overview** (`ctrl+b t`): a popup listing every tab, grouped by stage, with the selected task's note and links. Filter by space, jump to a tab, change stages, edit the note, add links, close done tabs.
- **Strip** (on by default, `ctrl+b i` hides or shows it): a short pane along the bottom of each tab with the task's stage, what the agent is doing, the note, links, and buttons. Links show in one row per kind: Linear, Slack, Pages, and Links for the rest. Click a button, or click into the strip and press its key, to change the stage (`a` `v` `p` `z` `d` `o`), make it a priority or not (`f`), edit the note (`n`, the strip grows while you type; `ctrl+s` saves), add a link (`l`), remove one (`L`, the strip grows and numbers every link; type the number and press `↵`), or rename the task (`r`). `↵` opens the overview, `esc` goes back to your work. After a change, focus returns to the pane above.
- **Tab bar**: a stage marker in front of each tab name, and tabs ordered active → in review → paused → parked → done → tools.
- **Tab bar, right side**: stage counts for the current space, and the current task's pause reason or first link.
- **Task board** (off by default, see [Settings](#settings)): every task of every running herdr session on one page in your browser, in columns by stage and by what the agent is doing. Cards show each agent's state and Claude's latest recap, and the task's links in a row per kind: Linear issues with their status, Slack messages with their date, pages marked web, file or local, and other links. A local `.html` page opens in your browser from the board. Drag a card to another column to change its stage. Open a card to see its tab's terminals, live, and type into them (see [Terminals on the board](#terminals-on-the-board)). The **Open task board** action in herdr opens it, and so does `herdr-tasks board`.
- **Spaces sidebar**: each space's tasks under it, most urgent first: agents asking (`?`, red), then your turn (coral), then agents working (`↻`, blue) and in review. Priority tasks come before all of them, in red with a `!` in front, even when paused or parked. Other paused, parked, done, and tool tabs stay out of the list; a line under it counts the hidden ones by stage (`2 paused 1 parked`). `ctrl+b a` switches to plain stage counts and back.

The Agents list is left alone.

<img src="docs/screenshots/sidebar-tasks.png" width="280" alt="The sidebar listing each space's tasks: an agent asking in red, your-turn tasks in coral, running tasks dimmed, and counts of paused, parked, and done tasks">

![Claude Code asking for permission in one pane. The strip at the bottom says "agent asking" and shows the task's note, Claude's entry, and links](docs/screenshots/agent-asking.png)

![Claude Code running a long test script. The strip says "agent running"; the sidebar counts each space's stages](docs/screenshots/agent-running.png)

![The strip along the bottom of a tab: stage and agent state, note, Claude's entry, links, and a row of buttons](docs/screenshots/strip.png)

The screenshots show real Claude Code sessions in throwaway demo repos.

## Install

Needs herdr 0.8.0+ on Linux or macOS; the task board was built and tested with herdr 0.8.2. If Go 1.23+ is installed, the plugin is built from source; otherwise the matching release binary is downloaded and checked against the release's checksums.

```bash
herdr plugin install alekkowalczyk/herdr-tasks
```

That clones the repo, builds it, and runs `herdr-tasks setup`, which:

- copies `herdr-tasks` into `~/.local/bin`, for the tab bar status and for agents. A `herdr-tasks` already there that setup didn't put there, such as your own link to a checkout, is left alone; the tab bar status and the Claude links hook then run a copy in `~/.local/state/herdr/plugins/herdr-tasks/bin`,
- adds `ctrl+b t`, `ctrl+b i`, `ctrl+b a`, the tab bar status, and the sidebar rows to `~/.config/herdr/config.toml`, inside a marked `# >>> herdr-tasks` block. The tab bar line goes into your existing `[ui]` section. Your previous config is saved once as `config.toml.before-herdr-tasks`. If `config.toml` is a link, as into a dotfiles repo, setup changes the file it points at, keeps the backup next to that file, and leaves the link in place. Anything you already configure yourself (your own `tab_bar_right`, sidebar rows, or a binding on `ctrl+b t`) is left alone, and setup tells you what to add by hand,
- turns every tab in every running herdr session into an Active task, so tab names get a `●` in front (set `markers = false` below to keep tab names as they are), adds a strip along the bottom of each tab, and reloads the config.

Adding a strip to a tab with several panes briefly moves the other panes out to temporary tabs and puts them back in the same arrangement; their processes keep running. New tabs get their strip as soon as they're created. Updates restart running strips with the new version in place.

### herdr --remote

If you attach from another machine with `herdr --remote`, that machine uses its own key bindings by default, and herdr never sends a client's own custom-command bindings to the server. So `ctrl+b t` and `ctrl+b i` only work with the server's bindings. Attach with `--remote-keybindings server`:

```bash
herdr --remote ssh://you@server --session NAME --remote-keybindings server
```

The sidebar counts and the tab bar status come from the server's config in the setups tested so far. If they don't show up on your machine, merge the herdr-tasks lines into that machine's config over SSH (run this there; `config --unmerge` takes them out again):

```bash
f=~/.config/herdr/config.toml; mkdir -p ~/.config/herdr; touch "$f"; ssh you@server '~/.local/bin/herdr-tasks config --merge' < "$f" > "$f.new" && cp "$f" "$f.before-herdr-tasks" && mv "$f.new" "$f"
```

Without the keys, the strips and tab markers still work, and `↵` in a strip opens the overview.

### Remove

```bash
herdr-tasks remove
```

Stops the task board, gives tabs their plain names back, clears the sidebar counts, takes the marked block out of your config and the Claude links hook out of Claude Code's settings, and uninstalls the plugin. Notes and links stay in `~/.local/state/herdr/plugins/herdr-tasks` unless you add `--purge`, which also deletes the Linear key. If the task board doesn't stop, or the Claude links hook can't be taken out (other than because Claude Code's settings don't parse), `remove` changes nothing else and says why, so you can fix that and run it again.

### Working on the plugin

`make link` builds this checkout, runs setup, and links it into herdr instead of a GitHub install. `make unlink` removes it.

## The overview

| Keys | Mouse | Does |
|---|---|---|
| `↑` `↓` / `j` `k` | click, wheel | select a task |
| `↵` | double-click | jump to the tab |
| `a` `v` `p` `z` `d` `o` | | set Active, In review (asks what for), Paused (asks why), Parked, Done, Tool |
| `f` | | make it a priority, or not (Done and Tool end it) |
| `n` | | edit the note (`ctrl+s` saves, `esc` cancels) |
| `l` / `L` | `×` next to a link | add a link / remove one |
| `r` | | rename the task (also renames the tab) |
| `x` | | close the tab (Done tasks only) |
| `1`–`9` | click a space chip | hide or show that space |
| `shift`+`1`–`9` | double-click a chip | show only that space |
| `0` | click "All" | show all spaces |
| `esc` / `q` / `t` | | close the overview |

Space chips count agents asking (`?`) and tasks where it's your turn (`●`). If a hidden space has either, a line at the top of the list says so. The space filter is remembered between opens.

## Terminals on the board

**Open** on a card, or a click on the card, shows its tab: the tab's panes, live, laid out as in herdr, and the whole card next to them (stage, priority, note, log, links, and each agent's recap, last prompt and last reply). The task strip is left out. The address ends in `#tab=<session>/<tab>`, so a reload keeps the view. **Back**, or `esc` when no terminal has focus, goes back to the board.

Click a pane to type into it; it gets an outline. Keys such as `enter`, `esc`, `tab`, the arrows, `F1`–`F12` and `ctrl`/`alt` combinations go to herdr by name, so herdr sends them the way the pane's program expects. Text goes as typed, and a paste goes in one piece. With a terminal focused, `esc` goes to the terminal. Cmd shortcuts stay with the browser: `cmd+c` copies selected text and `cmd+v` pastes. On Linux, `ctrl+shift+c` (or `ctrl+insert`) copies and `ctrl+shift+v` (or `shift+insert`) pastes, as in a terminal; `ctrl+c` goes to the pane. On macOS, Option types the letters it makes (`ą`, `ł`, `é`…), except in the chords terminal programs use: `option` with `b`, `d`, `f`, `p`, `t`, `y` or `.`, or with `enter`, `backspace`, `esc`, `tab` and the arrows, goes to the pane as `alt` (`option+b` is `alt+b`). If herdr turns something down, a note says so, and whatever you typed after it is dropped rather than sent late.

What it can't do:

- Resize panes. Each pane shows at its size in herdr, and the tab is scaled to fit the window.
- Scroll back. The browser shows what herdr shows; if you scrolled a pane back in herdr, it shows that, with a "scrolled back in herdr" badge.
- Use the mouse in terminal apps. Clicks and the wheel aren't sent. Dragging selects text, and the pane holds still while text is selected, until you type or paste into it.
- Show the cursor. herdr doesn't say where it is.
- Send Home, End, Page Up, Page Down, Delete and Insert in the pane's own key mode: herdr has no names for them, so they go as the usual sequences, which shells and Claude Code accept.

Screens are snapshots taken several times a second, so fast output can look choppy. If herdr quits while you watch, the view says the session isn't running rather than keep showing its last screens. Emoji with a variation selector, and joined emoji, can be one cell off in width; only their row shifts.

## Command line

Inside a herdr pane, commands apply to that pane's tab. Use `--tab ID` for another one.

```bash
herdr-tasks set parked
herdr-tasks review "approval + QA"
herdr-tasks tool
herdr-tasks pause "PR #212 review"
herdr-tasks priority                                   # off: herdr-tasks priority off
herdr-tasks note set "Old endpoint stays behind a flag until mobile ships."
herdr-tasks note add "Opened draft PR #431."          # a line with your name, or the agent's
herdr-tasks link add https://github.com/acme/api/pull/431
herdr-tasks link add docs/plan.html                    # a file, as a file:// link
herdr-tasks link rm 1
herdr-tasks show
herdr-tasks list
herdr-tasks board                                      # open the task board in your browser
herdr-tasks board --print                              # print its address instead (it works once, for a minute)
herdr-tasks board --new-token                          # sign every browser out of the board
herdr-tasks linear-key                                 # save a Linear API key, for issue status on the board
```

### Letting agents add notes and links

An agent runs in a pane of its tab, so it can use the same commands. `note add` and `link add` record who added them (`claude` when run from Claude Code, or `--as NAME`). To have Claude do it, add this to `~/.claude/CLAUDE.md`:

```markdown
When running inside herdr (HERDR_ENV=1), record decisions on the current task with
`herdr-tasks note add "<text>"`, and attach every PR, ticket, or doc you create or rely on
with `herdr-tasks link add <url>`. Don't change the task's stage.
```

`link add` fails, and saves nothing, when the URL has a control character or isn't a valid URL, such as one with a stray `%` in its path (write it as `%25`). The strip and the overview say "Not a valid link".

### Links Claude adds

With `claude_links = true` in the [settings](#settings), what Claude Code works with is linked to the task without Claude being asked, as added by `claude`, and only when Claude runs in a herdr pane:

- **Linear issues** Claude opens, creates, updates, comments on, or attaches a file to (`get_issue`, `save_issue`, `save_comment`, `create_attachment`), from any Linear MCP server, and issue links you paste into a prompt. Searches and lists don't count, and neither do issues that another issue only mentions.
- **Slack links** you paste into a prompt: messages, threads and channels on `*.slack.com/archives/…`, Enterprise Grid included. Also the messages Claude posts with a Slack MCP `slack_send_message`. Slack links in Claude's replies don't count, since digest and summary skills reply with lists of them.
- **Pages**: HTML artifacts, visualizations, explainers and showcases.
  - Artifacts Claude publishes with the Artifact tool, labeled with the page's title. When it publishes a local file, the task shows the artifact instead of the file.
  - localhost URLs, claude.ai artifacts, and pages on static hosts (`*.github.io`, `*.gitlab.io`, `*.vercel.app`, `*.netlify.app`, `*.pages.dev`, `*.web.app`, `*.firebaseapp.com`, `*.surge.sh`) in your prompt or Claude's reply. Other web links don't count.
  - `.html` files named in your prompt or Claude's reply, or opened in a browser tool (`navigate` to a `file://` or localhost URL). A name counts when it looks like a path (absolute, or starting with `~/`, `./` or `../`, or containing `/`), and the file is a whole page (`<!doctype html` or `<html`).
  - Whole `.html` pages Claude writes or edits, unless they're templates or sit in a source, build or dependency folder (`src`, `templates`, `static`, `node_modules`, `dist`, `build`, `public`, and so on). A subagent's files don't count.

Each link is added once, whatever form its URL takes: a Slack message with or without `?thread_ts`, an artifact with or without its share key, localhost with or without a trailing `/`. Different hash routes (`#/a`, `#/b`) are different pages.

If you remove a link (`link rm`, `L` in the overview or strip, or `×` on the board), it stays off that task: neither the hook nor an agent running `link add` puts it back (`link add` says so). Removing a published artifact also keeps its file off, and the other way round. Adding the link yourself brings it back.

Limits:

- A localhost or static-host link is never saved when it has a user name or password, a parameter whose name holds `token`, `code`, `key`, `secret`, `pass`, `auth`, `sig`, `session` or `jwt`, or an OAuth callback path. A Linear issue link is saved without a user name, password, query or fragment, and an artifact link keeps only its share key.
- One prompt, reply or tool call adds at most 20 links, and looks at no more than 200 files.
- Once a task has 60 links, Claude adds no new ones, and `claude-hook.log` says so once. It still fills in and replaces the links the task has.

Comments and attachments name only the issue's ID, and so does a `get_issue` call whose result leaves out the URL (Claude asks for a few fields only). Their links are built from the Linear workspace of an issue link seen before, in any herdr session. Until one has been seen, those calls are skipped and noted in `claude-hook.log`.

`herdr-tasks setup` adds the hook to `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR/settings.json`) for tool calls (`PostToolUse`), your prompts (`UserPromptSubmit`) and Claude's replies (`Stop`). It runs in the background, so it never holds Claude up. Setup leaves everything else in that file as it was. Your previous settings are saved once as `settings.json.before-herdr-tasks`. If the file doesn't parse, setup leaves it alone and says so. Turning `claude_links` off and running setup takes the hook out again, and so does `herdr-tasks remove`. Restart Claude Code sessions that are already running to pick up the change. The hook never prints anything to Claude; problems go to `claude-hook.log` in `~/.local/state/herdr/plugins/herdr-tasks`.

## Settings

`~/.config/herdr/plugins/config/herdr-tasks/config.toml`:

```toml
markers = true       # stage marker in front of tab names
sort_tabs = true     # order tabs active → in review → paused → parked → done → tools
strip = true         # a strip along the bottom of each tab
claude_links = false # link the Linear issues, Slack links, and pages Claude works with to the task
board = false        # serve the task board on 127.0.0.1
board_port = 7391    # the board's port
```

Run `herdr-tasks sync` after changing `markers`, `sort_tabs`, or `strip`. Run `herdr-tasks setup` after changing `claude_links`. The board's server reads `board` and `board_port` every 10 seconds, and stops when the board is turned off or moved to another port. To start it after turning the board on or changing its port, run `herdr-tasks board`, or restart herdr.

## How it works

- State lives in `~/.local/state/herdr/plugins/herdr-tasks/<session>.json`, one file per herdr session, keyed by tab ID. Only you can read it, or anything else in that folder, as it holds your notes and the share keys of artifact links. herdr keeps tab IDs across restarts. If a tab still gets a new ID, a named task follows it by space and name.
- herdr runs `herdr-tasks hook` on startup and when tabs or spaces are created, closed, renamed, or moved. Each run brings markers, order, and sidebar counts in line. Sidebar counts don't survive a herdr restart, so the startup hook reports them again.
- Unnamed tabs keep showing their position number after the marker, and the plugin updates that number when tabs move.
- The sidebar list is up to 7 rows per space, sent as workspace metadata tokens (`$t1`…`$t7`, with `_ask`, `_you`, `_run`, and `_pri` variants so asking, your-turn, working, and priority tasks can each be colored). A hook on agent status changes keeps it current.
- Renaming a tab yourself in herdr renames the task; the plugin puts the marker back.
- A strip is an ordinary pane labeled `Task strip` running `herdr-tasks strip`. After a herdr restart it comes back as a plain shell, and the startup hook starts the strip in it again. If everything else in a tab closes, the plugin closes the tab instead of leaving the strip alone in it. Closing a strip yourself counts as hiding it.
- The task board is one server, `herdr-tasks serve`, for every herdr session on this machine. herdr's startup hook starts it when `board = true`, and so does `herdr-tasks board`. Either one replaces a server that an older build left running. It listens on 127.0.0.1 only, stops when no herdr session has run for 3 minutes, and logs to `board/board.log` in `~/.local/state/herdr/plugins/herdr-tasks`.
- Browsers won't open `file://` links from a web page, so a file link on the board is a button that asks the board's server to open it (`open` on macOS, `xdg-open` on Linux), with a **Copy path** button next to it in the card's view. The server opens a file only when it's one of that task's links, and still a regular `.html` or `.htm` file once symlinks are followed.
- Signing in to the board: `herdr-tasks board` asks the server for a code that works once, within a minute, and opens the board with it. The page swaps the code for a key and keeps it. The key works until the server stops, so after a restart, as when no herdr session has run for 3 minutes or after an update, the board asks you to sign in again. The server's own token is `board-token` in `~/.local/state/herdr/plugins/herdr-tasks`. `herdr-tasks board --new-token` replaces it, which signs every browser out.
- With a Linear API key, the board shows the status of linked issues; a read-only key is enough. `herdr-tasks linear-key` reads it from stdin, or asks for it without showing what you type. On macOS it's kept in your login keychain; on Linux, in `linear-api-key` in the state folder, readable only by you. `herdr-tasks linear-key --delete` removes it. The board's server reads the key once and keeps it, so it doesn't ask the keychain every minute; `herdr-tasks linear-key` tells it to read the key again. While the keychain can't be read, as when it's locked, the server asks it again after 5 minutes, then less often, up to once an hour.
- herdr reads a plugin's actions when it's installed or linked. After updating herdr-tasks, run `herdr plugin link` again, or restart herdr, to get new actions such as Open task board.

## Not yet

- Attaching every PR an agent opens, the way the Claude links hook attaches issues and pages.
- A notification with the task's note when you switch tabs.
- `ctrl+click` on a PR or ticket link to attach it to the task.
- PR review and check status from `gh`, and tasks moving on when a PR is approved or merged.
