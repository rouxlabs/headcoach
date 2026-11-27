package tui

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/format"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/usage"
	"github.com/rouxlabs/headcoach/internal/version"
)

// The styles of the board. applyTheme fills them from the chosen palette.
var (
	plain   lipgloss.Style
	bold    lipgloss.Style
	dim     lipgloss.Style
	heading lipgloss.Style // the column headings of the table
	accent  lipgloss.Style
	alarm   lipgloss.Style
	// linkStyle marks a task id the detail of a task names: tab picks it, enter or a click opens it.
	linkStyle lipgloss.Style

	// stateStyles colours a state, of a task or of a child row.
	stateStyles map[string]lipgloss.Style
	// levelStyles colours a gauge: the context window of a task, or a usage limit.
	levelStyles map[string]lipgloss.Style
	whoStyles   map[string]lipgloss.Style
	box         lipgloss.Style
	selected    lipgloss.Color // background of the row under the cursor
	// glowStyles light up a task that just moved to Done: bright first, then soft.
	glowStyles [2]lipgloss.Style
	// teamStyles color the trees of tasks that drive others, one per tree.
	teamStyles []lipgloss.Style
)

const (
	gauge        = " ▁▂▃▄▅▆▇█"
	headerHeight = 6 // lines of the k9s-style block above the table
	help         = `j / k, arrows   move the cursor
g / G           go to the top or the bottom; pgup / pgdn move a page
a               attach: enter the session in this terminal, Ctrl-] comes back; a sub-agent enters
                its main session
enter           open the task: metadata, history, session log
tab             in an open task: pick in turn the ids of the tasks it names, shift+tab goes back,
                enter opens the one picked, escape stops; with mouse = true in the config, a click
                opens one, and Option-drag on macOS or Shift-drag elsewhere selects text
r               reply to the session; on a question, answer it in its "Type something" option
q               question: ask about the session without touching it; enter alone gives a summary
t               title: rename the task
m               move the task to another state
p               park the task: close its tmux pane, keep the session for later
A               adopt: swallow a session from your own tmux into the board's tmux
s               start or resume the session in a new tmux window
n               new task: its folder (tab lists the folders, a missing one is created), the profile when
                several are configured, then its title; the terminal then drops into the new
                session, where you type the prompt yourself
c               copy: a new task in the folder and with the profile of this one; you give its title
x               delete the task and the tasks under it (the Claude sessions are kept)
X               delete the task in 10 minutes, without a question; X again keeps it
v               select: space ticks the task under the cursor, then x deletes, m moves, p parks or h hides
                every ticked task after one y / n, escape leaves
/               fuzzy filter by title, repo or task id, the cursor on the best fit: "hcfz" finds
                "headcoach fuzzy"; the arrows move the cursor as you type, escape clears
:               command line: update, restart, columns, help, q
u               update: install the latest release, after its notes and a yes; the board reloads itself
o               order: by state, by idle time, by cost, by tokens used, by context size
P               pin the task to the top of the board, in any order, or unpin it; a second pin goes
                under the first
h               hide a done task, or show a hidden one again
d               show or hide the hidden done tasks
S               schedules: tasks that start themselves at a time of day, or on a cron line
R               review queue: the tasks in Review, the one waiting longest first, with their change
w               coordinator view: the tasks this one drives, or its parent drives, where each stands
                and what waits on you among them
C               columns: show, hide and reorder the columns of the board, kept after a restart
?               this help
Q               quit (q or escape closes a view)`
)

// welcome is the note the first board shows, to a user who has not met it yet.
const welcome = `Every Claude Code session shows up here as a task, the moment you send its first prompt.
A task that waits on you moves to Needs me or Review.

a        enter the session under the cursor, and work in it as in any terminal
Ctrl-]   leave the session and come back here: it keeps running
enter    open the task: what it did, its history, its log
q        ask about the session without interrupting it
n        start a new session: its folder, its profile, then its title
?        every key

This note shows once. Any key closes it.`

var logo = []string{
	"  _          ",
	" | |__   ___ ",
	" | '_ \\ / __|",
	" | | | | (__ ",
	" |_| |_|\\___|",
	"             ",
}

// hint is one key shown in the header, with what it does.
type hint struct{ key, does string }

// hintKeys are the keys shown in the header, k9s style, in columns. The full list is behind ?.
var hintKeys = [][]hint{
	{{"a", "attach"}, {"enter", "view"}, {"r", "reply"}, {"t", "title"}, {"m", "move"}, {"p", "park"}},
	{{"A", "adopt"}, {"s", "start"}, {"n", "new"}, {"x", "delete"}, {"/", "filter"}, {"?", "help"}},
	{{"S", "schedules"}, {"q", "ask"}, {"o", "order"}, {"d", "done"}, {":", "command"}, {"Q", "quit"}},
	{{"c", "copy"}, {"R", "review"}, {"u", "update"}, {"v", "select"}, {"C", "columns"}, {"w", "team"}},
}

