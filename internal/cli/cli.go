// Package cli is `hc`: the command line client. With no arguments it opens the TUI.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/format"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/install"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/sh"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
	"github.com/rouxlabs/headcoach/internal/transcript"
	"github.com/rouxlabs/headcoach/internal/tui"
	"github.com/rouxlabs/headcoach/internal/version"
)

// env is what a command works with.
type env struct {
	client *client.Client
	out    io.Writer
	color  bool
}

func (e *env) printf(format string, args ...any) { fmt.Fprintf(e.out, format, args...) }

func (e *env) json(value any) error {
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

type command struct {
	name  string
	args  string // the arguments, as shown in the help
	help  string
	flags []flag
	min   int // fewest arguments
	max   int // most arguments, -1 for any number
	run   func(e *env, p parsed) error
}

// usageError is a mistake in the command line: the exit status is 2, as for any Unix tool.
type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

// These are variables so that tests can watch what would replace the process or open a browser.
var (
	execProgram = func(argv, environ []string) error {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return fmt.Errorf("%s is not installed", argv[0])
		}
		return syscall.Exec(path, argv, environ)
	}
	// runAttach runs a tmux attach in this terminal. tmux prints why it left on stdout: all of it
	// shows but "[detached ...]".
	runAttach = func(argv, environ []string) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = environ, os.Stdin, tmux.QuietDetach(os.Stdout), os.Stderr
		return cmd.Run()
	}
	// runHerdrAttach runs a herdr agent attach in this terminal, Ctrl-] coming back.
	runHerdrAttach = func(argv []string) error {
		return herdr.NewAttach(argv).Run()
	}
	openBrowser = func(url string) {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		_ = exec.Command(opener, url).Start() // no browser is not worth an error: the address is printed
	}
	runTUI = tui.Run
	// runSwitch shows the switcher of recent tasks, and returns the task picked: nil when it was closed.
	runSwitch = tui.Switch
	// replaceClient hands a tmux client over to a command.
	replaceClient = tmux.ReplaceClient
	// switchClient moves a tmux client to the session of a pane.
	switchClient = tmux.SwitchClient
	// tellClient shows a line at the bottom of a tmux client.
	tellClient = tmux.Tell
)

var commands []command

