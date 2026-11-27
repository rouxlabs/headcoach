package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/format"
	"github.com/rouxlabs/headcoach/internal/handoff"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/review"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
	"github.com/rouxlabs/headcoach/internal/transcript"
	"github.com/rouxlabs/headcoach/internal/update"
	"github.com/rouxlabs/headcoach/internal/usage"
	"github.com/rouxlabs/headcoach/internal/version"
)

func task(id, state string, idle int) api.Task {
	return api.Task{
		Task:  store.Task{ID: id, Title: "title " + id, State: state, Repo: "repo", Last: "last " + id},
		Short: id[:min(8, len(id))], StateName: store.StateNames[state], Idle: idle, Live: true, CanReply: true,
	}
}

// refused is what the test daemon answers a reply to need0001 with.
const refused = "The dialog takes no typed answer. Answer it with its keys, or in the terminal."

// text joins cells without their styles.
func text(cells []cell) string {
	var out strings.Builder
	for _, piece := range cells {
		out.WriteString(piece.text)
	}
	return out.String()
}

// daemon is a stand-in for the daemon that records the requests it gets.
type daemon struct {
	calls []string // "METHOD path body"
	web   *httptest.Server
}

func newDaemon(t *testing.T) *daemon {
	d := &daemon{}
	d.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/ask"):
			w.Write([]byte(`{"answer": "All good.", "question": "what is left?"}`))
		case strings.HasSuffix(r.URL.Path, "/reply") && strings.Contains(r.URL.Path, "need"):
			w.WriteHeader(409)
			w.Write([]byte(`{"error": "` + refused + `"}`))
		case r.URL.Path == "/api/schedules" && r.Method == "GET":
			w.Write([]byte(`[{"id": "sched001-x", "short": "sched001", "title": "Standup", "at": "08:00", "days": "daily",
				"enabled": true, "catch_up": true, "last_task": "made0001", "last_state": "Running"}]`))
		case strings.HasPrefix(r.URL.Path, "/api/schedules"):
			w.Write([]byte(`{"id": "sched001-x", "short": "sched001", "title": "Standup", "last_task": "made0001"}`))
		case r.URL.Path == "/api/review":
			pr := task("revpr001", "review", 0)
			pr.TimerLeft = 300
			push := task("revpsh01", "review", 0)
			push.Cwd = "/x/repo"
			json.NewEncoder(w).Encode(api.Review{Items: []api.ReviewItem{
				{Task: pr, Waited: 7200, Tests: "failed", Delivery: "pr", PR: "https://github.com/o/r/pull/12", Verdict: "Verdict: approve"},
				{Task: push, Waited: 60, Tests: "passed", Delivery: "push", Change: &review.Change{
					Branch: "main", Default: true, From: "aaaaaaa1", To: "bbbbbbb2", Commits: 2, Files: 3, Added: 10, Deleted: 1, Unpushed: 2}},
			}, Metrics: api.ReviewMetrics{Days: 7, Reviews: 3, AvgWait: 600}})
		case strings.HasSuffix(r.URL.Path, "/team"):
			kid, grandkid := task("kid00001", "running", 60), task("gkid0001", "review", 5)
			json.NewEncoder(w).Encode(api.Team{Task: task("boss0001", "done", 0), Handoff: handoff.Summary{Status: "Two tasks out."},
				Members: []api.Member{
					{Task: kid, Kids: 1, Handoff: handoff.Summary{Status: "Halfway.", Queue: handoff.Queue{Open: 2, Done: 1}}},
					{Task: grandkid, Depth: 1, Tree: "└─ "},
				},
				OnYou: []api.OnYou{{Task: grandkid.ID, Short: "gkid0001", Title: "title gkid0001", Kind: "review", What: "PR #3"}}})
		case r.URL.Path == "/api/tasks" && r.Method == "GET":
			json.NewEncoder(w).Encode(api.Tasks{Tasks: []api.Task{task("fresh001", "running", 1)}})
		case strings.HasPrefix(r.URL.Path, "/api/tasks/gone"):
			w.WriteHeader(404)
			w.Write([]byte(`{"error": "No task matches"}`))
		case r.URL.Path == "/api/tasks" && strings.Contains(string(body), "refuse"):
			w.WriteHeader(400)
			w.Write([]byte(`{"error": "Unknown profile 'gone'. Known: work"}`))
		default:
			out := task("made0001", "backlog", 0)
			out.Pane = "%7"
			// A daemon from before pins, for the tasks named old, ignores them.
			out.Pinned = strings.Contains(string(body), `"pinned":true`) && !strings.Contains(r.URL.Path, "old")
			out.Detail = &api.Detail{Log: []transcript.Turn{{Who: "you", Text: "hello", Skipped: 3}, {Who: "tool", Text: "$ ls"},
				{Who: "you", Text: "[from coordinator 1a2b3c4d] stop", From: "coordinator 1a2b3c4d"}},
				Handoff: &api.Handoff{Path: "/state/handoff/made0001.md", Text: "## Status\nHalfway.",
					Summary: handoff.Summary{Status: "Halfway.", Queue: handoff.Queue{Open: 1, Next: "docs"}}}}
			json.NewEncoder(w).Encode(out)
		}
	}))
	t.Cleanup(d.web.Close)
	return d
}

// loaded is a model that has received its first poll.
func loaded(t *testing.T, d *daemon, tasks ...api.Task) model {
	t.Helper()
	url := "http://127.0.0.1:1"
	if d != nil {
		url = d.web.URL
	}
	m := newModel(client.New(url))
	return send(t, m, tea.WindowSizeMsg{Width: 200, Height: 20}, boardMsg{board: api.Tasks{Tasks: tasks}})
}

// send delivers messages to the model. Commands are not run.
func send(t *testing.T, m model, msgs ...tea.Msg) model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

// press types keys, one per word: a single character, or the name of a key such as enter.
func press(t *testing.T, m model, keys ...string) (model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, key := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		switch key {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "pgdown":
			msg = tea.KeyMsg{Type: tea.KeyPgDown}
		case "pgup":
			msg = tea.KeyMsg{Type: tea.KeyPgUp}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		}
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(model)
	}
	return m, cmd
}

// run executes a command and delivers what it returns, the way the Bubble Tea loop does.
func run(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("the key started nothing")
	}
	return send(t, m, cmd())
}

func TestRowsOrderFilterAndDone(t *testing.T) {
	run1 := task("run00001", "running", 5)
	run1.Agents = []api.Agent{{Name: "Explore", Desc: "look", State: "running"}, {Name: "command", Desc: "make", State: "done"}}
	run1.Tokens, run1.Cost = usage.Tokens{Out: 900}, 2.5
	need := task("need0001", "needs", 9)
	need.Ask, need.Live = "Shall I?", false
	other := task("other001", "review", 1)
	other.Repo, other.Tokens = "elsewhere", usage.Tokens{Out: 5000}
	hidden := task("done0001", "done", 1)
	hidden.Hidden = true
	m := loaded(t, nil, hidden, run1, need, other)

	if want := []string{"need0001", "run00001", "run00001/0", "run00001/1", "other001"}; !reflect.DeepEqual(m.keys, want) {
		t.Fatalf("rows = %v", m.keys)
	}
	screen := m.View()
	lines := strings.Split(screen, "\n")
	if len(lines) != 20 {
		t.Errorf("the board is %d lines high on a terminal of 20", len(lines))
	}
	for _, piece := range []string{
		"http://127.0.0.1:1", "3 of 4  (1 hidden)", "1 waiting for you", "<enter>", ")[3]",
		"TASK      STATE", "need0001  Needs me  -        -      -       -        repo ", "title need0001 (closed)  Shall I?",
		"├─ Explore: look", "└─ command: make", "title run00001", "last run00001",
		"tokens in 0 out 5.9k cache 0  cost $2.50",
	} {
		if !strings.Contains(screen, piece) {
			t.Errorf("the board lacks %q:\n%s", piece, screen)
		}
	}

	// d shows the done tasks, o goes through the orders, / filters as it is typed.
	m, _ = press(t, m, "d")
	if len(m.keys) != 6 || m.keys[5] != "done0001" || strings.Contains(m.View(), "(1 hidden)") {
		t.Errorf("with done tasks, rows = %v", m.keys)
	}
	m, _ = press(t, m, "o")
	if m.order != "idle" || m.keys[len(m.keys)-1] != "need0001" || !strings.Contains(m.View(), "by idle") {
		t.Errorf("sorted by %s, rows = %v", m.order, m.keys)
	}
	m, _ = press(t, m, "o", "o")
	if m.order != "tokens" || m.keys[0] != "other001" || !strings.Contains(m.View(), "by tokens") {
		t.Errorf("sorted by %s, rows = %v", m.order, m.keys)
	}
	m, _ = press(t, m, "o", "o", "d", "/", "e", "l", "s")
	if !reflect.DeepEqual(m.keys, []string{"other001"}) || !strings.Contains(m.View(), "/els") {
		t.Errorf("filtered rows = %v", m.keys)
	}
	m, _ = press(t, m, "enter", "j", "j")
	if m.asking != "" || m.needle != "els" || m.cursor != 0 {
		t.Errorf("after the filter: asking %q, needle %q, cursor %d", m.asking, m.needle, m.cursor)
	}
	m, _ = press(t, m, "esc")
	if m.needle != "" || len(m.keys) != 5 {
		t.Errorf("escape left the filter %q", m.needle)
	}
}

func TestTheFilterIsFuzzyAndPicksTheBestFit(t *testing.T) {
	a, b, c := task("aaaa0001", "running", 1), task("bbbb0001", "running", 2), task("cccc0001", "running", 3)
	a.Title, b.Title, c.Title = "rename the profile field", "fuzzy filter", "update docs"
	a.Repo, b.Repo = "web", "headcoach"
	m := loaded(t, nil, a, b, c)

	// The letters in order, not side by side; the board keeps its order, the cursor goes to the best fit.
	m, _ = press(t, m, "/", "f", "f")
	if !reflect.DeepEqual(m.keys, []string{"aaaa0001", "bbbb0001"}) || m.current().ID != "bbbb0001" {
		t.Errorf("ff shows %v, the cursor on %s", m.keys, m.current().ID)
	}
	// Each word is found on its own, here in the repo.
	m, _ = press(t, m, " ", "h", "c")
	if !reflect.DeepEqual(m.keys, []string{"bbbb0001"}) {
		t.Errorf("ff hc shows %v", m.keys)
	}
	// Emptied, the filter leaves the cursor where it is.
	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if len(m.keys) != 3 || m.current().ID != "bbbb0001" {
		t.Errorf("without a filter: %v, the cursor on %s", m.keys, m.current().ID)
	}
	// On a tie the highest on the board wins.
	if m, _ = press(t, m, "f"); m.current().ID != "aaaa0001" {
		t.Errorf("f puts the cursor on %s", m.current().ID)
	}
	// The arrows move the cursor, the line stays open, j and k are letters of the filter.
	m, _ = press(t, m, "down", "pgdown", "up")
	if m.asking != "filter" || m.input.Value() != "f" || m.current().ID != "aaaa0001" {
		t.Errorf("after the arrows: asking %q with %q, the cursor on %s", m.asking, m.input.Value(), m.current().ID)
	}
	m, _ = press(t, m, "j", "enter")
	if m.needle != "fj" || len(m.keys) != 0 {
		t.Errorf("j went to the cursor: filter %q, rows %v", m.needle, m.keys)
	}
}

func TestCursorFollowsItsTask(t *testing.T) {
	a, b, c := task("aaaa0001", "running", 1), task("bbbb0001", "running", 2), task("cccc0001", "running", 3)
	m := loaded(t, nil, a, b, c)
	m, _ = press(t, m, "j", "down")
	if m.current().ID != "cccc0001" {
		t.Fatalf("cursor on %s", m.current().ID)
	}
	// The task moves up the list: the cursor goes with it.
	c.Idle = 0
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{a, b, c}}})
	if m.cursor != 0 || m.current().ID != "cccc0001" {
		t.Errorf("after a reorder the cursor is on row %d, %s", m.cursor, m.current().ID)
	}
	// The task is gone: the cursor stays on a row.
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{a}}})
	if m.cursor != 0 || m.current().ID != "aaaa0001" {
		t.Errorf("after a removal the cursor is on row %d", m.cursor)
	}
	m, _ = press(t, m, "k", "k", "G", "j")
	if m.cursor != 0 {
		t.Errorf("the cursor left the table: %d", m.cursor)
	}
	// An empty board has no current task, and no key fails on it.
	m = send(t, m, boardMsg{board: api.Tasks{}})
	m, cmd := press(t, m, "enter", "r", "q", "m", "p", "s", "x", "a", "j")
	if m.current() != nil || cmd != nil || m.view != viewBoard {
		t.Errorf("an empty board reacted: view %d", m.view)
	}

	// A table longer than the screen scrolls with the cursor.
	var many []api.Task
	for i := 0; i < 40; i++ {
		many = append(many, task(string(rune('a'+i%26))+strings.Repeat("x", 6)+string(rune('a'+i/26)), "running", i))
	}
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: many}})
	m, _ = press(t, m, "G")
	if height := m.tableHeight(); m.cursor != 39 || m.top != 40-height || len(strings.Split(m.View(), "\n")) != 20 {
		t.Errorf("at the end: cursor %d, top %d, height %d", m.cursor, m.top, height)
	}
}

