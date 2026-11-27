# Changelog

All notable changes to headcoach. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- sessions in the board's tmux no longer inherit the environment of the terminal that first started it, such as the `HERDR_*`, `DIRENV_*` and `CLAUDE_*` variables of a Claude Code session in a Herdr pane, and a server already started that way is cleaned before the next session opens
- a session in tmux is no longer recorded with the Herdr pane that tmux was started from, where a reply could land once its tmux pane closed

## [1.2.0] - 2026-11-26

Sessions you start in a [Herdr](https://herdr.dev) pane now show on the board, and you answer them from the board or your phone like any other. The web board also fits a phone, and headcoach has a website: [headcoach.rouxlabs.dev](https://headcoach.rouxlabs.dev).

### Added

- a session you run in a Herdr pane takes `hc reply` and `hc keys` there, through `herdr agent prompt` and `herdr agent send-keys`, `a`, `hc attach` and the phone enter it with `herdr agent attach`, and the board never resumes it while Herdr runs it: a reply used to start a second `claude` on the same conversation in the board's tmux

### Changed

- the web board fits a phone: one list grouped by state with `Needs me` first, a full-screen task panel whose reply box and keys stay above the keyboard, and 44px buttons

## [1.1.1] - 2026-11-25

headcoach has a new home on GitHub. Nothing changes on your board.

### Changed

- headcoach moved to `github.com/rouxlabs/headcoach`: install with `go install github.com/rouxlabs/headcoach/cmd/hc@latest`, and `hc update` or `u` on the board finds the new releases on its own

## [1.1.0] - 2026-11-25

A task can now drive other tasks: it hands them work, and the board tells it when one stops or needs you. The board also gets a fuzzy filter, pins, an order by idle time, and a delete on a timer.

### Added

- a message `hc reply` sends from inside a session starts with its sender, such as `[from task 1a2b3c4d]`, so the other session never takes it for your words; `hc show`, the detail and the web board name the sender too
- `hc new --parent <task>`, `hc link` and `hc unlink` put a task under the one that drives it; the board, `hc ls` and the web list draw the tree in the TASK column, in a color per tree, and `self` names the task of the session hc runs in
- a task hears from the tasks it drives without polling: when one stops in `Needs me`, `Review`, `Done` or `Parked`, the board types a line about it into the parent's session once the parent can hear it; `hc inbox` lists them
- each task can keep a handoff file, where the work stands, outside the checkout: `hc handoff` prints it, `hc show` and the boards sum it up, and `hc reset` clears a task's context and points the fresh one at it
- `hc team`, `w` on the board, and the web detail show what a task drives: each task under it, where it stands, and what waits on you among them
- while you type the filter, the up and down arrows move the cursor, so you pick a task without leaving the line
- `o` on the board orders the tasks by idle time too, the most recently active first; so do `hc ls --sort idle` and the web board's Sort menu
- `P` pins a task to the top of the board with its sub-agents and the tasks it drives, whatever the order, a second pin under the first; `hc pin` and `hc unpin` do the same, and so does Pin to top in the web board's task panel
- `X` deletes a task from the board 10 minutes later, without a question; the ⏱ column counts down with a `✗`, and `X` again keeps the task
- the first board you open shows how it works, once: `a` enters a session, `Ctrl-]` comes back to the board

### Changed

- `/` filters the board with a fuzzy match: `hcfz` finds "headcoach fuzzy filter", and the cursor goes to the task that fits best
- `a`, which enters a session, comes first in the keys at the top of the board and in its help

## [1.0.0] - 2026-11-23

The first public release of headcoach. `hc` puts every Claude Code session on one board, in the terminal and in the browser, so you see what each agent does, what waits on you, and what it costs.

### Added

- every Claude Code session becomes a task the moment you send its first prompt, in `Running`, `Waiting`, `Needs me`, `Review`, `Parked` or `Done`
- a turn that ends on a question, or asks for your go such as `Reply "go"`, goes to `Needs me`, a plan or a change not committed, pushed or merged goes to `Review`
- a task in `Done` stays on the board, dimmed, until `h` hides it; `d` shows the hidden ones
- each task shows its context, tokens, cost at API prices, model, effort level and persona, with its sub-agents and background commands under it
- the top of the board shows the 5 hour and 7 day usage limits of each account
- `enter` opens a session, `a` enters it and `Ctrl-]` comes back, `q` asks about it without interrupting it, `r` replies to it
- `n` starts a task: its folder with tab completion, its profile, then its title; `c` copies one, `A` adopts a session from your own tmux
- `R` opens the review queue, `S` the schedules, `C` the columns, `v` selects several tasks to delete, move, park or hide at once, `?` lists every key
- schedules start a task with the same prompt at a time of day, or on a cron expression; a run replaced by the next one goes to `Done` hidden
- a task can carry a timer, and closes itself when it runs out
- the web board, `hc open`, shows the same tasks in one column per state, and `hc pair` lets a phone reach it
- `hc` commands do the same from a shell: `hc ls`, `hc new`, `hc ask`, `hc reply`, `hc show`, `hc review`, `hc limits`
- `hc update`, or `u` on the board, installs the latest release, checked against its `SHA256SUMS`
- four themes: `terminal`, `bright`, `nord` and `k9s`

[Unreleased]: https://github.com/rouxlabs/headcoach/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/rouxlabs/headcoach/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/rouxlabs/headcoach/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/rouxlabs/headcoach/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/rouxlabs/headcoach/releases/tag/v1.0.0
