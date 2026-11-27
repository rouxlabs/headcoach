package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rouxlabs/headcoach/internal/api"
)

// seenTasks are four tasks: two entered, b the latest, one never entered, and a hidden done one
// entered last of all.
func seenTasks() []api.Task {
	a, b, never, hidden := task("aaaa0001", "running", 1), task("bbbb0001", "review", 1),
		task("nevr0001", "running", 1), task("hide0001", "done", 1)
	a.SeenAt, b.SeenAt, hidden.SeenAt = 100, 300, 400
	a.Live, b.Live = true, true
	hidden.Hidden = true
	return []api.Task{a, never, b, hidden}
}

func ids(tasks []api.Task) []string {
	var out []string
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

func TestTheSwitcherListsTheTasksYouEnteredTheLatestFirst(t *testing.T) {
	sw := newSwitcher(seenTasks(), "")
	if got := strings.Join(ids(sw.tasks), " "); got != "bbbb0001 aaaa0001" || sw.cursor != 0 {
		t.Fatalf("listed %s, cursor %d", got, sw.cursor)
	}
	// Opened from inside the latest, the cursor starts on the one before it, as Ctrl-Tab does.
	if sw := newSwitcher(seenTasks(), "bbbb0001"); sw.cursor != 1 {
		t.Errorf("opened from the latest, the cursor is on %d", sw.cursor)
	}
	if sw := newSwitcher(seenTasks(), "aaaa0001"); sw.cursor != 0 {
		t.Errorf("opened from an older one, the cursor is on %d", sw.cursor)
	}

	// The cursor goes round both ways.
	for _, step := range []struct {
		key    string
		cursor int
	}{{"tab", 1}, {"tab", 0}, {"shift+tab", 1}, {"k", 0}, {"down", 1}, {"9", 1}} {
		if done, _ := sw.key(step.key); done || sw.cursor != step.cursor {
			t.Errorf("after %s: done %v, cursor %d, want %d", step.key, done, sw.cursor, step.cursor)
		}
	}
	for _, key := range []string{"enter", "a"} {
		if done, picked := sw.key(key); !done || picked == nil || picked.ID != "aaaa0001" {
			t.Errorf("%s picked %v", key, picked)
		}
	}
	if done, picked := sw.key("1"); !done || picked == nil || picked.ID != "bbbb0001" {
		t.Errorf("1 picked %v", picked)
	}
	for _, key := range []string{"esc", "q", "ctrl+\\"} {
		if done, picked := sw.key(key); !done || picked != nil {
			t.Errorf("%s: done %v, picked %v", key, done, picked)
		}
	}

	empty := newSwitcher(nil, "")
	if done, picked := empty.key("tab"); done || picked != nil {
		t.Error("tab on an empty switcher did something")
	}
	if done, picked := empty.key("enter"); !done || picked != nil {
		t.Error("enter on an empty switcher did not just close it")
	}
	if lines := empty.lines(80, 10, time.Unix(500, 0)); len(lines) != 1 || !strings.Contains(lines[0], "No task entered yet") {
		t.Errorf("an empty switcher shows %q", lines)
	}
}

// twelveTasks are twelve tasks entered, task00a1 the latest, task00l1 the oldest.
func twelveTasks() []api.Task {
	var tasks []api.Task
	for i := range 12 {
		one := task("task00"+string(rune('a'+i))+"1", "running", 1)
		one.SeenAt = float64(1000 - i)
		tasks = append(tasks, one)
	}
	return tasks
}

func TestTheSwitcherListsTheLastTen(t *testing.T) {
	sw := newSwitcher(twelveTasks(), "")
	if len(sw.tasks) != 10 || sw.tasks[9].ID != "task00j1" {
		t.Fatalf("listed %v", ids(sw.tasks))
	}
	// 0 enters the tenth, as the number in its row says.
	if done, picked := sw.key("0"); !done || picked == nil || picked.ID != "task00j1" {
		t.Errorf("0 picked %v", picked)
	}
	two := newSwitcher(seenTasks(), "")
	if done, picked := two.key("0"); done || picked != nil {
		t.Errorf("0 with two tasks: done %v, picked %v", done, picked)
	}
}

func TestTheSwitcherKeepsTheCursorInSight(t *testing.T) {
	sw := newSwitcher(twelveTasks(), "")
	sw.cursor = 9
	lines := sw.lines(120, 5, time.Unix(1000, 0))
	all := strings.Join(lines, "\n")
	if len(lines) != 5 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "#") ||
		!strings.Contains(lines[4], "task00j1") || strings.Contains(all, "task00a1") {
		t.Errorf("the last task out of sight:\n%s", all)
	}
	// Each has the number that enters it, 0 for the tenth.
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "7 ") || !strings.HasPrefix(strings.TrimSpace(lines[4]), "0 ") {
		t.Errorf("the numbers are wrong:\n%s", all)
	}
}

