package board

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/store"
)

const sid = "0123abcd-1111-2222-3333-444455556666"

// P is the body of a hook payload, without the event name and the session id.
type P map[string]any

type fixture struct {
	t   *testing.T
	st  *store.Store
	cfg *config.Config
	b   *Board
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DB = filepath.Join(t.TempDir(), "state", "b.db")
	cfg.Profiles = []config.Profile{{Name: "team", Command: "claude-team"}, {Name: "personal", Command: "claude-personal"}}
	cfg.PrivateTmux = false
	return &fixture{t: t, st: st, cfg: cfg, b: New(st, cfg)}
}

// on applies one event of session s1.
func (f *fixture) on(name string, body P) string { return f.from("s1", Origin{}, name, body) }

// from applies one event of any session, with what the hook script would add.
func (f *fixture) from(session string, o Origin, name string, body P) string {
	f.t.Helper()
	p := Payload{"hook_event_name": name, "session_id": session}
	for k, v := range body {
		p[k] = v
	}
	task, err := f.b.Apply(p, o)
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return task
}

func (f *fixture) task(id string) *store.Task {
	f.t.Helper()
	task, err := f.st.Task(id)
	if err != nil || task == nil {
		f.t.Fatalf("task %s: %v, %v", id, task, err)
	}
	return task
}

func (f *fixture) session(id string) *store.Session {
	f.t.Helper()
	sess, err := f.st.Session(id)
	if err != nil || sess == nil {
		f.t.Fatalf("session %s: %v, %v", id, sess, err)
	}
	return sess
}

func (f *fixture) move(id, state string, fields store.Fields) {
	f.t.Helper()
	if _, err := f.st.SetState(id, state, "you", fields); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.st.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// history lists the moves of a task as "state by".
func (f *fixture) history(id string) []string {
	f.t.Helper()
	return f.column("SELECT state || ' ' || by FROM history WHERE task_id = ? ORDER BY id", id)
}

// agents lists the child rows of a task as "name|descr|agent id|state".
func (f *fixture) agents(id string) []string {
	f.t.Helper()
	return f.column("SELECT name || '|' || descr || '|' || coalesce(agent_id, '-') || '|' || state"+
		" FROM agents WHERE task_id = ? ORDER BY id", id)
}

func (f *fixture) column(query string, args ...any) []string {
	f.t.Helper()
	rows, err := f.st.Query(query, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// --- task creation -------------------------------------------------------------------

func TestFirstPromptCreatesATask(t *testing.T) {
	f := newFixture(t)
	id := f.from(sid, Origin{Pane: "%1"}, "UserPromptSubmit", P{"prompt": "\nFix the login bug\nwith more detail"})
	task := f.task(id)
	eq(t, id, sid)
	eq(t, []string{task.Title, task.State, task.Source, task.Last},
		[]string{"Fix the login bug", "running", "session", "You: Fix the login bug"})
	eq(t, task.Prompt, "\nFix the login bug\nwith more detail")
	eq(t, f.history(id), []string{"running hook"})
	sess := f.session(sid)
	if sess.TaskID != id || sess.Pane != "%1" || !sess.Live {
		t.Errorf("session = %+v", sess)
	}
	// The status line reads the short task id from a file named after the session.
	short, err := os.ReadFile(filepath.Join(f.cfg.SessionsDir(), sid))
	if err != nil || string(short) != "0123abcd" {
		t.Errorf("status line file = %q, %v", short, err)
	}
}

func TestProfileComesFromTheConfigDir(t *testing.T) {
	f := newFixture(t)
	f.from("s1", Origin{ConfigDir: "/Users/x/.claude-team"}, "UserPromptSubmit", P{"prompt": "a"})
	f.from("s2", Origin{ConfigDir: "/Users/x/.claude-personal"}, "UserPromptSubmit", P{"prompt": "b"})
	eq(t, []string{f.task("s1").Profile, f.task("s2").Profile}, []string{"team", "personal"})
}

func TestRepoAndBranchFromCwd(t *testing.T) {
	f := newFixture(t)
	tmp := t.TempDir()
	plain, repo := filepath.Join(tmp, "plain"), filepath.Join(tmp, "proj")
	os.Mkdir(plain, 0o755)
	gitInit(t, repo, "feature-x")
	f.from("s1", Origin{}, "UserPromptSubmit", P{"prompt": "a", "cwd": plain})
	f.from("s2", Origin{}, "UserPromptSubmit", P{"prompt": "b", "cwd": repo})
	eq(t, []string{f.task("s1").Repo, f.task("s1").Branch}, []string{"plain", ""})
	eq(t, []string{f.task("s2").Repo, f.task("s2").Branch}, []string{"proj", "feature-x"})
	if out, err := exec.Command("git", "-C", repo, "checkout", "-q", "-b", "other").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	f.from("s2", Origin{}, "Stop", P{"cwd": repo, "last_assistant_message": "done"})
	eq(t, f.task("s2").Branch, "other")
}

func TestBaseIsWhereTheTaskStarts(t *testing.T) {
	f := newFixture(t)
	repo := filepath.Join(t.TempDir(), "proj")
	gitInit(t, repo, "main")
	commit := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x").CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return GitHead(repo)
	}
	f.on("UserPromptSubmit", P{"prompt": "before any commit", "cwd": repo})
	eq(t, f.task("s1").Base, "")
	first := commit()
	f.from("s2", Origin{}, "UserPromptSubmit", P{"prompt": "a", "cwd": repo})
	eq(t, f.task("s2").Base, first)
	commit()
	f.from("s2", Origin{}, "UserPromptSubmit", P{"prompt": "b", "cwd": repo})
	eq(t, f.task("s2").Base, first)
	// A task from the backlog starts where the checkout is when it runs.
	if _, err := f.st.CreateTask(store.Task{ID: "later", Title: "later", Cwd: repo}, "you"); err != nil {
		t.Fatal(err)
	}
	third := commit()
	f.from("s3", Origin{TaskHint: "later"}, "UserPromptSubmit", P{"prompt": "go", "cwd": repo})
	eq(t, f.task("later").Base, third)
}

func TestSecondPromptDoesNotCreateASecondTask(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "one"})
	f.on("UserPromptSubmit", P{"prompt": "one"}) // delivered twice
	eq(t, f.history("s1"), []string{"running hook"})
	f.on("Stop", P{"last_assistant_message": "ok"})
	f.on("UserPromptSubmit", P{"prompt": "two"})
	task := f.task("s1")
	eq(t, f.count("tasks"), 1)
	eq(t, []string{task.Title, task.State, task.Last}, []string{"one", "running", "You: two"})
	eq(t, f.history("s1"), []string{"running hook", "done hook", "running hook"})
}