// selectHintKeys are the keys shown in the header while the board is in select mode.
var selectHintKeys = [][]hint{
	{{"space", "tick"}, {"x", "delete"}, {"m", "move"}},
	{{"p", "park"}, {"esc", "back"}},
}

// hintsFrom and hintWidth say where the columns of hints start in the header, and how wide each is.
const (
	hintsFrom = 46
	hintWidth = 18
	hintsGap  = 2 // the spaces kept between the left column and the hints
	// labelWidth is the width of a label of the left column, "Board:" and its padding.
	labelWidth = 7
)

// scheduleHintKeys are the keys shown in the header of the schedules view.
var scheduleHintKeys = [][]hint{
	{{"n", "new"}, {"space", "pause"}, {"c", "catch-up"}, {"r", "run now"}, {"enter", "last task"}, {"x", "delete"}},
	{{"esc", "back"}, {"?", "help"}},
}

// reviewHintKeys are the keys shown in the header of the review queue.
var reviewHintKeys = [][]hint{
	{{"o", "open"}, {"enter", "task"}},
	{{"esc", "back"}, {"?", "help"}},
}

// column is the heading of a table column, and whether the column is right-aligned.
type column struct {
	name  string
	right bool
}

// icons are the headings drawn in place of a column's name, for a column whose cells are narrower
// than its name. The columns view and the saved layout keep the name.
var icons = map[string]string{"TIMER": "⏱", "SCHEDULE": "↻"}

// columnHead is the heading a table draws for a column.
func columnHead(col column) string {
	if icon, ok := icons[col.name]; ok {
		return icon
	}
	return col.name
}

// columns are the columns of the task table.
var columns = []column{
	{"TASK", false}, {"STATE", false}, {"PROFILE", false}, {"MODEL", false}, {"EFFORT", false}, {"PERSONA", false}, {"REPO", false}, {"TMUX", false}, {"AGE", true}, {"IDLE", true},
	{"CTX", true}, {"IN", true}, {"OUT", true}, {"CACHE", true}, {"COST", true}, {"TIMER", true}, {"SCHEDULE", false}, {"TITLE", false}, {"LAST", false},
}

// cell is one piece of text with its style. A row and a line of text are lists of cells.
type cell struct {
	text  string
	style lipgloss.Style
	// parts draw the text piece by piece, each in a style of its own, when there are some.
	parts []cell
}

func c(text string, style lipgloss.Style) cell { return cell{text: text, style: style} }

// cs is a cell drawn in parts: style is the one the row reads it by, such as dim for a task in Done.
func cs(style lipgloss.Style, parts ...cell) cell {
	var text strings.Builder
	for _, part := range parts {
		text.WriteString(part.text)
	}
	return cell{text: text.String(), style: style, parts: parts}
}

// treeCell draws before a task the branches of the tree down to it, and after it how many tasks it
// drives, in the color of its tree.
func treeCell(row format.Row, task cell) cell {
	drives := ""
	if row.Agent == nil {
		drives = format.Drives(row.Kids)
	}
	if row.Tree == "" && drives == "" {
		return task
	}
	team := teamStyle(row.Team)
	parts := []cell{c(row.Tree, team), task}
	if drives != "" {
		parts = append(parts, c(" "+drives, team))
	}
	return cs(task.style, parts...)
}

// clipStart cuts the start of a text wider than room, and marks the cut: the end of a path says
// more than its start.
func clipStart(text string, room int) string {
	if lipgloss.Width(text) <= room {
		return text
	}
	rest := []rune(text)
	for len(rest) > 0 && lipgloss.Width(string(rest))+1 > room {
		rest = rest[1:]
	}
	return "…" + string(rest)
}

// render joins cells into one styled line.
func render(cells []cell) string {
	var out strings.Builder
	for _, piece := range cells {
		out.WriteString(piece.style.Render(piece.text))
	}
	return out.String()
}

// taskStyle is the style of a task's state: a waiting task is drawn apart from one that works.
func taskStyle(t *api.Task) lipgloss.Style {
	if t.Waiting {
		return stateStyles["waiting"]
	}
	return stateStyles[t.State]
}

// contextCell is the context gauge of a task: the size, the share of the window and a bar that fills
// up, coloured by level.
func contextCell(ctx api.Context) cell {
	level := format.ContextLevel(ctx)
	if level == "empty" {
		return c("-", dim)
	}
	bar := []rune(gauge)[min(8, ctx.Pct*8/100+1)]
	return c(format.ContextCell(ctx)+" "+string(bar), levelStyles[level])
}