// headingLine is the line of a view that names the given column.
func headingLine(t *testing.T, view, name string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("no %s heading in:\n%s", name, view)
	return ""
}

func TestScrollKeepsColumnWidths(t *testing.T) {
	// The only wide row is the first one: once it scrolls out of view the columns stay as they were.
	tasks := []api.Task{task("need0001", "needs", 0)}
	for i := 1; i < 40; i++ {
		tasks = append(tasks, task(fmt.Sprintf("run%05d", i), "running", i))
	}
	m := loaded(t, nil, tasks...)
	before := headingLine(t, m.View(), "STATE")
	m, _ = press(t, m, "G")
	if m.top == 0 {
		t.Fatal("the board did not scroll")
	}
	if after := headingLine(t, m.View(), "STATE"); after != before {
		t.Errorf("the board columns moved while scrolling:\n%s\n%s", before, after)
	}

	// Select mode keeps them too.
	m, _ = press(t, m, "g", "v")
	if !m.selecting || m.top != 0 {
		t.Fatalf("not in select mode at the top: selecting %v, top %d", m.selecting, m.top)
	}
	before = headingLine(t, m.View(), "STATE")
	m, _ = press(t, m, strings.Split(strings.Repeat("j", 39), "")...)
	if m.top == 0 {
		t.Fatal("the board did not scroll in select mode")
	}
	if after := headingLine(t, m.View(), "STATE"); after != before {
		t.Errorf("the board columns moved while scrolling in select mode:\n%s\n%s", before, after)
	}

	m.view = viewReview
	queue := api.Review{Items: []api.ReviewItem{{Task: api.Task{Task: store.Task{Repo: "a-much-longer-repo"}, Short: "wide0001"}}}}
	for i := 1; i < 40; i++ {
		queue.Items = append(queue.Items, api.ReviewItem{Task: api.Task{Task: store.Task{Repo: "r"}, Short: fmt.Sprintf("rev%05d", i)}})
	}
	m = send(t, m, reviewMsg{queue: queue})
	m.reviewCursor = 0
	before = headingLine(t, m.View(), "WAITED")
	m.reviewCursor = 39
	if after := headingLine(t, m.View(), "WAITED"); after != before {
		t.Errorf("the review columns moved while scrolling:\n%s\n%s", before, after)
	}

	// The schedules scroll with the cursor, and keep their columns too.
	m.view = viewSchedules
	list := []api.Schedule{{Short: "wide0001", Schedule: store.Schedule{Title: "a much longer schedule title"}}}
	for i := 1; i < 40; i++ {
		list = append(list, api.Schedule{Short: fmt.Sprintf("sch%05d", i), Schedule: store.Schedule{Title: "t"}})
	}
	m = send(t, m, schedulesMsg{list: list})
	m.schedCursor = 0
	before = headingLine(t, m.View(), "TITLE")
	m.schedCursor = 39
	if !strings.Contains(m.View(), "sch00039") {
		t.Errorf("the schedule under the cursor is off screen:\n%s", m.View())
	}
	if after := headingLine(t, m.View(), "TITLE"); after != before {
		t.Errorf("the schedule columns moved while scrolling:\n%s\n%s", before, after)
	}
}

func TestDaemonDown(t *testing.T) {
	m := loaded(t, nil, task("aaaa0001", "running", 1))
	m = run(t, m, m.poll()) // nothing listens there
	if !strings.Contains(m.View(), "No daemon at http://127.0.0.1:1") || !strings.Contains(m.View(), "(retrying)") {
		t.Errorf("the headline does not say the daemon is down:\n%s", strings.Split(m.View(), "\n")[0])
	}
	if len(m.rows) != 1 {
		t.Error("the last known tasks were dropped")
	}
}

func TestActions(t *testing.T) {
	d := newDaemon(t)
	need := task("need0001", "needs", 1)
	need.CanReply = false
	m := loaded(t, d, task("run00001", "running", 5), need, task("rev00001", "review", 1))

	// A dialog takes a typed answer, which the daemon may refuse.
	m, _ = press(t, m, "r", "b", "l", "u", "e")
	if m.asking != "reply" || !strings.Contains(m.View(), "Answer the question need0001 shows, in its free text option") {
		t.Fatalf("reply during a dialog: asking %q", m.asking)
	}
	m, cmd := press(t, m, "enter")
	m = run(t, m, cmd)
	if m.notice != refused || !m.noticeAlarm || m.asking != "" {
		t.Errorf("a refused answer gave %q", m.notice)
	}
	// A permission prompt is refused up front.
	m.rows[m.cursor].Task.AskKind = "permission"
	if m, cmd = press(t, m, "r"); cmd != nil || m.asking != "" || m.notice != permission {
		t.Errorf("reply to a permission prompt: asking %q, notice %q", m.asking, m.notice)
	}
	m.rows[m.cursor].Task.AskKind = "dialog"
	// Reply to the running task.
	m, _ = press(t, m, "j", "r", "g", "o", " ", "o", "n")
	if m.asking != "reply" || m.input.Value() != "go on" || !strings.Contains(m.View(), "Reply to run00001: title run00001") {
		t.Fatalf("typing a reply: asking %q, value %q", m.asking, m.input.Value())
	}
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	if m.notice != "Message delivered to made0001" || m.asking != "" {
		t.Errorf("after the reply: %q", m.notice)
	}
	// An empty reply sends nothing.
	m, _ = press(t, m, "r")
	if m, cmd = press(t, m, "enter"); cmd != nil {
		t.Error("an empty reply was sent")
	}

	// Parking a running task asks first; n keeps it.
	m, _ = press(t, m, "p")
	if m.view != viewNote || !strings.Contains(m.View(), "run00001 is running. Park it anyway?") {
		t.Fatalf("park did not ask:\n%s", m.View())
	}
	if m, cmd = press(t, m, "z", "n"); cmd != nil || m.view != viewBoard {
		t.Error("n parked the task")
	}
	m, _ = press(t, m, "p")
	m, cmd = press(t, m, "y")
	m = run(t, m, cmd)
	// A task that is not running is parked at once.
	m, cmd = press(t, m, "j", "p")
	m = run(t, m, cmd)

	// Move: the picker opens on the current state.
	m, _ = press(t, m, "m")
	if m.view != viewPick || m.pick != 3 || !strings.Contains(m.View(), "Move rev00001 to") {
		t.Fatalf("the picker is on %d:\n%s", m.pick, m.View())
	}
	m, cmd = press(t, m, "j", "j", "enter")
	m = run(t, m, cmd)

	m, cmd = press(t, m, "s")
	m = run(t, m, cmd)
	if m.notice != "made0001 started in tmux pane %7" {
		t.Errorf("after start: %q", m.notice)
	}
	m, _ = press(t, m, "x")
	if !strings.Contains(m.View(), "Delete rev00001 title rev00001?  y / n") {
		t.Errorf("delete did not ask:\n%s", m.View())
	}
	m, cmd = press(t, m, "y")
	m = run(t, m, cmd)

	// A new task: enter keeps the prefilled folder, then the title. The cursor jumps to it.
	m, _ = press(t, m, "n")
	if m.asking != "newdir" {
		t.Fatalf("n asked %q", m.asking)
	}
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "d", "o")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	if m.jump != "made0001" {
		t.Errorf("after new: jump %q", m.jump)
	}

	// The daemon refusing is shown, not dropped.
	m = run(t, m, m.act("POST", "/api/tasks/need0001/reply", map[string]string{"text": "x"}, nil))
	if m.notice != refused || !m.noticeAlarm {
		t.Errorf("a refusal gave %q", m.notice)
	}

	want := []string{
		`POST /api/tasks/need0001/reply {"text":"blue"}`,
		`POST /api/tasks/run00001/reply {"text":"go on"}`,
		`PATCH /api/tasks/run00001 {"state":"parked"}`,
		`PATCH /api/tasks/rev00001 {"state":"parked"}`,
		`PATCH /api/tasks/rev00001 {"state":"done"}`,
		`POST /api/tasks/rev00001/start`,
		`DELETE /api/tasks/rev00001`,
		`POST /api/tasks/need0001/reply {"text":"x"}`,
	}
	var got []string
	for _, call := range d.calls {
		if !strings.Contains(call, `"cwd"`) {
			got = append(got, call)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the daemon got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if created := d.calls[len(d.calls)-2]; !strings.HasPrefix(created, `POST /api/tasks {"cwd":"`) || !strings.HasSuffix(created, `"title":"do"}`) {
		t.Errorf("new sent %s", created)
	}
}

func TestNewTaskFolder(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d)
	m.workdir = filepath.Join(t.TempDir(), "workdir")

	// A bare name lands under workdir; the missing folder is created after y.
	m, _ = press(t, m, "n")
	if m.asking != "newdir" || m.input.Value() != m.workdir+"/" {
		t.Fatalf("n prefilled %q", m.input.Value())
	}
	m, _ = press(t, m, "idea")
	m, _ = press(t, m, "enter")
	want := filepath.Join(m.workdir, "idea")
	if m.view != viewNote || !strings.Contains(m.View(), "Create it?") {
		t.Fatalf("no create question:\n%s", m.View())
	}
	m, cmd := press(t, m, "y")
	m = run(t, m, cmd)
	if m.asking != "new" || m.newDir != want {
		t.Fatalf("after y: asking %q in %q", m.asking, m.newDir)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("%s was not created: %v", want, err)
	}
	m, _ = press(t, m, "go")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	if call := d.calls[len(d.calls)-1]; call != `POST /api/tasks {"cwd":"`+want+`","title":"go"}` {
		t.Errorf("new sent %s", call)
	}

	// n on the question keeps the board untouched.
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "idea2")
	m, _ = press(t, m, "enter")
	m, cmd = press(t, m, "z", "n")
	if cmd != nil || m.view != viewBoard || m.asking != "" {
		t.Error("refusing the folder still went on")
	}
}

func TestCopyTask(t *testing.T) {
	d := newDaemon(t)
	source := task("src00001", "review", 5)
	source.Cwd, source.Profile = t.TempDir(), "personal"
	source.Agents = []api.Agent{{Name: "Explore", Desc: "look", State: "running"}}
	bare := task("bare0001", "review", 9)
	bare.Profile = "work"
	m := loaded(t, d, source, bare)
	m.profiles = []string{"work", "personal"}

	// c skips the folder and the profile: they are the ones of the task under the cursor.
	m, _ = press(t, m, "c")
	if m.asking != "new" || m.input.Value() != "" {
		t.Fatalf("c asked %q with %q", m.asking, m.input.Value())
	}
	if want := "Copy of src00001 in " + source.Cwd + " as personal: its title"; !strings.Contains(m.View(), want) {
		t.Errorf("the copy does not say %q:\n%s", want, m.View())
	}
	m, _ = press(t, m, "again")
	m, cmd := press(t, m, "enter")
	m = run(t, m, cmd)
	if call := d.calls[len(d.calls)-1]; call != `POST /api/tasks {"cwd":"`+source.Cwd+`","profile":"personal","title":"again"}` {
		t.Errorf("the copy sent %s", call)
	}
	if m.jump != "made0001" {
		t.Errorf("after the copy: jump %q", m.jump)
	}

	// On the row of a sub-agent, c copies its task. Escape drops the copy, and leaves the board free
	// to reload itself after an update.
	m, _ = press(t, m, "j", "c")
	if m.asking != "new" || !strings.Contains(m.input.Placeholder, "Copy of src00001") {
		t.Fatalf("c on a child row asked %q: %s", m.asking, m.input.Placeholder)
	}
	calls := len(d.calls)
	m, cmd = press(t, m, "esc")
	if m.asking != "" || cmd != nil || len(d.calls) != calls || !m.idle() {
		t.Error("escape did not drop the copy")
	}

	// A task without a folder is copied without one, not into the folder the board runs in.
	m, _ = press(t, m, "j", "c")
	if !strings.Contains(m.input.Placeholder, "Copy of bare0001 in no folder as work") {
		t.Fatalf("a task without a folder: %s", m.input.Placeholder)
	}
	m, _ = press(t, m, "x")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	if call := d.calls[len(d.calls)-1]; call != `POST /api/tasks {"cwd":"","profile":"work","title":"x"}` {
		t.Errorf("the copy sent %s", call)
	}

	// A folder that is gone is said before a title is typed.
	gone := task("gone0001", "review", 5)
	gone.Cwd, gone.Profile = filepath.Join(t.TempDir(), "removed"), "work"
	refused, cmd := press(t, loaded(t, d, gone), "c")
	if refused.asking != "" || cmd != nil || refused.notice != "The folder of gone0001 is gone: "+gone.Cwd || !refused.noticeAlarm {
		t.Errorf("c on a task without its folder asked %q and said %q", refused.asking, refused.notice)
	}

	// A profile this board has not heard of is the daemon's to accept: it may have been added since.
	added := task("new00001", "review", 5)
	added.Cwd, added.Profile = t.TempDir(), "added-later"
	later := loaded(t, d, added)
	later.profiles = m.profiles
	if later, _ = press(t, later, "c"); later.asking != "new" || later.newProfile != "added-later" {
		t.Errorf("c on a profile added later asked %q", later.asking)
	}

	// A long folder gives up its start, so that the line still says what to type.
	deep := task("deep0001", "review", 5)
	deep.Cwd, deep.Profile = filepath.Join(t.TempDir(), strings.Repeat("d", 60)), "work"
	os.Mkdir(deep.Cwd, 0o755)
	tight := send(t, loaded(t, d, deep), tea.WindowSizeMsg{Width: 80, Height: 20})
	tight, _ = press(t, tight, "c")
	if text := tight.input.Placeholder; lipgloss.Width(text) > 79 || !strings.HasPrefix(text, "Copy of deep0001 in …") ||
		!strings.HasSuffix(text, "ddd as work: its title. Enter drops you into the session") {
		t.Errorf("a long folder at 80 columns: %q", text)
	}

	// With no task under the cursor there is nothing to copy.
	empty, cmd := press(t, loaded(t, d), "c")
	if empty.asking != "" || cmd != nil {
		t.Error("c on an empty board started something")
	}

	// The header names the key when the terminal is wide enough for a fourth column of hints, and
	// the logo keeps the width it showed from.
	for width, want := range map[int][]bool{200: {true, true}, 130: {true, true}, 118: {true, false}, 110: {false, false}} {
		view := send(t, m, tea.WindowSizeMsg{Width: width, Height: 20}).View()
		if strings.Contains(view, "<c>") != want[0] || strings.Contains(view, logo[1]) != want[1] ||
			!strings.Contains(view, "<?>") || !strings.Contains(view, "<Q>") {
			t.Errorf("at %d columns: copy hint %v, logo %v:\n%s", width, strings.Contains(view, "<c>"), strings.Contains(view, logo[1]), view)
		}
	}
}

