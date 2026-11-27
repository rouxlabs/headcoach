package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
	"github.com/rouxlabs/headcoach/internal/transcript"
	"github.com/rouxlabs/headcoach/internal/version"
)

// --- meta, pages, the guard ----------------------------------------------------------

func TestMeta(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Join(f.tmp, "roots", "repo-a", ".git"), 0o755)
	os.MkdirAll(filepath.Join(f.tmp, "roots", "group", "repo-b", ".git"), 0o755)
	f.cfg.RepoRoots = []string{filepath.Join(f.tmp, "roots"), filepath.Join(f.tmp, "missing-root")}
	f.newTask(nil)
	meta := f.ok(f.get("/api/meta")).data
	var states, dirs []string
	for _, s := range meta["states"].([]any) {
		states = append(states, s.(obj)["id"].(string))
		if s.(obj)["name"] == "" {
			t.Error("a state has no name")
		}
	}
	for _, d := range meta["dirs"].([]any) {
		dirs = append(dirs, d.(string))
	}
	eq(t, states, []string{"backlog", "running", "needs", "review", "parked", "done"})
	eq(t, meta["profiles"], []any{"team", "personal"})
	eq(t, dirs, []string{f.tmp, filepath.Join(f.tmp, "roots", "group", "repo-b"), filepath.Join(f.tmp, "roots", "repo-a")})
	if !sort.StringsAreSorted(dirs) || meta["tmux"] != true || meta["version"] == "" || meta["private_tmux"] != false {
		t.Errorf("meta = %v", meta)
	}
	f.tmux.available = false
	eq(t, f.get("/api/meta").data["tmux"], false)
}

func TestIndexPage(t *testing.T) {
	f := newFixture(t)
	page := f.get("/")
	if page.status != 200 || !strings.Contains(page.header.Get("Content-Type"), "text/html") || !strings.Contains(string(page.raw), "<html") {
		t.Errorf("index = %d %s", page.status, page.header.Get("Content-Type"))
	}
}

func TestRouterErrorsUseTheErrorShape(t *testing.T) {
	f := newFixture(t)
	f.wantError(f.get("/api/nope"), 404)
	f.wantError(f.get("/nope"), 404)
	f.wantError(f.do("PUT", "/api/tasks", nil), 405)
	f.wantError(f.do("DELETE", "/api/meta", nil), 405)
}

func TestRequestsFromElsewhereAreRefused(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"prompt": "attacker prompt", "start": true}`)
	// What a form on another site can send: a simple content type, its own origin.
	f.wantError(f.do("POST", "/api/tasks", body, "Origin", "https://evil.example", "Content-Type", "text/plain"), 403)
	f.wantError(f.do("POST", "/api/tasks", body, "Content-Type", "text/plain"), 415)
	f.wantError(f.do("POST", "/api/tasks", body), 415)
	f.wantError(f.do("POST", "/api/tasks", obj{"title": "x"}, "Origin", "https://evil.example"), 403)
	// A name that resolves to this machine but is not this machine's name: DNS rebinding.
	f.wantError(f.do("GET", "/api/tasks", nil, "Host", "evil.example:7337"), 403)
	eq(t, f.count("tasks"), 0)
	eq(t, len(f.tmux.windows), 0)
	// The board's own page may.
	f.ok(f.do("POST", "/api/tasks", obj{"title": "x"}, "Origin", "http://127.0.0.1:7337"))
	f.ok(f.do("GET", "/api/tasks", nil, "Host", "localhost:7337"))
}

// --- POST /api/tasks -----------------------------------------------------------------

func TestCreateTaskRefusals(t *testing.T) {
	f := newFixture(t)
	for _, data := range []obj{{}, {"title": "", "prompt": ""}, {"title": "   ", "prompt": " \n "}} {
		f.wantError(f.post("/api/tasks", data), 400)
	}
	f.wantError(f.post("/api/tasks", obj{"title": "x", "cwd": filepath.Join(f.tmp, "nope")}), 400)
	unknown := f.wantError(f.post("/api/tasks", obj{"title": "x", "cwd": f.tmp, "profile": "nope"}), 400)
	if !strings.Contains(unknown.str("error"), "nope") {
		t.Errorf("error = %s", unknown.str("error"))
	}
	for _, raw := range []string{"{nope", "[1, 2]", `"text"`, "\xff\xfe"} {
		f.wantError(f.do("POST", "/api/tasks", []byte(raw), "Content-Type", "application/json"), 400)
	}
	f.wantError(f.do("POST", "/api/tasks", nil, "Content-Type", "application/json"), 400) // an empty body
	eq(t, f.count("tasks"), 0)
}

func TestCreateTask(t *testing.T) {
	f := newFixture(t)
	plain := filepath.Join(f.tmp, "plain")
	os.Mkdir(plain, 0o755)
	task := f.ok(f.post("/api/tasks", obj{"prompt": "  Refactor the parser\nkeep the API  ", "cwd": plain, "profile": "personal"}))
	id := task.str("id")
	wanted := obj{
		"title": "Refactor the parser", "prompt": "Refactor the parser\nkeep the API", "state": "backlog",
		"state_name": "Backlog", "profile": "personal", "cwd": plain, "repo": "plain", "branch": "", "source": "manual",
		"short": id[:8], "live": false, "pane": "", "session_id": "", "can_reply": true, "last": "Not started",
		"resume": "claude-personal --session-id " + id, "agents": []any{}, "attach": []any{}, "links": []any{},
		"tags": []any{"personal"}, "cost": 0.0,
		"tokens":  obj{"in": 0.0, "out": 0.0, "cache_read": 0.0, "cache_write": 0.0},
		"context": obj{"tokens": 0.0, "window": 200_000.0, "pct": 0.0},
	}
	for key, want := range wanted {
		if got := task.data[key]; !eqValue(got, want) {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if _, listed := task.data["log"]; listed {
		t.Error("the view of a new task carries the detail")
	}
	eq(t, len(f.tmux.windows), 0)
	detail := f.ok(f.get("/api/tasks/" + id)).data
	eq(t, detail["sessions"], []any{})
	eq(t, detail["log"], []any{})
	eq(t, detail["screen"], "")
	eq(t, f.history(id), []string{"backlog you"})

	// A title-only task keeps an empty prompt: its session opens empty, the prompt is typed there.
	only := f.ok(f.post("/api/tasks", obj{"title": "Just a title"}))
	eq(t, []any{only.data["title"], only.data["prompt"], only.data["profile"], only.data["cwd"]},
		[]any{"Just a title", "", "team", ""})
}

func eqValue(got, want any) bool { return fmt.Sprintf("%#v", got) == fmt.Sprintf("%#v", want) }

func TestCreateTaskWithStart(t *testing.T) {
	f := newFixture(t)
	f.cfg.TmuxSession = "main"
	task := f.newTask(obj{"start": true})
	id := task.str("id")
	eq(t, []any{task.data["state"], task.data["live"], task.data["pane"], task.data["session_id"]},
		[]any{"running", true, "%100", id})
	eq(t, len(f.tmux.windows), 1)
	win := f.tmux.windows[0]
	eq(t, shellWords(t, win.Command), []string{"claude-team", "--session-id", id, "--", "do the thing"})
	eq(t, win.Env, []string{"HC_TASK=" + id, "HC_URL=http://127.0.0.1:7337", "HC_HANDOFF=" + f.cfg.HandoffPath(id)})
	eq(t, []string{win.Cwd, win.Name, win.Session}, []string{f.tmp, id[:8] + "-do-the-thing", "main"})
	eq(t, f.history(id), []string{"backlog you", "running you"})
	short, _ := os.ReadFile(filepath.Join(f.cfg.SessionsDir(), id))
	eq(t, string(short), id[:8])

	// The first hooks of the session the board started land on the task.
	headers := []string{"X-Tmux-Pane", "%100", "X-HC-Task", id}
	eq(t, f.hook("SessionStart", id, obj{"source": "startup"}, headers...), id)
	eq(t, f.hook("UserPromptSubmit", id, obj{"prompt": "do the thing"}, headers...), id)
	eq(t, []int{f.count("tasks"), f.count("sessions")}, []int{1, 1})

	// Moving it back and forth opens no second window.
	f.patch("/api/tasks/"+id, obj{"state": "backlog"})
	eq(t, f.ok(f.patch("/api/tasks/"+id, obj{"state": "running"})).data["state"], "running")
	eq(t, len(f.tmux.windows), 1)
}

func TestStartWithoutTmux(t *testing.T) {
	f := newFixture(t)
	f.tmux.available = false
	f.wantError(f.post("/api/tasks", obj{"prompt": "x", "cwd": f.tmp, "start": true}), 503)
	eq(t, f.count("tasks"), 0) // no orphan
	id := f.newTask(nil).str("id")
	f.wantError(f.patch("/api/tasks/"+id, obj{"state": "running"}), 503)
	f.wantError(f.post("/api/tasks/"+id+"/start", nil), 503)
	eq(t, f.task(id).State, "backlog")

	f.tmux.available, f.tmux.fail = true, "no space for new window"
	broken := f.wantError(f.post("/api/tasks/"+id+"/start", nil), 502)
	if !strings.Contains(broken.str("error"), "no space for new window") {
		t.Errorf("error = %s", broken.str("error"))
	}
	eq(t, f.task(id).State, "backlog")
	eq(t, f.count("sessions"), 0)
}

func TestWindowCommandQuotesThePrompt(t *testing.T) {
	f := newFixture(t)
	for _, prompt := range []string{
		`it's a "quoted" prompt`,
		"run $(touch /tmp/pwned) and `id` and ${HOME}",
		"semi; colon && rm -rf ~ | cat > /dev/null",
		"line one\nline two\n\ttabbed",
		`back\slash and 'single' quotes ''`,
		"unicode éè 😀 !! * ? [a-z] ~ #comment",
		"--dangerously-skip-permissions",
		strings.TrimSpace(strings.Repeat("long 'prompt' $(x) ", 2_000)),
	} {
		task := f.newTask(obj{"prompt": prompt, "start": true})
		got := shellWords(t, f.tmux.windows[len(f.tmux.windows)-1].Command)
		eq(t, got, []string{"claude-team", "--session-id", task.str("id"), "--", prompt})
		if utf8.RuneCountInString(task.str("title")) > 100 {
			t.Error("the title of a long prompt is not clipped")
		}
	}
}