// loud is from how many tokens a column turns yellow, then red: input, output, cache.
var loud = [3][2]int{{1_000_000, 10_000_000}, {200_000, 1_000_000}, {20_000_000, 100_000_000}}

// tokenCells are the token cells of a task, louder as they grow, so the heavy tasks stand out.
func tokenCells(tokens usage.Tokens) []cell {
	in, out, cache := format.TokenCells(&tokens)
	counts := [3]int{tokens.In, tokens.Out, tokens.CacheRead + tokens.CacheWrite}
	cells := []cell{c(in, plain), c(out, plain), c(cache, plain)}
	for i, n := range counts {
		switch {
		case n >= loud[i][1]:
			cells[i].style = alarm
		case n >= loud[i][0]:
			cells[i].style = levelStyles["warm"]
		}
	}
	return cells
}

// rowCells are the table cells of one row: a task, or one of its child rows drawn as a branch under it.
func rowCells(row format.Row) []cell {
	empty := c("", plain)
	t := row.Task
	if a := row.Agent; a != nil {
		branch := format.Branch(row)
		in, out, cache := format.TokenCells(a.Tokens)
		cells := dimmed(t, []cell{
			empty, c(store.StateName(a.State), stateStyles[a.State]), empty, empty, empty, c(board.Clip(a.Persona, 26), plain), empty, empty, empty, empty, empty,
			c(in, dim), c(out, dim), c(cache, dim), empty, empty, empty,
			c(board.Clip(branch+" "+a.Name+": "+a.Desc, 70), plain), empty,
		})
		cells[0] = treeCell(row, cells[0])
		return cells
	}
	persona, personaStyle := t.Persona, plain
	if persona == "" {
		persona, personaStyle = "-", dim
	}
	title := format.PinMark(t) + board.Clip(t.Title, 60)
	if format.Closed(t) {
		title += " (closed)"
	}
	last := t.Last
	if t.State == "needs" {
		last = t.Ask
	}
	where, whereStyle := t.Tmux, plain
	if where == "" {
		where, whereStyle = "-", dim
	}
	profile, profileStyle := t.Profile, plain
	if profile == "" {
		profile, profileStyle = "-", dim
	}
	model, modelStyle := format.ModelCell(t.Model, t.SessionModel), plain
	if model == "" {
		model, modelStyle = "-", dim
	}
	effort, effortStyle := format.EffortCell(t.Effort, t.SessionEffort), plain
	if effort == "" {
		effort, effortStyle = "-", dim
	}
	timer, timerStyle := format.TimerCell(t.TimerLeft, t.DeleteLeft), plain
	switch {
	case strings.HasPrefix(timer, "✗"):
		timerStyle = alarm
	case timer == "":
		timer, timerStyle = "-", dim
	}
	schedule, scheduleStyle := format.ScheduleCell(t.Schedule), plain
	if schedule == "" {
		schedule, scheduleStyle = "-", dim
	}
	cells := []cell{
		c(t.Short, plain), c(t.StateName, taskStyle(t)), c(board.Clip(profile, 12), profileStyle), c(board.Clip(model, 10), modelStyle),
		c(effort, effortStyle),
		c(board.Clip(persona, 26), personaStyle),
		c(board.Clip(t.Repo, 16), plain), c(where, whereStyle), c(format.Human(t.Age), plain), c(format.Human(t.Idle), plain),
		contextCell(t.Context),
	}
	cells = append(cells, tokenCells(t.Tokens)...)
	cells = dimmed(t, append(cells, c(format.Money(t.Cost), plain), c(timer, timerStyle),
		c(schedule, scheduleStyle), c(title, plain), c(board.Clip(last, 80), dim)))
	cells[0] = treeCell(row, cells[0])
	return cells
}

// dimmed dims the cells of a task in Done, and of its child rows: one done a few minutes ago stays
// in sight that way before done tasks hide it, the others look the same once d shows them.
func dimmed(t *api.Task, cells []cell) []cell {
	if t.State == "done" {
		for i := range cells {
			cells[i].style = dim
		}
	}
	return cells
}

// A task the board sees move to Done stays lit up for glowFor: bright, then soft, then the grey of a
// done task. It only fades, it never blinks.
const (
	glowBright = 2 * time.Second
	glowFor    = 3500 * time.Millisecond
)