func TestFolderList(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5)) // 200 wide, 20 high
	m.workdir = t.TempDir()
	for _, name := range []string{"alpha", "alpha/one", "alpha/two", "beta", "bravo"} {
		os.Mkdir(filepath.Join(m.workdir, name), 0o755)
	}
	for i := 1; i <= 12; i++ {
		os.Mkdir(filepath.Join(m.workdir, fmt.Sprintf("dir%02d", i)), 0o755)
	}
	os.WriteFile(filepath.Join(m.workdir, "a-file"), nil, 0o644)
	shows := func(m model, pieces ...string) {
		t.Helper()
		for _, piece := range pieces {
			if !strings.Contains(m.View(), piece) {
				t.Errorf("the screen lacks %q:\n%s", piece, m.View())
			}
		}
	}
	height := func(m model) int { return strings.Count(m.View(), "\n") + 1 }
	list := func(m model, listing bool, folders, cursor int) {
		t.Helper()
		if m.listing != listing || len(m.folders) != folders || m.folderCursor != cursor {
			t.Fatalf("listing %v, %d folders, cursor %d: want %v, %d, %d", m.listing, len(m.folders), m.folderCursor, listing, folders, cursor)
		}
	}

	// Tab lists the folders above the input line, and the table gives it the room. None is chosen yet.
	m, _ = press(t, m, "n")
	closed, table := height(m), m.tableHeight()
	m, _ = press(t, m, "tab")
	list(m, true, 15, -1)
	shows(m, "folders("+m.workdir+"/)[15]", "   alpha/", "   beta/", "bravo/", "↓ 12 more", "↑↓ choose, tab picks, esc closes")
	if strings.Contains(m.View(), " > alpha/") || strings.Contains(m.View(), "dir01/") || strings.Contains(m.View(), "a-file") {
		t.Errorf("the list has a row chosen, more than its three rows, or a file:\n%s", m.View())
	}
	if height(m) != closed || m.tableHeight() != table-5 {
		t.Errorf("the list made the screen %d lines from %d, the table %d rows from %d", height(m), closed, m.tableHeight(), table)
	}

	// The arrows choose, and scroll once past the last row on screen. Both ends wrap.
	m, _ = press(t, m, "down", "down", "down", "down", "down", "down")
	shows(m, "[6/15]", " > dir03/", "↑ 3 more", "↓ 9 more")
	m, _ = press(t, m, "pgdown", "pgdown", "pgdown", "pgdown")
	shows(m, "[15/15]", " > dir12/", "↑ 12 more")
	m, _ = press(t, m, "down")
	shows(m, "[1/15]", " > alpha/")
	m, _ = press(t, m, "up", "pgup")
	shows(m, "[12/15]", " > dir09/")

	// The title fits its border whatever the path: the keys go first, then the start of the path.
	// The screen is ten columns short of the whole title, however long the temporary folder is.
	whole := lipgloss.Width("┌ folders(" + m.workdir + "/)[12/15] ┐")
	narrow := send(t, m, tea.WindowSizeMsg{Width: whole - 10, Height: 20}).View()
	if !strings.Contains(narrow, "┌ folders(…") || !strings.Contains(narrow, m.workdir[len(m.workdir)-8:]+"/)[12/15] ┐") {
		t.Errorf("a narrow list lost its title:\n%s", narrow)
	}
	if roomy := send(t, m, tea.WindowSizeMsg{Width: whole + 10, Height: 20}).View(); !strings.Contains(roomy, "folders("+m.workdir+"/)[12/15] ") || strings.Contains(roomy, "↑↓ choose") {
		t.Errorf("a list with no room for its keys:\n%s", roomy)
	}
	// A taller screen shows more folders, up to its middle. A short one keeps a row of the table.
	if tall := send(t, m, tea.WindowSizeMsg{Width: 200, Height: 40}); tall.folderRows() != 13 || !strings.Contains(tall.View(), "dir01/") {
		t.Errorf("a screen of 40 lines lists %d folders", tall.folderRows())
	}
	short := send(t, m, tea.WindowSizeMsg{Width: 200, Height: 18})
	if short.folderRows() != 1 || height(short) != 18 {
		t.Errorf("a screen of 18 lines lists %d folders on %d lines", short.folderRows(), height(short))
	}
	shows(short, " > dir09/", "↑ 11 more  ↓ 3 more")
	// A notice takes a line: the folder chosen stays on screen. With no line left there is no list.
	m, _ = press(t, m, "down", "down") // the last of the three rows on screen
	m.say("a notice", false)
	if m.folderRows() != 2 || m.folderTop != 12 || m.folderCursor != 13 {
		t.Errorf("under a notice the list has %d rows from %d, the cursor on %d", m.folderRows(), m.folderTop, m.folderCursor)
	}
	m.notice = ""
	m, _ = press(t, m, "up", "up")
	if gone := send(t, m, tea.WindowSizeMsg{Width: 200, Height: 17}); gone.listing || height(gone) != 17 {
		t.Errorf("a screen of 17 lines: listing %v on %d lines", gone.listing, height(gone))
	}
	tiny := send(t, loaded(t, d), tea.WindowSizeMsg{Width: 200, Height: 16})
	tiny.workdir = m.workdir
	tiny, _ = press(t, tiny, "n", "tab")
	if tiny.listing || tiny.notice != "This terminal is too short to list the folders" || height(tiny) > 16 {
		t.Errorf("tab on 16 lines: listing %v, notice %q, %d lines", tiny.listing, tiny.notice, height(tiny))
	}
	if tiny, _ = press(t, tiny, "a", "l", "tab"); tiny.input.Value() != m.workdir+"/alpha/" {
		t.Errorf("tab on 16 lines and one match left %q", tiny.input.Value())
	}

	// Typing narrows the list. A typo says so and keeps it open, so that a correction brings the folders back.
	m, _ = press(t, m, "b")
	list(m, true, 2, -1)
	shows(m, "[2]", "   beta/", "   bravo/")
	m, _ = press(t, m, "x")
	list(m, true, 0, -1)
	shows(m, "[0]", "No folder starts with bx")
	m, _ = press(t, m, "tab", "down", "backspace")
	list(m, true, 2, -1)

	// The name of a key that arrives as text, the way a paste or a slow line sends a word, is not that key.
	for _, word := range []string{"down", "tab", "enter", "esc"} {
		m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(word)})
		if m.input.Value() != m.workdir+"/b"+word || m.asking != "newdir" {
			t.Fatalf("the word %s left %q, asking %q", word, m.input.Value(), m.asking)
		}
		list(m, true, 0, -1)
		for range word {
			m, _ = press(t, m, "backspace")
		}
		list(m, true, 2, -1)
	}

	// Enter picks the folder the arrows chose. A folder with nothing inside closes the list, and
	// enter then takes the folder.
	m, _ = press(t, m, "down", "down", "enter")
	if m.input.Value() != m.workdir+"/bravo/" || m.asking != "newdir" {
		t.Fatalf("the pick left %q, asking %q", m.input.Value(), m.asking)
	}
	list(m, false, 0, -1)
	m, _ = press(t, m, "enter")
	if m.asking != "new" || m.newDir != filepath.Join(m.workdir, "bravo") {
		t.Fatalf("after the folder: asking %q in %q", m.asking, m.newDir)
	}
	m, _ = press(t, m, "esc")

	// With no folder chosen, enter takes what is typed: here a folder to create, next to those listed.
	m, _ = press(t, m, "n", "b", "tab")
	list(m, true, 2, -1)
	m, _ = press(t, m, "enter")
	if m.view != viewNote || !strings.Contains(m.View(), filepath.Join(m.workdir, "b")+" does not exist. Create it?") || m.listing {
		t.Fatalf("enter on the typed path:\n%s", m.View())
	}
	m, _ = press(t, m, "n")

	// One folder that fits completes at once, and the list goes on with what is inside it. Tab picks
	// the first folder when none is chosen.
	m, _ = press(t, m, "n", "A", "l", "tab")
	if m.input.Value() != m.workdir+"/alpha/" {
		t.Errorf("tab on one match left %q", m.input.Value())
	}
	list(m, true, 2, -1)
	shows(m, "folders("+m.workdir+"/alpha/)[2]", "   one/", "   two/")
	m, _ = press(t, m, "tab")
	if m.input.Value() != m.workdir+"/alpha/one/" {
		t.Errorf("tab in the list picked %q", m.input.Value())
	}
	list(m, false, 0, -1)

	// A folder that is not there yet says so: the flow offers to create it. Escape closes the list
	// first, then the input.
	m.input.SetValue("nope/zz")
	m, _ = press(t, m, "tab")
	list(m, true, 0, -1)
	shows(m, "nope does not exist yet")
	m, _ = press(t, m, "esc")
	if m.listing || m.asking != "newdir" {
		t.Fatalf("escape on the list: listing %v, asking %q", m.listing, m.asking)
	}
	m, _ = press(t, m, "esc")
	if m.asking != "" {
		t.Error("escape did not close the input")
	}

	// A name wider than the screen is cut, not wrapped: the frame keeps its lines and its width.
	long := strings.Repeat("w", 80)
	os.Mkdir(filepath.Join(m.workdir, "beta", long), 0o755)
	os.Mkdir(filepath.Join(m.workdir, "beta", "short"), 0o755)
	m = send(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})
	m, _ = press(t, m, "n")
	closed = height(m)
	m.input.SetValue(m.workdir + "/beta/")
	m, _ = press(t, m, "tab", "down", "down")
	list(m, true, 2, 1)
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > 60 {
			t.Errorf("a line is %d wide on a screen of 60: %q", lipgloss.Width(line), line)
		}
	}
	if height(m) != closed {
		t.Errorf("a long name made the screen %d lines from %d", height(m), closed)
	}
	m, _ = press(t, m, "esc", "esc")

	// A folder whose name starts with a dot is one of workdir like any other, when it is listed and
	// when it is taken.
	os.Mkdir(filepath.Join(m.workdir, ".dot"), 0o755)
	m, _ = press(t, m, "n", ".", "d", "tab")
	if m.input.Value() != m.workdir+"/.dot/" {
		t.Errorf("tab on a dot folder left %q", m.input.Value())
	}
	m.input.SetValue(".dot")
	m, _ = press(t, m, "enter")
	if m.asking != "new" || m.newDir != filepath.Join(m.workdir, ".dot") {
		t.Errorf("a dot folder went to %q, asking %q", m.newDir, m.asking)
	}
	m, _ = press(t, m, "esc")

	// A folder that does not answer, on a dead mount, does not freeze the board: it is given up on,
	// and asked again only once it has answered.
	var asked atomic.Int32
	release := make(chan struct{})
	defer func(read func(string, string) ([]string, error), wait time.Duration) { listDir, listWait = read, wait }(listDir, listWait)
	listDir, listWait = func(text, workdir string) ([]string, error) {
		asked.Add(1)
		<-release
		return dirSuggestions(text, workdir)
	}, 20*time.Millisecond
	m, _ = press(t, m, "n", "tab")
	list(m, true, 0, -1)
	shows(m, filepath.Base(m.workdir)+" does not answer")
	m, _ = press(t, m, "b")
	if list(m, true, 0, -1); asked.Load() != 1 {
		t.Errorf("a folder that does not answer was asked %d times", asked.Load())
	}
	close(release)
	for i := 0; i < 200; i++ { // until the first answer is in
		if _, waiting := unheard.Load(m.workdir); !waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m, _ = press(t, m, "r")
	list(m, true, 1, -1)
	m, _ = press(t, m, "esc", "esc")
	listDir = dirSuggestions

	// With no workdir the folder is the one the board runs in, and the first tab lists what is in it.
	bare := loaded(t, d)
	bare, _ = press(t, bare, "n")
	if cwd, _ := os.Getwd(); bare.input.Value() != cwd+"/" {
		t.Errorf("without a workdir n prefilled %q", bare.input.Value())
	}

	// A new schedule asks for its folder the same way. Typed bare, a folder is one of workdir, and
	// the list says so.
	m = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 20})
	m, cmd := press(t, m, "S")
	m = run(t, m, cmd)
	m, _ = press(t, m, "n")
	m.input.SetValue("")
	m, _ = press(t, m, "tab", "down")
	shows(m, "folders("+m.workdir+"/)[1/15]", " > alpha/")
}