func TestEmptyAndVeryLongPrompts(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": ""})
	eq(t, f.task("s1").Title, "Untitled session")
	prompt := strings.Repeat("p", 300_000)
	f.from("s2", Origin{}, "UserPromptSubmit", P{"prompt": prompt})
	task := f.task("s2")
	if n := utf8.RuneCountInString(task.Title); n < 20 || n > 100 {
		t.Errorf("title is %d characters", n)
	}
	if task.Prompt != prompt || utf8.RuneCountInString(task.Last) > 100 {
		t.Error("the prompt was cut, or the last activity was not")
	}
	var longest int
	f.st.QueryRow("SELECT max(length(payload)) FROM events").Scan(&longest)
	if longest > 20000 {
		t.Errorf("an event of %d characters was logged", longest)
	}
}

// --- ignored input -------------------------------------------------------------------

func TestPayloadsMissingSessionOrEventAreIgnored(t *testing.T) {
	f := newFixture(t)
	for _, p := range []Payload{
		{},
		{"hook_event_name": "UserPromptSubmit"},
		{"session_id": "s1"},
		{"hook_event_name": "", "session_id": "s1"},
		{"hook_event_name": "UserPromptSubmit", "session_id": ""},
		{"hook_event_name": nil, "session_id": nil},
		{"hook_event_name": 4.0, "session_id": []any{"s1"}},
		{"hook_event_name": "not a name", "session_id": "s1"},
	} {
		if task, err := f.b.Apply(p, Origin{}); task != "" || err != nil {
			t.Errorf("%v gave %q, %v", p, task, err)
		}
	}
	eq(t, []int{f.count("tasks"), f.count("sessions"), f.count("events")}, []int{0, 0, 0})
}

func TestUnknownEventNamesDoNotFail(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	for _, name := range []string{"PreCompact", "TotallyUnknown", "apply", "close_session", "__init__"} {
		eq(t, f.on(name, nil), "s1")
	}
	eq(t, f.task("s1").State, "running")
}

func TestEventsBeforeTheFirstPromptCreateNoTask(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"PreToolUse", "PostToolUse", "Notification", "Stop", "SubagentStart", "SubagentStop", "SessionEnd"} {
		got := f.on(name, P{"tool_name": "Bash", "agent_id": "a1", "notification_type": "permission_prompt"})
		eq(t, got, "")
	}
	eq(t, f.count("tasks"), 0)
}

// --- hostile session ids -------------------------------------------------------------

func TestSessionIDCannotWriteOutsideSessionsDir(t *testing.T) {
	f := newFixture(t)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	os.WriteFile(victim, []byte("precious"), 0o644)
	f.from(victim, Origin{}, "UserPromptSubmit", P{"prompt": "x"})
	f.from("../escaped", Origin{}, "UserPromptSubmit", P{"prompt": "x"})
	if got, _ := os.ReadFile(victim); string(got) != "precious" {
		t.Error("a session id overwrote a file")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.cfg.DB), "escaped")); err == nil {
		t.Error("a session id escaped the sessions directory")
	}
	eq(t, f.count("tasks"), 0)
}

// --- tools ---------------------------------------------------------------------------

func TestPreToolUseUpdatesLastAndSetsRunning(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("Stop", P{"last_assistant_message": "done"})
	f.on("PreToolUse", P{"tool_name": "Bash", "tool_input": map[string]any{"command": "make test"}})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"running", "$ make test"})
	f.on("PreToolUse", P{"tool_name": "Bash", "tool_input": "not a dict"})
	f.on("PreToolUse", P{"tool_input": nil})
	eq(t, f.task("s1").State, "running")
}

func TestAskUserQuestionSetsNeedsDialog(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	questions := func(q ...any) P {
		return P{"tool_name": "AskUserQuestion", "tool_input": map[string]any{"questions": q}}
	}
	f.on("PreToolUse", questions(map[string]any{"question": "Tabs or spaces?"}))
	task := f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "dialog", "Tabs or spaces?"})
	// Claude Code follows the question with a permission notification: the question stays.
	f.on("Notification", P{"notification_type": "permission_prompt", "message": "Claude needs your permission"})
	task = f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "dialog", "Tabs or spaces?"})
	f.on("PostToolUse", P{"tool_name": "AskUserQuestion"})
	task = f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"running", "", ""})
	for _, body := range []P{
		questions(), questions("x"), questions(map[string]any{}),
		{"tool_name": "AskUserQuestion", "tool_input": map[string]any{}},
		{"tool_name": "AskUserQuestion", "tool_input": nil},
	} {
		f.on("PreToolUse", body)
		task := f.task("s1")
		eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "dialog", "The agent has a question"})
	}
}

func TestExitPlanModeSetsNeedsDialog(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", P{"tool_name": "ExitPlanMode", "tool_input": map[string]any{"plan": "..."}})
	task := f.task("s1")
	if task.State != "needs" || task.AskKind != "dialog" || task.Ask == "" {
		t.Errorf("task = %+v", task)
	}
}

func TestPostToolUseLeavesOtherStatesAlone(t *testing.T) {
	for _, state := range []string{"review", "done", "backlog"} {
		f := newFixture(t)
		f.on("UserPromptSubmit", P{"prompt": "x"})
		f.move("s1", state, nil)
		f.on("PostToolUse", P{"tool_name": "Bash"})
		eq(t, f.task("s1").State, state)
	}
}

// --- notifications -------------------------------------------------------------------

func TestPermissionPromptSetsNeedsPermission(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", P{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm x"}})
	f.on("Notification", P{"notification_type": "permission_prompt", "message": "Claude needs your permission to use Bash"})
	task := f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "permission", "Claude needs your permission to use Bash"})
	f.on("PostToolUse", P{"tool_name": "Bash"})
	eq(t, f.task("s1").State, "running")
	// Without a type, the message decides.
	f.on("Notification", P{"message": "Claude needs your PERMISSION to use Write"})
	eq(t, []string{f.task("s1").State, f.task("s1").AskKind}, []string{"needs", "permission"})
}