func init() {
	jsonFlag := flag{name: "json", help: "print JSON"}
	task := "<task>"
	commands = []command{
		{name: "tui", help: "open the terminal board (the default)", run: cmdTUI, flags: []flag{
			{name: "theme", short: "t", value: true, help: "color theme: " + strings.Join(tui.ThemeNames, ", ")},
		}},
		{name: "ls", help: "list tasks as a tree", run: cmdLs, flags: []flag{
			{name: "all", short: "a", help: "include done tasks"},
			{name: "sort", short: "s", value: true, help: "state (default); idle, the most recently active first; cost, tokens or ctx, the biggest first"},
			jsonFlag,
		}},
		{name: "show", args: task, help: "show one task and the end of its session", min: 1, max: 1, run: cmdShow, flags: []flag{
			{name: "turns", short: "n", value: true, help: "how many turns of the log to print, your last prompt kept (default 12)"},
			jsonFlag,
		}},
		{name: "new", args: "<prompt>...", help: "add a task to the backlog", min: 1, max: -1, run: cmdNew, flags: []flag{
			{name: "title", short: "t", value: true, help: "title (default: the first line of the prompt)"},
			{name: "dir", short: "C", value: true, help: "working directory; a bare name goes under the configured workdir, a missing folder is created after a y/n prompt (default: the current one)"},
			{name: "profile", short: "p", value: true, help: "profile to launch Claude Code with"},
			{name: "start", short: "s", help: "start it right away"},
			{name: "timer", value: true, help: "close the task once this long has passed, e.g. 10m or 1h30m, whatever it is doing"},
			{name: "model", value: true, help: "model to start Claude Code on, e.g. sonnet (default: the profile's)"},
			{name: "effort", value: true, help: "effort level: " + efforts + " (default: the profile's)"},
			{name: "parent", value: true, help: "the task that drives this one; self is the task of the session hc runs in"},
		}},
		{name: "link", args: task + " <parent>", help: "put a task under the one that drives it; self is the task of the session hc runs in", min: 2, max: 2, run: cmdLink},
		{name: "unlink", args: task, help: "take a task from under its parent", min: 1, max: 1, run: cmdUnlink},
		{name: "handoff", args: "[init | path] [" + task + "]", help: "print a task's handoff file; init writes the template, path prints where it goes (default: self)", max: 2, run: cmdHandoff, flags: []flag{jsonFlag}},
		{name: "reset", args: task, help: "clear a task's context between two turns, and point the fresh one at its handoff", min: 1, max: 1, run: cmdReset},
		{name: "team", args: "[" + task + "]", help: "a coordinator's view: the tasks it drives, where each stands, and what waits on you (default: self)", max: 1, run: cmdTeam, flags: []flag{jsonFlag}},
		{name: "inbox", args: "[" + task + "]", help: "what a task heard from the tasks it drives, and what waits to be said (default: self)", max: 1, run: cmdInbox, flags: []flag{jsonFlag}},
		{name: "review", help: "list the tasks in Review, the one waiting longest first, with their change", run: cmdReview, flags: []flag{jsonFlag}},
		{name: "start", args: task, help: "start or resume a task in a new tmux window", min: 1, max: 1, run: cmdStart},
		{name: "attach", args: task, help: "enter a task's session from this terminal; Ctrl-] comes back", min: 1, max: 1, run: cmdAttach},
		{name: "switch", help: "pick one of the last 10 tasks you entered, the latest first, and enter it; Ctrl-\\ opens it inside a session", run: cmdSwitch, flags: []flag{
			{name: "popup", help: "in a tmux popup: the terminal around it enters the task"},
			{name: "last", help: "from a tmux key: the terminal it came from goes straight back to the last task, as Ctrl-^ does"},
		}},
		{name: "ask", args: task + " [question]...", help: "ask about a task without touching its session; no question means a summary", min: 1, max: -1, run: cmdAsk},
		{name: "reply", args: task + " <text>...", help: "send a message to a task's session, or the answer to its question; from a session, it names its sender", min: 2, max: -1, run: cmdReply},
		{name: "timer", args: task + " <duration|off>", help: "close a task once this long has passed from now, e.g. 10m; off removes its timer", min: 2, max: 2, run: cmdTimer},
		{name: "move", args: task + " <state>", help: "move a task to another state: " + stateNames(), min: 2, max: -1, run: cmdMove},
		{name: "pin", args: task, help: "pin a task to the top of the board, under the tasks pinned before it", min: 1, max: 1, run: cmdPin},
		{name: "unpin", args: task, help: "take a task's pin off", min: 1, max: 1, run: cmdUnpin},
		{name: "adopt", args: task, help: "move a session from your tmux into the board's own tmux", min: 1, max: 1, run: cmdAdopt},
		{name: "screen", args: task, help: "print what a task's terminal is showing", min: 1, max: 1, run: cmdScreen},
		{name: "keys", args: task + " <key>...", help: "press keys in a task's terminal to answer a dialog, e.g. Down Enter", min: 2, max: -1, run: cmdKeys},
		{name: "rm", args: task, help: "delete a task and the tasks under it from the board (the Claude sessions are kept)", min: 1, max: 1, run: cmdRm},
		{name: "schedule", args: "[ls | add <prompt>... | run|pause|resume|rm <schedule> | timer <schedule> <duration|off> | model|effort <schedule> <value|default>]", max: -1, run: cmdSchedule,
			help: "tasks that start themselves at a time of day, or on a cron line", flags: []flag{
				{name: "at", value: true, help: "add: time of day, HH:MM"},
				{name: "days", value: true, help: "add: with --at, daily (default), weekdays, weekends, or days such as mon,wed,fri"},
				{name: "cron", value: true, help: "add: instead of --at, a cron line: minute hour day month weekday, e.g. '*/15 * * * *'"},
				{name: "catch-up", help: "add: when the machine was asleep or off at that time, run once it is back, however late"},
				{name: "title", short: "t", value: true, help: "add: title (default: the first line of the prompt)"},
				{name: "dir", short: "C", value: true, help: "add: working directory, as for hc new"},
				{name: "profile", short: "p", value: true, help: "add: profile to launch Claude Code with"},
				{name: "timer", value: true, help: "add: close each run once this long has passed, e.g. 10m"},
				{name: "model", value: true, help: "add: model each run starts on, e.g. sonnet (default: the profile's)"},
				{name: "effort", value: true, help: "add: effort level of each run: " + efforts + " (default: the profile's)"},
				{name: "json", help: "ls: print JSON"},
			}},
		{name: "pair", args: "<name>", help: "let a phone or another computer reach the board: prints a code to type on it", min: 1, max: -1, run: cmdPair},
		{name: "devices", args: "[ls | rm <name>]", help: "list or unpair the devices hc pair let in", max: -1, run: cmdDevices, flags: []flag{jsonFlag}},
		{name: "limits", help: "show how much of the 5 hour and 7 day limits is used", run: cmdLimits, flags: []flag{jsonFlag}},
		{name: "open", help: "open the web board in a browser", run: cmdOpen},
		{name: "hooks", args: "[install|uninstall|print]", help: "connect Claude Code to the board", max: 1, run: cmdHooks, flags: []flag{
			{name: "config-dir", short: "c", value: true, multi: true, help: "a Claude Code config directory; repeat for several (default: $CLAUDE_CONFIG_DIR or ~/.claude)"},
		}},
		{name: "config", args: "[init]", help: "write a commented config file if there is none", max: 1, run: cmdConfig},
		{name: "service", args: "[install|uninstall|restart|status]", help: "run the daemon at login", max: 1, run: cmdService},
		{name: "restart", help: "restart the daemon service", run: cmdRestart},
		{name: "update", help: "replace hc with the latest release, checked against its SHA256SUMS", run: cmdUpdate, flags: []flag{
			{name: "yes", short: "y", help: "do not ask before replacing"},
		}},
		{name: "serve", help: "run the daemon and the web board", run: cmdServe, flags: []flag{
			{name: "verbose", short: "v", help: "log every request"},
		}},
	}
}

func find(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, "hc: a task board for Claude Code sessions.\n\nUsage: hc [--url URL] [command] [options]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.help)
	}
	fmt.Fprint(w, "\nOptions:\n  --url URL  daemon address (default: $HC_URL, else the config file)\n  --version  print the version\n"+
		"\nEvery <task> accepts a unique prefix of its id. `hc <command> --help` shows the options of a command.\n")
}