func TestNewTaskProfile(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d)
	m.profiles = []string{"work", "personal"}

	// With several profiles, a picker sits between the folder and the title.
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "enter") // the current directory exists
	if m.view != viewProfile || !strings.Contains(m.View(), "the profile") {
		t.Fatalf("no profile picker:\n%s", m.View())
	}
	m, _ = press(t, m, "j")
	m, cmd := press(t, m, "enter")
	_ = cmd
	if m.asking != "new" || m.newProfile != "personal" || !strings.Contains(m.input.Placeholder, "Profile: personal") {
		t.Fatalf("after the picker: asking %q, profile %q", m.asking, m.newProfile)
	}
	m, _ = press(t, m, "go")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	call := d.calls[len(d.calls)-1]
	if !strings.HasPrefix(call, `POST /api/tasks {"cwd":"`) || !strings.HasSuffix(call, `"profile":"personal","title":"go"}`) {
		t.Errorf("new sent %s", call)
	}

	// Escape on the picker goes back to the board.
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "esc")
	if m.view != viewBoard {
		t.Error("escape did not close the picker")
	}
}

func TestReviewView(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("revpsh01", "review", 5), task("revpr001", "review", 5))
	m, cmd := press(t, m, "R")
	if !m.reviewLoading {
		t.Error("R does not count its poll")
	}
	if next, _ := m.Update(tickMsg{}); !next.(model).reviewLoading {
		t.Error("a tick lost the poll on its way")
	}
	m = run(t, m, cmd)
	if m.reviewLoading {
		t.Error("the answer did not end the poll")
	}
	view := m.View()
	if m.view != viewReview || len(m.review.Items) != 2 {
		t.Fatalf("the review view did not open:\n%s", view)
	}
	for _, want := range []string{"review[2]", "2h00", "failed", "PR #12", "Verdict: approve", "2 commits, 3 files +10 -1",
		"2 to push", "passed", "a task waited 10m in Review on average (3 reviews)", "⏱ 5m title revpr001"} {
		if !strings.Contains(view, want) {
			t.Errorf("the view lacks %q:\n%s", want, view)
		}
	}
	var opened string
	saved := openURL
	openURL = func(url string) { opened = url }
	defer func() { openURL = saved }()
	if m, _ = press(t, m, "o"); opened != "https://github.com/o/r/pull/12" {
		t.Errorf("o opened %q", opened)
	}
	m, _ = press(t, m, "j")
	if _, cmd = press(t, m, "o"); cmd == nil {
		t.Error("o on a pushed change does not show its diff")
	}
	m, _ = press(t, m, "?")
	if m.view != viewNote || !strings.Contains(m.View(), "Review keys") {
		t.Fatalf("no help:\n%s", m.View())
	}
	if m, _ = press(t, m, "x"); m.view != viewReview {
		t.Errorf("closing the help went to view %d", m.view)
	}
	m, _ = press(t, m, "enter")
	if m.view != viewBoard || m.current() == nil || m.current().ID != "revpsh01" {
		t.Errorf("enter did not show the task on the board: view %d", m.view)
	}
	if m, _ = press(t, m, "R", "esc"); m.view != viewBoard {
		t.Errorf("escape went to view %d", m.view)
	}
}

func TestReviewTarget(t *testing.T) {
	change := &review.Change{From: "a1", To: "b2", Commits: 1}
	for _, tc := range []struct {
		item      api.ReviewItem
		url, diff string
	}{
		{api.ReviewItem{}, "", ""},
		{api.ReviewItem{PR: "https://x/pull/1", Change: change}, "https://x/pull/1", ""},
		{api.ReviewItem{Task: api.Task{Task: store.Task{Cwd: "/r"}}, Change: change}, "", "git -C /r log --reverse --stat -p a1..b2"},
		{api.ReviewItem{Task: api.Task{Task: store.Task{Cwd: "/r"}}, Change: &review.Change{From: "a1", Dirty: true}}, "", "git -C /r diff HEAD"},
		{api.ReviewItem{Change: &review.Change{From: "a1"}}, "", ""},
	} {
		url, diff := reviewTarget(tc.item)
		if url != tc.url || strings.Join(diff, " ") != tc.diff {
			t.Errorf("reviewTarget(%+v) = %q %q, want %q %q", tc.item, url, diff, tc.url, tc.diff)
		}
	}
}

func TestSchedulesView(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("made0001", "running", 5))
	m, cmd := press(t, m, "S")
	m = run(t, m, cmd)
	if m.view != viewSchedules || len(m.schedules) != 1 || !strings.Contains(m.View(), "Standup") {
		t.Fatalf("the schedules view did not open:\n%s", m.View())
	}

	// A new schedule: folder, time (a wrong one is asked again), days, catch-up, title, prompt.
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "enter") // the current directory exists, and there is no profile to pick
	if m.asking != "schedule-at" || m.input.Value() != "08:00" {
		t.Fatalf("after the folder: asking %q with %q", m.asking, m.input.Value())
	}
	m.input.SetValue("25:00")
	m, _ = press(t, m, "enter")
	if m.asking != "schedule-at" || !m.noticeAlarm {
		t.Fatalf("a wrong time was accepted: asking %q", m.asking)
	}
	m.input.SetValue("7:30")
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "enter") // daily
	if m.view != viewNote || !strings.Contains(m.View(), "07:30") || !strings.Contains(m.View(), "as soon as the machine is back") {
		t.Fatalf("no catch-up question:\n%s", m.View())
	}
	m, cmd = press(t, m, "y")
	m = run(t, m, cmd)
	if m.asking != "schedule-title" || m.view != viewSchedules || !m.draft.catchUp {
		t.Fatalf("after catch-up: asking %q, view %d", m.asking, m.view)
	}
	m, _ = press(t, m, "Notes")
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "enter") // an empty prompt is refused
	if m.asking != "schedule-prompt" {
		t.Fatalf("an empty prompt was accepted")
	}
	m, _ = press(t, m, "go")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	var posted string
	for _, call := range d.calls {
		if strings.HasPrefix(call, "POST /api/schedules ") {
			posted = call
		}
	}
	for _, want := range []string{`"at":"07:30"`, `"catch_up":true`, `"days":"daily"`, `"prompt":"go"`, `"title":"Notes"`} {
		if !strings.Contains(posted, want) {
			t.Errorf("the schedule sent lacks %s: %s", want, posted)
		}
	}
	if !strings.Contains(m.notice, "Schedule Standup added") {
		t.Errorf("notice = %q", m.notice)
	}

	// A cron line skips the days and goes to catch-up.
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "enter")
	m.input.SetValue("*/15 * * * *")
	m, _ = press(t, m, "enter")
	if m.view != viewNote || !strings.Contains(m.View(), "its time") || !strings.Contains(m.View(), "3/5, catch-up") {
		t.Fatalf("a cron line did not go to catch-up:\n%s", m.View())
	}
	m, cmd = press(t, m, "n")
	m = run(t, m, cmd)
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "go")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	for _, call := range d.calls {
		if strings.HasPrefix(call, "POST /api/schedules ") {
			posted = call
		}
	}
	if !strings.Contains(posted, `"at":"*/15 * * * *"`) || !strings.Contains(posted, `"days":"daily"`) {
		t.Errorf("the cron schedule sent: %s", posted)
	}

	// Pause, run now, and enter shows the last run's task on the board.
	m, cmd = press(t, m, " ")
	m = run(t, m, cmd)
	m, cmd = press(t, m, "r")
	m = run(t, m, cmd)
	if !strings.Contains(m.notice, "started as made0001") {
		t.Errorf("run now: %q", m.notice)
	}
	m, _ = press(t, m, "enter")
	if m.view != viewBoard || m.keys[m.cursor] != "made0001" {
		t.Errorf("enter did not show the last task: view %d, cursor on %v", m.view, m.keys)
	}
}

func TestRename(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5))
	m, _ = press(t, m, "t")
	if m.asking != "title" || m.input.Value() != "title run00001" {
		t.Fatalf("t asked %q with %q", m.asking, m.input.Value())
	}
	m, _ = press(t, m, "!")
	m, cmd := press(t, m, "enter")
	m = run(t, m, cmd)
	if m.notice != "made0001 renamed" {
		t.Errorf("after rename: %q", m.notice)
	}
	if call := d.calls[len(d.calls)-1]; call != `PATCH /api/tasks/run00001 {"title":"title run00001!"}` {
		t.Errorf("rename sent %s", call)
	}
}

func TestSelectAndDelete(t *testing.T) {
	d := newDaemon(t)
	run1 := task("run00001", "running", 5)
	run1.Agents = []api.Agent{{Name: "Explore", Desc: "look", State: "running"}}
	m := loaded(t, d, run1, task("need0001", "needs", 9), task("rev00001", "review", 1))
	m, _ = press(t, m, "v")
	if !m.selecting || !strings.Contains(m.View(), "[ ]  run00001") || !strings.Contains(m.View(), "select: 0 ticked") {
		t.Fatalf("v did not open select mode:\n%s", m.View())
	}
	// need0001 is ticked then unticked. The child row has no box: space on it ticks nothing. Other keys
	// do nothing.
	m, _ = press(t, m, " ", "down", " ", "down", " ", "up", "up", " ", "down", "down", "down", " ", "d", "enter")
	if !m.ticked["run00001"] || len(m.ticked) != 2 || !m.ticked["rev00001"] || m.view != viewBoard || m.showDone {
		t.Fatalf("ticked %v, view %v", m.ticked, m.view)
	}
	if !strings.Contains(m.View(), "[x]  rev00001") || !strings.Contains(m.View(), "select: 2 ticked") {
		t.Errorf("the ticks are not drawn:\n%s", m.View())
	}
	// One question for the batch. No keeps the ticks.
	m, _ = press(t, m, "x")
	if m.view != viewNote || m.noteText != "Delete 2 tasks? 1 running.  y / n" {
		t.Fatalf("x asked %v %q", m.view, m.noteText)
	}
	m, _ = press(t, m, "n")
	if !m.selecting || len(m.ticked) != 2 || len(d.calls) != 0 {
		t.Fatalf("no: %v %v, calls %v", m.selecting, m.ticked, d.calls)
	}
	m, cmd := press(t, m, "x", "y")
	if m.selecting || m.ticked != nil {
		t.Errorf("y left select mode on: %v %v", m.selecting, m.ticked)
	}
	m = run(t, m, cmd)
	want := []string{"DELETE /api/tasks/run00001", "DELETE /api/tasks/rev00001"}
	if !slices.Equal(d.calls, want) || m.notice != "2 tasks deleted" || m.noticeAlarm {
		t.Errorf("x sent %v, said %q", d.calls, m.notice)
	}
	if strings.Contains(m.View(), "[ ]") {
		t.Errorf("the boxes stayed after x:\n%s", m.View())
	}
}