func TestWindowFallsBackToHomeWhenTheCwdIsGone(t *testing.T) {
	f := newFixture(t)
	gone := filepath.Join(f.tmp, "gone")
	os.Mkdir(gone, 0o755)
	id := f.newTask(obj{"cwd": gone}).str("id")
	os.Remove(gone)
	f.ok(f.post("/api/tasks/"+id+"/start", nil))
	home, _ := os.UserHomeDir()
	eq(t, f.tmux.windows[0].Cwd, home)
}

// --- id prefixes ---------------------------------------------------------------------

func TestPrefixResolution(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"aaaa1111-x", "aaaa2222-x", "bbbb3333-x", "abc", "abcdef"} {
		f.st.CreateTask(store.Task{ID: id, Title: id}, "you")
	}
	eq(t, f.get("/api/tasks/bbbb").str("id"), "bbbb3333-x")
	eq(t, f.get("/api/tasks/aaaa1").str("id"), "aaaa1111-x")
	eq(t, f.get("/api/tasks/abc").str("id"), "abc") // an exact id wins over a longer match
	eq(t, f.patch("/api/tasks/b", obj{"title": "Renamed"}).str("title"), "Renamed")
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		var body any
		if method == "PATCH" {
			body = obj{"title": "x"}
		}
		f.wantError(f.do(method, "/api/tasks/aaaa", body), 409)
		f.wantError(f.do(method, "/api/tasks/zzzz", body), 404)
		f.wantError(f.do(method, "/api/tasks/a_aa1", body), 404) // an underscore is not a wildcard
		f.wantError(f.do(method, "/api/tasks/%25", body), 404)   // nor is a percent sign
	}
	for _, action := range []string{"start", "focus", "reply", "ask", "keys", "adopt"} {
		f.wantError(f.post("/api/tasks/zzzz/"+action, obj{"text": "x"}), 404)
	}
	eq(t, f.task("aaaa1111-x").Title, "aaaa1111-x")
	eq(t, f.do("DELETE", "/api/tasks/bbbb3", nil).data, obj{"deleted": "bbbb3333-x"})
	eq(t, f.count("tasks"), 4)
}

// --- PATCH ---------------------------------------------------------------------------