func TestDialogNotifications(t *testing.T) {
	for _, kind := range []string{"agent_needs_input", "elicitation_dialog", "elicitation_url_dialog"} {
		f := newFixture(t)
		f.on("UserPromptSubmit", P{"prompt": "x"})
		f.on("Notification", P{"notification_type": kind})
		task := f.task("s1")
		eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "dialog", "The agent is waiting for you"})
	}
}

func TestIdlePromptDoesNotChangeState(t *testing.T) {
	for _, state := range []string{"running", "review", "done"} {
		f := newFixture(t)
		f.on("UserPromptSubmit", P{"prompt": "x"})
		f.move("s1", state, nil)
		before := f.history("s1")
		f.on("Notification", P{"notification_type": "idle_prompt", "message": "Claude is waiting for your permission or input"})
		eq(t, f.task("s1").State, state)
		eq(t, f.history("s1"), before)
	}
}

// --- stop ----------------------------------------------------------------------------

func TestStop(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("Stop", P{"last_assistant_message": "I fixed the parser.\n\nShould I also update the docs?"})
	task := f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "question", "Should I also update the docs?"})

	f.on("UserPromptSubmit", P{"prompt": "yes"})
	eq(t, []string{f.task("s1").State, f.task("s1").Ask}, []string{"running", ""})

	f.on("Stop", P{"last_assistant_message": "\n**All 12 tests pass.**\n\nDetails follow."})
	f.on("Stop", P{"last_assistant_message": "\n**All 12 tests pass.**\n\nDetails follow."})
	task = f.task("s1")
	eq(t, []string{task.State, task.Last, task.Ask, task.AskKind}, []string{"done", "All 12 tests pass.", "", ""})
	eq(t, f.history("s1"), []string{"running hook", "needs hook", "running hook", "done hook"})

	f.on("Stop", nil)
	eq(t, f.task("s1").Last, "Turn finished")
}

func TestStopOnAnAskWithoutQuestionMark(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "/tester check it"})
	eq(t, f.task("s1").Persona, "tester")
	f.on("Stop", P{"last_assistant_message": "The plan is ready.\n\nReply \"go\", or change the steps."})
	task := f.task("s1")
	eq(t, []string{task.State, task.AskKind, task.Ask}, []string{"needs", "question", "Reply \"go\", or change the steps."})

	// A plan persona that asks for your go keeps its plan in Review.
	f.on("UserPromptSubmit", P{"prompt": "/architect plan it"})
	f.on("Stop", P{"last_assistant_message": "Plan:\n1. a\n2. b\n\nReply \"go\" and I start."})
	eq(t, []string{f.task("s1").State, f.task("s1").Ask}, []string{"review", ""})
}

// commitIn makes a commit in a test checkout.
func commitIn(t *testing.T, repo string) {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestStopLeavesInReviewWhatWaitsOnYou(t *testing.T) {
	f := newFixture(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(t.TempDir(), "proj")
	gitInit(t, repo, "main")
	stop := func() string {
		t.Helper()
		f.on("Stop", P{"cwd": repo, "last_assistant_message": "Here it is."})
		return f.task("s1").State
	}
	f.on("UserPromptSubmit", P{"prompt": "x", "cwd": repo})
	// A local repository with no remote has nothing to ship to.
	commitIn(t, repo)
	eq(t, stop(), "done")
	os.WriteFile(filepath.Join(repo, "a"), []byte("one"), 0o644)
	exec.Command("git", "-C", repo, "add", "a").Run()
	eq(t, stop(), "review") // a change not committed
	exec.Command("git", "-C", repo, "reset", "-q", "--hard").Run()
	eq(t, stop(), "done")
	// A plan waits for your go, whatever the checkout holds.
	f.on("UserPromptSubmit", P{"prompt": "/architect plan it", "cwd": repo})
	eq(t, f.task("s1").Persona, "architect")
	eq(t, stop(), "review")
	f.on("UserPromptSubmit", P{"prompt": "/developer go", "cwd": repo})
	eq(t, stop(), "done")
	// A checkout removed after the release holds nothing left to ship.
	os.RemoveAll(repo)
	eq(t, stop(), "done")
}

// --- sub-agents ----------------------------------------------------------------------

func agent(kind, descr string) P {
	input := map[string]any{}
	if kind != "" {
		input["subagent_type"] = kind
	}
	if descr != "" {
		input["description"] = descr
	}
	return P{"tool_name": "Agent", "tool_input": input}
}

func TestSubAgentLifecycle(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", "find the parser"))
	f.on("PreToolUse", agent("Plan", "plan the fix"))
	eq(t, f.agents("s1"), []string{"Explore|find the parser|-|running", "Plan|plan the fix|-|running"})
	eq(t, f.task("s1").State, "running")

	// SubagentStart matches by type, not by position.
	f.on("SubagentStart", P{"agent_id": "a-plan", "agent_type": "Plan"})
	eq(t, f.agents("s1"), []string{"Explore|find the parser|-|running", "Plan|plan the fix|a-plan|running"})
	f.on("SubagentStart", P{"agent_id": "a-exp", "agent_type": "Explore"})
	f.on("SubagentStop", P{"agent_id": "a-plan"})
	eq(t, f.agents("s1"), []string{"Explore|find the parser|a-exp|running", "Plan|plan the fix|a-plan|done"})
}

func TestTaskToolNameAlsoCreatesAnAgentRow(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", P{"tool_name": "Task", "tool_input": map[string]any{}})
	eq(t, f.agents("s1"), []string{"agent||-|running"})
}

func TestEventsFromInsideASubAgentDoNotOverwriteLast(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", ""))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	last := f.task("s1").Last
	inner := func(tool string, input map[string]any) P {
		return P{"agent_id": "a1", "tool_name": tool, "tool_input": input}
	}
	f.on("PreToolUse", inner("Bash", map[string]any{"command": "inner command"}))
	f.on("PostToolUse", inner("Bash", nil))
	f.on("PreToolUse", inner("AskUserQuestion", map[string]any{"questions": []any{map[string]any{"question": "inner?"}}}))
	f.on("PreToolUse", inner("Agent", map[string]any{"subagent_type": "nested"}))
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"running", last})
	eq(t, len(f.agents("s1")), 1)
}

func TestStopWhileSubAgentsRun(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", ""))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.on("Stop", P{"last_assistant_message": "Started a background agent. Anything else?"})
	task := f.task("s1")
	eq(t, []string{task.State, task.Last, task.Ask}, []string{"running", "Waiting for 1 sub-agent", ""})
	f.on("SubagentStart", P{"agent_id": "a2", "agent_type": "Explore"})
	f.on("Stop", P{"last_assistant_message": "waiting"})
	eq(t, f.task("s1").Last, "Waiting for 2 sub-agents")
	f.on("SubagentStop", P{"agent_id": "a1"})
	f.on("SubagentStop", P{"agent_id": "a2"})
	f.on("Stop", P{"last_assistant_message": "All finished."})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"done", "All finished."})
}