func TestSelectParkAndMove(t *testing.T) {
	d := newDaemon(t)
	tasks := []api.Task{task("need0001", "needs", 9), task("run00001", "running", 5), task("park0001", "parked", 3), task("done0001", "done", 1)}
	m := loaded(t, d, tasks...)
	m, _ = press(t, m, "d", "v", " ", "down", " ", "down", " ", "down", " ")
	if len(m.ticked) != 4 || !strings.Contains(m.View(), "[x]  done0001") {
		t.Fatalf("a done task got no box: %v\n%s", m.ticked, m.View())
	}
	// p parks what it can, and names what it skips.
	m, _ = press(t, m, "p")
	if m.noteText != "Park 2 tasks? 1 running. Skipped: park0001 is already parked, done0001 is done.  y / n" {
		t.Fatalf("p asked %q", m.noteText)
	}
	m, cmd := press(t, m, "y")
	m = run(t, m, cmd)
	want := []string{`PATCH /api/tasks/need0001 {"state":"parked"}`, `PATCH /api/tasks/run00001 {"state":"parked"}`}
	if !slices.Equal(d.calls, want) || m.notice != "2 tasks parked, 2 skipped" {
		t.Fatalf("p sent %v, said %q", d.calls, m.notice)
	}
	// Nothing left to park: no question.
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: tasks}})
	m, _ = press(t, m, "v", "up", " ", "p") // the cursor stayed on done0001
	if m.view != viewBoard || !m.selecting || !m.noticeAlarm || m.notice != "Nothing to park. Skipped: park0001 is already parked." {
		t.Fatalf("p on a parked task: %v %q", m.view, m.notice)
	}
	// m picks one state for every ticked task. Escape in the picker keeps the ticks.
	m, _ = press(t, m, "up", "up", " ", "m")
	if m.view != viewPick || m.pick != slices.Index(store.States, "needs") || !strings.Contains(m.View(), "Move 2 tasks to") {
		t.Fatalf("m opened %v at %d:\n%s", m.view, m.pick, m.View())
	}
	m, _ = press(t, m, "esc")
	if m.view != viewBoard || len(m.ticked) != 2 || m.pickBatch {
		t.Fatalf("escape in the picker: %v %v", m.view, m.ticked)
	}
	m, _ = press(t, m, "m", "enter")
	if m.noteText != "Move 1 task to Needs me? Skipped: need0001 is already in Needs me.  y / n" {
		t.Fatalf("m asked %q", m.noteText)
	}
	m, _ = press(t, m, "n", "m", "down", "enter")
	if m.noteText != "Move 2 tasks to Review?  y / n" {
		t.Fatalf("m asked %q", m.noteText)
	}
	calls := len(d.calls)
	m, cmd = press(t, m, "y")
	m = run(t, m, cmd)
	want = []string{`PATCH /api/tasks/need0001 {"state":"review"}`, `PATCH /api/tasks/park0001 {"state":"review"}`}
	if !slices.Equal(d.calls[calls:], want) || m.notice != "2 tasks moved to Review" || m.selecting {
		t.Errorf("m sent %v, said %q", d.calls[calls:], m.notice)
	}
}

func TestHideADoneTask(t *testing.T) {
	d := newDaemon(t)
	hidden := task("hid00001", "done", 1)
	hidden.Hidden = true
	tasks := []api.Task{task("run00001", "running", 5), task("done0001", "done", 1), hidden}
	m := loaded(t, d, tasks...)
	m, _ = press(t, m, "h")
	if len(d.calls) != 0 || !m.noticeAlarm || m.notice != "Only a done task can be hidden" {
		t.Fatalf("h on a running task sent %v, said %q", d.calls, m.notice)
	}
	m, cmd := press(t, m, "down", "h")
	m = run(t, m, cmd)
	if !slices.Equal(d.calls, []string{`PATCH /api/tasks/done0001 {"hidden":true}`}) || m.notice != "made0001 hidden, d shows it" {
		t.Fatalf("h on a done task sent %v, said %q", d.calls, m.notice)
	}
	// d shows the hidden one, and h on it shows it again.
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: tasks}})
	m, cmd = press(t, m, "d", "G", "h")
	m = run(t, m, cmd)
	if d.calls[1] != `PATCH /api/tasks/hid00001 {"hidden":false}` || m.notice != "made0001 shown again" {
		t.Fatalf("h on a hidden task sent %v, said %q", d.calls, m.notice)
	}
	// In select mode h hides every ticked done task, and names what it skips.
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: tasks}})
	m, _ = press(t, m, "v", " ", "up", " ", "up", " ", "h")
	if m.noteText != "Hide 1 task? Skipped: run00001 is not done, hid00001 is already hidden.  y / n" {
		t.Fatalf("h asked %q", m.noteText)
	}
	calls := len(d.calls)
	m, cmd = press(t, m, "y")
	m = run(t, m, cmd)
	if !slices.Equal(d.calls[calls:], []string{`PATCH /api/tasks/done0001 {"hidden":true}`}) || m.notice != "1 task hidden, 2 skipped" {
		t.Errorf("h sent %v, said %q", d.calls[calls:], m.notice)
	}
}

func TestPinATask(t *testing.T) {
	d := newDaemon(t)
	pinned := task("pin00001", "running", 9)
	pinned.PinnedAt = 100
	m := loaded(t, d, task("run00001", "running", 5), pinned, task("old00001", "running", 7))
	if m.keys[0] != "pin00001" || !strings.Contains(m.View(), "▲ title pin00001") {
		t.Fatalf("the pinned task is not on top, marked: %v", m.keys)
	}
	m, cmd := press(t, m, "P")
	m = run(t, m, cmd)
	if !slices.Equal(d.calls, []string{`PATCH /api/tasks/pin00001 {"pinned":false}`}) || m.notice != "made0001 unpinned" {
		t.Fatalf("P on a pinned task sent %v, said %q", d.calls, m.notice)
	}
	m, cmd = press(t, m, "down", "P")
	m = run(t, m, cmd)
	if d.calls[1] != `PATCH /api/tasks/run00001 {"pinned":true}` || m.notice != "made0001 pinned to the top" {
		t.Fatalf("P on a task sent %v, said %q", d.calls, m.notice)
	}
	m, cmd = press(t, m, "down", "P")
	m = run(t, m, cmd)
	if d.calls[2] != `PATCH /api/tasks/old00001 {"pinned":true}` || !m.noticeAlarm || !strings.Contains(m.notice, "hc restart") {
		t.Fatalf("P against an older daemon sent %v, said %q", d.calls, m.notice)
	}
}

func TestSelectEscapeAndFailures(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5), task("gone0001", "running", 5))
	m, _ = press(t, m, "v", " ", "esc")
	if m.selecting || m.ticked != nil || len(d.calls) != 0 {
		t.Fatalf("escape: %v %v, calls %v", m.selecting, m.ticked, d.calls)
	}
	for _, key := range []string{"x", "m", "p"} {
		m, cmd := press(t, m, "v", key)
		if !m.selecting || cmd != nil || m.view != viewBoard || m.notice != "Nothing is ticked: space ticks the task under the cursor" {
			t.Errorf("%s on nothing: %v %v %q", key, m.selecting, m.view, m.notice)
		}
	}
	m, cmd := press(t, m, "v", " ", "down", " ", "x", "y")
	m = run(t, m, cmd)
	if !m.noticeAlarm || m.notice != "1 of 2 tasks deleted. gone0001: No task matches" {
		t.Errorf("a failed delete said %q", m.notice)
	}
	m, cmd = press(t, m, "v", "up", " ", "x", "y")
	if m = run(t, m, cmd); m.notice != "1 task deleted" {
		t.Errorf("one delete said %q", m.notice)
	}
}

func TestSelectKeepsTicksInStep(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("need0001", "needs", 9), task("run00001", "running", 5), task("rev00001", "review", 1))
	m, _ = press(t, m, "v", " ", "down", " ")
	if len(m.ticked) != 2 || m.idle() {
		t.Fatalf("ticked %v, or select mode reads as idle", m.ticked)
	}
	// need0001 left the board, run00001 finished elsewhere and is hidden with the done tasks: their
	// ticks go with their boxes.
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{task("run00001", "running", 5), task("rev00001", "review", 1)}}})
	m, _ = press(t, m, "down", " ")
	if !m.ticked["run00001"] || !m.ticked["rev00001"] || len(m.ticked) != 2 {
		t.Fatalf("after a poll: %v", m.ticked)
	}
	finished := task("run00001", "done", 5)
	finished.Hidden = true
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{finished, task("rev00001", "review", 1)}}})
	if len(m.ticked) != 1 || !strings.Contains(m.View(), "select: 1 ticked") {
		t.Fatalf("a task hidden since stayed ticked: %v", m.ticked)
	}
	m, cmd := press(t, m, "x", "y")
	if m = run(t, m, cmd); !slices.Equal(d.calls, []string{"DELETE /api/tasks/rev00001"}) || m.notice != "1 task deleted" {
		t.Errorf("x sent %v, said %q", d.calls, m.notice)
	}
	// Not while a new task is on its way.
	m.sending = 1
	if m, _ = press(t, m, "v"); m.selecting {
		t.Error("v opened select mode under a new task on its way")
	}
}

func TestSelectQuestionStaysTrue(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5), task("rev00001", "review", 1))
	// A release found while the batch question is open waits: y answers the batch, not the update.
	m, _ = press(t, m, "v", " ", "down", " ", "x")
	m = send(t, m, checkedMsg{rel: update.Release{Tag: "v9.9.9"}, newer: true})
	if m.noteText != "Delete 2 tasks? 1 running.  y / n" || m.notice != "v9.9.9 is out: press u once you are done to install it" {
		t.Fatalf("the update took over the question: %q, %q", m.noteText, m.notice)
	}
	m, _ = press(t, m, "n")
	if m = send(t, m, checkedMsg{rel: update.Release{Tag: "v9.9.9"}, newer: true}); m.view != viewBoard || len(m.ticked) != 2 {
		t.Fatalf("the update opened over the ticks: %v %v", m.view, m.ticked)
	}
	// y plans the batch again: rev00001 parked meanwhile is skipped, run00001 done meanwhile is too.
	m, _ = press(t, m, "p")
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{task("run00001", "running", 5), task("rev00001", "parked", 1)}}})
	m, cmd := press(t, m, "y")
	m = run(t, m, cmd)
	if !slices.Equal(d.calls, []string{`PATCH /api/tasks/run00001 {"state":"parked"}`}) || m.notice != "1 task parked, 1 skipped" {
		t.Fatalf("p sent %v, said %q", d.calls, m.notice)
	}
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{task("run00001", "running", 5), task("rev00001", "review", 1)}}})
	m, _ = press(t, m, "v", "up", " ", "p")
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{task("run00001", "parked", 5), task("rev00001", "review", 1)}}})
	m, cmd = press(t, m, "y")
	if cmd != nil || !m.selecting || len(m.ticked) != 1 || m.notice != "Nothing to park. Skipped: run00001 is already parked." {
		t.Fatalf("y on nothing left to do: %v %v %q", m.selecting, m.ticked, m.notice)
	}
	// The picker closes when a poll takes every ticked task away.
	m, _ = press(t, m, "m")
	m = send(t, m, boardMsg{board: api.Tasks{Tasks: []api.Task{task("rev00001", "review", 1)}}})
	if m.view != viewBoard || m.pickBatch || m.notice != "The ticked tasks left the board" {
		t.Errorf("the picker stayed with no ticks: %v %q", m.view, m.notice)
	}
}

func TestDirSuggestions(t *testing.T) {
	workdir := t.TempDir()
	// The last two cannot be typed, and would draw over the board: they are left out.
	for _, dir := range []string{"idea", "ideal", "other", ".hidden", "idea\nbroken", "idea\x1b[31mred"} {
		if err := os.Mkdir(filepath.Join(workdir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, "ideafile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err) // a file: never suggested
	}
	// A link counts for what it points at: a folder, not a file, not nothing.
	for link, target := range map[string]string{"linked": "other", "linkfile": "ideafile", "linkless": "gone"} {
		if err := os.Symlink(filepath.Join(workdir, target), filepath.Join(workdir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, text, workdir string
		want                []string
	}{
		{"bare name", "ide", workdir, []string{"idea", "ideal"}},
		{"absolute", workdir + "/ide", "", []string{workdir + "/idea", workdir + "/ideal"}},
		{"inside the folder", workdir + "/", "", []string{workdir + "/idea", workdir + "/ideal", workdir + "/linked", workdir + "/other"}},
		{"dot folders", workdir + "/.", "", []string{workdir + "/.hidden"}},
		{"a link to a folder", "li", workdir, []string{"linked"}},
		{"another case", "IDE", workdir, []string{"idea", "ideal"}},
		{"nothing fits", "zzz", workdir, nil},
	} {
		if got, err := dirSuggestions(c.text, c.workdir); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v, %v", c.name, got, err)
		}
	}
	if got, err := dirSuggestions("nope/zzz", workdir); got != nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing parent = %v, %v", got, err)
	}
	// A name is looked up under workdir, one that starts with a dot too. Only ./ and ../ say otherwise.
	for text, want := range map[string]string{
		"ide": workdir, ".hid": workdir, ".": workdir, "sub/x": workdir + "/sub", "./x": "./", "../x": "../", "/abs/x": "/abs/",
	} {
		if _, _, parent := dirParent(text, workdir); parent != want {
			t.Errorf("the parent of %q = %q, want %q", text, parent, want)
		}
	}
	if _, _, parent := dirParent("ide", ""); parent != "." {
		t.Errorf("without a workdir the parent = %q", parent)
	}
}

func TestDetailQuestionAndHelp(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5))

	m, cmd := press(t, m, "enter")
	if m.view != viewDetail || !strings.Contains(m.View(), "Loading run00001") {
		t.Fatalf("the detail did not open")
	}
	m = run(t, m, cmd)
	m.detailID = "made0001" // the stand-in daemon answers with this task whatever is asked
	m = run(t, m, m.loadDetail())
	m = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 60}) // tall enough for the whole detail
	for _, piece := range []string{
		"title made0001", "  state    Backlog", "Session log", "  you   | hello", "        | … 3 turns …", "  tool  | $ ls",
		"  coord | [from coordinator 1a2b3c4d] stop", "escape or q back",
		"Handoff", "  /state/handoff/made0001.md, written ", "    status   Halfway.", "    queue    1 open, 0 done, next: docs",
	} {
		if !strings.Contains(m.View(), piece) {
			t.Errorf("the detail lacks %q:\n%s", piece, m.View())
		}
	}
	m, _ = press(t, m, "j", "q")
	if m.view != viewBoard {
		t.Error("q did not close the detail")
	}

	// A question: the waiting box runs until the answer arrives.
	m, _ = press(t, m, "q", "w", "h", "y")
	m, cmd = press(t, m, "enter")
	if m.view != viewWaiting || !strings.Contains(m.View(), "Asking about run00001") || !strings.Contains(m.View(), "esc to give up") {
		t.Fatalf("the waiting box did not open:\n%s", m.View())
	}
	m = send(t, m, frameMsg{}, frameMsg{})
	if m.ticks != 3 || !strings.Contains(m.View(), "6 m") {
		t.Errorf("the ball has flown %d ticks", m.ticks)
	}
	m = send(t, m, m.question("run00001", "why")())
	if m.view != viewNote || !strings.Contains(m.View(), "run00001: what is left?") || !strings.Contains(m.View(), "All good.") {
		t.Errorf("the answer is not shown:\n%s", m.View())
	}
	m, _ = press(t, m, "x") // any key closes it, and is not taken as the delete key
	if m.view != viewBoard {
		t.Error("the answer did not close")
	}
	// Given up on with escape: the answer that arrives later is dropped.
	m, _ = press(t, m, "q")
	m, _ = press(t, m, "enter")
	late := m.question("run00001", "")
	m, _ = press(t, m, "esc")
	if m = send(t, m, late()); m.view != viewBoard {
		t.Error("an abandoned question still showed its answer")
	}

	m, _ = press(t, m, "?")
	if m.view != viewNote || !strings.Contains(m.View(), "attach: enter the session in this terminal") {
		t.Errorf("the help is not shown:\n%s", m.View())
	}
}