func (c *command) usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s\n\n%s\n", strings.TrimSpace("hc "+c.name+" "+c.args), strings.ToUpper(c.help[:1])+c.help[1:])
	if len(c.flags) > 0 {
		fmt.Fprint(w, "\nOptions:\n")
	}
	for _, f := range c.flags {
		names := "--" + f.name
		if f.short != "" {
			names = "-" + f.short + ", " + names
		}
		if f.value {
			names += " VALUE"
		}
		fmt.Fprintf(w, "  %-22s %s\n", names, f.help)
	}
}

// Main runs one command line and returns the exit status.
func Main(argv []string, stdout, stderr io.Writer) int {
	url := ""
	for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
		name, value, inline := strings.Cut(argv[0], "=")
		switch {
		case name == "--version":
			fmt.Fprintf(stdout, "hc %s\n", version.Version)
			return 0
		case name == "-h" || name == "--help":
			usage(stdout)
			return 0
		case name == "--url" && inline:
			url, argv = value, argv[1:]
		case name == "--url" && len(argv) > 1:
			url, argv = argv[1], argv[2:]
		default:
			fmt.Fprintf(stderr, "hc: unknown option %s\n", argv[0])
			return 2
		}
	}
	name := "tui"
	if len(argv) > 0 {
		name, argv = argv[0], argv[1:]
	}
	if name == "help" {
		usage(stdout)
		return 0
	}
	cmd := find(name)
	if cmd == nil {
		fmt.Fprintf(stderr, "hc: unknown command '%s'. See: hc help\n", name)
		return 2
	}
	p, err := parse(argv, append([]flag{{name: "help", short: "h"}}, cmd.flags...))
	if err == nil && p.has("help") {
		cmd.usage(stdout)
		return 0
	}
	if err == nil && (len(p.args) < cmd.min || (cmd.max >= 0 && len(p.args) > cmd.max)) {
		err = fmt.Errorf("usage: %s", strings.TrimSpace("hc "+cmd.name+" "+cmd.args))
	}
	if err != nil {
		fmt.Fprintf(stderr, "hc %s: %v\n", cmd.name, err)
		return 2
	}

	if url == "" {
		url = os.Getenv("HC_URL")
	}
	if url == "" {
		cfg, err := config.Load("")
		if err != nil {
			fmt.Fprintf(stderr, "hc: %v\n", err)
			return 1
		}
		url = cfg.URL()
	}
	file, isFile := stdout.(*os.File)
	e := &env{client: client.New(url), out: stdout}
	if isFile && os.Getenv("NO_COLOR") == "" {
		if info, err := file.Stat(); err == nil {
			e.color = info.Mode()&os.ModeCharDevice != 0
		}
	}
	err = cmd.run(e, p)
	var mistake usageError
	switch {
	case errors.As(err, &mistake):
		fmt.Fprintf(stderr, "hc %s: %v\n", cmd.name, err)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "hc: %v\n", err)
		return 1
	}
	return 0
}

// choice returns the optional first argument when it is one of the allowed words.
func choice(p parsed, fallback string, allowed ...string) (string, error) {
	if len(p.args) == 0 {
		return fallback, nil
	}
	for _, word := range allowed {
		if p.args[0] == word {
			return word, nil
		}
	}
	return "", usageError{fmt.Sprintf("'%s' is not one of: %s", p.args[0], strings.Join(allowed, ", "))}
}

// --- listing -------------------------------------------------------------------------

func (e *env) paint(text, style string) string { return format.Paint(text, style, e.color) }

// cut keeps the first n characters of a cell.
func cut(text string, n int) string {
	if runes := []rune(text); len(runes) > n {
		return string(runes[:n])
	}
	return text
}

func (e *env) printLimits(entries []limits.Entry) {
	for _, l := range entries {
		bar := strings.Repeat("#", int(float64(l.Pct)/5+0.5))
		line := fmt.Sprintf("%-10s %-3s [%-20s] %3d%%  resets in %s", l.Profile, l.Label, bar, l.Pct, format.Human(l.ResetsIn))
		line = e.paint(line, l.Level)
		if warning := format.LimitWarning(l); warning != "" {
			line += e.paint("   ! "+warning, "hot")
		}
		if l.Stale {
			line += e.paint("   (not updated lately)", "dim")
		}
		e.printf("%s\n", line)
	}
}

func cmdLimits(e *env, p parsed) error {
	entries, err := e.client.Limits()
	if err != nil {
		return err
	}
	if p.has("json") {
		return e.json(entries)
	}
	if len(entries) == 0 {
		e.printf("No limits reported yet. Call hc-status.sh from your status line script: see the README.\n")
		return nil
	}
	e.printLimits(entries)
	return nil
}