func TestPatch(t *testing.T) {
	f := newFixture(t)
	path := "/api/tasks/" + f.newTask(nil).str("id")
	eq(t, f.ok(f.patch(path, obj{"title": "  New title  "})).str("title"), "New title")
	f.wantError(f.patch(path, obj{"title": "   "}), 400)
	if long := f.patch(path, obj{"title": strings.Repeat("t", 5000)}).str("title"); utf8.RuneCountInString(long) > 200 {
		t.Error("a long title is not clipped")
	}
	f.wantError(f.patch(path, obj{"state": "archived"}), 400)
	f.wantError(f.do("PATCH", path, []byte("nope"), "Content-Type", "application/json"), 400)
	f.wantError(f.do("PATCH", path, []byte("nope")), 415)

	out := f.ok(f.patch(path, obj{"state": "done"}))
	eq(t, []string{out.str("state"), out.str("state_name"), out.str("last")}, []string{"done", "Done", "Moved by you"})
	f.ok(f.patch(path, obj{"state": "done"}))
	eq(t, f.history(out.str("id")), []string{"backlog you", "done you"})
	eq(t, len(f.tmux.windows), 0)
}

// --- start, live sessions, reply, focus ----------------------------------------------

func TestStartEndpoint(t *testing.T) {
	f := newFixture(t)
	path := "/api/tasks/" + f.newTask(nil).str("id")
	out := f.ok(f.post(path+"/start", nil))
	eq(t, []any{out.data["state"], out.data["live"]}, []any{"running", true})
	f.wantError(f.post(path+"/start", nil), 409)
	eq(t, len(f.tmux.windows), 1)
	// The window closed at once and no hook was ever received: starting again still sends the prompt.
	delete(f.tmux.known, "%100")
	f.ok(f.post(path+"/start", nil))
	words := shellWords(t, f.tmux.windows[1].Command)
	eq(t, words[len(words)-1], "do the thing")
}

func TestLiveTaskView(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	task := f.ok(f.get("/api/tasks/" + id)).data
	eq(t, []any{task["live"], task["pane"], task["session_id"], task["profile"], task["resume"]},
		[]any{true, "%1", "s1", "personal", "claude-personal --resume s1"})
	sessions := task["sessions"].([]any)
	eq(t, []any{len(sessions), sessions[0].(obj)["session_id"], sessions[0].(obj)["live"]}, []any{1, "s1", true})
	eq(t, f.tmux.renamed, []string{"%1|s1-hello"})

	// A pane tmux no longer knows is not live, whatever the database says.
	delete(f.tmux.known, "%1")
	task = f.get("/api/tasks/" + id).data
	eq(t, []any{task["live"], task["pane"], task["session_id"]}, []any{false, "", "s1"})
	eq(t, f.session("s1").Live, true)

	// A session outside tmux has no pane to be live in.
	outside := f.hook("UserPromptSubmit", "s9", obj{"prompt": "no tmux here"}, "X-Config-Dir", "")
	eq(t, f.get("/api/tasks/" + outside).data["live"], false)
}

func TestResumeSkipsASessionOfAnotherProfile(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	// A newer session of the team profile got filed under this personal task.
	transcript := filepath.Join(f.tmp, "s2.jsonl")
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Exec("INSERT INTO sessions (session_id, task_id, config_dir, transcript_path, started_at)"+
		" VALUES ('s2', ?, '/Users/x/.claude-team', ?, ?)", id, transcript, store.Now()+60); err != nil {
		t.Fatal(err)
	}
	eq(t, f.ok(f.get("/api/tasks/" + id)).data["resume"], "claude-personal --resume s1")
}

func TestResumeSkipsASessionWithoutItsTranscript(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	if _, err := f.st.Exec("INSERT INTO sessions (session_id, task_id, config_dir, transcript_path, started_at)"+
		" VALUES ('s2', ?, '/Users/x/.claude-personal', ?, ?)", id, filepath.Join(f.tmp, "gone.jsonl"), store.Now()+60); err != nil {
		t.Fatal(err)
	}
	eq(t, f.ok(f.get("/api/tasks/" + id)).data["resume"], "claude-personal --resume s1")
}

func TestHookNamesTheWindowOnce(t *testing.T) {
	f := newFixture(t)
	const sid = "0a1b2c3d-0000-4000-8000-000000000001"
	f.hook("SessionStart", sid, nil, "X-Tmux-Pane", "%9")
	eq(t, len(f.tmux.renamed), 0)
	f.hook("UserPromptSubmit", sid, obj{"prompt": "look at PROJ-42 please"}, "X-Tmux-Pane", "%9")
	f.hook("UserPromptSubmit", sid, obj{"prompt": "look at PROJ-42 please"}, "X-Tmux-Pane", "%9")
	eq(t, f.tmux.renamed, []string{"%9|0a1b2c3d-PROJ-42"})

	// A /clear you type opens a new task, and its first prompt names the window again.
	const next = "1b2c3d4e-0000-4000-8000-000000000002"
	f.hook("SessionStart", next, obj{"source": "clear"}, "X-Tmux-Pane", "%9")
	f.hook("UserPromptSubmit", next, obj{"prompt": "write the docs"}, "X-Tmux-Pane", "%9")
	f.hook("UserPromptSubmit", next, obj{"prompt": "and the tests"}, "X-Tmux-Pane", "%9")
	eq(t, f.tmux.renamed, []string{"%9|0a1b2c3d-PROJ-42", "%9|1b2c3d4e-cleared-session", "%9|1b2c3d4e-write-the-docs"})
}