func TestProfilesThatShareALimitAreOneAccount(t *testing.T) {
	// The work profile has reported the week, not yet the session, of the account it shares with personal.
	shared := model{limits: []limits.Entry{
		{Profile: "personal", Label: "5h", Pct: 30, ResetsIn: 3600},
		{Profile: "personal+work", Label: "7d", Pct: 60, ResetsIn: 86400},
	}, width: 200, client: client.New("http://127.0.0.1:7337")}
	head := strings.Join(shared.header(), "\n")
	for _, want := range []string{"Current session · 5h · personal", "30% used", "Current week · 7d · personal+work", "60% used"} {
		if !strings.Contains(head, want) {
			t.Errorf("panel without %q:\n%s", want, head)
		}
	}
	if got, want := text(shared.limitRow("7d")), "━━━━━━────  60%  1d  personal+work"; got != want {
		t.Errorf("7d row of a shared limit = %q, want %q", got, want)
	}
	shared.width = 90 // no room for the panel: the rows keep the names, within the left column
	for i, line := range shared.header()[4:6] {
		if want := []string{"5h:    ━━━─────── ", "7d:    ━━━━━━──── "}[i]; !strings.HasPrefix(line, want) ||
			!strings.Contains(line, []string{"personal", "personal+work"}[i]) || lipgloss.Width(line) > hintsFrom {
			t.Errorf("narrow row %d = %q", i, line)
		}
	}
	shared.limits[1].Profile = "a-very-long-profile-name+another-very-long-profile-name+personal"
	if panel := shared.usagePanel(40); len(panel) != 6 || lipgloss.Width(panel[3]) > 40 {
		t.Errorf("a long name overflows the panel: %q", panel)
	}
	for name, entries := range map[string][]limits.Entry{
		"no profile in common": {{Profile: "personal", Label: "5h"}, {Profile: "work", Label: "7d"}},
		"a window twice":       {{Profile: "personal", Label: "5h"}, {Profile: "personal+work", Label: "5h"}},
	} {
		apart := model{limits: entries, width: 200, client: shared.client}
		if head := strings.Join(apart.header(), "\n"); strings.Contains(head, "Current session") {
			t.Errorf("%s: a panel for several accounts:\n%s", name, head)
		}
	}
}

func TestLimitRowsKeepAGapBeforeTheHints(t *testing.T) {
	later, soon := 19*3600+11*60, 600
	m := model{limits: []limits.Entry{
		{Profile: "personal", Label: "5h", Pct: 2, ResetsIn: 3600},
		{Profile: "personal", Label: "7d", Pct: 40, ResetsIn: 86400, LimitIn: &later},
	}, width: 110, client: client.New("http://127.0.0.1:7337")}
	head := m.header()
	if got, want := text(m.limitRow("7d")), "━━━━━━━───────────  40%  limit 19h11"; got != want {
		t.Errorf("a long limit row = %q, want %q", got, want)
	}
	for i, line := range head[4:6] {
		if want := []string{"5h:    ━─────", "7d:    ━━━━━━━─"}[i]; !strings.HasPrefix(line, want) {
			t.Errorf("row %d = %q, want it to start with %q", i, line, want)
		}
		if !gapBeforeHints(line) {
			t.Errorf("row %d runs into the hints: %q", i, line)
		}
	}
	m.width = 90 // no hints: the words stay
	if got, want := text(m.limitRow("7d")), "━━━━━━━───────────  40%  limit in 19h11"; got != want {
		t.Errorf("a long limit row without hints = %q, want %q", got, want)
	}
	m.width, m.limits[1].LimitIn = 110, &soon // it fits: the words stay
	if got, want := text(m.limitRow("7d")), "━━━━━━━───────────  40%  limit in 10m"; got != want {
		t.Errorf("a short limit row = %q, want %q", got, want)
	}
	reached := 0
	m.limits[1].Pct, m.limits[1].LimitIn = 100, &reached
	if line := m.header()[5]; !strings.HasPrefix(line, "7d:    ━━━━━━━━━━━━━━━━━━ 100%  limit hit ") || !gapBeforeHints(line) {
		t.Errorf("a reached limit next to the hints = %q", line)
	}
	m.limits = append(m.limits, limits.Entry{Profile: "a-long-profile-name", Label: "5h", Pct: 7, ResetsIn: 7200})
	if line := m.header()[4]; !gapBeforeHints(line) {
		t.Errorf("a row of several accounts runs into the hints: %q", line)
	}
}

// gapBeforeHints is whether a header line has two spaces right before the hints.
func gapBeforeHints(line string) bool {
	runes := []rune(line)
	return len(runes) > hintsFrom && string(runes[hintsFrom-hintsGap:hintsFrom+1]) == "  <"
}

func TestLimitsLineAndGauges(t *testing.T) {
	soon := 600
	entries := []limits.Entry{
		{Profile: "personal", Label: "5h", Pct: 50, ResetsIn: 3600, Level: "hot", LimitIn: &soon},
		{Profile: "personal", Label: "7d", Pct: 4, ResetsIn: 86400 * 3, Level: "ok", Stale: true},
		{Profile: "work", Label: "5h", Pct: 100, ResetsIn: 60, Level: "hot"},
	}
	m := model{limits: entries}
	got := text(m.limitRow("5h"))
	want := "personal 50% (limit 10m)  work 100% (1m)  "
	if got != want {
		t.Errorf("5h row = %q, want %q", got, want)
	}
	one := model{limits: entries[:2]} // one account needs no name, and gets a meter
	if got, want := text(one.limitRow("5h")), "━━━━━━━━━─────────  50%  limit in 10m"; got != want {
		t.Errorf("5h row of one account = %q, want %q", got, want)
	}
	if got, want := text(one.limitRow("7d")), "━─────────────────   4%  3d"; got != want {
		t.Errorf("7d row of one account = %q, want %q", got, want)
	}
	m.width, m.client = 140, client.New("http://127.0.0.1:7337")
	m.limits = append(m.limits, limits.Entry{Profile: "a-long-profile+another", Label: "5h", Pct: 7, ResetsIn: 7200})
	for i, line := range m.header() { // a long line is cut to its column, never wrapped
		if strings.Contains(line, "\n") || lipgloss.Width(line) > m.width {
			t.Errorf("header line %d is wrapped or too wide: %q", i, line)
		}
	}
	reached := 0
	if got := text(model{limits: []limits.Entry{{Label: "5h", Pct: 100, LimitIn: &reached}}}.limitRow("5h")); got != "━━━━━━━━━━━━━━━━━━ 100%  limit reached" {
		t.Errorf("a reached limit = %q", got)
	}
	for pct, want := range map[int]string{0: "────", 2: "━───", 13: "━───", 50: "━━──", 99: "━━━─", 100: "━━━━", 140: "━━━━"} {
		if got := text(meter(pct, 4, plain)); got != want {
			t.Errorf("meter at %d%% = %q, want %q", pct, got, want)
		}
	}
	one.width, one.client = 200, m.client // room for the panel: it takes the place of the 5h and 7d rows
	head := strings.Join(one.header(), "\n")
	for _, want := range []string{"Current session · 5h", "50% used", "Limit in 10m", "Current week · 7d", "4% used", "Resets in 3d"} {
		if !strings.Contains(head, want) {
			t.Errorf("panel without %q:\n%s", want, head)
		}
	}
	for i, line := range one.header() {
		if strings.HasPrefix(line, "5h:") || strings.HasPrefix(line, "7d:") || lipgloss.Width(line) > one.width {
			t.Errorf("header line %d with the panel = %q", i, line)
		}
	}
	if head := one.header(); lastRuneIndex(head[1], "─") != lastRuneIndex(head[4], "─") { // 50% and 4%
		t.Errorf("the two meters of the panel end apart:\n%s\n%s", head[1], head[4])
	}
	down := one
	down.down = "connection refused" // the panel would lose its first line to the error
	if head := strings.Join(down.header(), "\n"); strings.Contains(head, "Current week") || !strings.Contains(head, "5h:    ━━━━") {
		t.Errorf("header with the board down:\n%s", head)
	}
	one.width = 150 // the panel shows at the same widths in every view
	for _, v := range []view{viewBoard, viewSchedules, viewReview} {
		one.view = v
		if head := strings.Join(one.header(), "\n"); strings.Contains(head, "Current session") {
			t.Errorf("panel at 150 columns in view %v:\n%s", v, head)
		}
	}
	one.view = viewBoard
	one.width = 140 // no room: the rows of the left column come back
	if head := strings.Join(one.header(), "\n"); strings.Contains(head, "Current session") || !strings.Contains(head, "5h:    ━━━━") {
		t.Errorf("narrow header:\n%s", head)
	}
	m.width = 200 // several accounts keep their line, and get no panel
	if head := strings.Join(m.header(), "\n"); strings.Contains(head, "Current session") || !strings.Contains(head, "5h:    personal 50%") {
		t.Errorf("header of several accounts:\n%s", head)
	}
	if got := text(model{}.limitRow("5h")); got != "n/a" {
		t.Errorf("no limits = %q", got)
	}
	if got := text(tokensLine(nil)); got != "tokens in 0 out 0 cache 0" {
		t.Errorf("tokens line = %q", got)
	}
	for pct, want := range map[int]string{0: "1.0k 0% ▁", 50: "1.0k 50% ▅", 99: "1.0k 99% █", 100: "1.0k 100% █"} {
		if got := contextCell(api.Context{Tokens: 1000, Pct: pct}).text; got != want {
			t.Errorf("gauge at %d%% = %q, want %q", pct, got, want)
		}
	}
	if contextCell(api.Context{}).text != "-" {
		t.Error("an empty context should show a dash")
	}
	frame := pitchFrame(40)
	if lines := strings.Split(frame, "\n"); len(lines) != 7 || !strings.Contains(frame, "80 m") || !strings.Contains(lines[4], "o") {
		t.Errorf("frame =\n%s", frame)
	}
}

func TestNewTaskWhenTheBoardsFolderIsGone(t *testing.T) {
	defer func(real func() (string, error)) { getwd = real }(getwd)
	getwd = func() (string, error) { return "", errors.New("getwd: no such file or directory") }

	// With no folder to offer the input starts empty: a lone slash would put the task at the root.
	m, _ := press(t, loaded(t, nil), "n")
	if m.asking != "newdir" || m.input.Value() != "" {
		t.Errorf("n prefilled %q, asking %q", m.input.Value(), m.asking)
	}
	// Sent empty, it asks for a folder again rather than to create one with no name.
	if m, _ = press(t, m, "enter"); m.view != viewBoard || m.asking != "newdir" || m.notice != "Type a folder: the one the board runs in is gone" {
		t.Errorf("an empty folder: view %d, asking %q, notice %q", m.view, m.asking, m.notice)
	}
	// A workdir does not depend on where the board runs.
	m = loaded(t, nil)
	m.workdir = t.TempDir()
	if m, _ = press(t, m, "n"); m.input.Value() != m.workdir+"/" {
		t.Errorf("with a workdir n prefilled %q", m.input.Value())
	}
}