// glow lights up the cells of a task the board saw move to Done a moment ago, and of its child rows.
func (m model) glow(row format.Row, cells []cell) []cell {
	at, lit := m.lit[row.Task.ID]
	if !lit || row.Task.State != "done" {
		return cells
	}
	style := glowStyles[0]
	switch age := time.Since(at); {
	case age >= glowFor:
		return cells
	case age >= glowBright:
		style = glowStyles[1]
	}
	out := make([]cell, len(cells))
	for i, piece := range cells {
		out[i] = c(piece.text, style)
	}
	return out
}

// glowing says whether a cell is lit up: it keeps its colour on the cursor bar.
func glowing(style lipgloss.Style) bool {
	return reflect.DeepEqual(style, glowStyles[0]) || reflect.DeepEqual(style, glowStyles[1])
}

// faded says whether every cell of a row that shows text is dimmed, as the row of a done task is.
func faded(cells []cell) bool {
	shown := false
	for _, piece := range cells {
		if piece.text == "" {
			continue
		}
		if !reflect.DeepEqual(piece.style, dim) {
			return false
		}
		shown = true
	}
	return shown
}

// tickable says whether a row gets a box in select mode: every task does, in any state, a child row
// does not.
func tickable(row format.Row) bool {
	return row.Agent == nil
}

// checkbox is the cell left of a task id in select mode.
func (m model) checkbox(row format.Row) cell {
	switch {
	case !tickable(row):
		return c("", plain)
	case m.ticked[row.Task.ID]:
		return c("[x]", accent)
	}
	return c("[ ]", dim)
}

// columnWidths are the widths of the columns of a table: as wide as their heading and their widest cell.
func columnWidths(columns []column, rows [][]cell) []int {
	widths := make([]int, len(columns))
	for i, col := range columns {
		widths[i] = lipgloss.Width(columnHead(col))
	}
	for _, row := range rows {
		for i, piece := range row {
			widths[i] = max(widths[i], lipgloss.Width(piece.text))
		}
	}
	return widths
}

// tableLines draws the heading and the rows of the table, each cut to the width of the terminal.
// widths come from every row of a table that scrolls, so its columns keep still; widths that do not
// match the columns are taken from the rows drawn. cursor is the index of the highlighted row.
func tableLines(columns []column, widths []int, rows [][]cell, cursor, width int) []string {
	if len(widths) != len(columns) {
		widths = columnWidths(columns, rows)
	}
	line := func(cells []cell, background bool) string {
		var out strings.Builder
		readable := background && active.cursorText == "" && faded(cells)
		// look puts a style on the cursor bar, on the row under the cursor.
		look := func(style lipgloss.Style) lipgloss.Style {
			if !background {
				return style
			}
			if active.cursorText != "" && !glowing(style) { // a light cursor bar needs dark text on it
				style = style.Foreground(lipgloss.Color(active.cursorText))
			}
			return style.Background(selected)
		}
		for i, piece := range cells {
			if readable { // dim text all but vanishes on the cursor bar
				piece.style = plain
			}
			gap := plain
			if background {
				gap = plain.Background(selected)
				if active.cursorText != "" && !glowing(piece.style) {
					gap = gap.Foreground(lipgloss.Color(active.cursorText))
				}
			}
			if piece.parts == nil {
				style := look(piece.style).Width(widths[i])
				if columns[i].right {
					style = style.Align(lipgloss.Right)
				}
				out.WriteString(style.Render(piece.text))
			} else {
				for _, part := range piece.parts {
					if readable && reflect.DeepEqual(part.style, dim) {
						part.style = plain
					}
					out.WriteString(look(part.style).Render(part.text))
				}
				out.WriteString(gap.Render(strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(piece.text)))))
			}
			out.WriteString(gap.Render("  "))
		}
		// Truncated first: Width would wrap a long row to a second line instead of cutting it.
		text := plain.MaxWidth(width).Render(out.String())
		if background {
			if pad := width - lipgloss.Width(text); pad > 0 { // the highlight runs to the edge
				text += plain.Background(selected).Render(strings.Repeat(" ", pad))
			}
		}
		return text
	}
	head := make([]cell, len(columns))
	for i, col := range columns {
		head[i] = c(columnHead(col), heading)
	}
	lines := []string{line(head, false)}
	for i, row := range rows {
		lines = append(lines, line(row, i == cursor))
	}
	return lines
}

