package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/format"
)

// switchHelp is the line under the switcher of recent tasks.
const switchHelp = "tab or j next, shift+tab or k back, enter, a or the number go, esc closes"

// recentMax is how many tasks the switcher lists: one key, 1 to 9 then 0, enters each.
const recentMax = 10

// switcher is the box of recent tasks: the tasks you went into, the most recent first, to go back to
// one in a key or two, the way Ctrl-Tab goes back to a browser tab. It keeps the list it opened with.
type switcher struct {
	tasks   []api.Task
	cursor  int
	current string // the task the switcher was opened from, if any
}

// recent is the last recentMax tasks you went into, the most recent first, without the done ones you
// hid.
func recent(tasks []api.Task) []api.Task {
	var out []api.Task
	for _, t := range tasks {
		if t.SeenAt > 0 && !t.Hidden {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SeenAt > out[j].SeenAt })
	return out[:min(len(out), recentMax)]
}

// newSwitcher opens the switcher on the tasks of the board. current is the task it is opened from:
// the cursor starts on the one you were in before it, as Ctrl-Tab does.
func newSwitcher(tasks []api.Task, current string) switcher {
	sw := switcher{tasks: recent(tasks), current: current}
	if len(sw.tasks) > 1 && sw.tasks[0].ID == current {
		sw.cursor = 1
	}
	return sw
}

// Previous is the task to go straight back to from current, the way Ctrl-Tab goes back to the last
// browser tab: the one the switcher opens on. Nil when you entered no other task.
func Previous(tasks []api.Task, current string) *api.Task {
	sw := newSwitcher(tasks, current)
	if len(sw.tasks) == 0 || sw.tasks[sw.cursor].ID == current {
		return nil
	}
	return &sw.tasks[sw.cursor]
}

// key moves the cursor, and says whether the switcher is done: closed, or with the task picked.
func (sw *switcher) key(key string) (done bool, picked *api.Task) {
	n := len(sw.tasks)
	switch key {
	case "esc", "q", "ctrl+\\":
		return true, nil
	case "enter", "a":
		if n == 0 {
			return true, nil
		}
		return true, &sw.tasks[sw.cursor]
	case "tab", "j", "down", "ctrl+n":
		if n > 0 {
			sw.cursor = (sw.cursor + 1) % n
		}
	case "shift+tab", "k", "up", "ctrl+p":
		if n > 0 {
			sw.cursor = (sw.cursor + n - 1) % n
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "0":
		if i := (int(key[0]-'0') + 9) % 10; i < n {
			return true, &sw.tasks[i]
		}
	}
	return false, nil
}

// switchColumns are the columns of the switcher: the title last, so a narrow box cuts only its end.
var switchColumns = []column{{"#", true}, {"TASK", false}, {"STATE", false}, {"SEEN", true}, {"REPO", false},
	{"TITLE", false}}

// lines draws the list as a table, width wide at most and height lines high with its heading, the
// cursor in sight.
func (sw switcher) lines(width, height int, now time.Time) []string {
	if len(sw.tasks) == 0 {
		return []string{dim.Render("No task entered yet. The tasks you enter show here, the latest first.")}
	}
	room := max(1, height-1)
	top := max(0, min(sw.cursor-room/2, len(sw.tasks)-room))
	var rows [][]cell
	for i := top; i < min(len(sw.tasks), top+room); i++ {
		t := &sw.tasks[i]
		number := fmt.Sprint((i + 1) % 10) // 0 for the tenth, the last
		seen := c(format.Human(max(0, int(float64(now.Unix())-t.SeenAt))), dim)
		if t.ID == sw.current {
			seen = c("here", accent)
		}
		rows = append(rows, []cell{c(number, dim), c(t.Short, plain), c(t.StateName, taskStyle(t)), seen,
			c(board.Clip(t.Repo, 20), dim), c(t.Title, plain)})
	}
	return tableLines(switchColumns, nil, rows, sw.cursor-top, width)
}

// switchView is the switcher in a box at the center of the board.
func (m model) switchView() string {
	width := min(100, max(30, m.width-6))
	lines := m.sw.lines(width, max(3, m.height-8), time.Now())
	return m.centered(titled("Recent tasks", strings.Join(lines, "\n")+"\n"+dim.Render(switchHelp)))
}

// --- the switcher on its own, in a tmux popup over a session -----------------------------

// popup is the program `hc switch` runs: the switcher alone, over the whole terminal it is given.
type popup struct {
	client  *client.Client
	current string
	sw      switcher
	loaded  bool
	err     error
	picked  *api.Task
	width   int
	height  int
}

func (p popup) Init() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		out, err := c.Board()
		return boardMsg{out, err}
	}
}

func (p popup) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case boardMsg:
		if msg.err != nil {
			p.err = msg.err
			return p, tea.Quit
		}
		p.sw, p.loaded = newSwitcher(msg.board.Tasks, p.current), true
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return p, tea.Quit
		}
		if !p.loaded {
			if key == "esc" || key == "q" {
				return p, tea.Quit
			}
			return p, nil
		}
		if done, picked := p.sw.key(key); done {
			p.picked = picked
			return p, tea.Quit
		}
	}
	return p, nil
}

func (p popup) View() string {
	if !p.loaded {
		return dim.Render(" Loading the recent tasks")
	}
	lines := []string{" " + accent.Render("Recent tasks")}
	for _, line := range p.sw.lines(max(20, p.width-1), max(1, p.height-3), time.Now()) {
		lines = append(lines, " "+line)
	}
	return strings.Join(append(lines, " "+dim.Render(switchHelp)), "\n")
}

// Switch shows the switcher of recent tasks over the whole terminal, and returns the task picked; nil
// when it was closed. current is the task it is opened from, empty when none.
func Switch(c *client.Client, theme, current string) (*api.Task, error) {
	if err := setTheme(theme); err != nil {
		return nil, err
	}
	final, err := tea.NewProgram(popup{client: c, current: current}, tea.WithAltScreen()).Run()
	if err != nil {
		return nil, err
	}
	p := final.(popup)
	return p.picked, p.err
}