func cmdLs(e *env, p parsed) error {
	by := p.get("sort", "state")
	known := false
	for _, s := range format.Sorts {
		known = known || s == by
	}
	if !known {
		return usageError{fmt.Sprintf("cannot sort by '%s'. Known: %s", by, strings.Join(format.Sorts, ", "))}
	}
	board, err := e.client.Board()
	if err != nil {
		return err
	}
	tasks := board.Tasks
	if !p.has("all") {
		tasks = []api.Task{}
		for _, t := range board.Tasks {
			if t.State != "done" {
				tasks = append(tasks, t)
			}
		}
	}
	if p.has("json") {
		return e.json(tasks)
	}
	if len(tasks) == 0 {
		e.printf("No open tasks. Start a Claude session, or add one with: hc new\n")
		return nil
	}
	const columns = "%-9s %-8s %-6s %-6s %-26s %-16s %-4s %6s %6s %9s %6s %6s %6s %6s %5s %-8s  "
	rows := format.TreeRows(tasks, by)
	// The TASK column grows with the tree: the branches down to a task, and how many it drives.
	width := 9
	for _, row := range rows {
		width = max(width, utf8.RuneCountInString(taskCell(row)))
	}
	e.printf("%s\n", e.paint(fmt.Sprintf("%-*s "+columns+"TITLE", width, "TASK", "STATE", "PROFILE", "MODEL", "EFFORT", "PERSONA", "REPO", "TMUX", "AGE", "IDLE", "CTX", "IN", "OUT", "CACHE", "COST", "⏱", "↻"), "dim"))
	for _, row := range rows {
		t := row.Task
		if a := row.Agent; a != nil {
			branch := format.Branch(row)
			in, out, cache := format.TokenCells(a.Tokens)
			line := fmt.Sprintf("%-*s "+columns+"%s %s: %s", width, row.Tree, store.StateName(a.State), "", "", "", cut(a.Persona, 26), "", "", format.Human(a.Age), "", "", in, out, cache, "", "", "", branch, a.Name, a.Desc)
			e.printf("%s\n", e.paint(line, "dim"))
			continue
		}
		persona := orDash(t.Persona)
		closed := ""
		if format.Closed(t) {
			closed = " (closed)"
		}
		where := orDash(t.Tmux)
		profile := orDash(t.Profile)
		model := orDash(format.ModelCell(t.Model, t.SessionModel))
		effort := orDash(format.EffortCell(t.Effort, t.SessionEffort))
		timer := orDash(format.TimerCell(t.TimerLeft, t.DeleteLeft))
		schedule := orDash(format.ScheduleCell(t.Schedule))
		in, out, cache := format.TokenCells(&t.Tokens)
		e.printf("%-*s %s %-8s %-6s %-6s %-26s %-16s %-4s %6s %6s %s %6s %6s %6s %6s %5s %-8s  %s%s\n",
			width, taskCell(row), e.paint(fmt.Sprintf("%-9s", t.StateName), t.State), cut(profile, 8), cut(model, 6), cut(effort, 6), cut(persona, 26), cut(t.Repo, 16), where,
			format.Human(t.Age), format.Human(t.Idle),
			e.paint(fmt.Sprintf("%9s", format.ContextCell(t.Context)), format.ContextLevel(t.Context)),
			in, out, cache, format.Money(t.Cost), timer,
			schedule, format.PinMark(t)+t.Title, closed)
		if t.State == "needs" {
			e.printf("%s\n", e.paint(fmt.Sprintf("%-*s? %s", 149+width, "", t.Ask), "needs"))
		}
	}
	if len(board.Limits) > 0 {
		e.printf("\n")
		e.printLimits(board.Limits)
	}
	return nil
}

func cmdShow(e *env, p parsed) error {
	turns, err := strconv.Atoi(p.get("turns", "12"))
	if err != nil || turns < 0 {
		return usageError{"--turns takes a number"}
	}
	t, err := e.client.Task(p.args[0])
	if err != nil {
		return err
	}
	if p.has("json") {
		return e.json(t)
	}
	state := t.StateName
	if !t.Live {
		state += "  (no live session)"
	}
	persona := orDash(t.Persona)
	where := orDash(map[string]string{"hc": "the board's own tmux, Ctrl-] detaches", "own": "your tmux", "herdr": "Herdr"}[t.Tmux])
	e.printf("%s\n\n", t.Title)
	lines := [][2]string{
		{"task", t.ID}, {"state", state}, {"repo", t.Repo}, {"branch", t.Branch}, {"dir", t.Cwd},
		{"tmux", where}, {"profile", t.Profile}, {"model", orDefault(t.Model)}, {"effort", orDefault(t.Effort)},
		{"persona", persona},
	}
	if t.Parent != "" {
		lines = append(lines, [2]string{"parent", board.Short(t.Parent)})
	}
	for _, line := range append(lines, [2]string{"resume", t.Resume}) {
		e.printf("  %-8s %s\n", line[0], line[1])
	}
	if t.Context.Tokens > 0 {
		e.printf("  %-8s %s of %s (%d%%)\n", "context", format.Count(t.Context.Tokens), format.Count(t.Context.Window), t.Context.Pct)
	}
	if t.Cost > 0 {
		e.printf("  %-8s %s at API prices\n", "cost", format.Money(t.Cost))
	}
	if t.Tokens.Total() > 0 {
		e.printf("  %-8s %s\n", "tokens", format.TokenLine(t.Tokens))
		e.printf("  %-8s of which sub-agents: %s\n", "", format.TokenLine(t.AgentTokens))
	}
	if t.State == "needs" {
		e.printf("\n  needs you: %s\n", t.Ask)
	}
	if t.Detail != nil && t.Handoff != nil && t.Handoff.Text != "" {
		e.printf("\n")
		for _, line := range format.HandoffLines(*t.Handoff, time.Now()) {
			e.printf("  %s\n", line)
		}
	}
	for _, a := range t.Agents {
		kind := "sub-agent"
		if a.Name == "command" || a.Name == "monitor" {
			kind = "background"
		}
		used := ""
		if a.Tokens != nil {
			used = "  [" + format.TokenLine(*a.Tokens) + "]"
		}
		e.printf("  %s %s (%s): %s%s\n", kind, a.Name, store.StateName(a.State), a.Desc, used)
	}
	var log []string
	if t.Detail != nil {
		for _, turn := range transcript.Window(t.Log, turns) {
			log = append(log, fmt.Sprintf("%-5s", format.Who(turn))+" | "+strings.ReplaceAll(turn.Text, "\n", "\n      | "))
			if gap := turn.Gap(); gap != "" {
				log = append(log, "      | "+gap)
			}
		}
	}
	if len(log) > 0 {
		e.printf("\n%s\n", strings.Join(log, "\n"))
	}
	return nil
}