// header is the k9s-style block above the table: the facts on the left, key hints in the middle,
// the logo on the right. Hints and logo give way on a narrow terminal.
func (m model) header() []string {
	count := map[string]int{}
	for i := range m.tasks {
		count[m.tasks[i].State]++
	}
	pair := func(name string, value []cell) string {
		return render(append([]cell{c(fmt.Sprintf("%-*s", labelWidth, name+":"), accent)}, value...))
	}
	board := []cell{c(m.client.URL, plain)}
	if m.down != "" {
		board = []cell{c(m.client.URL+"  "+m.down+" (retrying)", alarm)}
	}
	tasks := fmt.Sprintf("%d of %d", m.shownTasks(), len(m.tasks))
	if hidden := m.hiddenTasks(); hidden > 0 && !m.showDone {
		tasks += fmt.Sprintf("  (%d hidden)", hidden)
	}
	needs := c(fmt.Sprintf("%d waiting for you", count["needs"]), dim)
	if count["needs"] > 0 {
		needs.style = stateStyles["needs"]
	}
	left := []string{
		pair("Board", board),
		pair("Rev", m.revRow()),
		pair("Tasks", []cell{c(tasks, plain)}),
		pair("Needs", []cell{needs}),
		pair("5h", m.limitRow("5h")),
		pair("7d", m.limitRow("7d")),
	}
	key := stateStyles["running"]
	var out []string
	hints := hintKeys
	switch {
	case m.view == viewBoard && m.selecting:
		hints = selectHintKeys
	case m.view == viewSchedules:
		hints = scheduleHintKeys
	case m.view == viewReview:
		hints = reviewHintKeys
	case m.view == viewColumns:
		hints = columnsHintKeys
	}
	// One account gets a panel like Claude Code's /status right of the hints, when it has the room;
	// the 5h and 7d rows of the left column then give way to it.
	// It is placed after the hints of the board in every view, so it shows at the same widths in all.
	shown := min(len(hintKeys), max(0, m.width-hintsFrom)/hintWidth)
	panelFrom := hintsFrom + hintWidth*shown + 2
	var panel []string
	if width := min(72, m.width-panelFrom-len(logo[0])-3); m.width >= 104 && width >= 40 && m.down == "" {
		panel = m.usagePanel(width)
	}
	if panel != nil {
		left[4], left[5] = "", ""
	}
	for i := 0; i < headerHeight; i++ {
		line := left[i]
		if i == 0 && m.down != "" { // the error must not be cut to the column
			out = append(out, plain.MaxWidth(m.width).Render(line))
			continue
		}
		if m.width >= 104 {
			// Cut, then padded: Width alone would wrap a long line onto a line of its own.
			line = plain.MaxWidth(hintsFrom - hintsGap).Render(line)
			line += strings.Repeat(" ", max(0, hintsFrom-lipgloss.Width(line)))
			var row []cell
			for n, col := range hints {
				if hintsFrom+hintWidth*(n+1) > m.width { // a column that does not fit is left out, not cut
					break
				}
				last := n == len(hints)-1 // nothing follows it but the logo: no padding to push that away
				switch {
				case i < len(col) && last:
					row = append(row, c(fmt.Sprintf("%-8s", "<"+col[i].key+">"), key), c(col[i].does, dim))
				case i < len(col):
					row = append(row, c(fmt.Sprintf("%-8s", "<"+col[i].key+">"), key), c(fmt.Sprintf("%-10s", col[i].does), dim))
				case !last: // a short column
					row = append(row, c(strings.Repeat(" ", hintWidth), plain))
				}
			}
			line += render(row)
		}
		if panel != nil {
			line += strings.Repeat(" ", max(0, panelFrom-lipgloss.Width(line))) + panel[i]
		}
		if free := m.width - lipgloss.Width(line) - lipgloss.Width(logo[i]); m.width >= 130 && free > 0 {
			line += strings.Repeat(" ", free) + accent.Render(logo[i])
		}
		out = append(out, plain.MaxWidth(m.width).Render(line))
	}
	return out
}

// meter is a thin bar that fills up with pct, to the nearest cell, on a dim track: the way Claude
// Code's /status draws a limit. Whole cells join the used part to the track the same way at any share;
// a share above none shows at least one cell, a share below all leaves one on the track, from 2 cells.
func meter(pct, cells int, style lipgloss.Style) []cell {
	pct = max(0, min(100, pct))
	used := (pct*cells + 50) / 100
	switch {
	case pct > 0 && used == 0:
		used = 1
	case pct < 100 && used == cells:
		used = cells - 1
	}
	return []cell{c(strings.Repeat("━", used), style), c(strings.Repeat("─", cells-used), dim)}
}

// limitStyles are the style of a limit's share, and what comes next for it with its style: the reset,
// or the limit when the current pace reaches it first.
func limitStyles(l limits.Entry) (style lipgloss.Style, next string, nextStyle lipgloss.Style) {
	style = levelStyles[l.Level]
	if l.Stale {
		style = dim
	}
	next, nextStyle = "resets in "+format.Human(l.ResetsIn), dim
	switch {
	case l.LimitIn != nil && *l.LimitIn <= 0 && !l.Stale:
		next, nextStyle = "limit reached", alarm
	case l.LimitIn != nil && !l.Stale:
		next, nextStyle = "limit in "+format.Human(*l.LimitIn), alarm
	}
	return style, next, nextStyle
}