func TestRefusedTaskKeepsItsTitle(t *testing.T) {
	d := newDaemon(t)
	source := task("src00001", "review", 5)
	source.Cwd, source.Profile = t.TempDir(), "gone"
	m := loaded(t, d, source)

	// The daemon refuses the copy: its reason is shown, and the title is back in its input, with the
	// folder and the profile of the copy, even after a new task was begun and dropped meanwhile.
	m, _ = press(t, m, "c")
	asked := m.input.Placeholder
	m, _ = press(t, m, "refuse", " ", "me")
	m, cmd := press(t, m, "enter")
	m, _ = press(t, m, "n", "esc")
	m = run(t, m, cmd)
	if m.notice != "Unknown profile 'gone'. Known: work" || !m.noticeAlarm {
		t.Errorf("the refusal said %q", m.notice)
	}
	if m.asking != "new" || m.input.Value() != "refuse me" || m.input.Placeholder != asked || !strings.HasPrefix(asked, "Copy of src00001") {
		t.Fatalf("after the refusal: asking %q with %q under %q", m.asking, m.input.Value(), m.input.Placeholder)
	}
	if m.newDir != source.Cwd || m.newProfile != "gone" {
		t.Errorf("after the refusal: folder %q, profile %q", m.newDir, m.newProfile)
	}
	// Sent again once corrected, it goes through. Escape would have dropped it.
	for range "refuse me" {
		m, _ = press(t, m, "backspace")
	}
	m, _ = press(t, m, "again")
	m, cmd = press(t, m, "enter")
	m = run(t, m, cmd)
	if call := d.calls[len(d.calls)-1]; call != `POST /api/tasks {"cwd":"`+source.Cwd+`","profile":"gone","title":"again"}` || m.jump != "made0001" || m.asking != "" {
		t.Errorf("the second try sent %s, jump %q, asking %q", call, m.jump, m.asking)
	}

	// A new task is kept the same way. A refusal that comes back over another input leaves that one alone.
	m, _ = press(t, m, "n", "enter")
	m, _ = press(t, m, "refuse")
	m, cmd = press(t, m, "enter")
	late := cmd()
	if m = send(t, m, late); m.asking != "new" || m.input.Value() != "refuse" || !strings.HasPrefix(m.input.Placeholder, "New task") {
		t.Errorf("a refused new task: asking %q with %q", m.asking, m.input.Value())
	}
	m, _ = press(t, m, "esc", "/")
	if m = send(t, m, late); m.asking != "filter" || m.input.Value() != "" || !m.noticeAlarm {
		t.Errorf("a late refusal took over the filter: asking %q with %q", m.asking, m.input.Value())
	}

	// A daemon that did not answer in time may have made the task: the title is not offered again.
	m, _ = press(t, m, "esc")
	slow := newTaskMsg{err: client.NoAnswer{URL: "http://127.0.0.1:1", After: 10 * time.Second}, asked: "New task", title: "slow"}
	if m = send(t, m, slow); m.asking != "" || !strings.HasSuffix(m.notice, "did not answer within 10s: the task may be on the board already") {
		t.Errorf("no answer: asking %q, notice %q", m.asking, m.notice)
	}
}

func TestInputFollowsTheWidth(t *testing.T) {
	m := loaded(t, nil, task("run00001", "running", 5)) // 200 wide
	m, _ = press(t, m, "r")
	empty := strings.Count(send(t, m, tea.WindowSizeMsg{Width: 60, Height: 20}).View(), "\n")
	m.input.SetValue(strings.Repeat("a long reply ", 12) + "the end")

	// A terminal made narrower shows the text around the cursor on its one line, not the text
	// wrapped, wherever the cursor is.
	for _, at := range []struct {
		pos  int
		seen string
	}{{-1, "the end"}, {0, "a long reply"}, {80, "a long"}} {
		wide := m
		if at.pos < 0 {
			wide.input.CursorEnd()
		} else {
			wide.input.SetCursor(at.pos)
		}
		view := send(t, wide, tea.WindowSizeMsg{Width: 60, Height: 20}).View()
		if lines := strings.Count(view, "\n"); lines != empty || !strings.Contains(view, at.seen) {
			t.Errorf("cursor at %d: the screen is %d lines, %d with an empty input:\n%s", at.pos, lines, empty, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > 60 {
				t.Errorf("cursor at %d: a line is %d wide on a screen of 60: %q", at.pos, lipgloss.Width(line), line)
			}
		}
	}
}

func TestWordsThatReadLikeKeys(t *testing.T) {
	d := newDaemon(t)
	m := loaded(t, d, task("run00001", "running", 5))
	word := func(text string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)} }

	// In an input line, text that arrives several characters at once is typed, whatever it spells.
	m, _ = press(t, m, "r")
	m = send(t, m, word("press "), word("enter"), word(" then "), word("esc"), word(" or "), word("ctrl+c"), word(" left"))
	if m.asking != "reply" || m.input.Value() != "press enter then esc or ctrl+c left" {
		t.Fatalf("the words left %q, asking %q", m.input.Value(), m.asking)
	}
	m, cmd := press(t, m, "enter")
	m = run(t, m, cmd)
	if call := d.calls[len(d.calls)-1]; call != `POST /api/tasks/run00001/reply {"text":"press enter then esc or ctrl+c left"}` {
		t.Errorf("the reply sent %s", call)
	}

	// On the board such text is no key at all: nothing opens, nothing quits.
	for _, text := range []string{"enter", "ctrl+c", "ctrl+q", "esc", "nn", "xy"} {
		next, cmd := m.Update(word(text))
		if m = next.(model); cmd != nil || m.view != viewBoard || m.asking != "" {
			t.Errorf("the text %q did something on the board: view %d, asking %q", text, m.view, m.asking)
		}
	}
	// One character is a key as before.
	if m, _ = press(t, m, "?"); m.view != viewNote {
		t.Error("? did not open the help")
	}
}

func TestOlderThanTheDaemon(t *testing.T) {
	m := loaded(t, nil, task("aaaa0001", "running", 1))
	next, cmd := m.Update(boardMsg{board: api.Tasks{Version: "v9.9.9"}})
	m = next.(model)
	if cmd == nil || m.daemon != "v9.9.9" {
		t.Fatal("a newer daemon does not trigger a look at the binary")
	}
	if _, again := m.Update(boardMsg{board: api.Tasks{Version: "v9.9.9"}}); again != nil {
		t.Error("the binary is checked again on every poll")
	}

	// The binary on disk is still the old one: say so, do not reload.
	stale := send(t, m, binaryMsg{daemon: "v9.9.9", binary: version.Version})
	if stale.reload || !strings.Contains(stale.View(), "daemon v9.9.9") {
		t.Errorf("reload = %v, header:\n%s", stale.reload, stale.View())
	}
	// The update is installed: quit to run it.
	next, cmd = m.Update(binaryMsg{daemon: "v9.9.9", binary: "v9.9.9"})
	if !next.(model).reload || cmd == nil {
		t.Error("the board does not reload on the installed update")
	}

	// Not while something is being typed: it waits for the input line to close.
	typing := m
	typing.asking = "reply"
	next, cmd = typing.Update(binaryMsg{daemon: "v9.9.9", binary: "v9.9.9"})
	typing = next.(model)
	if !typing.reload || cmd != nil {
		t.Fatal("the board quit while a reply was being typed")
	}
	if _, cmd = typing.Update(tickMsg{}); cmd == nil {
		t.Fatal("no poll")
	}
	typing.asking = ""
	if _, cmd = typing.Update(tickMsg{}); cmd == nil || fmt.Sprint(cmd()) != fmt.Sprint(tea.Quit()) {
		t.Error("the board does not reload once the input line is closed")
	}

	// Nor in the middle of a new task or a new schedule: its folder, its profile, its title. Once the
	// flow is over, dropped or done, the board reloads as before.
	flow := m
	flow.profiles = []string{"work", "personal"}
	flow = send(t, flow, binaryMsg{daemon: "v9.9.9", binary: "v9.9.9"})
	for _, step := range [][]string{{"n"}, {"enter"}, {"enter"}} {
		if flow, _ = press(t, flow, step...); flow.idle() {
			t.Fatalf("the board is idle in the middle of a new task, after %v", step)
		}
		if _, cmd = flow.Update(tickMsg{}); cmd == nil || fmt.Sprint(cmd()) == fmt.Sprint(tea.Quit()) {
			t.Fatalf("the board quit in the middle of a new task, after %v", step)
		}
	}
	flow, _ = press(t, flow, "esc")
	if _, cmd = flow.Update(tickMsg{}); cmd == nil || fmt.Sprint(cmd()) != fmt.Sprint(tea.Quit()) {
		t.Error("the board does not reload once a new task was dropped")
	}
	flow, cmd = press(t, flow, "S")
	if flow, _ = press(t, flow, "n"); flow.idle() {
		t.Error("the board is idle in the middle of a new schedule")
	}
	if flow, _ = press(t, flow, "esc"); !flow.idle() {
		t.Error("the board is not idle once a new schedule was dropped")
	}

	// Nor while what the flow sent is on its way: the folder being created, the task being sent.
	quiet := func(when string) {
		t.Helper()
		if _, cmd := flow.Update(tickMsg{}); flow.idle() || cmd == nil || fmt.Sprint(cmd()) == fmt.Sprint(tea.Quit()) {
			t.Fatalf("the board would reload %s", when)
		}
	}
	flow = send(t, m, binaryMsg{daemon: "v9.9.9", binary: "v9.9.9"})
	flow, _ = press(t, flow, "n")
	dir := filepath.Join(t.TempDir(), "made")
	flow.input.SetValue(dir)
	flow, _ = press(t, flow, "enter", "y")
	quiet("while the folder is being created")
	flow = send(t, flow, mkdirMsg{dir: dir})
	flow, _ = press(t, flow, "x")
	flow, _ = press(t, flow, "enter")
	quiet("while the task is being sent")
	flow = send(t, flow, newTaskMsg{err: errors.New("refused"), asked: "New task", title: "x"})
	quiet("with the refused title back in its input")
	if flow, _ = press(t, flow, "esc"); !flow.idle() {
		t.Error("the board is not idle once the refused task was dropped")
	}

	// A board newer than the daemon says to restart the daemon, an older one to update hc.
	saved := version.Version
	version.Version = "v1.3.0"
	defer func() { version.Version = saved }()
	if got := send(t, m, binaryMsg{daemon: "v1.2.0", binary: "v1.3.0"}).notice; !strings.Contains(got, "restart the daemon with hc restart") {
		t.Errorf("newer board: %q", got)
	}
	if got := send(t, m, binaryMsg{daemon: "v1.4.0", binary: "v1.3.0"}).notice; !strings.Contains(got, "update hc, then reopen") {
		t.Errorf("older board: %q", got)
	}
}

func TestMainSessionHint(t *testing.T) {
	command := tmux.AttachCommand("%1", "/tmp/s")
	got := mainSessionHint(command)
	if got[len(got)-1] != tmux.MainSessionHint || command[len(command)-1] != tmux.DetachHint {
		t.Errorf("got %v, the original became %v", got, command)
	}
	if other := []string{"tmux", "attach"}; len(mainSessionHint(other)) != 2 || mainSessionHint(nil) != nil {
		t.Error("a command without the hint changed")
	}
}

