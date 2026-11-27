package server

import (
	"errors"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/tmux"
)

// inHerdr makes a running task whose session s1 runs in Herdr pane w1:p2 in /w/repo, and returns its id and
// the socket.
func (f *fixture) inHerdr() (string, string) {
	f.t.Helper()
	socket := filepath.Join(f.tmp, "herdr.sock")
	f.herdr.agents[socket] = map[string]herdr.Agent{"w1:p2": {Session: "s1", Cwd: "/w/repo"}}
	id := f.hook("UserPromptSubmit", "s1", obj{"prompt": "hello", "cwd": "/w/repo", "transcript_path": f.write("s1.jsonl", "")},
		"X-Herdr-Pane", "w1:p2", "X-Herdr-Socket", socket, "X-Config-Dir", "/Users/x/.claude-personal")
	return id, socket
}

func TestHookStoresTheHerdrPane(t *testing.T) {
	f := newFixture(t)
	_, socket := f.inHerdr()
	sess := f.session("s1")
	eq(t, []string{sess.Pane, sess.HerdrPane, sess.HerdrSocket}, []string{"", "w1:p2", socket})
	// An event without headers keeps what is known, and a pane without its socket is no pane.
	f.hook("PreToolUse", "s1", obj{"tool_name": "Bash"}, "X-Herdr-Pane", "")
	f.hook("PreToolUse", "s2", obj{"tool_name": "Bash"}, "X-Herdr-Pane", "w1:p3")
	eq(t, []string{f.session("s1").HerdrPane, f.session("s2").HerdrPane}, []string{"w1:p2", ""})
}

func TestHookInTmuxIgnoresTheHerdrPaneTmuxWasStartedFrom(t *testing.T) {
	f := newPrivate(t)
	socket := filepath.Join(f.tmp, "herdr.sock")
	f.herdr.agents[socket] = map[string]herdr.Agent{"w1:p2": {Cwd: "/w/repo"}}
	f.hook("SessionStart", "s1", obj{"cwd": "/w/repo"}, "X-Tmux-Pane", "%0", "X-Tmux-Socket", f.cfg.TmuxSocket(),
		"X-Herdr-Pane", "w1:p2", "X-Herdr-Socket", socket)
	sess := f.session("s1")
	eq(t, []string{sess.Pane, sess.HerdrPane, sess.HerdrSocket}, []string{"%0", "", ""})
}

func TestReplyGoesToTheHerdrPane(t *testing.T) {
	f := newFixture(t)
	id, socket := f.inHerdr()
	f.hook("Stop", "s1", obj{"last_assistant_message": "All done."})
	task := f.get("/api/tasks/" + id).data
	eq(t, []any{task["live"], task["tmux"], task["pane"]}, []any{true, "herdr", "w1:p2"})

	out := f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": "go on"}))
	eq(t, f.herdr.prompted, []string{"w1:p2|go on|" + socket})
	eq(t, out.str("last"), "You: go on")
	eq(t, []int{len(f.tmux.sent), len(f.tmux.windows), len(f.tmux.sessions)}, []int{0, 0, 0})

	f.herdr.fail = "agent w1:p2 is blocked"
	f.wantError(f.post("/api/tasks/"+id+"/reply", obj{"text": "again"}), 502)
	f.herdr.fail = ""
	// A dialog takes its keys: Herdr refuses a prompt while one shows.
	f.hook("Notification", "s1", obj{"notification_type": "permission_prompt", "message": "Claude needs your permission to use Bash"})
	f.wantError(f.post("/api/tasks/"+id+"/reply", obj{"text": "yes"}), 409)
	f.herdr.screen = "Do you want to proceed?\n❯ 1. Yes\n"
	out = f.ok(f.post("/api/tasks/"+id+"/keys", obj{"keys": []any{"Down", "Enter"}}))
	eq(t, f.herdr.pressed, []string{"w1:p2|Down Enter|" + socket})
	eq(t, out.str("screen"), "Do you want to proceed?\n❯ 1. Yes")
	eq(t, len(f.tmux.pressed), 0)

	// A pane moved to another workspace has a new id: the session id finds it.
	f.hook("PostToolUse", "s1", obj{"tool_name": "Bash"})
	f.herdr.agents[socket] = map[string]herdr.Agent{"w2:p1": {Session: "s1"}}
	f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": "moved"}))
	eq(t, f.herdr.prompted[1], "w2:p1|moved|"+socket)
}

func TestNoResumeWhileHerdrRunsTheSession(t *testing.T) {
	// Without its Claude Code integration, Herdr knows the agent in a pane but not its session id.
	for name, sid := range map[string]string{"with its session id": "s1", "without": ""} {
		t.Run(name, func(t *testing.T) {
			f := newPrivate(t)
			id, socket := f.inHerdr()
			f.herdr.agents[socket]["w1:p2"] = herdr.Agent{Session: sid, Cwd: "/w/repo"}
			f.hook("Stop", "s1", obj{"last_assistant_message": "All done."})
			path := "/api/tasks/" + id
			f.wantError(f.post(path+"/start", nil), 409)
			f.wantError(f.post(path+"/adopt", nil), 409)
			// Going to its terminal brings its pane to the front in Herdr.
			f.ok(f.post(path+"/focus", nil))
			eq(t, f.herdr.focused, []string{"w1:p2|" + socket})

			// Parking leaves the Herdr pane alone, and Herdr still runs the session: a reply goes there.
			f.ok(f.patch(path, obj{"state": "parked"}))
			f.ok(f.patch(path, obj{"state": "running"}))
			f.ok(f.post(path+"/reply", obj{"text": "still there?"}))
			eq(t, f.herdr.prompted, []string{"w1:p2|still there?|" + socket})
			eq(t, []int{len(f.tmux.killed), len(f.tmux.windows), len(f.tmux.sessions)}, []int{0, 0, 0})
		})
	}
}