// oneAccount is whether the limits are those of one account, with one figure per window. Profiles
// that share an account are joined with +, and a window can be reported by more of them than another
// one, personal next to personal+work: entries whose profiles overlap are the same account.
func (m model) oneAccount() bool {
	if len(m.limits) == 0 {
		return false
	}
	labels := map[string]bool{}
	for _, l := range m.limits {
		if labels[l.Label] {
			return false
		}
		labels[l.Label] = true
	}
	joined := map[string]bool{}
	for _, name := range strings.Split(m.limits[0].Profile, "+") {
		joined[name] = true
	}
	done := map[int]bool{0: true}
	for grew := true; grew; {
		grew = false
		for i, l := range m.limits {
			names := strings.Split(l.Profile, "+")
			if done[i] || !slices.ContainsFunc(names, func(n string) bool { return joined[n] }) {
				continue
			}
			for _, name := range names {
				joined[name] = true
			}
			done[i], grew = true, true
		}
	}
	return len(done) == len(m.limits)
}

// named is whether the windows of one account are reported by different profiles, so that each says
// which.
func (m model) named() bool {
	return slices.ContainsFunc(m.limits, func(l limits.Entry) bool { return l.Profile != m.limits[0].Profile })
}

// usagePanel is the usage of one account the way Claude Code's /status shows it, six lines of width
// cells: per window a name, a long meter with the share used, and the reset. Nil for several accounts.
// When the windows are reported by different profiles of the account, each name says which.
func (m model) usagePanel(width int) []string {
	if !m.oneAccount() {
		return nil
	}
	named := m.named()
	var out []string
	for _, w := range []struct{ label, name string }{{"5h", "Current session · 5h"}, {"7d", "Current week · 7d"}} {
		title := render([]cell{c(w.name, plain)})
		found := false
		for _, l := range m.limits {
			if l.Label != w.label {
				continue
			}
			found = true
			if named {
				title = plain.MaxWidth(width).Render(render([]cell{c(w.name+" · ", plain), c(l.Profile, dim)}))
			}
			out = append(out, title)
			style, next, nextStyle := limitStyles(l)
			// The share is padded so the two meters end in the same column.
			used := fmt.Sprintf("  %3d%% used", l.Pct)
			out = append(out, render(append(meter(l.Pct, width-len("  100% used"), style), c(used, style))),
				render([]cell{c(strings.ToUpper(next[:1])+next[1:], nextStyle)}))
		}
		if !found {
			out = append(out, title, render([]cell{c("n/a", dim)}), "")
		}
	}
	return out
}

// limitRow is one usage window in the left column of the header, when the panel has no room: a short
// meter, the share used and when it resets, or when the limit is reached when that comes first.
// Several accounts share the line, without meters. Profiles of one account that differ per window get
// a shorter meter and their name.
func (m model) limitRow(label string) []cell {
	several, named := !m.oneAccount(), m.named()
	var row []cell
	for _, l := range m.limits {
		if l.Label != label {
			continue
		}
		style, next, nextStyle := limitStyles(l)
		if several { // no room for a meter
			next = strings.NewReplacer("resets in ", "", "limit in ", "limit ").Replace(next)
			row = append(row, c(l.Profile+" ", plain), c(fmt.Sprintf("%d%%", l.Pct), style),
				c(" (", dim), c(next, nextStyle), c(")  ", dim))
			continue
		}
		cells := 18
		if named {
			cells = 10
		}
		row = append(row, meter(l.Pct, cells, style)...)
		row = append(row, c(fmt.Sprintf(" %3d%%", l.Pct), style), c("  "+strings.TrimPrefix(next, "resets in "), nextStyle))
		if named {
			row = append(row, c("  "+l.Profile, dim))
		}
	}
	if len(row) == 0 {
		row = append(row, c("n/a", dim))
	}
	// Next to the hints, the row after its "5h:" label ends before them: the words give way first.
	if m.width >= 104 && lipgloss.Width(render(row)) > hintsFrom-hintsGap-labelWidth {
		short := strings.NewReplacer("limit in ", "limit ", "limit reached", "limit hit")
		for i := range row {
			row[i].text = short.Replace(row[i].text)
		}
	}
	return row
}

// shownTasks is how many tasks the table shows, child rows not counted.
func (m model) shownTasks() int {
	shown := 0
	for _, row := range m.rows {
		if row.Agent == nil {
			shown++
		}
	}
	return shown
}

