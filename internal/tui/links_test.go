package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/transcript"
)

const (
	coordID  = "c0ffee01-0000-4000-8000-000000000001"
	workerID = "a1b2c3d4-0000-4000-8000-000000000002"
	oldID    = "d0d0d0d0-0000-4000-8000-000000000003"
)

// linked is a board with a coordinator, a task it drives, and a hidden done task, the detail of the
// coordinator open on a log that names the other two.
func linked(t *testing.T, width, height int, log ...string) model {
	t.Helper()
	coord, worker, old := task(coordID, "running", 1), task(workerID, "review", 1), task(oldID, "done", 1)
	old.Hidden = true
	m := loaded(t, nil, coord, worker, old)
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	m.jump = coordID
	m.rebuild()
	m, _ = press(t, m, "enter")
	coord.Detail = &api.Detail{}
	for _, line := range log {
		coord.Detail.Log = append(coord.Detail.Log, transcript.Turn{Who: "agent", Text: line})
	}
	return send(t, m, detailMsg{task: coord})
}

func linkIDs(links []link) []string {
	var out []string
	for _, l := range links {
		out = append(out, l.id)
	}
	return out
}

// screenAt is the text the detail shows on screen line y, from column from to column to.
func screenAt(m model, y, from, to int) string {
	line := []rune(escapes.ReplaceAllString(strings.Split(m.View(), "\n")[y], ""))
	return string(line[from:min(to, len(line))])
}

// The ids of the tasks on the board are links, short or whole, Done tasks too. Other hex is not, nor
// the task the detail shows.
func TestLinksMatchTaskIDsOnly(t *testing.T) {
	m := loaded(t, nil, task(coordID, "running", 1), task(workerID, "review", 1), task(oldID, "done", 1),
		task("abcdef01-0000-4000-8000-000000000004", "backlog", 1), task("abcdef01-0000-4000-8000-000000000005", "backlog", 1))
	cells := []cell{
		c("created task a1b2c3d4, ", plain),
		c("then "+oldID+" (done); commit 0123456789abcdef0123456789abcdef01234567, sha a1b2c3d, deadbeef, ", dim),
		c("A1B2C3D4, xa1b2c3d4, self c0ffee01, shared abcdef01, whole abcdef01-0000-4000-8000-000000000005", plain),
	}
	out, links := markLinks(cells, m.knownIDs(), coordID, 1)
	if text(out) != text(cells) {
		t.Errorf("marking changed the text:\n%s", text(out))
	}
	if want := []string{workerID, oldID, "abcdef01-0000-4000-8000-000000000005"}; !slices.Equal(linkIDs(links), want) {
		t.Errorf("links = %v, want %v", linkIDs(links), want)
	}
	if links[0].text != "a1b2c3d4" || links[1].text != oldID {
		t.Errorf("link texts = %q, %q", links[0].text, links[1].text)
	}
}

// tab picks the links in turn, shift+tab goes back, a refresh keeps the pick, escape stops, and enter
// opens the task picked: on the board, Done tasks shown, its detail open.
func TestLinksCycleAndJump(t *testing.T) {
	m := linked(t, 100, 40, "created task a1b2c3d4, then sent d0d0d0d0 a note")
	if !slices.Equal(linkIDs(m.links), []string{workerID, oldID}) {
		t.Fatalf("links = %v", linkIDs(m.links))
	}
	if !strings.Contains(m.View(), "tab task ids") {
		t.Errorf("the footer does not offer tab:\n%s", m.View())
	}
	m, _ = press(t, m, "tab")
	if !m.linking || m.linkAt != 0 || !strings.Contains(m.View(), "enter opens a1b2c3d4") {
		t.Fatalf("tab picked %v %d:\n%s", m.linking, m.linkAt, m.View())
	}
	for _, step := range []struct {
		key  string
		want int
	}{{"tab", 1}, {"tab", 0}, {"shift+tab", 1}} {
		if m, _ = press(t, m, step.key); m.linkAt != step.want {
			t.Errorf("%s picked %d, want %d", step.key, m.linkAt, step.want)
		}
	}
	coord := *m.taskByID(coordID)
	coord.Detail = &api.Detail{Log: []transcript.Turn{{Who: "you", Text: "go"}, {Who: "agent", Text: "created task a1b2c3d4, then sent d0d0d0d0 a note"}}}
	if m = send(t, m, detailMsg{task: coord}); !m.linking || m.linkAt != 1 {
		t.Errorf("a refresh moved the pick to %v %d", m.linking, m.linkAt)
	}
	if m, _ = press(t, m, "esc"); m.view != viewDetail || m.linking {
		t.Fatalf("escape left view %d, picking %v", m.view, m.linking)
	}
	m, _ = press(t, m, "shift+tab")
	m, cmd := press(t, m, "enter")
	if m.view != viewDetail || m.detailID != oldID || cmd == nil || !strings.Contains(m.View(), "Loading d0d0d0d0") {
		t.Fatalf("enter opened view %d on %q:\n%s", m.view, m.detailID, m.View())
	}
	if !m.showDone || m.current() == nil || m.current().ID != oldID {
		t.Errorf("the hidden done task is not under the cursor: done shown %v", m.showDone)
	}
	if m, _ = press(t, m, "esc"); m.view != viewBoard || m.current().ID != oldID {
		t.Errorf("escape went to view %d", m.view)
	}

	// A filter that hides the task is cleared; one that shows it stays.
	m.needle = "c0ffee01"
	m.rebuild()
	m.jumpTo(workerID)
	if m.needle != "" || m.current() == nil || m.current().ID != workerID {
		t.Errorf("the filter %q hid the task", m.needle)
	}
}