func TestPreviousIsTheTaskEnteredBefore(t *testing.T) {
	for _, step := range []struct{ current, want string }{
		{"bbbb0001", "aaaa0001"}, // from the latest, the one before it
		{"aaaa0001", "bbbb0001"}, // and back
		{"", "bbbb0001"},         // from no task, the latest
		{"nevr0001", "bbbb0001"}, // from one never entered, the latest
	} {
		if got := Previous(seenTasks(), step.current); got == nil || got.ID != step.want {
			t.Errorf("Previous from %q = %v, want %s", step.current, got, step.want)
		}
	}
	only := seenTasks()[:2] // aaaa0001 entered, nevr0001 not
	if got := Previous(only, "aaaa0001"); got != nil {
		t.Errorf("Previous with no other task entered = %v", got.ID)
	}
	if got := Previous(nil, ""); got != nil {
		t.Errorf("Previous with no task = %v", got.ID)
	}
}

func TestTabOpensTheSwitcherOverTheBoard(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, seenTasks()...)
	m, _ = press(t, m, "tab")
	if m.view != viewSwitch {
		t.Fatalf("tab opened view %d", m.view)
	}
	view := m.View()
	for _, piece := range []string{"Recent tasks", "bbbb0001", "aaaa0001", "esc closes"} {
		if !strings.Contains(view, piece) {
			t.Errorf("the switcher lacks %q:\n%s", piece, view)
		}
	}
	if strings.Contains(view, "nevr0001") || strings.Contains(view, "hide0001") {
		t.Errorf("the switcher lists a task never entered, or a hidden one:\n%s", view)
	}
	if strings.Index(view, "bbbb0001") > strings.Index(view, "aaaa0001") {
		t.Errorf("the latest is not first:\n%s", view)
	}

	// esc closes it; Ctrl-\ opens it too, and enter goes into the task, the way a does.
	m, _ = press(t, m, "esc")
	if m.view != viewBoard {
		t.Fatalf("esc left view %d", m.view)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlBackslash})
	m = next.(model)
	if m.view != viewSwitch {
		t.Fatalf("Ctrl-\\ opened view %d", m.view)
	}
	m, cmd := press(t, m, "tab", "enter")
	if m.view != viewBoard || cmd == nil {
		t.Fatalf("enter: view %d, command %v", m.view, cmd)
	}
	run(t, m, cmd)
	want := []string{`PATCH /api/tasks/aaaa0001 {"seen":true}`, "POST /api/tasks/aaaa0001/focus"}
	if strings.Join(d.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("entering from the switcher called %q, want %q", d.calls, want)
	}
}

func TestThePopupSwitcher(t *testing.T) {
	p := popup{current: "bbbb0001"}
	if view := p.View(); !strings.Contains(view, "Loading") {
		t.Errorf("before the board comes, the popup shows %q", view)
	}
	next, _ := p.Update(tea.WindowSizeMsg{Width: 90, Height: 12})
	next, _ = next.Update(boardMsg{board: api.Tasks{Tasks: seenTasks()}})
	p = next.(popup)
	view := p.View()
	if !strings.Contains(view, "Recent tasks") || !strings.Contains(view, "here") || !strings.Contains(view, "aaaa0001") {
		t.Errorf("the popup shows:\n%s", view)
	}
	next, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	p = next.(popup)
	if p.picked == nil || p.picked.ID != "aaaa0001" || cmd == nil {
		t.Errorf("enter picked %v", p.picked)
	}

	// A daemon that does not answer ends it with the error.
	down := errors.New("no daemon")
	next, cmd = popup{}.Update(boardMsg{err: down})
	if next.(popup).err != down || cmd == nil {
		t.Error("the popup lost the error of the daemon")
	}
}