// hiddenTasks counts the done tasks hidden from the board.
func (m model) hiddenTasks() int {
	hidden := 0
	for i := range m.tasks {
		if m.tasks[i].State == "done" && m.tasks[i].Hidden {
			hidden++
		}
	}
	return hidden
}

// tokensLine is the bottom line: what every task together has consumed.
func tokensLine(tasks []api.Task) []cell {
	var used usage.Tokens
	cost := 0.0
	for i := range tasks {
		used = used.Add(tasks[i].Tokens)
		cost += tasks[i].Cost
	}
	line := []cell{c(fmt.Sprintf("tokens in %s out %s cache %s",
		format.Count(used.In), format.Count(used.Out), format.Count(used.CacheRead+used.CacheWrite)), dim)}
	if cost > 0 {
		line = append(line, c("  cost "+format.Money(cost), dim))
	}
	return line
}

// boxTitle is the title on the table's border, k9s style: what the table shows and how many tasks.
func (m model) boxTitle() []cell {
	scope := "shown"
	if m.showDone {
		scope = "all"
	}
	if m.needle != "" {
		scope = "/" + m.needle
	}
	if m.order != "state" {
		scope += ", by " + m.order
	}
	title := []cell{c("tasks", stateStyles["parked"].Bold(true)), c("(", plain),
		c(scope, stateStyles["review"]), c(")", plain), c(fmt.Sprintf("[%d]", m.shownTasks()), plain)}
	if m.selecting {
		title = append(title, c(fmt.Sprintf(" select: %d ticked", len(m.ticked)), accent))
	}
	return title
}

// boxed draws lines inside a border with a centered title on top, the way k9s frames its tables.
func boxed(title []cell, lines []string, width int) []string {
	frame := stateStyles["running"]
	inner := max(10, width-2)
	text := render(title)
	tw := lipgloss.Width(text)
	top := frame.Render("┌" + strings.Repeat("─", inner) + "┐")
	if tw+2 <= inner {
		leftPad := (inner - tw - 2) / 2
		top = frame.Render("┌"+strings.Repeat("─", leftPad)) + " " + text + " " +
			frame.Render(strings.Repeat("─", inner-leftPad-tw-2)+"┐")
	}
	out := []string{top}
	pad := plain.Width(inner).MaxWidth(inner)
	for _, line := range lines {
		out = append(out, frame.Render("│")+pad.Render(line)+frame.Render("│"))
	}
	return append(out, frame.Render("└"+strings.Repeat("─", inner)+"┘"))
}

// describe is the detail view of one task, as returned by GET /api/tasks/{id}.
func describe(t *api.Task) []cell {
	out := []cell{c(t.Title+"\n\n", bold)}
	add := func(text string, style lipgloss.Style) { out = append(out, c(text, style)) }
	label := func(name string) { add(fmt.Sprintf("  %-8s ", name), dim) }
	state := t.StateName
	if format.Closed(t) {
		state += " (closed)"
	}
	persona := t.Persona
	if persona == "" {
		persona = "-"
	}
	where := map[string]string{"hc": "the board's own tmux, Ctrl-] detaches", "own": "your tmux", "herdr": "Herdr"}[t.Tmux]
	if where == "" {
		where = "-"
	}
	lines := [][2]string{
		{"task", t.ID}, {"state", state}, {"repo", t.Repo}, {"branch", t.Branch}, {"dir", t.Cwd},
		{"tmux", where}, {"profile", t.Profile}, {"persona", persona},
	}
	if t.Parent != "" {
		lines = append(lines, [2]string{"parent", board.Short(t.Parent)})
	}
	for _, line := range append(lines, [2]string{"resume", t.Resume}) {
		label(line[0])
		style := plain
		if line[0] == "state" {
			style = taskStyle(t)
		}
		add(line[1]+"\n", style)
	}
	if t.Context.Tokens > 0 {
		label("context")
		add(fmt.Sprintf("%s of %s (%d%%)\n", format.Count(t.Context.Tokens), format.Count(t.Context.Window), t.Context.Pct),
			levelStyles[format.ContextLevel(t.Context)])
	}
	if t.Cost > 0 {
		label("cost")
		add(format.Money(t.Cost)+" at API prices\n", plain)
	}
	if t.Tokens.Total() > 0 {
		label("tokens")
		add(format.TokenLine(t.Tokens)+"\n", plain)
		add(fmt.Sprintf("  %-8s of which sub-agents: %s\n", "", format.TokenLine(t.AgentTokens)), dim)
	}
	if t.State == "needs" {
		add("\n  needs you: "+t.Ask+"\n", stateStyles["needs"])
	}
	detail := t.Detail
	if detail == nil {
		detail = &api.Detail{}
	}
	if h := detail.Handoff; h != nil && h.Text != "" {
		add("\nHandoff\n", bold)
		for i, line := range format.HandoffLines(*h, time.Now()) {
			style := plain
			if i == 0 {
				line, style = strings.TrimPrefix(line, "handoff  "), dim
			}
			add("  "+line+"\n", style)
		}
	}
	add("\nHistory\n", bold)
	for _, move := range detail.History {
		add("  "+time.Unix(int64(move.At), 0).Format("2006-01-02 15:04:05")+"  ", dim)
		add(fmt.Sprintf("%-9s", store.StateNames[move.State]), stateStyles[move.State])
		add(" by "+move.By+"\n", dim)
	}
	if len(t.Agents) > 0 {
		add("\nSub-agents and background jobs\n", bold)
	}
	for _, a := range t.Agents {
		add(fmt.Sprintf("  %-8s", store.StateName(a.State)), stateStyles[a.State])
		used := ""
		if a.Tokens != nil {
			used = "  [" + format.TokenLine(*a.Tokens) + "]"
		}
		add(fmt.Sprintf(" %s: %s (%s)%s\n", a.Name, a.Desc, format.Human(a.Age), used), plain)
	}
	add("\nSession log\n", bold)
	if len(detail.Log) == 0 {
		add("  Nothing yet\n", dim)
	}
	for _, turn := range detail.Log {
		who := format.Who(turn)
		style := whoStyles[turn.Who]
		if turn.From != "" {
			style = whoStyles["from"]
		}
		add(fmt.Sprintf("  %-5s | ", who), style)
		style = plain
		if turn.Who == "tool" {
			style = dim
		}
		add(strings.ReplaceAll(turn.Text, "\n", "\n        | ")+"\n", style)
		if gap := turn.Gap(); gap != "" {
			add("        | "+gap+"\n", dim)
		}
	}
	return out
}