func TestWaitingFlag(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	eq(t, f.task("s1").Waiting, false)

	// A foreground sub-agent blocks the main agent.
	f.on("PreToolUse", agent("Explore", ""))
	eq(t, f.task("s1").Waiting, true)
	// Work inside the sub-agent does not wake the main agent.
	f.on("PreToolUse", P{"agent_id": "a1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}})
	eq(t, f.task("s1").Waiting, true)
	// Its own next tool call does.
	f.on("PreToolUse", P{"tool_name": "Read", "tool_input": map[string]any{"file_path": "/x"}})
	eq(t, f.task("s1").Waiting, false)

	// A background sub-agent leaves the main agent working.
	f.on("PreToolUse", P{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "Plan", "run_in_background": true}})
	f.on("SubagentStart", P{"agent_id": "a2", "agent_type": "Plan"})
	eq(t, f.task("s1").Waiting, false)
	// Until its turn ends with the sub-agent still going.
	f.on("Stop", P{"last_assistant_message": "Started it."})
	eq(t, []any{f.task("s1").State, f.task("s1").Waiting}, []any{"running", true})
	// A new prompt is activity again.
	f.on("UserPromptSubmit", P{"prompt": "y"})
	eq(t, f.task("s1").Waiting, false)
}

func TestStopClosesSpawnsThatNeverReportedAnAgentID(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", ""))
	f.on("Stop", P{"last_assistant_message": "The agent was denied."})
	eq(t, f.task("s1").State, "done")
	eq(t, f.agents("s1"), []string{"Explore||-|done"})
}

func TestSubagentStartWithoutASpawnRowInsertsOne(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	eq(t, f.agents("s1"), []string{"Explore||a1|running"})
}

func TestDuplicateSubagentStartAndStop(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", ""))
	f.on("PreToolUse", agent("Explore", ""))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	eq(t, f.agents("s1"), []string{"Explore||a1|running", "Explore||-|running"})
	f.on("SubagentStop", P{"agent_id": "a1"})
	ended := f.column("SELECT ended_at FROM agents WHERE agent_id = 'a1'")
	f.on("SubagentStop", P{"agent_id": "a1"})
	f.on("SubagentStop", P{"agent_id": "never-seen"})
	f.on("SubagentStop", nil)
	eq(t, f.agents("s1"), []string{"Explore||a1|done", "Explore||-|running"})
	eq(t, f.column("SELECT ended_at FROM agents WHERE agent_id = 'a1'"), ended)
}

func TestSubagentStartFallsBackToAnyUntrackedSpawn(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("general-purpose", "d"))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "renamed-type"})
	eq(t, f.agents("s1"), []string{"renamed-type|d|a1|running"})
}

func TestPermissionInsideASubAgentReturnsToRunningAfterApproval(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", ""))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.on("PreToolUse", P{"agent_id": "a1", "tool_name": "Bash", "tool_input": map[string]any{"command": "rm x"}})
	f.on("Notification", P{"notification_type": "permission_prompt", "message": "Claude needs your permission to use Bash"})
	eq(t, f.task("s1").State, "needs")
	f.on("PostToolUse", P{"agent_id": "a1", "tool_name": "Bash"}) // the user approved, the tool ran
	eq(t, f.task("s1").State, "running")
}

// --- session end ---------------------------------------------------------------------

func TestSessionEndMovesARunningTaskToReview(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("", ""))
	f.on("SubagentStart", P{"agent_id": "a1"})
	f.on("SessionEnd", P{"reason": "exit"})
	f.on("SessionEnd", P{"reason": "exit"})
	sess := f.session("s1")
	if sess.Live || sess.EndedAt == nil {
		t.Errorf("session = %+v", sess)
	}
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"review", "Session closed"})
	eq(t, f.agents("s1"), []string{"agent||a1|done"})
	eq(t, f.history("s1"), []string{"running hook", "review hook"})
}

func TestSessionEndLeavesOtherStatesAlone(t *testing.T) {
	for _, state := range []string{"needs", "review", "done", "backlog"} {
		f := newFixture(t)
		f.on("UserPromptSubmit", P{"prompt": "x"})
		f.move("s1", state, store.Fields{"ask": "q?"})
		f.on("SessionEnd", nil)
		eq(t, f.session("s1").Live, false)
		eq(t, f.task("s1").State, state)
	}
}

func TestSessionEndOfAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	eq(t, f.from("ghost", Origin{}, "SessionEnd", nil), "")
	eq(t, f.session("ghost").Live, false)
}

func TestOnlyANewPromptRevivesAnEndedSession(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("SessionEnd", nil)
	f.on("PostToolUse", P{"tool_name": "Bash"}) // an async hook delivered late
	eq(t, f.session("s1").Live, false)
	f.on("UserPromptSubmit", P{"prompt": "again"})
	sess := f.session("s1")
	if !sess.Live || sess.EndedAt != nil {
		t.Errorf("session = %+v", sess)
	}
	eq(t, f.task("s1").State, "running")
}

// --- session start, /clear -----------------------------------------------------------

func TestSessionStartCompactInTheSamePaneLinksToThePreviousTask(t *testing.T) {
	for _, pane := range []Origin{{Pane: "%1"}, {HerdrPane: "w1-p1", HerdrSocket: "/run/herdr.sock"}} {
		f := newFixture(t)
		f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
		f.from("s1", pane, "SessionEnd", P{"reason": "other"})
		eq(t, f.from("s2", pane, "SessionStart", P{"source": "compact"}), "s1")
		short, _ := os.ReadFile(filepath.Join(f.cfg.SessionsDir(), "s2"))
		eq(t, string(short), "s1")
		eq(t, f.from("s2", pane, "UserPromptSubmit", P{"prompt": "second"}), "s1")
		eq(t, f.count("tasks"), 1)
		eq(t, []string{f.task("s1").State, f.task("s1").Title}, []string{"running", "first"})
	}
}