// tab picks the first link on screen, and scrolls to it when it is below.
func TestLinksFromWhereTheDetailIsScrolled(t *testing.T) {
	log := []string{"created task a1b2c3d4"}
	for i := range 40 {
		log = append(log, fmt.Sprintf("line %d", i))
	}
	m := linked(t, 100, 20, append(log, "sent d0d0d0d0 a note")...)
	m.detail.GotoTop()
	m.detail.SetYOffset(m.links[0].spans[0].line + 1) // the first link is above the screen
	if m, _ = press(t, m, "tab"); m.linkAt != 1 {
		t.Fatalf("tab picked %d, not the link below the screen", m.linkAt)
	}
	s := m.links[1].spans[0]
	if y := s.line - m.detail.YOffset; y < 0 || y >= m.detail.Height || screenAt(m, y, s.from, s.to) != "d0d0d0d0" {
		t.Errorf("the link is not on screen at line %d: %q", y, screenAt(m, max(0, y), s.from, s.to))
	}
}

func click(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

// A click on a link opens its task, wherever the detail is scrolled; a click beside it does nothing.
func TestLinkClick(t *testing.T) {
	var log []string
	for i := range 40 {
		log = append(log, fmt.Sprintf("line %d", i))
	}
	m := linked(t, 100, 20, append(log, "created task a1b2c3d4, done")...)
	m.mouseOn = true
	m.detail.GotoBottom()
	s := m.links[0].spans[0]
	y := s.line - m.detail.YOffset
	if got := screenAt(m, y, s.from, s.to); got != "a1b2c3d4" {
		t.Fatalf("the link is drawn as %q", got)
	}
	if !strings.Contains(m.View(), "tab or a click") {
		t.Errorf("the footer does not offer a click:\n%s", m.View())
	}
	for _, miss := range []tea.MouseMsg{click(s.to, y), click(s.from-1, y), click(s.from, y-1),
		{X: s.from, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonRight}} {
		if next := send(t, m, miss); next.view != viewDetail || next.detailID != coordID {
			t.Errorf("a click at %d,%d opened %q", miss.X, miss.Y, next.detailID)
		}
	}
	next, cmd := m.Update(click(s.to-1, y))
	if m = next.(model); m.detailID != workerID || cmd == nil || m.current().ID != workerID {
		t.Errorf("the click opened %q", m.detailID)
	}
	// The wheel scrolls the detail.
	m = send(t, m, detailMsg{task: *m.taskByID(workerID)})
	m.detail.SetContent(strings.Repeat("x\n", 100))
	if m = send(t, m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}); m.detail.YOffset == 0 {
		t.Error("the wheel did not scroll")
	}
}

// A whole id wrapped across two lines is a link on both, styles drawn or not.
func TestWrappedLink(t *testing.T) {
	profile := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(profile)
	for _, colors := range []bool{false, true} {
		if colors {
			lipgloss.SetColorProfile(0) // termenv.TrueColor
		}
		m := linked(t, 22, 30, "see the old "+oldID+" now")
		if len(m.links) != 1 || len(m.links[0].spans) < 2 {
			t.Fatalf("colors %v: spans %v", colors, m.links)
		}
		var read strings.Builder
		for _, s := range m.links[0].spans {
			read.WriteString(screenAt(m, s.line-m.detail.YOffset, s.from, s.to))
		}
		if read.String() != oldID {
			t.Errorf("colors %v: the spans read %q", colors, read.String())
		}
		last := m.links[0].spans[len(m.links[0].spans)-1]
		if m = send(t, m, click(last.from, last.line-m.detail.YOffset)); m.detailID != oldID {
			t.Errorf("colors %v: a click on the second line opened %q", colors, m.detailID)
		}
	}
}

// The mouse stays the terminal's unless the config gives it to the board, which takes it again when
// a session gives the terminal back.
func TestMouseOffByDefault(t *testing.T) {
	m := newModel(client.New("http://127.0.0.1:1"))
	if m.mouseOn || m.takeMouse() != nil {
		t.Fatal("the board takes the mouse by default")
	}
	enabled := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, one := range batch {
				if one != nil && fmt.Sprintf("%T", one()) == fmt.Sprintf("%T", tea.EnableMouseCellMotion()) {
					return true
				}
			}
			return false
		}
		return fmt.Sprintf("%T", msg) == fmt.Sprintf("%T", tea.EnableMouseCellMotion())
	}
	if _, cmd := m.Update(attachedMsg{taskID: workerID}); enabled(cmd) {
		t.Error("the board took the mouse after a session")
	}
	m.mouseOn = true
	if !enabled(m.takeMouse()) {
		t.Error("mouse = true did not take the mouse")
	}
	if _, cmd := m.Update(attachedMsg{taskID: workerID}); !enabled(cmd) {
		t.Error("the board did not take the mouse back after a session")
	}
	if _, cmd := m.Update(diffDoneMsg{}); !enabled(cmd) {
		t.Error("the board did not take the mouse back after a diff")
	}
	if !strings.Contains(help, "tab             in an open task") || !strings.Contains(help, "mouse = true") {
		t.Error("the help does not list tab and the mouse setting")
	}
}