func TestReplyResumesWhenHerdrNoLongerRunsTheSession(t *testing.T) {
	for _, c := range []struct {
		name   string
		agents map[string]herdr.Agent
		newer  bool // a newer session started in the pane
	}{
		{"herdr gone", nil, false},
		{"another session there", map[string]herdr.Agent{"w1:p2": {Session: "s9", Cwd: "/w/repo"}}, false},
		{"a newer session there, no session id", map[string]herdr.Agent{"w1:p2": {Cwd: "/w/repo"}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPrivate(t)
			id, socket := f.inHerdr()
			f.hook("Stop", "s1", obj{"last_assistant_message": "All done."})
			f.herdr.agents[socket] = c.agents
			if c.newer {
				f.hook("SessionStart", "s2", nil, "X-Herdr-Pane", "w1:p2", "X-Herdr-Socket", socket)
			}
			eq(t, f.get("/api/tasks/" + id).data["live"], false)
			f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": "back"}))
			eq(t, len(f.herdr.prompted), 0)
			eq(t, shellWords(t, f.tmux.sessions[0].Command), []string{"claude-personal", "--resume", "s1", "--", "back"})
		})
	}
}

func TestAMovedPaneWithoutSessionIdIsFoundByItsDirectory(t *testing.T) {
	f := newPrivate(t)
	id, socket := f.inHerdr()
	f.hook("Stop", "s1", obj{"last_assistant_message": "All done."})
	// The pane moved from w1:p2 to w2:p2, and Herdr knows no session id. Pane w2:p3 holds s2, in the same
	// directory and just as unknown to Herdr.
	f.hook("SessionStart", "s2", obj{"cwd": "/w/repo"}, "X-Herdr-Pane", "w2:p3", "X-Herdr-Socket", socket)
	f.herdr.agents[socket] = map[string]herdr.Agent{
		"w2:p2": {Cwd: "/w/repo"}, "w2:p3": {Cwd: "/w/repo"}, "w2:p4": {Cwd: "/w/other"}}
	task := f.get("/api/tasks/" + id).data
	eq(t, []any{task["live"], task["pane"]}, []any{true, "w2:p2"})
	f.ok(f.post("/api/tasks/"+id+"/reply", obj{"text": "moved"}))
	eq(t, f.herdr.prompted, []string{"w2:p2|moved|" + socket})
	eq(t, len(f.tmux.sessions), 0)

	// Two unknown agents in its directory: either may be it, so nothing reaches it and nothing resumes it.
	f.herdr.agents[socket]["w2:p5"] = herdr.Agent{Cwd: "/w/repo"}
	path := "/api/tasks/" + id
	eq(t, f.get(path).data["live"], false)
	f.wantError(f.post(path+"/reply", obj{"text": "which?"}), 409)
	f.wantError(f.post(path+"/start", nil), 409)
	f.wantError(f.post(path+"/adopt", nil), 409)
	f.ok(f.patch(path, obj{"state": "parked"}))
	f.wantError(f.patch(path, obj{"state": "running"}), 409)
	eq(t, []int{len(f.herdr.prompted), len(f.tmux.sessions)}, []int{1, 0})
}

func TestAttachGoesThroughHerdrUnlessTmuxHasTheSession(t *testing.T) {
	f := newPrivate(t)
	id, socket := f.inHerdr()
	task := f.get("/api/tasks/" + id).data
	eq(t, task["attach"], []any{"env", "HERDR_SOCKET_PATH=" + socket, "herdr", "agent", "attach", "w1:p2"})

	// A live tmux pane wins: that is tmux inside Herdr.
	f.tmux.privateKnown["%0"] = true
	f.hook("UserPromptSubmit", "s1", obj{"prompt": "again"}, "X-Tmux-Pane", "%0", "X-Tmux-Socket", f.cfg.TmuxSocket())
	task = f.get("/api/tasks/" + id).data
	want := []any{}
	for _, word := range tmux.AttachCommand("%0", f.cfg.TmuxSocket()) {
		want = append(want, word)
	}
	eq(t, []any{task["tmux"], task["attach"]}, []any{"hc", want})
}

func TestAttachFromThePhoneToHerdrLeavesOnCtrlBracket(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w9:p9") // the daemon's own pane, when it runs in one
	f := newPrivate(t)
	id, socket := f.inHerdr()
	a := &attached{t: t, f: f, id: id, commands: make(chan []string, 1),
		want: herdr.AttachCommand("w1:p2", socket)}
	f.srv.AttachProcess = func(command []string) *exec.Cmd {
		a.commands <- command
		// A fake herdr agent attach: Herdr leaves quietly when its client is hung up.
		return exec.Command("/bin/sh", "-c",
			`trap 'exit 0' HUP; echo "pane=[$HERDR_PANE_ID]"; while read -r line; do echo "got:$line"; done`)
	}
	a.srv = httptest.NewServer(f.h)
	t.Cleanup(a.srv.Close)
	if err := a.dial("", ""); err != nil {
		t.Fatal(err)
	}
	a.mustSee("pane=[]")
	a.send("hello\n")
	a.mustSee("got:hello")
	a.send("bye\x1d")
	err := a.waitFor("never shown")
	eq(t, websocket.CloseStatus(err), websocket.StatusNormalClosure)
	var closed websocket.CloseError
	if errors.As(err, &closed) {
		eq(t, closed.Reason, "Detached")
	}
}