func TestSessionStartClearByHandStartsANewTask(t *testing.T) {
	for _, pane := range []Origin{{Pane: "%1"}, {HerdrPane: "w1-p1", HerdrSocket: "/run/herdr.sock"}} {
		f := newFixture(t)
		f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
		f.from("s1", pane, "Stop", P{"last_assistant_message": "Ship it?"})
		eq(t, f.task("s1").State, "needs")
		f.from("s1", pane, "SessionEnd", P{"reason": "clear"})
		eq(t, f.from("s2", pane, "SessionStart", P{"source": "clear", "cwd": "/nowhere"}), "s2")

		// The old task is closed, by you, and keeps its session, its title and its history.
		old := f.task("s1")
		eq(t, []string{old.State, old.Title, old.Last}, []string{"done", "first", "You: first"})
		eq(t, f.history("s1"), []string{"running hook", "needs hook", "done you"})
		eq(t, f.session("s1").TaskID, "s1")

		// The new one waits in the backlog, live on the cleared session, until its first prompt names it.
		next := f.task("s2")
		eq(t, []string{next.State, next.Title, next.Prompt, next.Last, next.Source, next.Cwd},
			[]string{"backlog", Cleared, "", "Context cleared", "session", "/nowhere"})
		eq(t, f.session("s2").TaskID, "s2")
		short, _ := os.ReadFile(filepath.Join(f.cfg.SessionsDir(), "s2"))
		eq(t, string(short), "s2")

		eq(t, f.from("s2", pane, "UserPromptSubmit", P{"prompt": "\nSecond thing\nwith more detail"}), "s2")
		next = f.task("s2")
		eq(t, []string{next.State, next.Title, next.Prompt, next.Last},
			[]string{"running", "Second thing", "\nSecond thing\nwith more detail", "You: Second thing"})
		eq(t, f.history("s2"), []string{"backlog hook", "running hook"})
		// Its first prompt is its own: the next one does not rename it.
		f.from("s2", pane, "UserPromptSubmit", P{"prompt": "and a third"})
		eq(t, f.task("s2").Title, "Second thing")
		eq(t, f.count("tasks"), 2)
	}
}

func TestSessionStartClearAgainBeforeTheFirstPromptKeepsTheNewTask(t *testing.T) {
	f := newFixture(t)
	pane := Origin{Pane: "%1"}
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	eq(t, f.from("s2", pane, "SessionStart", P{"source": "clear"}), "s2")
	eq(t, f.from("s3", pane, "SessionStart", P{"source": "clear"}), "s2")
	eq(t, f.count("tasks"), 2)
	eq(t, f.task("s2").State, "backlog")
}

func TestATitleOnlyTaskKeepsItsTitleOnTheFirstPrompt(t *testing.T) {
	f := newFixture(t)
	const id = "task-0001-aaaa"
	if _, err := f.st.CreateTask(store.Task{ID: id, Title: "Planned work"}, "you"); err != nil {
		t.Fatal(err)
	}
	f.from("s1", Origin{TaskHint: id}, "UserPromptSubmit", P{"prompt": "do it"})
	eq(t, []string{f.task(id).Title, f.task(id).Prompt}, []string{"Planned work", ""})
}

func TestSessionStartClearBeforeTheOldSessionEnd(t *testing.T) {
	f := newFixture(t)
	pane := Origin{Pane: "%1"}
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	f.from("s0", Origin{Pane: "%2"}, "UserPromptSubmit", P{"prompt": "elsewhere"})
	f.st.Exec("UPDATE sessions SET task_id = 's1' WHERE session_id = 's0'")
	eq(t, f.from("s2", pane, "SessionStart", P{"source": "clear"}), "s2")
	// The cleared session is over, so nothing reaches its pane through the old task; another pane goes on.
	eq(t, []bool{f.session("s1").Live, f.session("s0").Live}, []bool{false, true})
	f.from("s1", pane, "SessionEnd", nil)
	// Closed already: the end of its session does not park it in review.
	eq(t, f.task("s1").State, "done")
	eq(t, f.task("s2").State, "backlog")
}

func TestSessionStartClearOfASessionTheBoardStarted(t *testing.T) {
	f := newFixture(t)
	const id = "task-0001-aaaa"
	if _, err := f.st.CreateTask(store.Task{ID: id, Title: "Planned work", Prompt: "do it"}, "you"); err != nil {
		t.Fatal(err)
	}
	// The process keeps HC_TASK across a /clear, so every session in it carries the hint.
	hinted := Origin{Pane: "%1", TaskHint: id}
	eq(t, f.from("s1", hinted, "UserPromptSubmit", P{"prompt": "do it"}), id)
	eq(t, f.from("s2", hinted, "SessionStart", P{"source": "clear"}), "s2")
	eq(t, f.task(id).State, "done")
	eq(t, f.from("s2", hinted, "UserPromptSubmit", P{"prompt": "next"}), "s2")
	// The pane wins over the hint: a compaction goes on with the task the /clear opened.
	eq(t, f.from("s3", hinted, "SessionStart", P{"source": "compact"}), "s2")
	// Outside tmux and Herdr, the hint names the task.
	f2 := newFixture(t)
	if _, err := f2.st.CreateTask(store.Task{ID: id, Title: "Planned work", Prompt: "do it"}, "you"); err != nil {
		t.Fatal(err)
	}
	bare := Origin{TaskHint: id}
	f2.from("s1", bare, "UserPromptSubmit", P{"prompt": "do it"})
	eq(t, f2.from("s2", bare, "SessionStart", P{"source": "clear"}), "s2")
	eq(t, f2.task(id).State, "done")
}