// renderLines renders cells that span several lines, one line at a time, so that a style never
// runs across a line break.
func renderLines(cells []cell) string {
	var out strings.Builder
	for _, piece := range cells {
		for i, part := range strings.Split(piece.text, "\n") {
			if i > 0 {
				out.WriteByte('\n')
			}
			if part != "" {
				out.WriteString(piece.style.Render(part))
			}
		}
	}
	return out.String()
}

const (
	pitchWidth    = 40
	pitchAnchor   = 26 // the column the ball settles at, once the view starts following it
	metersPerTick = 2  // at one frame every 0.06 s that is a 120 km/h throw
)

var (
	pitcher  = [3]string{" O ", `/|\`, `/ \`}
	pitching = [3]string{" O ", "/|-", `/ \`}
)

// pitchFrame is one frame of the waiting box: the ball leaves the pitcher's hand, then the view
// follows it and the ground rolls by.
func pitchFrame(ticks int) string {
	scroll := max(0, ticks-pitchAnchor)
	ball := 3 + ticks - scroll
	figure := pitcher
	if ticks < 5 {
		figure = pitching
	}
	ground := dim.Foreground(lipgloss.Color(active.done))
	var out strings.Builder
	out.WriteString("Your question is being thrown to the agents,\nwaiting for the ball to arrive.\n\n")
	for row := 0; row < 3; row++ {
		for col := 0; col < pitchWidth; col++ {
			world := col + scroll // the pitcher stands at world columns 0 to 2
			switch {
			case world < 3:
				out.WriteString(stateStyles["parked"].Render(string([]rune(figure[row])[world])))
			case row == 1 && col == ball:
				out.WriteString(bold.Render("o"))
			case row == 1 && ball-9 < col && col < ball && world%2 == 0:
				out.WriteString(dim.Render("-")) // the trail
			case row == 2 && world%10 == 0:
				out.WriteString(ground.Render("|")) // a post every 20 m
			case row == 2:
				out.WriteString(ground.Render("."))
			default:
				out.WriteByte(' ')
			}
		}
		out.WriteByte('\n')
	}
	out.WriteString(bold.Render(fmt.Sprintf("%*d m", pitchWidth-20, ticks*metersPerTick)))
	out.WriteString(dim.Render("     esc to give up"))
	return out.String()
}

// titled draws a box with a title above its content.
func titled(title, content string) string {
	return box.Render(accent.Render(title) + "\n" + content)
}

// revRow is this board's version, and the daemon's next to it when they differ.
func (m model) revRow() []cell {
	if m.daemon == "" || m.daemon == version.Version {
		return []cell{c(version.Version, plain)}
	}
	return []cell{c(version.Version, plain), c("  daemon "+m.daemon, alarm)}
}