func TestReply(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	path := "/api/tasks/" + id + "/reply"
	out := f.ok(f.post(path, obj{"text": "  yes, go on\nplease  "}))
	eq(t, f.tmux.sent, []string{"%1|yes, go on\nplease|"})
	eq(t, out.str("last"), "You: yes, go on")
	for _, text := range []any{"", "   ", nil} {
		f.wantError(f.post(path, obj{"text": text}), 400)
	}

	// A closing question is answered by a reply.
	f.hook("Stop", "s1", obj{"last_assistant_message": "Shall I continue?"})
	task := f.get("/api/tasks/" + id).data
	eq(t, []any{task["state"], task["ask_kind"], task["can_reply"]}, []any{"needs", "question", true})
	f.ok(f.post(path, obj{"text": "yes"}))

	// A dialog with no free text option is not: it wants keys, in the terminal.
	f.tmux.screen = "Do you want to proceed?\n  1. Yes   \n\n\n"
	f.hook("Notification", "s1", obj{"notification_type": "permission_prompt", "message": "Claude needs your permission to use Bash"})
	task = f.get("/api/tasks/" + id).data
	eq(t, []any{task["can_reply"], task["screen"]}, []any{false, "Do you want to proceed?\n  1. Yes"})
	f.wantError(f.post(path, obj{"text": "yes"}), 409)
	f.hook("PostToolUse", "s1", obj{"tool_name": "Bash"})
	f.hook("PreToolUse", "s1", obj{"tool_name": "AskUserQuestion", "tool_input": obj{"questions": []any{obj{"question": "A or B?"}}}})
	f.wantError(f.post(path, obj{"text": "A"}), 409)
	eq(t, len(f.tmux.sent), 2)
	eq(t, len(f.tmux.windows), 0)
	eq(t, []any{len(f.tmux.pressed), len(f.tmux.typed)}, []any{0, 0})

	// A question with a free text option takes the reply there, on one line, then submits it, even after
	// the permission notification Claude Code sends with it.
	f.hook("Notification", "s1", obj{"notification_type": "permission_prompt", "message": "Claude needs your permission"})
	f.tmux.screen = "A or B?\n❯ 1. A\n  2. B\n  3. Type something.\n───\n  4. Chat about this\n"
	out = f.ok(f.post(path, obj{"text": " neither,\n\tboth \x1b[A "}))
	eq(t, f.tmux.pressed, []string{"%1|3", "%1|Enter"})
	eq(t, f.tmux.typed, []string{"%1|neither, both [A"})
	eq(t, out.str("last"), "You: neither, both [A")
	f.wantError(f.post(path, obj{"text": "\x1b"}), 400)
	eq(t, len(f.tmux.sent), 2)
	f.tmux.fail = "pane is dead"
	f.wantError(f.post(path, obj{"text": "C"}), 502)
	f.tmux.fail = ""
	// With the cursor already on the option, its number would land in the answer: only the text is typed.
	f.tmux.pressed, f.tmux.typed = nil, nil
	f.tmux.screen = "A or B?\n  1. A\n  2. B\n❯ 3. Type something.\n───\n  4. Chat about this\n"
	f.ok(f.post(path, obj{"text": "C"}))
	eq(t, f.tmux.pressed, []string{"%1|Enter"})
	eq(t, f.tmux.typed, []string{"%1|C"})

	// Text above a dialog is not its option: a plan about this very feature waits for its keys.
	f.tmux.pressed, f.tmux.typed = nil, nil
	f.tmux.screen = "Plan:\n  1. Type something.\n  2. Chat about this\n\nWould you like to proceed?\n❯ 1. Yes\n  2. No\n"
	f.wantError(f.post(path, obj{"text": "shorter"}), 409)
	// A terminal that cannot be read is a tmux failure, not a dialog without an option.
	f.tmux.screen = ""
	f.wantError(f.post(path, obj{"text": "C"}), 502)
	eq(t, []any{len(f.tmux.pressed), len(f.tmux.typed)}, []any{0, 0})

	// A permission prompt's own free text option rejects the tool: a reply never picks it.
	f.tmux.pressed, f.tmux.typed = nil, nil
	f.tmux.screen = "  3. Type something.\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n"
	f.hook("PostToolUse", "s1", obj{"tool_name": "AskUserQuestion"})
	f.hook("Notification", "s1", obj{"notification_type": "permission_prompt", "message": "Claude needs your permission to use Bash"})
	f.wantError(f.post(path, obj{"text": "no"}), 409)
	eq(t, []any{len(f.tmux.pressed), len(f.tmux.typed)}, []any{0, 0})

	f.hook("PostToolUse", "s1", obj{"tool_name": "AskUserQuestion"})
	f.tmux.fail = "pane is dead"
	f.wantError(f.post(path, obj{"text": "x"}), 502)
}

func TestReplyToAClosedTaskResumesItInANewWindow(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.hook("Stop", "s1", obj{"last_assistant_message": "done"})
	f.hook("SessionEnd", "s1", nil)
	delete(f.tmux.known, "%1")
	text := `it's $(whoami) "again"`
	out := f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": text}))
	eq(t, len(f.tmux.sent), 0)
	win := f.tmux.windows[0]
	eq(t, shellWords(t, win.Command), []string{"claude-personal", "--resume", "s1", "--", text})
	eq(t, win.Env[0], "HC_TASK="+id)
	eq(t, []any{out.data["state"], out.data["live"], out.data["pane"]}, []any{"running", true, "%100"})
	sess := f.session("s1")
	if !sess.Live || sess.Pane != "%100" || sess.EndedAt != nil {
		t.Errorf("session = %+v", sess)
	}
}

func TestFocus(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.ok(f.post("/api/tasks/"+id+"/focus", nil))
	eq(t, f.tmux.focused, []string{"%1"})
	eq(t, len(f.tmux.windows), 0)
	// A closed task gets a window first.
	f.hook("SessionEnd", "s1", nil)
	delete(f.tmux.known, "%1")
	f.ok(f.post("/api/tasks/"+id+"/focus", nil))
	eq(t, f.tmux.windows[0].Command, "claude-personal --resume s1")
	eq(t, f.tmux.focused, []string{"%1", "%100"})
	f.tmux.fail = "no client"
	f.wantError(f.post("/api/tasks/"+id+"/focus", nil), 502)
}

func TestScreenAndKeys(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.tmux.screen = strings.Repeat("old\n", 50) + "Pick one   \n> A\n  B\n\n"
	screen := f.ok(f.get("/api/tasks/" + id + "/screen")).str("screen")
	if lines := strings.Split(screen, "\n"); len(lines) != 40 || !strings.HasSuffix(screen, "Pick one\n> A\n  B") {
		t.Errorf("screen has %d lines: %q", len(lines), screen)
	}
	out := f.ok(f.post("/api/tasks/"+id+"/keys", obj{"keys": []any{"Down", "Enter"}}))
	eq(t, f.tmux.pressed, []string{"%1|Down Enter"})
	eq(t, out.str("screen"), screen)
	for _, keys := range []any{"Enter", []any{}, []any{"Enter", 3}, []any{"C-c"}, []any{"rm -rf"}, nil} {
		f.wantError(f.post("/api/tasks/"+id+"/keys", obj{"keys": keys}), 400)
	}
	eq(t, len(f.tmux.pressed), 1)
	delete(f.tmux.known, "%1")
	f.wantError(f.get("/api/tasks/"+id+"/screen"), 409)
	f.wantError(f.post("/api/tasks/"+id+"/keys", obj{"keys": []any{"Enter"}}), 409)
}

// --- delete, list, reconcile, agents -------------------------------------------------

func TestDelete(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.hook("SubagentStart", "s1", obj{"agent_id": "a1", "agent_type": "Explore"})
	eq(t, f.do("DELETE", "/api/tasks/"+id, nil).data, obj{"deleted": id})
	eq(t, []int{f.count("tasks"), f.count("history"), f.count("agents")}, []int{0, 0, 0})
	eq(t, f.session("s1").TaskID, "")
	f.wantError(f.get("/api/tasks/"+id), 404)
	eq(t, f.get("/api/tasks").data, obj{"tasks": []any{}, "limits": []any{}, "version": version.Version})
}