func TestSessionStartClearAfterAResetKeepsTheTask(t *testing.T) {
	f := newFixture(t)
	pane := Origin{Pane: "%1"}
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	f.from("s1", pane, "Stop", P{"last_assistant_message": "Halfway."})
	f.move("s1", "done", nil) // closed by hand, yet hc reset asks for the same task
	reset := func(at float64) {
		t.Helper()
		if err := f.st.AddMessage(store.Message{TaskID: "s1", Sender: FromYou, Kind: "reset", Text: "/clear", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	reset(store.Now())
	eq(t, f.from("s2", pane, "SessionStart", P{"source": "clear"}), "s1")
	eq(t, f.count("tasks"), 1)
	eq(t, f.task("s1").Title, "first")
	// A reset counts once: the next /clear is yours.
	eq(t, f.from("s3", pane, "SessionStart", P{"source": "clear"}), "s3")
	eq(t, f.task("s1").State, "done")

	// A reset long gone does not count either.
	f = newFixture(t)
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	reset(store.Now() - 2*resetWindow)
	eq(t, f.from("s2", pane, "SessionStart", P{"source": "clear"}), "s2")
}

func TestSessionStartThatDoesNotLink(t *testing.T) {
	pane := Origin{Pane: "%1"}
	for _, source := range []any{"startup", "resume", nil} {
		f := newFixture(t)
		f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
		eq(t, f.from("s2", pane, "SessionStart", P{"source": source}), "")
		eq(t, f.from("s2", pane, "UserPromptSubmit", P{"prompt": "second"}), "s2")
		eq(t, f.count("tasks"), 2)
	}
	f := newFixture(t)
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	eq(t, f.from("s2", Origin{Pane: "%2"}, "SessionStart", P{"source": "clear"}), "") // another pane
	eq(t, f.from("s3", Origin{}, "SessionStart", P{"source": "clear"}), "")           // no pane
}

func TestSessionStartCompactKeepsADoneTask(t *testing.T) {
	f := newFixture(t)
	pane := Origin{Pane: "%1"}
	f.from("s1", pane, "UserPromptSubmit", P{"prompt": "first"})
	f.from("s1", pane, "Stop", P{"last_assistant_message": "Shipped."})
	eq(t, f.task("s1").State, "done")
	eq(t, f.from("s2", pane, "SessionStart", P{"source": "compact"}), "s1")
	f.from("s2", pane, "UserPromptSubmit", P{"prompt": "one more thing"})
	eq(t, []string{f.task("s1").State, fmt.Sprint(f.count("tasks"))}, []string{"running", "1"})
	// Moved to Done by you, it is closed: what follows the compaction is a new task.
	f.move("s1", "done", nil)
	eq(t, f.from("s3", pane, "SessionStart", P{"source": "compact"}), "")
}

func TestSessionStartOnlyConsidersTheLatestSessionOfThePane(t *testing.T) {
	f := newFixture(t)
	pane := Origin{Pane: "%1"}
	f.from("sA", pane, "UserPromptSubmit", P{"prompt": "old task"})
	f.from("sA", pane, "Stop", P{"last_assistant_message": "done"})
	f.from("sA", pane, "SessionEnd", nil)
	f.st.Exec("UPDATE sessions SET started_at = started_at - 1000 WHERE session_id = 'sA'")
	f.from("sB", pane, "SessionStart", P{"source": "startup"})
	f.from("sB", pane, "UserPromptSubmit", P{"prompt": "new task"})
	f.from("sB", pane, "SessionEnd", P{"reason": "clear"})
	eq(t, f.from("sC", pane, "SessionStart", P{"source": "compact"}), "sB")
	eq(t, f.task("sA").State, "done")
	// Another Herdr server's pane of the same id is another pane.
	herdr := Origin{HerdrPane: "w1-p1", HerdrSocket: "/run/herdr.sock"}
	f.from("sD", herdr, "UserPromptSubmit", P{"prompt": "in herdr"})
	eq(t, f.from("sE", Origin{HerdrPane: "w1-p1", HerdrSocket: "/run/other.sock"}, "SessionStart",
		P{"source": "compact"}), "")
}

// --- the X-HC-Task hint --------------------------------------------------------------

func TestHintLinksANewSessionToABacklogTask(t *testing.T) {
	f := newFixture(t)
	const id = "task-0001-aaaa"
	if _, err := f.st.CreateTask(store.Task{ID: id, Title: "Planned work", Prompt: "do it"}, "you"); err != nil {
		t.Fatal(err)
	}
	hint := Origin{TaskHint: id}
	eq(t, f.from("s1", hint, "SessionStart", P{"source": "startup"}), id)
	eq(t, f.task(id).State, "backlog")
	short, _ := os.ReadFile(filepath.Join(f.cfg.SessionsDir(), "s1"))
	eq(t, string(short), "task-000")
	eq(t, f.from("s1", hint, "UserPromptSubmit", P{"prompt": "do it"}), id)
	eq(t, f.count("tasks"), 1)
	eq(t, []string{f.task(id).State, f.task(id).Title}, []string{"running", "Planned work"})
	eq(t, f.history(id), []string{"backlog you", "running hook"})
}

func TestHintsThatAreIgnored(t *testing.T) {
	f := newFixture(t)
	eq(t, f.from("s1", Origin{TaskHint: "no-such-task"}, "UserPromptSubmit", P{"prompt": "x"}), "s1")
	f.st.CreateTask(store.Task{ID: "other", Title: "Other"}, "you")
	// A session that already has a task keeps it.
	eq(t, f.from("s1", Origin{TaskHint: "other"}, "PreToolUse", P{"tool_name": "Bash"}), "s1")
}

func TestHintFromAnotherProfileDoesNotLink(t *testing.T) {
	f := newFixture(t)
	const id = "task-0001-aaaa"
	if _, err := f.st.CreateTask(store.Task{ID: id, Title: "Planned work", Profile: "team"}, "you"); err != nil {
		t.Fatal(err)
	}
	eq(t, f.from("s1", Origin{TaskHint: id, ConfigDir: "/home/me/.claude-team"}, "UserPromptSubmit", P{"prompt": "x"}), id)
	// A personal claude run nested in the team session inherits the hint.
	eq(t, f.from("s2", Origin{TaskHint: id, ConfigDir: "/home/me/.claude-personal"}, "UserPromptSubmit", P{"prompt": "y"}), "s2")
	eq(t, f.count("tasks"), 2)
}

func TestSecondSessionAttachedToOneTask(t *testing.T) {
	f := newFixture(t)
	f.from("s1", Origin{Pane: "%1"}, "UserPromptSubmit", P{"prompt": "x"})
	eq(t, f.from("s2", Origin{Pane: "%2", TaskHint: "s1"}, "UserPromptSubmit", P{"prompt": "y"}), "s1")
	eq(t, f.count("tasks"), 1)
	// A sub-agent of one session keeps the shared task running.
	f.from("s2", Origin{}, "SubagentStart", P{"agent_id": "a1"})
	f.from("s1", Origin{}, "Stop", P{"last_assistant_message": "done"})
	eq(t, f.task("s1").State, "running")
	f.from("s1", Origin{}, "SessionEnd", nil)
	eq(t, f.task("s1").State, "running") // s2 is still live
	f.from("s2", Origin{}, "SessionEnd", nil)
	eq(t, f.task("s1").State, "review")
}

// fork starts session s2 as a fork of s1, the way Claude Code moves a session to the background.
func (f *fixture) fork() {
	f.t.Helper()
	eq(f.t, f.from("s2", Origin{TaskHint: "s1"}, "SessionStart", P{"source": "fork"}), "s1")
}

func TestForkTakesOverARunningSubAgent(t *testing.T) {
	f := newFixture(t)
	f.from("s1", Origin{Pane: "%1"}, "UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("general-purpose", "review the range"))
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "general-purpose"})
	f.fork()
	// The terminal the session left can close: the sub-agent runs on in the fork.
	f.on("SessionEnd", nil)
	f.from("s2", Origin{}, "Stop", P{"last_assistant_message": "Reviewing."})
	eq(t, f.agents("s1"), []string{"general-purpose|review the range|a1|running"})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"running", "Waiting for 1 sub-agent"})
	// The fork announces the sub-agent again, and every later event carries the new session id.
	f.from("s2", Origin{}, "SubagentStart", P{"agent_id": "a1", "agent_type": "general-purpose"})
	f.from("s2", Origin{}, "PreToolUse", P{"agent_id": "a1", "tool_name": "Skill", "tool_input": map[string]any{"skill": "reviewer"}})
	eq(t, f.column("SELECT session_id || ' ' || persona || ' ' || state FROM agents"), []string{"s2 reviewer running"})
	f.from("s2", Origin{}, "SubagentStop", P{"agent_id": "a1"})
	f.from("s2", Origin{}, "Stop", P{"last_assistant_message": "Reviewed."})
	eq(t, f.agents("s1"), []string{"general-purpose|review the range|a1|done"})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"done", "Reviewed."})
}

