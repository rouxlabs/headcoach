package tui

import (
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/format"
)

// teamMsg carries the coordinator view, after a poll.
type teamMsg struct {
	id   string // the coordinator it was asked for
	team api.Team
	err  error
}

var teamColumns = []column{
	{"TASK", false}, {"STATE", false}, {"IDLE", true}, {"QUEUE", false}, {"TITLE", false}, {"WHERE IT STANDS", false},
}

const teamHelp = `j / k, arrows   move the cursor
g / G           go to the top or the bottom
enter           show the task on the board
esc or q        back to the board

A coordinator's view: the tasks it drives, at any depth, and what waits on you among them. A task
waits on you in Needs me, in Review, or when its handoff file has a "- on you:" line under Waiting.
WHERE IT STANDS is the status of its handoff, else its question or its last line.`

func (m model) loadTeam() tea.Cmd {
	c, id := m.client, m.teamID
	return func() tea.Msg {
		var team api.Team
		err := c.Call("GET", "/api/tasks/"+id+"/team", nil, &team)
		return teamMsg{id: id, team: team, err: err}
	}
}

// teamOf is the coordinator w shows for a task: the task itself when it drives others, else its parent.
func (m model) teamOf(t *api.Task) string {
	for i := range m.tasks {
		if m.tasks[i].Parent == t.ID {
			return t.ID
		}
	}
	if t.Parent != "" {
		return t.Parent
	}
	return t.ID
}

// under returns the ids of the tasks under a task at any depth, among those on the board.
func (m model) under(id string) []string {
	var out []string
	for queue := []string{id}; len(queue) > 0; queue = queue[1:] {
		for i := range m.tasks {
			if t := &m.tasks[i]; t.Parent == queue[0] && t.ID != id && !slices.Contains(out, t.ID) {
				out, queue = append(out, t.ID), append(queue, t.ID)
			}
		}
	}
	return out
}

// deleteOrder puts the ticked tasks under another ticked one first: deleting a task deletes the tasks
// under it, so one after it would be gone already. It says how many more tasks go with them.
func (m model) deleteOrder(ids []string) string {
	under, more := map[string]int{}, map[string]bool{}
	for _, id := range ids {
		kids := m.under(id)
		under[id] = len(kids)
		for _, kid := range kids {
			more[kid] = true
		}
	}
	for _, id := range ids {
		delete(more, id)
	}
	// A task has fewer tasks under it than any task above it.
	slices.SortStableFunc(ids, func(a, b string) int { return under[a] - under[b] })
	if len(more) == 0 {
		return ""
	}
	return " with " + format.Counted(len(more), "more task") + " under them"
}

// teamKey handles a key on the coordinator view.
func (m model) teamKey(key string) (tea.Model, tea.Cmd) {
	members := m.team.Members
	switch key {
	case "Q":
		return m, tea.Quit
	case "esc", "q", "w":
		m.view = viewBoard
	case "j", "down":
		m.teamCursor = min(m.teamCursor+1, len(members)-1)
	case "k", "up":
		m.teamCursor = max(m.teamCursor-1, 0)
	case "g", "home":
		m.teamCursor = 0
	case "G", "end":
		m.teamCursor = max(0, len(members)-1)
	case "?":
		m.note("Coordinator keys (any key closes)", teamHelp, nil)
	case "enter":
		if m.teamCursor < len(members) {
			m.view, m.jump = viewBoard, members[m.teamCursor].ID
			m.rebuild()
		}
	}
	return m, nil
}

// teamCells are the cells of one task of the coordinator view, its tree in the color of team.
func teamCells(mem api.Member, team string) []cell {
	queue, queueStyle := format.QueueCell(mem.Handoff.Queue), plain
	if queue == "" {
		queue, queueStyle = "-", dim
	}
	where, whereStyle := format.Where(mem), plain
	if mem.Handoff.Status == "" {
		whereStyle = dim
	}
	cells := dimmed(&mem.Task, []cell{
		c(mem.Short, plain), c(mem.StateName, taskStyle(&mem.Task)), c(format.Human(mem.Idle), plain), c(queue, queueStyle),
		c(board.Clip(mem.Title, 50), plain), c(board.Clip(where, 100), whereStyle),
	})
	cells[0] = treeCell(format.Row{Task: &mem.Task, Tree: mem.Tree, Kids: mem.Kids, Team: team}, cells[0])
	return cells
}

// teamColor is the tree the coordinator view's task is in on the board, to draw its own in that color.
func (m model) teamColor() string {
	for _, row := range m.rows {
		if row.Agent == nil && row.Task.ID == m.team.Task.ID {
			return row.Team
		}
	}
	return m.team.Task.ID
}

func (m model) teamView() string {
	t := m.team.Task
	var lines []string
	if len(m.team.OnYou) > 0 {
		lines = append(lines, render([]cell{c(" Waits on you", stateStyles["needs"])}))
		for _, o := range m.team.OnYou {
			lines = append(lines, render([]cell{c("  "+o.Short+"  ", plain), c(board.Clip(o.Title, 40)+"  ", plain),
				c(board.Clip(format.OnYouWhat(o), max(20, m.width-60)), plain)}))
		}
		lines = append(lines, "")
	}
	// The rows scroll with the cursor, under what waits on you.
	height := max(3, m.tableHeight()-len(lines))
	top := max(0, m.teamCursor-height+1)
	var rows [][]cell
	for _, mem := range m.team.Members[top:min(len(m.team.Members), top+height)] {
		rows = append(rows, teamCells(mem, m.teamColor()))
	}
	lines = append(lines, tableLines(teamColumns, m.teamWidths, rows, m.teamCursor-top, max(10, m.width-2))...)
	if len(m.team.Members) == 0 {
		lines = append(lines, dim.Render(" No task under it. Put one there with: hc new --parent "+t.Short+", or hc link <task> "+t.Short))
	}
	name := t.Short + " " + board.Clip(t.Title, 50)
	if t.ID == "" { // its first answer is on its way
		name = board.Short(m.teamID)
	}
	title := []cell{c("team", accent.Bold(true)), c(fmt.Sprintf("(%s)[%d]", name, len(m.team.Members)), plain)}
	bottom := []cell{c(t.StateName, taskStyle(&t))}
	if status := m.team.Handoff.Status; status != "" {
		bottom = append(bottom, c("  "+board.Clip(status, max(20, m.width-20)), dim))
	}
	return m.frame(title, lines, bottom)
}