func ids(r response) []string {
	var out []string
	for _, task := range r.data["tasks"].([]any) {
		out = append(out, task.(obj)["id"].(string))
	}
	return out
}

func TestListTasksMostRecentFirst(t *testing.T) {
	f := newFixture(t)
	f.st.CreateTask(store.Task{ID: "t-old", Title: "old"}, "you")
	f.st.CreateTask(store.Task{ID: "t-new", Title: "new"}, "you")
	f.st.Exec("UPDATE tasks SET updated_at = updated_at - 100 WHERE id = 't-old'")
	eq(t, ids(f.get("/api/tasks")), []string{"t-new", "t-old"})
}

func TestADoneTaskStaysOnTheBoardUntilYouHideIt(t *testing.T) {
	f := newFixture(t)
	f.st.CreateTask(store.Task{ID: "t-done", Title: "done"}, "you")
	f.st.CreateTask(store.Task{ID: "t-open", Title: "open"}, "you")
	f.ok(f.patch("/api/tasks/t-done", obj{"state": "done"}))
	hidden := func() map[string]any {
		out := map[string]any{}
		for _, task := range f.get("/api/tasks").data["tasks"].([]any) {
			out[task.(obj)["id"].(string)] = task.(obj)["hidden"]
		}
		return out
	}
	// Hours later it is still in sight: only you hide it.
	f.st.Exec("UPDATE history SET at = at - 36000")
	f.st.Exec("UPDATE tasks SET updated_at = updated_at - 36000")
	eq(t, hidden(), map[string]any{"t-done": false, "t-open": false})

	// Only a done task hides, and a refused request changes nothing.
	f.wantError(f.patch("/api/tasks/t-open", obj{"hidden": true}), 400)
	f.wantError(f.patch("/api/tasks/t-open", obj{"title": "renamed", "hidden": "yes"}), 400)
	f.wantError(f.patch("/api/tasks/t-open", obj{"title": "renamed", "hidden": true}), 400)
	eq(t, f.task("t-open").Title, "open")
	f.wantError(f.patch("/api/tasks/t-done", obj{"state": "review", "hidden": true}), 400)
	eq(t, f.task("t-done").State, "done")

	// Hiding is not activity: the task keeps how long it has been idle.
	before := f.task("t-done").UpdatedAt
	eq(t, f.ok(f.patch("/api/tasks/t-done", obj{"hidden": true})).data["hidden"], true)
	eq(t, hidden(), map[string]any{"t-done": true, "t-open": false})
	eq(t, f.task("t-done").UpdatedAt, before)
	f.ok(f.patch("/api/tasks/t-done", obj{"hidden": false}))
	eq(t, f.task("t-done").Hidden, false)

	// Done again after a move, it is back in sight; a move and a hide in one request hide it.
	f.ok(f.patch("/api/tasks/t-done", obj{"hidden": true}))
	f.ok(f.patch("/api/tasks/t-done", obj{"state": "done"}))
	eq(t, f.task("t-done").Hidden, true)
	f.ok(f.patch("/api/tasks/t-done", obj{"state": "review"}))
	f.ok(f.patch("/api/tasks/t-done", obj{"state": "done"}))
	eq(t, f.task("t-done").Hidden, false)
	f.ok(f.patch("/api/tasks/t-open", obj{"state": "done", "hidden": true}))
	eq(t, f.task("t-open").Hidden, true)
}

func TestPinATaskToTheTop(t *testing.T) {
	f := newFixture(t)
	f.st.CreateTask(store.Task{ID: "t-pin", Title: "pin"}, "you")
	f.st.Exec("UPDATE tasks SET updated_at = updated_at - 600")
	before := f.task("t-pin").UpdatedAt

	// A refused request changes nothing.
	f.wantError(f.patch("/api/tasks/t-pin", obj{"title": "renamed", "pinned": "yes"}), 400)
	eq(t, f.task("t-pin").Title, "pin")

	// Pinning is not activity: the task keeps how long it has been idle.
	got := f.ok(f.patch("/api/tasks/t-pin", obj{"pinned": true})).data
	if got["pinned"] != true || got["pinned_at"].(float64) == 0 {
		t.Errorf("not pinned: %v at %v", got["pinned"], got["pinned_at"])
	}
	eq(t, got["tags"], []any{"default", "pinned"})
	eq(t, f.task("t-pin").UpdatedAt, before)
	eq(t, f.ok(f.patch("/api/tasks/t-pin", obj{"pinned": false})).data["pinned"], false)
	eq(t, f.task("t-pin").PinnedAt, 0.0)
	eq(t, f.task("t-pin").UpdatedAt, before)
}

func TestReconcileClosesSessionsWhosePaneDisappeared(t *testing.T) {
	f := newFixture(t)
	f.tmux.known["%1"], f.tmux.known["%2"] = true, true
	gone := f.hook("UserPromptSubmit", "s1", obj{"prompt": "a"})
	kept := f.hook("UserPromptSubmit", "s2", obj{"prompt": "b"}, "X-Tmux-Pane", "%2")
	f.hook("SubagentStart", "s1", obj{"agent_id": "a1"})
	delete(f.tmux.known, "%1")
	tasks := map[string]obj{}
	for _, task := range f.get("/api/tasks").data["tasks"].([]any) {
		tasks[task.(obj)["id"].(string)] = task.(obj)
	}
	eq(t, []any{tasks[gone]["state"], tasks[gone]["live"], tasks[gone]["last"]}, []any{"review", false, "Session closed"})
	eq(t, []any{tasks[kept]["state"], tasks[kept]["live"]}, []any{"running", true})
	if sess := f.session("s1"); sess.Live || sess.EndedAt == nil || !f.session("s2").Live {
		t.Errorf("sessions are wrong: %+v", sess)
	}
	eq(t, tasks[gone]["agents"].([]any)[0].(obj)["state"], "done")

	// Throttled: the next look at tmux is seconds away.
	delete(f.tmux.known, "%2")
	f.get("/api/tasks")
	eq(t, f.session("s2").Live, true)
	// And your tmux answering nothing means it cannot be reached, not that every session died.
	f.srv.lastReconcile = 0
	f.get("/api/tasks")
	eq(t, f.session("s2").Live, true)
}