func TestForkBeforeASpawnReportsItsAgentID(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("PreToolUse", agent("Explore", "find the parser"))
	f.fork()
	f.from("s2", Origin{}, "SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	eq(t, f.agents("s1"), []string{"Explore|find the parser|a1|running"})
	f.from("s2", Origin{}, "SubagentStop", P{"agent_id": "a1"})
	f.from("s2", Origin{}, "Stop", P{"last_assistant_message": "Found it."})
	eq(t, f.agents("s1"), []string{"Explore|find the parser|a1|done"})
	eq(t, f.task("s1").State, "done")
}

func TestForkedSessionThatLivesOnStillReportsItsSubAgent(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.fork()
	// A fork can be a copy: the first session then keeps the sub-agent, and is the one to report it.
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.on("SubagentStop", P{"agent_id": "a1"})
	eq(t, f.agents("s1"), []string{"Explore||a1|done"})
}

func TestForkTakesOverABackgroundJob(t *testing.T) {
	// Only the end of a turn lists the jobs, so the fork's first list can come after a job ended.
	for _, first := range [][]any{{job("b1", "shell", "running")}, {}} {
		f := newFixture(t)
		f.on("UserPromptSubmit", P{"prompt": "clean the logs"})
		f.on("Stop", P{"last_assistant_message": "The dump is running.", "background_tasks": []any{job("b1", "shell", "running")}})
		f.fork()
		f.from("s2", Origin{}, "Stop", P{"last_assistant_message": "Still running.", "background_tasks": first})
		eq(t, len(f.agents("s1")), 1)
		f.from("s2", Origin{}, "Stop", P{"last_assistant_message": "All sorted.", "background_tasks": []any{}})
		eq(t, f.agents("s1"), []string{"command|dump the database|bg:b1|done"})
		eq(t, f.task("s1").State, "done")
	}
}

func TestOnlyAForkTakesOverChildRows(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "Explore"})
	f.from("s2", Origin{TaskHint: "s1"}, "SessionStart", P{"source": "startup"})
	f.from("s3", Origin{}, "SessionStart", P{"source": "fork"}) // not a session of the task
	eq(t, f.column("SELECT session_id FROM agents"), []string{"s1"})
}

// --- manual moves --------------------------------------------------------------------

func TestManualDoneThenNewPromptGoesBackToRunning(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	f.on("Stop", P{"last_assistant_message": "Shall I go on?"})
	f.move("s1", "done", store.Fields{"last": "Moved by you"})
	f.on("UserPromptSubmit", P{"prompt": "one more thing"})
	eq(t, f.task("s1").State, "running")
	h := f.history("s1")
	eq(t, h[len(h)-2:], []string{"done you", "running hook"})
	eq(t, f.count("tasks"), 1)
}

func TestDeletedTaskIsNotTouchedByLaterToolEvents(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "x"})
	if err := f.st.DeleteTask("s1"); err != nil {
		t.Fatal(err)
	}
	eq(t, f.on("PreToolUse", P{"tool_name": "Bash"}), "")
	eq(t, f.on("Stop", nil), "")
	eq(t, f.count("tasks"), 0)
	// A new prompt makes a new task. It cannot reuse the id of the session: nothing says the old one is gone.
	if id := f.on("UserPromptSubmit", P{"prompt": "again"}); id != "s1" {
		t.Errorf("new task id = %q", id)
	}
}

// --- background jobs -----------------------------------------------------------------

func job(id any, kind, status string) map[string]any {
	return map[string]any{"id": id, "type": kind, "status": status, "description": "dump the database", "command": "make dump"}
}

func TestBackgroundCommandKeepsTheTaskRunningUntilItIsGone(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "clean the logs"})
	f.on("Stop", P{"last_assistant_message": "The dump is running.", "background_tasks": []any{job("b1", "shell", "running")}})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"running", "Waiting for 1 background job"})
	eq(t, f.agents("s1"), []string{"command|dump the database|bg:b1|running"})
	// The same job listed again is not a second row.
	f.on("SubagentStop", P{"background_tasks": []any{job("b1", "shell", "running")}})
	eq(t, len(f.agents("s1")), 1)
	f.on("Stop", P{"last_assistant_message": "All sorted.", "background_tasks": []any{}})
	eq(t, f.task("s1").State, "done")
	eq(t, f.agents("s1"), []string{"command|dump the database|bg:b1|done"})
}

func TestBackgroundMonitorAndPlural(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "watch the deploy"})
	f.on("Stop", P{"last_assistant_message": "Watching.", "background_tasks": []any{job(7.0, "shell", "running"), job("m1", "monitor", "running")}})
	eq(t, f.task("s1").Last, "Waiting for 2 background jobs")
	eq(t, f.column("SELECT name || ' ' || agent_id FROM agents ORDER BY name"), []string{"command bg:7", "monitor bg:m1"})
	f.on("SessionEnd", nil)
	eq(t, f.column("SELECT DISTINCT state FROM agents"), []string{"done"})
}

