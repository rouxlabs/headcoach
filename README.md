<h1 align="center"><img src="docs/logo.svg" alt="headcoach" width="560"></h1>

<!-- The README of headcoach: what it does, how to install and use it. -->

[![ci](https://github.com/rouxlabs/headcoach/actions/workflows/ci.yml/badge.svg)](https://github.com/rouxlabs/headcoach/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/rouxlabs/headcoach)](https://github.com/rouxlabs/headcoach/releases/latest)
[![license](https://img.shields.io/github/license/rouxlabs/headcoach)](LICENSE)

Coach a team of Claude Code agents: one board for every session, in the terminal and in the browser.

The website: [headcoach.rouxlabs.dev](https://headcoach.rouxlabs.dev)

![The terminal board: entering a session, replying to the agent and coming back with Ctrl-\], a coordinator and the tasks it drives, the fuzzy filter, the coordinator view, a handoff in the details, a pin, the review queue, the schedules, a task lighting up as it moves to Done, and a delete in 10 minutes](docs/demo.gif)

The same board in the browser:

![The web board: one column per state, each task tagged with the coordinator that drives it](docs/web.png)

## What it does

- every Claude Code session becomes a task on the board the moment you send its first prompt
- each task shows its state: `Backlog`, `Running`, `Needs me`, `Review`, `Parked` or `Done`
- `Waiting` is a running task that waits on its sub-agents or background jobs
- a turn that ends on a question, or on an ask such as `Reply "go"` outside a plan, goes to `Needs me`
- a turn that leaves a plan waiting for your go, or a change not committed, pushed or merged yet, goes to `Review`
- any other finished turn goes to `Done`; an open board lights the task up green for a few seconds, then greys it
- each task shows its context window, its tokens and its cost at API prices
- sub-agents and background commands show up under their task
- a task can drive other tasks: they show up under it, see [Coordinators](#coordinators)
- a session or a sub-agent that loads a persona skill, such as `/reviewer`, shows it in the PERSONA column
- the top of the board shows how much of your 5 hour and 7 day limits is used, see [Limits and cost](#limits-and-cost)
- you enter a session with one key, leave it with another, and it keeps running
- you ask a question about a session without interrupting it
- you pin a task to the top of the board, with its sub-agents and the tasks it drives, whatever the order
- a task can carry a timer, and closes itself when it runs out
- a task or a schedule can pick the model and the effort level its sessions start with
- the review queue shows what waits on you in Review, and how big each change is

## Requirements

- macOS or Linux
- Claude Code
- tmux
- `curl`
- [Herdr](https://herdr.dev), only for sessions you run in it
- Go 1.25 or later, only to build from source

## Install

Download the binary for your machine, macOS or Linux, Intel or ARM, check it, and install it:

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
url=https://github.com/rouxlabs/headcoach/releases/latest/download
cd "$(mktemp -d)" && curl -fsSL -O "$url/hc-$os-$arch" -O "$url/SHA256SUMS"
shasum -a 256 -c SHA256SUMS --ignore-missing && mkdir -p ~/.local/bin && install -m 755 "hc-$os-$arch" ~/.local/bin/hc
```

- `~/.local/bin` must be in your `PATH`
- `hc update` installs the next releases the same way; it talks to GitHub only when you run it
- with Go: `go install github.com/rouxlabs/headcoach/cmd/hc@latest`
- from source: `git clone https://github.com/rouxlabs/headcoach.git && cd headcoach && make install`

Then:

- `hc hooks install` connects Claude Code to the board
- `hc service install` starts the daemon now and at every login
- `hc` opens the board; the first one shows how it works, once

## Keys

| Key | Action |
|---|---|
| `j` `k`, arrows | move the cursor; `g` `G` go to the top or the bottom, `pgup` `pgdn` move a page |
| `a` | enter the session, `Ctrl-]` comes back; on a sub-agent, its main session |
| `enter` | open the session: details, history, log |
| `q` | ask about the session without touching it |
| `r` | reply to the session; on a question, the answer goes in its "Type something" option |
| `n` | new session: its folder, its profile, then its title; `tab` lists the folders |
| `c` | copy: a new session in the same folder, with the same profile |
| `t` | rename |
| `A` | adopt a session from your own tmux |
| `S` | schedules |
| `R` | review queue: what waits on you in Review, `o` opens its pull request or its diff |
| `w` | coordinator view: the tasks this one drives, or its parent drives, where each stands, and what waits on you; `enter` shows one on the board |
| `C` | columns: `space` shows or hides one, `J` and `K` move it, `r` goes back to the default; the board keeps them after a restart |
| `s` | start or resume |
| `p` | park: close the session, keep it for later |
| `m` | move to another state |
| `o` | order by state, idle time, cost, tokens or context |
| `P` | pin to the top, in any order, with its sub-agents and the tasks it drives, its title marked ▲; a second pin goes under the first, and a task under a parent goes first among its parent's tasks; `P` again unpins; a pin stays until you take it off, in Done too |
| `/` | fuzzy filter by title, repo or task id: `hcfz` finds "headcoach fuzzy", the cursor goes to the best fit, the arrows move it as you type |
| `:` | command line: `update` installs the latest release, `restart` restarts the daemon, `columns` opens the columns, `help`, `q` |
| `u` | update: install the latest release, after its notes and a yes |
| `h` | hide a Done task, or show a hidden one again; a Done task stays on the board, dimmed, until you hide it |
| `d` | show or hide the hidden Done tasks |
| `x` | delete from the board |
| `X` | delete from the board in 10 minutes, without a question; `X` again keeps it |
| `v` | select: `space` ticks tasks, then `x`, `m`, `p` or `h` deletes, moves, parks or hides all of them after one y / n, `esc` leaves |
| `?` | help |
| `Q` | quit |

## Commands

- `hc ls` lists the sessions and the limits
- `hc ls --sort cost` puts the most expensive session first, `--sort idle` the most recently active one
- `hc new -s "<prompt>"` starts a session in the current directory
- `hc attach <id>` enters a session
- `hc ask <id> ["<question>"]` answers a question about a session, or summarises it
- `hc reply <id> "<text>"` sends a message to a session; from another session, it says who sent it, see [Coordinators](#coordinators)
- on a question, `hc reply` types the answer in its "Type something" option and submits it; a dialog without one refuses it
- a dialog of several questions takes one reply per question, then its own submit step: `hc keys <id> Enter`
- `hc keys <id> <key>...` presses keys to answer a dialog, such as `Down Enter`
- `hc show <id>` prints a session and the end of its log
- `hc new --timer 10m "<prompt>"` gives a task a timer, see [Timers](#timers)
- `hc new --model sonnet --effort low "<prompt>"` picks the model and the effort level, see [Model and effort](#model-and-effort)
- `hc new --parent <id> "<prompt>"`, `hc link <id> <parent>` and `hc unlink <id>` put a task under the one that drives it, see [Coordinators](#coordinators)
- `hc inbox [<id>]` lists what a task heard from the tasks it drives
- `hc handoff [init | path] [<id>]` prints a task's handoff file, `hc reset <id>` starts it again from there
- `hc team [<id>]` shows the tasks a task drives, where each stands, and what waits on you among them
- `hc pin <id>` pins a task to the top of the board, under those pinned before it, `hc unpin <id>` takes the pin off; `hc ls` marks its title with ▲, the web board tags it `pinned`, and its panel has a Pin to top button
- `hc move <id> <state>` moves a session, the state as the board shows it, `Running` or `Needs me`, and `Parked` closes it
- `hc adopt <id>` moves a session from your tmux into the board's
- `hc limits` shows each limit and when it resets
- `hc review` lists the tasks in Review, see [Review queue](#review-queue)
- `hc open` opens the web board on `http://127.0.0.1:7337`
- `hc pair <name>` lets a phone reach the board, see [From your phone](#from-your-phone)
- `hc restart` restarts the daemon
- `hc update` installs the latest release, checked against its `SHA256SUMS`, then restarts the daemon; `-y` skips the question; `u` or `:update` does the same from the board
- a board left open reloads itself once the daemon runs a newer `hc`, and keeps its view: Done shown or not, the order, the filter, the open view, the cursor and the scroll
- `hc help` lists every command
- every `<id>` accepts a unique prefix

## Where sessions run

- sessions started from the board run in a tmux server of their own
- they never show up in your tmux
- they start without what belongs to the terminal hc was started from: no `TMUX`, `HERDR_*`, `DIRENV_*`, `CLAUDE_*` or terminal ids, and what direnv exported there is undone
- so a profile's `CLAUDE_CONFIG_DIR` or other Claude Code variables go in its `[profiles]` command or its `settings.json`, as they must for the service
- a session in tmux is driven through tmux, whatever `HERDR_*` variables that tmux was started with
- a session you start yourself is tracked too, and stays where you started it
- closing the board stops nothing
- `private_tmux = false` opens the board's sessions as windows in your tmux instead
- a session you start in a [Herdr](https://herdr.dev) pane is tracked with that pane, and the board shows `herdr` where it runs
- a pane moved to another workspace changes its id: the board finds it again by its session id, or, without Herdr's Claude Code integration, as the only agent in the session's directory that no other session holds
- with two such agents it cannot tell which one is the session: it takes no reply and is not resumed, answer it in Herdr
- `hc reply` sends to it with `herdr agent prompt`, `hc keys` with `herdr agent send-keys`
- the board never resumes a session Herdr still runs, parked or not: a second `claude` would answer in the same conversation
- `a`, `hc attach` and the phone enter it with `herdr agent attach`, and `Ctrl-]` comes back: it hangs that attach up, the agent runs on
- while you are in, the pane takes the size of your screen, and its own again once you leave
- the web board's Go to terminal brings its pane to the front in Herdr
- a dialog there takes its keys, not a reply: Herdr takes no prompt while one shows
- parking it, or its timer running out, leaves its Herdr pane open
- `hc reset` and a coordinator's news reach only a session in tmux
- once Herdr no longer runs it in a pane, or `herdr` is not installed, it counts as closed, and a reply resumes it in tmux

## Schedules

- a schedule starts a task with the same prompt at a time of day: `08:00`, daily, weekdays, weekends or `mon,wed,fri`
- or on a cron line, minute hour day month weekday: `hc schedule add --cron '*/15 * * * *' -t Triage "sort new issues"`
- a cron run waits while the previous one still works or waits on you, then follows catch-up as below
- `S` on the board lists them; `n` there creates one, `space` pauses it, `r` runs it now
- `hc schedule add --at 08:00 --catch-up -t Standup "summarise yesterday's commits"` does the same from the shell
- each run is a normal task on the board, titled with the schedule and the day, and the time for a cron line
- the ↻ column of the board and of `hc ls` names the schedule of a run by its short id, `-` for a task started by hand, the review queue shows ↻ before its title, and the web board a `schedule` tag
- a schedule with a timer closes each run once its timer runs out, see [Timers](#timers)
- a new run moves the previous one to Done, unless you moved it, answered it, replied to it or it is still working
- with catch-up, a run the machine slept through starts once it wakes, however late, and only once
- without catch-up, a missed run is skipped until the next time
- the daemon runs schedules, so it must be running: `hc service install`

## Timers

- a timer closes a task once it runs out: `hc new --timer 10m "<prompt>"`, or `hc timer <id> 10m` on a task already there
- a timer counts from when you set it, takes a duration such as `90s`, `10m` or `1h30m`, up to a week, and `off` removes it
- the ⏱ column of the board and of `hc ls` shows the time left, `-` without a timer, the review queue ⏱ before the title, and the web board a tag
- once it runs out, within half a minute, the task moves to Done and its session ends, whatever it was doing
- a task you moved to Done yourself only loses its timer: a session you kept open there stays
- `X` on the board deletes a task in 10 minutes, as `x` does but without a question: the ⏱ column shows `✗` and the time left, unless the timer runs out first
- `X` again keeps the task, and so does a task that runs again: a reply or a start brings it back to Running
- a schedule gives each of its runs a timer: `hc schedule add --timer 10m ...`, or `hc schedule timer <schedule> 10m` on one already there
- the daemon runs timers, so it must be running

## Model and effort

- a task starts on the model and the effort level of its profile, unless you pick them
- `hc new --model sonnet --effort low "<prompt>"` picks both, each one on its own too
- the model is an alias such as `sonnet` or `haiku`, or a full model name
- the effort level is `low`, `medium`, `high`, `xhigh` or `max`
- every session of the task, a resume included, starts `claude` with `--model` and `--effort`
- a schedule picks them for each of its runs: `hc schedule add --model haiku --effort low ...`
- `hc schedule model <schedule> sonnet` or `hc schedule effort <schedule> low` changes one already there, `default` goes back to the profile's
- `hc show <id>` and `hc schedule ls` show both
- the MODEL column of the board and of `hc ls` shows the model short, such as `opus`: the one picked, or else the one the latest session answers with
- the EFFORT column shows the effort level: the one picked, or else the one the latest session reports, through the status line forwarder of [Limits and cost](#limits-and-cost)

## Review queue

- `R` on the board, or `hc review`, lists the tasks in Review, the one waiting longest first, with how long each has waited
- each row shows its repo, its branch and its change: the commits since the task started, the files and lines
- TESTS is the last test command the task ran, passed or failed; `test_pattern` says which commands run tests
- REVIEWER is the verdict of the task's last reviewer sub-agent: one that loaded `/reviewer`, or of a type such as `code-reviewer`
- DELIVERY is how the change ships, from what the task did, whatever the rule it followed
- a pull or merge request the task opened, or mentions on a branch, shows as `PR #12`, with its state when `gh` or `glab` is installed
- commits on the default branch show what is left to push, or `pushed`
- changes not committed yet show `uncommitted`; files git does not track do not count
- commits on another branch with no pull request show `no PR yet`
- `o` opens the pull request in the browser, or else the diff of the commits in git's pager
- `enter` shows the task on the board
- a task whose change has shipped moves to Done when the queue is read
- shipped is pushed to the default branch by the task, with nothing left to commit
- or merged, through a pull request the task opened or that holds its commits
- the queue asks `gh` or `glab` about a pull request only while it is open, at most every 2 minutes
- the last line: how long a task waited in Review on average, and the average cost of a task Done, over 7 days

## Coordinators

- a task can drive other tasks: it briefs them with `hc new`, relays decisions with `hc reply`, and reads them with `hc show`
- `hc new --parent <task> "<prompt>"` puts the new task under the one that drives it; `--parent self` is the task of the session hc runs in
- `hc link <task> <parent>` puts a task already there under another one, `hc unlink <task>` takes it out
- a task has one parent at most, and a parent can be any task; a loop is refused
- the board, `hc ls` and the web list draw the tree in the TASK column: a parent says how many tasks it drives, `▾ 2`, and each one hangs under it, after the parent's sub-agents, on a `├─` or `└─` branch
- each tree takes a color of its own, picked from the task at its top, on the board and on the web board
- a card of the web board names its parent with a `↳` tag, and a parent card says how many tasks it drives with a `▾` tag, in the color of their tree
- `self` works wherever a command takes a task, from inside a session: `hc show self`
- a parent hears from its children without polling: when a child stops in `Needs me`, `Review`, `Done` or `Parked`, the board types one line about it into the parent's session, after `[from hc]`
- the line says what the child asks, or its last words: `- 1a2b3c4d "Fix login" is in Review: pushed, PR #12 open`
- a child has to stay put 10 seconds first, so a permission prompt answered at once is no news
- a newer state replaces an older one not said yet, and a child back at work by then says nothing
- a move the parent made itself, such as `hc move <child> done`, is no news to it
- the board waits until the parent can hear: its turn is over, or it only waits on its own jobs, and its prompt is on screen
- it never types while the parent works, waits on you, shows a dialog, or has words in its prompt; a suggestion Claude Code draws there does not count
- it types into a parent once a minute at most, all its children's news in one message, and never opens a session for it
- `hc inbox [<task>]` lists what a task heard and what waits to be said, the calling task by default
- the history of a task names the task that moved it: `by task 1a2b3c4d`
- each task can keep a handoff file: where the work stands, written so that a fresh context, or the task driving it, can pick it up
- it lives next to the board's database, never in the checkout: `hc handoff path` prints where, and a session the board starts finds it in `HC_HANDOFF`
- `hc handoff init` writes a template, `hc handoff [<task>]` prints it, the calling task by default; the agent keeps it up to date with its own tools
- its sections, each optional: `## Status`, `## Running`, `## Waiting` with one `- on <who>: <what>` line each, `## Next`, and a `## Queue` of `- [ ]` and `- [x]` items
- `<who>` is `you`, a task id, or anything else such as `ci`; HTML comments are guidance and never count
- `hc show`, the detail and the web board sum it up: its status, what waits on whom, what is next, and the queue
- `hc reset <task>` clears a task's context between two turns, then points the fresh one at its handoff; it refuses while the task works, and without a handoff that says something
- `hc team [<task>]` is a coordinator's view: the tasks under it at any depth, their state, their queue, and where each stands, the calling task by default
- it lists first what waits on you among them: a question, a change in `Review`, or a `- on you:` line of a handoff, the coordinator's own included
- `w` on the board opens the same view for the task under the cursor, or for its parent when it drives none; the web board shows it in the detail of a task that drives others
- `hc reply` from inside a session puts its sender before the message: `[from task 1a2b3c4d] stop`, so the other session never takes it for your words
- `hc new` from inside a session does the same to the task's first prompt
- a slash command keeps its place and the label goes before its arguments; a bare one such as `/compact` has none
- your own messages, sent from a board or from a shell outside any session, carry no label
- hc tells an agent from you by what Claude Code and the board put in a session's environment: `CLAUDE_CODE_SESSION_ID`, `CLAUDECODE` and `HC_TASK`
- the last line on the board, `hc show`, the detail and the web board name the sender of each labeled message: `coord`, `task`, `other` or `hc`
- `hc keys` cannot carry a label: the board records who pressed them
- the label keeps honest agents apart, not hostile ones: anything on the machine can reach the daemon

## From your phone

- the daemon listens on `127.0.0.1` only: nothing outside the computer reaches it
- [Tailscale Serve](https://tailscale.com/kb/1312/serve) puts it on HTTPS inside your tailnet: `tailscale serve --bg 7337`
- when the computer already serves HTTPS on 443, give Serve another port instead: `tailscale serve --bg --https=7443 7337`, and the board's address ends in `:7443`
- Serve routes add up: `tailscale serve --https=443 off` removes one already on 443
- a request from another machine needs a paired device, whatever Host it claims, and only a tailnet name (`*.ts.net`) is let in
- `hc pair phone` prints a code; open the board's tailnet address on the phone and type it within 10 minutes
- a code works once, and five wrong codes cancel it
- the phone keeps a token in a cookie; the daemon keeps only its hash
- on a phone the board is one list grouped by state, `Needs me` first, and a task opens full screen with the reply box and the keys for its question above the keyboard
- `hc devices` lists the paired devices, `hc devices rm phone` unpairs one
- hooks and pairing stay on the computer: a paired device cannot pair another
- a paired app can enter a session in the board's tmux, or in Herdr, the way `a` does on the terminal board: `GET /api/tasks/<id>/attach` runs `tmux attach`, or `herdr agent attach`, on the computer and streams it over a WebSocket
- leaving from the phone, or losing the connection, detaches: the session keeps running
- the phone and the computer can be in the same session at once, and the window takes the size of the screen used last
- a session you started in your own tmux can only be entered from the computer
- an app opens `wss://<board>/api/tasks/<id>/attach?cols=<n>&rows=<n>` with its device cookie
- binary frames carry the keys to tmux and the screen back; a text frame `{"type":"resize","cols":<n>,"rows":<n>}` resizes it
- byte `0x1d`, Ctrl-], leaves the session, and the socket closes with `Detached`
- the stream carries no hint about Ctrl-], unlike `hc attach`: an app shows its own way out
- the daemon pings an attached app every 15 seconds: one that stops answering, or is unpaired with `hc devices rm`, is hung up
- on Android, Chrome's "Add to Home screen" installs the board as an app

## Limits and cost

- the top of the board draws each limit like Claude Code's `/status`: a thin meter, the share used and when it resets
- when the current pace hits a limit before it resets, it says when instead
- a narrow board keeps short meters in its left column
- profiles that share an account count as one, and each limit names its profiles when they differ
- Claude Code reports limits, cost, context size and effort level to the status line only
- add this line to your status line script, after it has read its input into `$input`:
- `printf '%s' "$input" | ~/.local/share/headcoach/hc-status.sh`
- cost is what the tokens would cost at API prices, also on a subscription
- limits per model are not reported and are not shown

## Configuration

- `hc config init` writes `~/.config/headcoach/config.toml`, with every key explained
- `hc hooks install -c ~/.claude-team -c ~/.claude-personal` covers several Claude Code profiles
- one `[profiles]` line per profile tells the board how to launch it
- `theme` picks the board's colors: `terminal` (follows your terminal), `bright`, `nord` or `k9s`
- `hc tui --theme k9s` tries a theme without touching the config
- `C` on the board saves its columns to `columns` next to the board's database, one name per line, a hidden one after a `-`
- `welcomed` next to the board's database says the board showed its welcome; delete it to see the welcome again
- `test_pattern` names the commands that run tests, for the review queue, by default `make check`, `go test`, `npm test`, `pytest` and the like; `""` turns it off
- `persona_pattern` names the skills that count as a persona, by default `(^|[-_])(developer|reviewer|architect|researcher|planner|tester)($|[-_])`
- `workdir = "~/code"` makes `hc new -C myidea` mean `~/code/myidea`
- a `--dir` folder that does not exist yet is created after a y/n prompt

## Uninstall

- `hc service uninstall`
- `hc hooks uninstall`
- delete the `hc` binary: `~/.local/bin/hc`, or the one in your Go bin directory
- delete `~/.local/state/headcoach` and `~/.local/share/headcoach` to drop the board data

## Develop

- `make check` runs gofmt, go vet and the tests
- `make build` builds `./hc`
- `./hc serve -v` runs the daemon in the foreground and logs every request
- `scripts/demo.sh` fills a throwaway board with example sessions
- `vhs scripts/demo.tape` records `docs/demo.gif` from that board, its head says how to set one up
- `make dist` cross-compiles for macOS and Linux into `dist/`
- `hc restart` restarts the service after `make install`

## Layout

- `cmd/hc`: the entry point
- `internal/board`: the state machine, hook events in and task state out
- `internal/store`: the SQLite storage
- `internal/review`: what the review queue reads from transcripts, git and the forge
- `internal/schedule`: when a schedule runs next
- `internal/handoff`: what the board reads from a task's handoff file
- `internal/usage` and `internal/limits`: tokens, cost and the usage limits
- `internal/api` and `internal/client`: the JSON shapes of the API, and its client
- `internal/server`: the daemon, its JSON API and the web board
- `internal/cli`: the commands
- `internal/tui`: the terminal board
- `internal/install`: the hooks in `settings.json` and the login service
- `internal/tmux`: everything that touches tmux
- `internal/herdr`: everything that touches Herdr

## License

- Apache-2.0, see `LICENSE`
- bugs and ideas are welcome as issues, see `CONTRIBUTING.md`