func TestASessionStuckAtAPromptAsksForYou(t *testing.T) {
	f := newFixture(t)
	id := f.newTask(obj{"start": true}).str("id")
	f.tmux.screen = "Do you trust the files in this folder?"
	f.get("/api/tasks")
	eq(t, f.task(id).State, "running") // still within its grace period
	f.st.Exec("UPDATE tasks SET updated_at = updated_at - 60")
	f.srv.lastReconcile = 0
	task := f.get("/api/tasks").data["tasks"].([]any)[0].(obj)
	eq(t, []any{task["state"], task["ask"], task["ask_kind"]}, []any{"needs", "Waiting at the folder trust prompt.", "dialog"})
	eq(t, f.history(id), []string{"backlog you", "running you", "needs board"})
}

func TestAgentsRunningAreListedAndFinishedOnesLinger(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.hook("PreToolUse", "s1", obj{"tool_name": "Agent", "tool_input": obj{"subagent_type": "Explore", "description": "look around"}})
	f.hook("SubagentStart", "s1", obj{"agent_id": "a1", "agent_type": "Explore"})
	f.hook("SubagentStart", "s1", obj{"agent_id": "a2", "agent_type": "Plan"})
	f.hook("SubagentStart", "s1", obj{"agent_id": "a3", "agent_type": "Old"})
	f.hook("SubagentStop", "s1", obj{"agent_id": "a2"})
	f.hook("SubagentStop", "s1", obj{"agent_id": "a3"})
	now := store.Now()
	f.st.Exec("UPDATE agents SET started_at = ?, ended_at = ? WHERE agent_id = 'a3'", now-5000, now-4000)
	f.st.Exec("UPDATE agents SET started_at = ? WHERE agent_id = 'a1'", now-3600)
	for _, task := range []obj{f.get("/api/tasks/" + id).data, f.get("/api/tasks").data["tasks"].([]any)[0].(obj)} {
		agents := task["agents"].([]any)
		if len(agents) != 2 {
			t.Fatalf("%d agents", len(agents))
		}
		first, second := agents[0].(obj), agents[1].(obj)
		eq(t, []any{first["name"], first["desc"], first["state"], first["tokens"]}, []any{"Explore", "look around", "running", nil})
		eq(t, []any{second["name"], second["desc"], second["state"]}, []any{"Plan", "", "done"})
		if age := first["age"].(float64); age < 3590 || age > 3700 || len(first) != 6 {
			t.Errorf("agent = %v", first)
		}
	}
}

func TestDetailHasHistorySessionsAndLog(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.write("s1.jsonl", `{"type": "user", "message": {"role": "user", "content": "hello"}}
{"type": "assistant", "message": {"role": "assistant", "content": [{"type": "text", "text": "Hi there."}, {"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}]}}
`)
	f.hook("Stop", "s1", obj{"last_assistant_message": "Hi there."})
	task := f.get("/api/tasks/" + id).data
	eq(t, task["log"], []any{
		obj{"who": "you", "text": "hello"},
		obj{"who": "agent", "text": "Hi there.", "html": "<p>Hi there.</p>\n"},
		obj{"who": "tool", "text": "$ ls"},
	})
	eq(t, f.history(id), []string{"running hook", "done hook"})
	eq(t, len(task["history"].([]any)), 2)
	listed := f.get("/api/tasks").data["tasks"].([]any)[0].(obj)
	for _, key := range []string{"log", "history", "sessions", "screen"} {
		if _, there := listed[key]; there {
			t.Errorf("the list carries %s", key)
		}
	}
}

func TestDetailLogReadsTheNewestSessionFirst(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.write("s1.jsonl", `{"type": "user", "message": {"content": "the prompt"}}`+"\n")
	var long strings.Builder
	for i := 0; i < logTurns+10; i++ {
		fmt.Fprintf(&long, `{"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Bash", "input": {"command": "step %d"}}]}}`+"\n", i)
	}
	if _, err := f.st.Exec("INSERT INTO sessions (session_id, task_id, transcript_path, started_at) VALUES ('s2', ?, ?, ?)",
		id, f.write("s2.jsonl", long.String()), store.Now()+60); err != nil {
		t.Fatal(err)
	}
	// The log is the end of the newest session, and the prompt it works on, from the session before.
	log := f.get("/api/tasks/" + id).data["log"].([]any)
	eq(t, len(log), logTurns+1)
	eq(t, log[0], obj{"who": "you", "text": "the prompt", "skipped": float64(10)})
	eq(t, []any{log[1], log[logTurns]}, []any{obj{"who": "tool", "text": "$ step 10"}, obj{"who": "tool", "text": "$ step 89"}})
}

func TestTranscriptsAreReadOutsideTheLock(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.write("s1.jsonl", `{"type": "user", "message": {"content": "hello"}}`+"\n")
	reads := 0
	f.srv.Log = func(paths []string, limit int) []transcript.Turn {
		// A long session takes time to read: a hook must get in meanwhile.
		if !f.srv.mu.TryLock() {
			t.Error("the board is locked while a transcript is read")
		} else {
			f.srv.mu.Unlock()
		}
		reads++
		return transcript.Log(paths, limit)
	}
	f.srv.Ask = func(launcher, model, prompt, cwd string) (string, error) { return "fine", nil }
	eq(t, len(f.get("/api/tasks/" + id).data["log"].([]any)), 1)
	f.ok(f.post("/api/tasks/"+id+"/ask", obj{"question": "where are we?"}))
	eq(t, reads, 2)
}

func TestLinksAndTags(t *testing.T) {
	f := newFixture(t)
	f.cfg.JiraURL = "https://jira.example.org/browse/"
	f.st.CreateTask(store.Task{ID: "t1", Title: "check PROJ-123 (MR !12)", Remote: "https://git.example.org/a/b", Profile: "team"}, "you")
	task := f.get("/api/tasks/t1").data
	eq(t, task["tags"], []any{"team", "jira", "mr"})
	eq(t, task["links"], []any{
		obj{"label": "PROJ-123", "url": "https://jira.example.org/browse/PROJ-123"},
		obj{"label": "!12", "url": "https://git.example.org/a/b/-/merge_requests/12"},
	})
}

// --- hook endpoint -------------------------------------------------------------------