// --- actions -------------------------------------------------------------------------

// confirmCreate asks on the terminal whether to create a missing folder. Tests replace it.
var confirmCreate = func(e *env, dir string) bool {
	return confirm(e, dir+" does not exist. Create it?")
}

// confirm asks a yes/no question on the terminal. No terminal means no.
func confirm(e *env, question string) bool {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false // not a terminal: nobody can answer
	}
	e.printf("%s [y/N] ", question)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// newTaskDir resolves --dir: ~ is expanded, a bare name lands under the configured workdir, and a
// missing folder is created after a y/n prompt.
func newTaskDir(e *env, dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	dir = config.ExpandUser(dir)
	if !filepath.IsAbs(dir) && !strings.HasPrefix(dir, ".") {
		cfg, err := config.Load("")
		if err != nil {
			return "", err
		}
		if cfg.Workdir != "" {
			dir = filepath.Join(cfg.Workdir, dir)
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return dir, nil
	case err == nil:
		return "", fmt.Errorf("%s is not a directory", dir)
	case !confirmCreate(e, dir):
		return "", fmt.Errorf("%s does not exist", dir)
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func cmdNew(e *env, p parsed) error {
	dir, err := newTaskDir(e, p.get("dir", ""))
	if err != nil {
		return err
	}
	data := map[string]any{
		"prompt": strings.Join(p.args, " "), "title": p.get("title", ""), "cwd": dir, "start": p.has("start"),
	}
	for _, name := range []string{"profile", "parent"} {
		if value := p.get(name, ""); value != "" {
			data[name] = value
		}
	}
	if p.has("timer") {
		timer, err := parseTimer(p.get("timer", ""))
		if err != nil {
			return err
		}
		data["timer"] = timer
	}
	launchOptions(p, data)
	t, err := e.client.Do("POST", "/api/tasks", data)
	if err != nil {
		return err
	}
	e.printf("%s  %s  %s\n", t.Short, t.StateName, t.Title)
	return nil
}

// efforts are the effort levels Claude Code takes, for the help.
const efforts = "low, medium, high, xhigh, max"

// launchOptions puts --model and --effort into a request. The daemon checks them.
func launchOptions(p parsed, data map[string]any) {
	for _, name := range []string{"model", "effort"} {
		if p.has(name) {
			data[name] = p.get(name, "")
		}
	}
}

// orDefault shows an empty model or effort as the profile's own.
func orDefault(value string) string {
	if value == "" {
		return "default"
	}
	return value
}

// parseTimer reads a timer as a duration, 10m or 1h30m, in whole seconds. off is no timer.
func parseTimer(text string) (int, error) {
	if text == "off" {
		return 0, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < time.Second || d > 7*24*time.Hour {
		return 0, usageError{fmt.Sprintf("a timer reads like 10m, 1h30m or off, up to a week, not %q", text)}
	}
	return int(d.Round(time.Second) / time.Second), nil
}

func cmdTimer(e *env, p parsed) error {
	timer, err := parseTimer(p.args[1])
	if err != nil {
		return err
	}
	t, err := e.client.Do("PATCH", "/api/tasks/"+p.args[0], map[string]any{"timer": timer})
	if err != nil {
		return err
	}
	if t.TimerLeft == 0 {
		e.printf("%s  no timer\n", t.Short)
		return nil
	}
	e.printf("%s  closes in %s\n", t.Short, format.Human(t.TimerLeft))
	return nil
}

// stateNames lists the states as the boards show them.
func stateNames() string {
	names := make([]string, len(store.States))
	for i, id := range store.States {
		names[i] = store.StateNames[id]
	}
	return strings.Join(names, ", ")
}

// stateID reads a state the way it is shown, "Needs me", or by its id, "needs", in any case.
func stateID(text string) (string, bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	for _, id := range store.States {
		if text == id || text == strings.ToLower(store.StateNames[id]) {
			return id, true
		}
	}
	return "", false
}

func cmdMove(e *env, p parsed) error {
	name := strings.Join(p.args[1:], " ") // "needs me" may come unquoted, as two words
	state, known := stateID(name)
	if !known {
		return usageError{fmt.Sprintf("'%s' is not a state. Known: %s", name, stateNames())}
	}
	t, err := e.client.Do("PATCH", "/api/tasks/"+p.args[0], map[string]string{"state": state})
	if err != nil {
		return err
	}
	e.printf("%s  %s  %s\n", t.Short, t.StateName, t.Title)
	return nil
}

func cmdLink(e *env, p parsed) error {
	t, err := e.client.Do("PATCH", "/api/tasks/"+p.args[0], map[string]string{"parent": p.args[1]})
	if err != nil {
		return err
	}
	e.printf("%s  now under %s\n", t.Short, board.Short(t.Parent))
	return nil
}

func cmdUnlink(e *env, p parsed) error {
	t, err := e.client.Do("PATCH", "/api/tasks/"+p.args[0], map[string]string{"parent": ""})
	if err != nil {
		return err
	}
	e.printf("%s  now at the top of the board\n", t.Short)
	return nil
}

// taskCell is a task in the TASK column: the branches of the tree down to it, its id, and how many
// tasks it drives.
func taskCell(row format.Row) string {
	if row.Agent != nil {
		return row.Tree
	}
	cell := row.Tree + row.Task.Short
	if drives := format.Drives(row.Kids); drives != "" {
		cell += " " + drives
	}
	return cell
}

func cmdInbox(e *env, p parsed) error {
	id := "self"
	if len(p.args) > 0 {
		id = p.args[0]
	}
	var inbox api.Inbox
	if err := e.client.Call("GET", "/api/tasks/"+id+"/inbox", nil, &inbox); err != nil {
		return err
	}
	if p.has("json") {
		return e.json(inbox)
	}
	if len(inbox.Items) == 0 {
		e.printf("Nothing from the tasks under %s yet.\n", board.Short(inbox.Task))
		return nil
	}
	now := store.Now()
	for _, it := range inbox.Items {
		status, at := "waiting", it.At
		if it.DeliveredAt > 0 {
			status, at = "sent", it.DeliveredAt
		}
		title := it.ChildTitle
		if title == "" {
			title = "(deleted)"
		}
		e.printf("%-7s %5s  %-8s  %s: %s\n", status, format.Human(int(now-at)), it.ChildShort, cut(title, 40), it.News)
	}
	return nil
}

func cmdHandoff(e *env, p parsed) error {
	action, args := "", p.args
	if len(args) > 0 && (args[0] == "init" || args[0] == "path") {
		action, args = args[0], args[1:]
	}
	if len(args) > 1 {
		return usageError{"hc handoff [init | path] [<task>]"}
	}
	id := "self"
	if len(args) > 0 {
		id = args[0]
	}
	var (
		h    api.Handoff
		err  error
		path = "/api/tasks/" + id + "/handoff"
	)
	if action == "init" {
		err = e.client.Call("POST", path, map[string]bool{"init": true}, &h)
	} else {
		err = e.client.Call("GET", path, nil, &h)
	}
	if err != nil {
		return err
	}
	switch {
	case p.has("json"):
		return e.json(h)
	case action != "":
		e.printf("%s\n", h.Path)
	case h.Text == "":
		e.printf("No handoff yet. Write one at %s, or start from the template: hc handoff init %s\n", h.Path, id)
	default:
		e.printf("%s\n", strings.TrimRight(h.Text, "\n"))
	}
	return nil
}

func cmdReset(e *env, p parsed) error {
	var t api.Task
	if err := e.client.CallFor(30*time.Second, "POST", "/api/tasks/"+p.args[0]+"/reset", nil, &t); err != nil {
		return err
	}
	e.printf("%s  cleared, and pointed at its handoff\n", t.Short)
	return nil
}

func cmdTeam(e *env, p parsed) error {
	id := "self"
	if len(p.args) > 0 {
		id = p.args[0]
	}
	var team api.Team
	if err := e.client.Call("GET", "/api/tasks/"+id+"/team", nil, &team); err != nil {
		return err
	}
	if p.has("json") {
		return e.json(team)
	}
	t := team.Task
	e.printf("%s  %s  %s\n", t.Short, e.paint(t.StateName, t.State), t.Title)
	if team.Handoff.Status != "" {
		e.printf("  %s\n", team.Handoff.Status)
	}
	if len(team.OnYou) > 0 {
		e.printf("\n%s\n", e.paint("Waits on you", "needs"))
		for _, o := range team.OnYou {
			e.printf("  %-8s  %-30s  %s\n", o.Short, cut(o.Title, 30), format.OnYouWhat(o))
		}
	}
	if len(team.Members) == 0 {
		e.printf("\nNo task under it. Put one there with: hc new --parent %s, or hc link <task> %s\n", t.Short, t.Short)
		return nil
	}
	width := 8
	for _, m := range team.Members {
		width = max(width, utf8.RuneCountInString(memberCell(m)))
	}
	e.printf("\n%s\n", e.paint(fmt.Sprintf("%-*s  %-9s %5s  %-16s  %-40s  %s", width, "TASK", "STATE", "IDLE", "QUEUE", "TITLE", "WHERE IT STANDS"), "dim"))
	for _, m := range team.Members {
		e.printf("%-*s  %s %5s  %-16s  %-40s  %s\n", width, memberCell(m), e.paint(fmt.Sprintf("%-9s", m.StateName), m.State),
			format.Human(m.Idle), orDash(format.QueueCell(m.Handoff.Queue)), cut(m.Title, 40), cut(format.Where(m), 100))
	}
	return nil
}

// memberCell is a task of a team in the TASK column, as taskCell draws it on the board.
func memberCell(m api.Member) string {
	return taskCell(format.Row{Task: &m.Task, Tree: m.Tree, Kids: m.Kids})
}

func cmdPin(e *env, p parsed) error { return pinTask(e, p.args[0], true) }

func cmdUnpin(e *env, p parsed) error { return pinTask(e, p.args[0], false) }

func pinTask(e *env, id string, pin bool) error {
	t, err := e.client.Pin(id, pin)
	if err != nil {
		return err
	}
	done := "unpinned"
	if pin {
		done = "pinned to the top"
	}
	e.printf("%s  %s  %s\n", t.Short, done, t.Title)
	return nil
}

func cmdStart(e *env, p parsed) error {
	t, err := e.client.Do("POST", "/api/tasks/"+p.args[0]+"/start", nil)
	if err != nil {
		return err
	}
	e.printf("%s  started in tmux pane %s\n", t.Short, t.Pane)
	return nil
}

func cmdReply(e *env, p parsed) error {
	t, err := e.client.Do("POST", "/api/tasks/"+p.args[0]+"/reply", map[string]string{"text": strings.Join(p.args[1:], " ")})
	if err != nil {
		return err
	}
	e.printf("%s  message delivered\n", t.Short)
	return nil
}

func cmdAttach(e *env, p parsed) error { return enter(e, p.args[0]) }

// cmdSwitch opens the switcher of recent tasks, then enters the task picked. In a popup over a session
// of the board's tmux, the terminal the key came from, named in HC_CLIENT, leaves that session for the
// task. With --last, that terminal goes straight back to the task entered before, without the switcher.
func cmdSwitch(e *env, p parsed) error {
	client := os.Getenv("HC_CLIENT")
	popup, last := p.has("popup"), p.has("last")
	if (popup || last) && client == "" {
		return errors.New("hc switch --popup and --last run from Ctrl-\\ and Ctrl-^ inside a session of the board's tmux; use hc switch")
	}
	var t *api.Task
	if last {
		board, err := e.client.Board()
		if err == nil {
			if t = tui.Previous(board.Tasks, os.Getenv("HC_TASK")); t == nil {
				err = errors.New("no task entered before this one")
			}
		}
		if err != nil {
			// Said where the key was pressed: tmux would show the error in a pane of its own.
			if tellClient(client, "hc: "+err.Error()) != nil {
				return err
			}
			return nil
		}
	} else {
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		if t, err = runSwitch(e.client, cfg.Theme, os.Getenv("HC_TASK")); err != nil || t == nil {
			return err
		}
		if !popup {
			return enter(e, t.ID)
		}
	}
	// In the board's tmux, as the session the key came from: the terminal moves to the task's session,
	// as tmux moves between sessions. Anywhere else, a new attach takes the terminal over, and the one
	// before waits under it until Ctrl-].
	fresh, err := e.client.Task(t.ID)
	if err != nil {
		return err
	}
	if fresh.Live && fresh.Tmux == "hc" && fresh.Pane != "" {
		if _, err := e.client.Enter(fresh); err != nil { // notes it as entered
			return err
		}
		return switchClient(client, fresh.Pane)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// It writes to the terminal, not to what tmux wrote to: the board reads that through a pipe, which
	// holds a screen back. Read and write, so that what asks the terminal a question reads its answer,
	// rather than leave it typed into the session. /bin/sh reads it whatever your shell.
	return replaceClient(client, "/bin/sh -c "+sh.Quote("exec "+sh.Quote(exe)+" attach "+t.ID+" 1<>/dev/tty"))
}

// enter takes this terminal into a task's session, wherever it runs, starting it when needed.
func enter(e *env, id string) error {
	// Ctrl-\ pressed again while a switch hands the terminal over is no reason to die: it is caught and
	// dropped. Caught, not ignored, so that what runs next still gets it.
	quits := make(chan os.Signal, 1)
	signal.Notify(quits, syscall.SIGQUIT)
	defer func() { signal.Stop(quits); close(quits) }()
	go func() {
		for range quits {
		}
	}()
	t, err := e.client.Task(id)
	if err != nil {
		return err
	}
	attach, err := e.client.Enter(t)
	if err != nil {
		return err
	}
	if len(attach) > 0 && t.Tmux == "herdr" {
		return runHerdrAttach(attach)
	}
	if len(attach) > 0 {
		return runAttach(attach, tmux.AttachEnv())
	}
	if t.Live {
		if _, err := e.client.Do("POST", "/api/tasks/"+t.ID+"/focus", nil); err != nil {
			return err
		}
		e.printf("%s  switched tmux to pane %s\n", t.Short, t.Pane)
		return nil
	}
	// No live session: take over this terminal, the way k9s drops you into a shell.
	if t.Cwd != "" {
		_ = os.Chdir(t.Cwd) // a directory that is gone is not a reason to refuse
	}
	return execProgram([]string{"/bin/sh", "-c", t.Resume}, append(os.Environ(), "HC_TASK="+t.ID))
}

func cmdAsk(e *env, p parsed) error {
	var out struct {
		Answer string `json:"answer"`
	}
	question := map[string]string{"question": strings.Join(p.args[1:], " ")}
	if err := e.client.CallFor(150*time.Second, "POST", "/api/tasks/"+p.args[0]+"/ask", question, &out); err != nil {
		return err
	}
	e.printf("%s\n", out.Answer)
	return nil
}

func cmdAdopt(e *env, p parsed) error {
	t, err := e.client.Do("POST", "/api/tasks/"+p.args[0]+"/adopt", nil)
	if err != nil {
		return err
	}
	e.printf("%s  now runs in the board's tmux. Enter it with: hc attach %s\n", t.Short, t.Short)
	return nil
}

type screen struct {
	Screen string `json:"screen"`
}

func cmdScreen(e *env, p parsed) error {
	var out screen
	if err := e.client.Call("GET", "/api/tasks/"+p.args[0]+"/screen", nil, &out); err != nil {
		return err
	}
	e.printf("%s\n", out.Screen)
	return nil
}

func cmdKeys(e *env, p parsed) error {
	var out screen
	if err := e.client.Call("POST", "/api/tasks/"+p.args[0]+"/keys", map[string][]string{"keys": p.args[1:]}, &out); err != nil {
		return err
	}
	e.printf("%s\n", out.Screen)
	return nil
}

func cmdRm(e *env, p parsed) error {
	var out struct {
		Deleted  string   `json:"deleted"`
		Children []string `json:"children"`
	}
	if err := e.client.Call("DELETE", "/api/tasks/"+p.args[0], nil, &out); err != nil {
		return err
	}
	e.printf("deleted %s", board.Short(out.Deleted))
	if n := len(out.Children); n > 0 {
		e.printf(" with %s under it", format.Counted(n, "task"))
	}
	e.printf("\n")
	return nil
}

func cmdOpen(e *env, p parsed) error {
	openBrowser(e.client.URL)
	e.printf("%s\n", e.client.URL)
	return nil
}

func cmdTUI(e *env, p parsed) error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	state := filepath.Dir(cfg.DB)
	return runTUI(e.client, p.get("theme", cfg.Theme), cfg.Workdir, cfg.ProfileNames(),
		filepath.Join(state, "columns"), filepath.Join(state, "welcomed"))
}

// --- setup ---------------------------------------------------------------------------

func cmdHooks(e *env, p parsed) error {
	action, err := choice(p, "print", "install", "uninstall", "print")
	if err != nil {
		return err
	}
	if action == "print" {
		script, err := install.HookScript()
		if err != nil {
			return err
		}
		e.printf("%s\n", install.HooksJSON(script))
		return nil
	}
	remove := action == "uninstall"
	dirs := p.flags["config-dir"]
	if len(dirs) == 0 {
		dirs = install.DefaultConfigDirs()
	}
	for _, dir := range dirs {
		path, changed, err := install.EditSettings(dir, remove)
		if err != nil {
			return err
		}
		done := map[[2]bool]string{
			{false, true}: "installed in", {false, false}: "already in",
			{true, true}: "removed from", {true, false}: "already absent from",
		}[[2]bool{remove, changed}]
		e.printf("hooks %s %s\n", done, path)
	}
	if !remove {
		e.printf("Sessions started from now on report to the board.\n")
	}
	return nil
}

func cmdConfig(e *env, p parsed) error {
	if _, err := choice(p, "init", "init"); err != nil {
		return err
	}
	path, created, err := install.InitConfig()
	if err != nil {
		return err
	}
	if created {
		e.printf("wrote %s\n", path)
	} else {
		e.printf("kept the existing %s\n", path)
	}
	return nil
}

// runningVersion asks the daemon for its version. Empty when nothing answers.
func runningVersion(c *client.Client) string {
	meta, err := c.Meta()
	if err != nil {
		return ""
	}
	return meta.Version
}

// daemonWait is how long a daemon may take to come up.
var daemonWait = 10 * time.Second

func waitForDaemon(c *client.Client) (string, error) {
	for deadline := time.Now().Add(daemonWait); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if v := runningVersion(c); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("The daemon did not come up at %s. See %s", c.URL, logHint())
}

// logHint tells where the daemon writes its log.
func logHint() string {
	if manager, _ := install.Manager(); manager == "systemd" {
		return "journalctl --user -u headcoach"
	}
	return install.LogPath()
}

func cmdService(e *env, p parsed) error {
	action, err := choice(p, "status", "install", "uninstall", "restart", "status")
	if err != nil {
		return err
	}
	switch action {
	case "install":
		if !install.ServiceInstalled() && runningVersion(e.client) != "" {
			return fmt.Errorf("A daemon already runs at %s. Stop it first, then install the service.", e.client.URL)
		}
		path, err := install.ServiceInstall()
		if err != nil {
			return err
		}
		e.printf("installed %s\n", path)
		v, err := waitForDaemon(e.client)
		if err != nil {
			return err
		}
		e.printf("hc %s is running at %s and starts at login\n", v, e.client.URL)
	case "uninstall":
		path, err := install.ServiceUninstall()
		if err != nil {
			return err
		}
		e.printf("removed %s\n", path)
	case "restart":
		return cmdRestart(e, p)
	default:
		service, daemon, log := "not installed", "not running", "none"
		if install.ServiceInstalled() {
			path, _ := install.ServicePath()
			service, log = "installed: "+path, logHint()
		}
		if v := runningVersion(e.client); v != "" {
			daemon = "hc " + v + " at " + e.client.URL
		}
		e.printf("service  %s\ndaemon   %s\nlog      %s\n", service, daemon, log)
	}
	return nil
}

func cmdRestart(e *env, p parsed) error {
	if err := install.ServiceRestart(); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond) // let the old process let go of the port before asking who answers
	v, err := waitForDaemon(e.client)
	if err != nil {
		return err
	}
	e.printf("hc %s is running at %s\n", v, e.client.URL)
	return nil
}