func TestAutoArmedMonitorDoesNotKeepATurnRunning(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "draw the logo"})
	armed := job("m1", "monitor", "running")
	armed["description"] = "live updates for artifact https://claude.ai/artifact/x (auto-armed on publish)"
	f.on("Stop", P{"last_assistant_message": "Published.", "background_tasks": []any{armed}})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"done", "Published."})
	eq(t, f.agents("s1"), []string{"monitor|" + armed["description"].(string) + "|bg:m1|running"})
	// A monitor the agent armed, or a command, still keeps it running, even one that says auto-armed.
	f.on("UserPromptSubmit", P{"prompt": "watch the deploy"})
	own := job("m2", "monitor", "running")
	own["description"] = "watch auto-armed monitors in the log"
	f.on("Stop", P{"last_assistant_message": "Watching.", "background_tasks": []any{armed, own}})
	eq(t, []string{f.task("s1").State, f.task("s1").Last}, []string{"running", "Waiting for 1 background job"})
}

func TestClipJobKeepsTheAutoArmedMark(t *testing.T) {
	long := "live updates for artifact https://claude.ai/code/artifact/" + strings.Repeat("x", 120) + " (auto-armed on publish)"
	got := clipJob(long)
	if n := utf8.RuneCountInString(got); n > 120 || !strings.HasSuffix(got, "(auto-armed on publish)") {
		t.Errorf("clipped to %d runes: %q", n, got)
	}
	eq(t, clipJob("dump the database"), "dump the database")
	eq(t, utf8.RuneCountInString(clipJob(strings.Repeat("y", 200))), 120)
}

func TestQuestionWinsOverABackgroundJob(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "clean the logs"})
	f.on("Stop", P{"last_assistant_message": "Shall I delete them?", "background_tasks": []any{job("b1", "shell", "running")}})
	eq(t, []string{f.task("s1").State, f.task("s1").Ask}, []string{"needs", "Shall I delete them?"})
	eq(t, f.agents("s1"), []string{"command|dump the database|bg:b1|running"})
}

func TestBackgroundJobsThatAreNotRunningOrNotKnownAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.on("UserPromptSubmit", P{"prompt": "go"})
	f.on("Stop", P{"last_assistant_message": "Done.", "background_tasks": []any{
		job("b1", "shell", "completed"), job("x", "agent", "running"), "junk", nil,
	}})
	eq(t, f.task("s1").State, "done")
	eq(t, f.agents("s1"), []string{})
}

// --- personas ------------------------------------------------------------------------

func TestPersona(t *testing.T) {
	f := newFixture(t)
	skill := func(name string) P { return P{"tool_name": "Skill", "tool_input": map[string]any{"skill": name}} }
	f.on("UserPromptSubmit", P{"prompt": "act as a Go developer"})
	eq(t, f.task("s1").Persona, "")
	f.on("PreToolUse", skill("acme-rust-developer"))
	eq(t, f.task("s1").Persona, "acme-rust-developer")
	f.on("PreToolUse", skill("acme-toolkit:reviewer")) // the plugin prefix is dropped
	eq(t, f.task("s1").Persona, "reviewer")
	f.on("PreToolUse", skill("acme-toolkit:commit")) // not a persona
	eq(t, f.task("s1").Persona, "reviewer")
	f.on("PreToolUse", skill("autotester")) // a persona name inside another word is not one
	eq(t, f.task("s1").Persona, "reviewer")
	f.on("PreToolUse", skill("developer-go"))
	eq(t, f.task("s1").Persona, "developer-go")
	f.on("UserPromptSubmit", P{"prompt": "/acme-go-developer fix PROJ-1"})
	eq(t, f.task("s1").Persona, "acme-go-developer")
	f.on("UserPromptSubmit", P{"prompt": "/clear"})
	eq(t, f.task("s1").Persona, "acme-go-developer")

	// A sub-agent's persona is its own, the task keeps the main session's.
	f.on("SubagentStart", P{"agent_id": "a1", "agent_type": "general-purpose"})
	sub := skill("reviewer")
	sub["agent_id"] = "a1"
	f.on("PreToolUse", sub)
	eq(t, f.task("s1").Persona, "acme-go-developer")
	eq(t, f.column("SELECT persona FROM agents WHERE agent_id = 'a1'"), []string{"reviewer"})
	// Its first tool call can arrive before its SubagentStart: one row either way.
	f.on("PreToolUse", P{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": "Plan", "description": "plan it"}})
	early := skill("architect")
	early["agent_id"], early["agent_type"] = "a2", "Plan"
	f.on("PreToolUse", early)
	f.on("SubagentStart", P{"agent_id": "a2", "agent_type": "Plan"})
	eq(t, f.column("SELECT name || '|' || descr || '|' || persona FROM agents WHERE agent_id = 'a2' OR (name = 'Plan')"),
		[]string{"Plan|plan it|architect"})
	sub = skill("commit")
	sub["agent_id"] = "a1"
	f.on("PreToolUse", sub)
	eq(t, f.column("SELECT persona FROM agents WHERE agent_id = 'a1'"), []string{"reviewer"})

	f.from("s2", Origin{}, "UserPromptSubmit", P{"prompt": "/planner sketch it"})
	eq(t, f.task("s2").Persona, "planner")

	f.cfg.PersonaPattern = "(" // a pattern that does not compile matches nothing
	broken := New(f.st, f.cfg)
	if _, err := broken.Apply(Payload{"hook_event_name": "UserPromptSubmit", "session_id": "s3", "prompt": "/developer x"}, Origin{}); err != nil {
		t.Fatal(err)
	}
	eq(t, f.task("s3").Persona, "")
}

func TestStoreResolve(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"abc-1", "abc-2", "xyz-1"} {
		f.st.CreateTask(store.Task{ID: id, Title: id}, "you")
	}
	for prefix, want := range map[string]string{"abc-1": "abc-1", "x": "xyz-1", "nope": "", "": ""} {
		got, err := f.st.Resolve(prefix)
		id := ""
		if got != nil {
			id = got.ID
		}
		if err != nil || id != want {
			t.Errorf("Resolve(%q) = %q, %v", prefix, id, err)
		}
	}
	if _, err := f.st.Resolve("abc"); err == nil || !strings.Contains(fmt.Sprint(err), "more than one") {
		t.Errorf("an ambiguous prefix gave %v", err)
	}
}