func TestHookEndpoint(t *testing.T) {
	f := newFixture(t)
	for _, raw := range []string{"{nope", "not json at all", "\xff\xfe\x00", "[]", "42", `"x"`, "null"} {
		f.wantError(f.do("POST", "/api/hook", []byte(raw), "Content-Type", "application/json"), 400)
	}
	eq(t, f.count("events"), 0)
	// A payload the board ignores is still a good request.
	for _, payload := range []obj{
		{}, {"hook_event_name": "Stop"},
		{"hook_event_name": "UserPromptSubmit", "session_id": 12345, "prompt": "x"},
		{"hook_event_name": "UserPromptSubmit", "session_id": []any{"a"}, "prompt": "x"},
	} {
		got := f.ok(f.post("/api/hook", payload))
		if task, there := got.data["task"]; !there || task != nil {
			t.Errorf("%v gave %s", payload, got.raw)
		}
	}

	f.st.CreateTask(store.Task{ID: "planned-1", Title: "Planned"}, "you")
	got := f.hook("SessionStart", "s1", nil, "X-Tmux-Pane", "%7", "X-Config-Dir", "/Users/x/.claude-team", "X-HC-Task", "planned-1")
	sess := f.session("s1")
	eq(t, []string{got, sess.Pane, sess.ConfigDir, sess.TaskID}, []string{"planned-1", "%7", "/Users/x/.claude-team", "planned-1"})
	// An event without headers keeps what is known.
	f.hook("PreToolUse", "s1", obj{"tool_name": "Bash"}, "X-Tmux-Pane", "")
	eq(t, f.session("s1").Pane, "%7")
}

// --- parking -------------------------------------------------------------------------

func TestParkingClosesThePaneAndStartResumesIt(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	path := "/api/tasks/" + id
	f.hook("Stop", "s1", obj{"last_assistant_message": "All done."})
	out := f.ok(f.patch(path, obj{"state": "parked"}))
	eq(t, []any{out.data["state"], out.data["state_name"], out.data["live"], out.data["last"]},
		[]any{"parked", "Parked", false, "Parked by you"})
	eq(t, f.tmux.killed, []string{"%1|"})
	eq(t, f.session("s1").Live, false)
	// The SessionEnd of the killed process arrives late and must not move the task.
	f.hook("SessionEnd", "s1", nil)
	eq(t, f.task(id).State, "parked")

	out = f.ok(f.post(path+"/start", nil))
	eq(t, []any{out.data["state"], out.data["live"]}, []any{"running", true})
	eq(t, f.tmux.windows[0].Command, "claude-personal --resume s1")

	// Moving a parked task to running resumes it too.
	f.ok(f.patch(path, obj{"state": "parked"}))
	out = f.ok(f.patch(path, obj{"state": "running"}))
	eq(t, []any{out.data["state"], out.data["live"], len(f.tmux.windows)}, []any{"running", true, 2})

	// Parking a task without a terminal only moves it.
	killed := len(f.tmux.killed)
	other := f.newTask(nil).str("id")
	eq(t, f.ok(f.patch("/api/tasks/"+other, obj{"state": "parked"})).data["state"], "parked")
	eq(t, len(f.tmux.killed), killed)
}

// --- tokens, context, cost, limits ---------------------------------------------------

func usageLine(id string, in, out, read, write int) string {
	return fmt.Sprintf(`{"type": "assistant", "message": {"id": %q, "usage": {"input_tokens": %d, "output_tokens": %d,`+
		` "cache_read_input_tokens": %d, "cache_creation_input_tokens": %d}}}`+"\n", id, in, out, read, write)
}

func TestTokensOfATaskAndItsSubAgents(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.write("s1.jsonl", usageLine("m1", 10, 5, 100, 0)+usageLine("m1", 10, 9, 100, 0)+usageLine("m2", 1, 2, 0, 50))
	f.write("s1/subagents/agent-a1.jsonl", usageLine("x1", 1000, 200, 3000, 0))
	f.hook("SubagentStart", "s1", obj{"agent_id": "a1", "agent_type": "Explore"})
	task := f.get("/api/tasks/" + id).data
	eq(t, task["agent_tokens"], obj{"in": 1000.0, "out": 200.0, "cache_read": 3000.0, "cache_write": 0.0})
	eq(t, task["tokens"], obj{"in": 1011.0, "out": 211.0, "cache_read": 3100.0, "cache_write": 50.0})
	eq(t, task["agents"].([]any)[0].(obj)["tokens"], task["agent_tokens"])
}

func TestContextCostAndLimitsComeFromTheStatusLine(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	path := "/api/tasks/" + id
	f.write("s1.jsonl", usageLine("m1", 150_000, 1, 0, 0))
	eq(t, f.get(path).data["session_model"], "")
	eq(t, f.get(path).data["context"], obj{"tokens": 150_000.0, "window": 200_000.0, "pct": 75.0}) // of the configured window

	// More than fits: the session runs a larger window than configured.
	f.write("s1.jsonl", usageLine("m1", 150_000, 1, 0, 0)+usageLine("m2", 0, 1, 250_000, 0))
	eq(t, f.get(path).data["context"], obj{"tokens": 250_000.0, "window": 1_000_000.0, "pct": 25.0})

	status := obj{
		"session_id": "s1", "context_window": obj{"context_window_size": 500_000},
		"rate_limits": obj{"five_hour": obj{"used_percentage": 42.4, "resets_at": 9_999_999_999}},
	}
	f.ok(f.do("POST", "/api/status", status, "X-Config-Dir", "/Users/x/.claude-personal"))
	eq(t, f.get(path).data["context"], obj{"tokens": 250_000.0, "window": 500_000.0, "pct": 50.0})
	limits := f.get("/api/limits").data["limits"].([]any)
	entry := limits[0].(obj)
	eq(t, []any{len(limits), entry["profile"], entry["label"], entry["pct"], entry["limit_in"]}, []any{1, "personal", "5h", 42.0, nil})
	eq(t, f.get("/api/tasks").data["limits"], f.get("/api/limits").data["limits"])

	// The cost never goes down: a resumed session may start counting from zero again.
	eq(t, f.get(path).data["cost"], 0.0)
	for _, spent := range []float64{1.239, 4.5, 0.2} {
		f.ok(f.post("/api/status", obj{"session_id": "s1", "cost": obj{"total_cost_usd": spent}}))
	}
	eq(t, f.get(path).data["cost"], 4.5)

	// The effort level it reports is the session's. Without one, or with one claude does not take, the
	// session's model has none.
	eq(t, f.get(path).data["session_effort"], "")
	f.ok(f.post("/api/status", obj{"session_id": "s1", "effort": obj{"level": "high"}}))
	eq(t, f.get(path).data["session_effort"], "high")
	f.ok(f.post("/api/status", obj{"session_id": "s1", "effort": obj{"level": "turbo"}}))
	eq(t, f.get(path).data["session_effort"], "")
	f.ok(f.post("/api/status", obj{"session_id": "s1", "effort": obj{"level": "max"}}))
	f.ok(f.post("/api/status", obj{"session_id": "s1"}))
	eq(t, f.get(path).data["session_effort"], "")

	// A status line with nothing to say, or with nonsense, is still welcome.
	for _, payload := range []obj{{}, {"session_id": 4, "context_window": "x", "cost": []any{}, "rate_limits": "junk", "effort": "x"}} {
		eq(t, f.ok(f.post("/api/status", payload)).data, obj{})
	}
}