// lastRuneIndex is the column of the last sub in s, or -1.
func lastRuneIndex(s, sub string) int {
	i := strings.LastIndex(s, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

func TestDoneTasksAreDimmedAndStayInSightUntilHidden(t *testing.T) {
	fresh := task("fre00001", "done", 100000)
	old := task("old00001", "done", 1)
	old.Hidden = true
	m := loaded(t, nil, fresh, old, task("run00001", "running", 1))
	if !reflect.DeepEqual(m.keys, []string{"run00001", "fre00001"}) {
		t.Fatalf("rows = %v", m.keys)
	}
	agent := api.Agent{Name: "Explore", Desc: "look", State: "done"}
	for _, row := range []format.Row{{Task: &fresh}, {Task: &fresh, Agent: &agent, Last: true}} {
		for i, cell := range rowCells(row) {
			if !reflect.DeepEqual(cell.style, dim) {
				t.Errorf("cell %d %q of a done task is not dimmed", i, cell.text)
			}
		}
	}
	running := task("run00001", "running", 1)
	if cells := rowCells(format.Row{Task: &running}); reflect.DeepEqual(cells[1].style, dim) {
		t.Error("the state of a running task is dimmed")
	}
	m, _ = press(t, m, "d")
	if len(m.keys) != 3 {
		t.Errorf("with done tasks, rows = %v", m.keys)
	}
	for i, cell := range rowCells(format.Row{Task: &old}) {
		if !reflect.DeepEqual(cell.style, dim) {
			t.Errorf("cell %d %q of a hidden done task is not dimmed", i, cell.text)
		}
	}
	if !reflect.DeepEqual(stateStyles["done"], dim) {
		t.Error("the Done state is not grey outside the table")
	}
}

func TestARunShowsItsSchedule(t *testing.T) {
	run := task("run00001", "running", 1)
	run.Source, run.Schedule = "schedule", "sched123456"
	old := task("old00001", "running", 1)
	old.Source = "schedule"
	m := loaded(t, nil, run, old)
	view := m.View()
	if !regexp.MustCompile(` ↻ +TITLE`).MatchString(view) || !regexp.MustCompile(` sched123  +title run00001`).MatchString(view) ||
		!regexp.MustCompile(` -  +title old00001`).MatchString(view) || strings.Contains(view, "↻ title") {
		t.Errorf("the runs do not show their schedule in its column:\n%s", view)
	}
	own := task("own00001", "running", 1)
	for i, col := range columns {
		if cell := rowCells(format.Row{Task: &own})[i]; col.name == "SCHEDULE" && (cell.text != "-" || !reflect.DeepEqual(cell.style, dim)) {
			t.Errorf("a task started by hand shows the schedule %q", cell.text)
		}
	}
}

func TestATimedTaskShowsItsTimeLeft(t *testing.T) {
	timed := task("tim00001", "running", 1)
	timed.TimerLeft = 420
	m := loaded(t, nil, timed)
	view := m.View()
	if !regexp.MustCompile(` ⏱  +↻`).MatchString(view) || !regexp.MustCompile(` 7m  +-  +title tim00001`).MatchString(view) || strings.Contains(view, "⏱ 7m") {
		t.Errorf("the row does not show the timer in its column:\n%s", view)
	}
	untimed := task("unt00001", "running", 1)
	cells := rowCells(format.Row{Task: &untimed})
	for i, col := range columns {
		if col.name == "TIMER" && (cells[i].text != "-" || !reflect.DeepEqual(cells[i].style, dim)) {
			t.Errorf("a task without a timer has a timer cell %q", cells[i].text)
		}
	}
	if strings.Contains(text(cells), "⏱") || len(cells) != len(columns) {
		t.Errorf("a task without a timer has the cells %q", text(cells))
	}
}

func TestXDeletesInTenMinutesWithoutAQuestion(t *testing.T) {
	d := newDaemon(t)
	doomed := task("doom0001", "done", 1)
	doomed.DeleteLeft, doomed.TimerLeft = 420, 60
	m := loaded(t, d, task("run00001", "running", 5), doomed)

	m, cmd := press(t, m, "X")
	if m.view != viewBoard {
		t.Fatalf("X asked a question:\n%s", m.View())
	}
	m = run(t, m, cmd)
	if m.notice != "made0001 deleted in 0s, X again keeps it" {
		t.Errorf("after X: %q", m.notice)
	}
	// On a task already on its way out, X keeps it.
	m, _ = press(t, m, "d")
	m.cursor = slices.Index(m.keys, "doom0001")
	m, cmd = press(t, m, "X")
	m = run(t, m, cmd)
	if m.notice != "made0001 kept" {
		t.Errorf("X again: %q", m.notice)
	}
	want := []string{`PATCH /api/tasks/run00001 {"delete_in":600}`, `PATCH /api/tasks/doom0001 {"delete_in":0}`}
	if !reflect.DeepEqual(d.calls, want) {
		t.Errorf("the daemon got %q", d.calls)
	}

	// The row shows the delete to come in the timer column as an alarm, unless its timer runs out first.
	running := task("run00002", "running", 1)
	running.DeleteLeft, running.TimerLeft = 420, 900
	timed := running
	timed.TimerLeft = 60
	for _, c := range []struct {
		task  api.Task
		want  string
		style lipgloss.Style
	}{{running, "✗ 7m", alarm}, {timed, "1m", plain}} {
		cells := rowCells(format.Row{Task: &c.task})
		for i, col := range columns {
			if col.name == "TIMER" && (cells[i].text != c.want || !reflect.DeepEqual(cells[i].style, c.style)) {
				t.Errorf("a task about to be deleted has the timer cell %q, want %q", cells[i].text, c.want)
			}
		}
	}
}

func TestAttachIsTheFirstKey(t *testing.T) {
	m := loaded(t, nil, task("run00001", "running", 1))
	if first := m.header()[0]; !regexp.MustCompile(`<a> +attach +<A> +adopt`).MatchString(first) {
		t.Errorf("the hints do not open on attach: %q", first)
	}
	if a, enter := strings.Index(help, "\na "), strings.Index(help, "\nenter "); a < 0 || a > enter {
		t.Errorf("the help lists attach after enter:\n%s", help)
	}
}

func TestTheFirstBoardWelcomesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "welcomed")
	m := loaded(t, nil, task("run00001", "running", 1))
	m.welcome(path, true)
	if m.view != viewNote || !strings.Contains(m.View(), "Welcome to headcoach") || !strings.Contains(m.View(), "Ctrl-]") {
		t.Fatalf("the first board did not welcome:\n%s", m.View())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the welcome left no trace: %v", err)
	}
	// Any key closes it, and is not taken as a key of the board.
	m, cmd := press(t, m, "a")
	if m.view != viewBoard || cmd != nil {
		t.Errorf("a key did not just close the welcome: view %d", m.view)
	}

	// The next board goes straight to work.
	m = loaded(t, nil, task("run00001", "running", 1))
	m.welcome(path, true)
	if m.view != viewBoard {
		t.Errorf("the welcome showed twice:\n%s", m.View())
	}
	m.welcome("", true)
	if m.view != viewBoard {
		t.Error("a board without a state folder welcomed")
	}

	// A board reloaded after an update says nothing, and is past its welcome from then on.
	other := filepath.Join(dir, "welcomed")
	m.welcome(other, false)
	if _, err := os.Stat(other); m.view != viewBoard || err != nil {
		t.Errorf("a reloaded board: view %d, marker %v", m.view, err)
	}
}

func TestTheModelColumn(t *testing.T) {
	chosen := task("cho00001", "running", 1)
	chosen.Model, chosen.SessionModel = "claude-sonnet-4-5", "claude-opus-4-5"
	reported := task("rep00001", "running", 1)
	reported.SessionModel = "claude-haiku-4-5-20251001"
	unknown := task("unk00001", "running", 1)
	m := loaded(t, nil, chosen, reported, unknown)
	view := m.View()
	for _, piece := range []string{"MODEL", "cho00001  Running  -        sonnet  ", "rep00001  Running  -        haiku   ", "unk00001  Running  -        -       "} {
		if !strings.Contains(view, piece) {
			t.Errorf("the board lacks %q:\n%s", piece, view)
		}
	}
}

func TestTheEffortColumn(t *testing.T) {
	chosen := task("cho00001", "running", 1)
	chosen.Effort, chosen.SessionEffort = "low", "high"
	reported := task("rep00001", "running", 1)
	reported.SessionEffort = "xhigh"
	unknown := task("unk00001", "running", 1)
	m := loaded(t, nil, chosen, reported, unknown)
	view := m.View()
	for _, piece := range []string{"EFFORT", "cho00001  Running  -        -      low     ", "rep00001  Running  -        -      xhigh   ", "unk00001  Running  -        -      -       "} {
		if !strings.Contains(view, piece) {
			t.Errorf("the board lacks %q:\n%s", piece, view)
		}
	}
}

func TestATaskLightsUpAsTheBoardSeesItMoveToDone(t *testing.T) {
	m := loaded(t, nil, task("old00001", "done", 1), task("run00001", "running", 2))
	if len(m.lit) != 0 {
		t.Fatalf("a task already done when the board opens lights up: %v", m.lit)
	}
	done := task("run00001", "done", 2)
	next, cmd := m.Update(boardMsg{board: api.Tasks{Tasks: []api.Task{task("old00001", "done", 1), done}}})
	m = next.(model)
	if _, lit := m.lit["run00001"]; !lit || len(m.lit) != 1 || !m.glowing || cmd == nil {
		t.Fatalf("lit = %v, glowing = %v, cmd = %v", m.lit, m.glowing, cmd)
	}
	agent := api.Agent{Name: "Explore", Desc: "look", State: "done"}
	for _, step := range []struct {
		ago  time.Duration
		want lipgloss.Style
	}{{0, glowStyles[0]}, {glowBright, glowStyles[1]}, {glowFor, dim}} {
		m.lit["run00001"] = time.Now().Add(-step.ago)
		for _, row := range []format.Row{{Task: &done}, {Task: &done, Agent: &agent, Last: true}} {
			for i, cell := range m.glow(row, rowCells(row)) {
				if !reflect.DeepEqual(cell.style, step.want) {
					t.Errorf("%v after the move, cell %d %q is not drawn in the expected style", step.ago, i, cell.text)
				}
			}
		}
	}
	m = send(t, m, glowMsg{})
	if len(m.lit) != 0 || m.glowing {
		t.Errorf("after the glow, lit = %v, glowing = %v", m.lit, m.glowing)
	}
}

// A task shows under the one that drives it, with its sub-agents indented as deep.
func TestBoardNestsChildTasks(t *testing.T) {
	boss, kid := task("boss0001", "review", 9), task("kid00001", "needs", 1)
	kid.Parent = boss.ID
	kid.Agents = []api.Agent{{Name: "Explore", Desc: "look around", State: "running"}}
	m := loaded(t, nil, kid, boss, task("other001", "running", 2))
	// The child needs you, yet stays under its parent: the tree wins over the order of states.
	if want := []string{"other001", "boss0001", "kid00001", "kid00001/0"}; !reflect.DeepEqual(m.keys, want) {
		t.Errorf("keys = %v", m.keys)
	}
	screen := m.View()
	for _, piece := range []string{"│boss0001 ▾ 1  Review ", "│└─ kid00001   Needs me ", "  title kid00001 ", "  └─ Explore: look around"} {
		if !strings.Contains(screen, piece) {
			t.Errorf("the board lacks %q:\n%s", piece, screen)
		}
	}
}

// x on a parent says how many tasks go with it. A batch deletes the ticked tasks under a ticked one
// first: the parent takes them along, so their own delete would find nothing.
func TestDeleteNamesTheTasksUnder(t *testing.T) {
	d := newDaemon(t)
	boss, kid, gkid := task("boss0001", "running", 9), task("kid00001", "running", 5), task("gkid0001", "running", 3)
	kid.Parent, gkid.Parent = boss.ID, kid.ID
	m := loaded(t, d, boss, kid, gkid, task("lone0001", "review", 1))
	if want := []string{"boss0001", "kid00001", "gkid0001", "lone0001"}; !reflect.DeepEqual(m.keys, want) {
		t.Fatalf("keys = %v", m.keys)
	}
	m, _ = press(t, m, "x")
	if m.noteText != "Delete boss0001 title boss0001 with 2 tasks under it?  y / n" {
		t.Errorf("x on the parent asked %q", m.noteText)
	}
	m, _ = press(t, m, "n", "down", "down", "x")
	if m.noteText != "Delete gkid0001 title gkid0001?  y / n" {
		t.Errorf("x on a task with none under it asked %q", m.noteText)
	}
	m, _ = press(t, m, "n", "v", " ", "up", "up", " ", "x")
	if m.noteText != "Delete 2 tasks with 1 more task under them? 2 running.  y / n" {
		t.Fatalf("the batch asked %q", m.noteText)
	}
	m, cmd := press(t, m, "y")
	m = run(t, m, cmd)
	if want := []string{"DELETE /api/tasks/gkid0001", "DELETE /api/tasks/boss0001"}; !slices.Equal(d.calls, want) || m.notice != "2 tasks deleted" {
		t.Errorf("the batch sent %v, said %q", d.calls, m.notice)
	}
}

// w opens the coordinator view of the task under the cursor, or of the task driving it.
func TestTeamView(t *testing.T) {
	d := newDaemon(t)
	boss, kid, gkid := task("boss0001", "done", 0), task("kid00001", "running", 60), task("gkid0001", "review", 5)
	kid.Parent, gkid.Parent = boss.ID, kid.ID
	lone := task("lone0001", "running", 1)
	m := loaded(t, d, boss, kid, gkid, lone)
	for _, c := range []struct{ task, want string }{{"boss0001", "boss0001"}, {"kid00001", "kid00001"}, {"gkid0001", "kid00001"}, {"lone0001", "lone0001"}} {
		for i := range m.tasks {
			if m.tasks[i].ID == c.task {
				if got := m.teamOf(&m.tasks[i]); got != c.want {
					t.Errorf("teamOf(%s) = %s, want %s", c.task, got, c.want)
				}
			}
		}
	}
	m.jump = "boss0001"
	m.rebuild()
	m, cmd := press(t, m, "w")
	if m.view != viewTeam || m.teamID != "boss0001" || !m.teamLoading {
		t.Fatalf("w opened view %d for %q", m.view, m.teamID)
	}
	m = run(t, m, cmd)
	view := m.View()
	for _, want := range []string{"team(boss0001 title boss0001)[2]", "Waits on you", "gkid0001  title gkid0001  in Review: PR #3", "2 open, 1 done",
		"kid00001 ▾ 1", "└─ gkid0001", "title gkid0001", "Halfway.", "last gkid0001", "Two tasks out."} {
		if !strings.Contains(view, want) {
			t.Errorf("the view lacks %q:\n%s", want, view)
		}
	}
	// An answer for another coordinator, come late, is dropped.
	if m2 := send(t, m, teamMsg{id: "lone0001"}); len(m2.team.Members) != 2 {
		t.Error("a late answer replaced the view")
	}
	m, _ = press(t, m, "?")
	if m.view != viewNote || !strings.Contains(m.View(), "Coordinator keys") {
		t.Fatalf("no help:\n%s", m.View())
	}
	m, _ = press(t, m, "x", "j", "enter")
	if m.view != viewBoard || m.current() == nil || m.current().ID != "gkid0001" {
		t.Errorf("enter did not show the task on the board: view %d", m.view)
	}
	if m, _ = press(t, m, "w", "esc"); m.view != viewBoard {
		t.Errorf("escape went to view %d", m.view)
	}
}