// --- ask -----------------------------------------------------------------------------

func TestAsk(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	f.write("s1.jsonl", `{"type": "user", "message": {"content": "fix the flaky test"}}
{"type": "assistant", "message": {"content": [{"type": "text", "text": "The sleep was too short. Fixed."}]}}
`)
	var seen []string
	f.srv.Ask = func(launcher, model, prompt, cwd string) (string, error) {
		seen = []string{launcher, model, cwd}
		if !strings.Contains(prompt, "USER: fix the flaky test") || !strings.Contains(prompt, "AGENT: The sleep was too short. Fixed.") ||
			!strings.HasSuffix(prompt, "Question: where are we?") {
			t.Errorf("prompt = %s", prompt)
		}
		return "The flaky test is fixed.", nil
	}
	before := *f.task(id)
	out := f.ok(f.post("/api/tasks/"+id+"/ask", obj{"question": "where are we?"}))
	eq(t, out.data, obj{"answer": "The flaky test is fixed.", "question": "where are we?"})
	eq(t, seen, []string{"claude-personal", "haiku", filepath.Join(f.tmp, "state")})
	// Nothing typed, nothing moved.
	eq(t, len(f.tmux.sent), 0)
	eq(t, *f.task(id), before)

	f.srv.Ask = func(launcher, model, prompt, cwd string) (string, error) {
		return "", errors.New("Claude Code could not answer: not logged in")
	}
	failed := f.wantError(f.post("/api/tasks/"+id+"/ask", obj{}), 502)
	eq(t, failed.str("error"), "Claude Code could not answer: not logged in")
	empty := f.newTask(nil).str("id")
	f.wantError(f.post("/api/tasks/"+empty+"/ask", obj{}), 409)
}

// --- the board's own tmux server -----------------------------------------------------

func TestStartOpensASessionInTheBoardServer(t *testing.T) {
	f := newPrivate(t)
	f.tmux.available = false // your own tmux is not even running
	task := f.newTask(obj{"start": true})
	id := task.str("id")
	socket := filepath.Join(f.tmp, "state", "tmux.sock")
	eq(t, len(f.tmux.windows), 0)
	eq(t, len(f.tmux.sessions), 1)
	sess := f.tmux.sessions[0]
	eq(t, []string{sess.Socket, sess.Conf, sess.Name}, []string{socket, filepath.Join(f.tmp, "state", "tmux.conf"), id[:8] + "-do-the-thing"})
	eq(t, sess.Env, []string{"HC_TASK=" + id, "HC_URL=http://127.0.0.1:7337", "HC_HANDOFF=" + f.cfg.HandoffPath(id)})
	eq(t, []any{task.data["live"], task.data["state"]}, []any{true, "running"})
	var attach []any
	for _, word := range tmux.AttachCommand("%0", socket) {
		attach = append(attach, word)
	}
	eq(t, task.data["attach"], attach)
	eq(t, f.session(id).Socket, socket)
	eq(t, f.get("/api/meta").data["private_tmux"], true)

	// Reply and park go to the board's server.
	f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": "go on"}))
	eq(t, f.tmux.sent, []string{"%0|go on|" + socket})
	// Focus cannot switch your terminal there: it points to hc attach.
	refused := f.wantError(f.post("/api/tasks/"+id+"/focus", nil), 409)
	if !strings.Contains(refused.str("error"), "hc attach "+id[:8]) || len(f.tmux.focused) != 0 {
		t.Errorf("focus gave %s", refused.raw)
	}
	f.ok(f.patch("/api/tasks/"+id, obj{"state": "parked"}))
	eq(t, f.tmux.killed, []string{"%0|" + socket})
}

func TestPrivateTmuxNeedsTmuxInstalled(t *testing.T) {
	f := newPrivate(t)
	f.tmux.installed = false
	f.wantError(f.post("/api/tasks", obj{"prompt": "x", "cwd": f.tmp, "start": true}), 503)
}

func TestSamePaneIDInAnotherServerIsNotThisSession(t *testing.T) {
	f := newPrivate(t)
	id := f.newTask(obj{"start": true}).str("id")
	f.tmux.known["%0"] = true // your tmux happens to have a pane with the same id
	delete(f.tmux.privateKnown, "%0")
	eq(t, f.get("/api/tasks/" + id).data["live"], false)
	// The board's server exits with its last session: its sessions are closed.
	f.get("/api/tasks")
	eq(t, f.session(id).Live, false)
}

func TestAdoptMovesASessionIntoTheBoardServer(t *testing.T) {
	f := newPrivate(t)
	f.tmux.known["%1"] = true
	if err := os.WriteFile(filepath.Join(f.tmp, "s1.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	yours := filepath.Join(f.tmp, "tmux-501", "default")
	id := f.hook("UserPromptSubmit", "s1", obj{"prompt": "hi", "transcript_path": filepath.Join(f.tmp, "s1.jsonl")},
		"X-Tmux-Pane", "%1", "X-Tmux-Socket", yours, "X-Config-Dir", "/x/.claude-team")
	// A session you started yourself keeps its own server.
	task := f.get("/api/tasks/" + id).data
	eq(t, []any{task["live"], task["attach"]}, []any{true, []any{}})
	f.ok(f.post("/api/tasks/"+id+"/focus", nil))
	eq(t, f.tmux.focused, []string{"%1"})

	out := f.ok(f.post("/api/tasks/"+id+"/adopt", nil))
	eq(t, f.tmux.killed, []string{"%1|" + yours})
	eq(t, f.tmux.sessions[0].Command, "claude-team --resume s1")
	if out.data["live"] != true || len(out.data["attach"].([]any)) == 0 {
		t.Errorf("adopt gave %s", out.raw)
	}
	eq(t, f.session("s1").Socket, f.cfg.TmuxSocket())
	f.wantError(f.post("/api/tasks/"+id+"/adopt", nil), 409) // already there

	fresh := f.newTask(nil).str("id")
	f.wantError(f.post("/api/tasks/"+fresh+"/adopt", nil), 409) // nothing to move yet
	f.wantError(newFixture(t).post("/api/tasks/x/adopt", nil), 404)
}
