package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/install"
	"github.com/rouxlabs/headcoach/internal/server"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
)

// fakeTmux is a tmux where every pane it was told about exists, and that remembers what was typed.
type fakeTmux struct {
	panes map[string]bool
	sent  []string
}

func (f *fakeTmux) Installed() bool                                     { return true }
func (f *fakeTmux) Available() bool                                     { return true }
func (f *fakeTmux) Panes(socket string) map[string]bool                 { return f.panes }
func (f *fakeTmux) RenameWindow(pane, name, socket string)              {}
func (f *fakeTmux) Capture(pane, socket string) string                  { return "the screen\n" }
func (f *fakeTmux) CaptureStyled(pane, socket string) string            { return "the screen\n" }
func (f *fakeTmux) KillPane(pane, socket string)                        { delete(f.panes, pane) }
func (f *fakeTmux) SendKeys(pane string, keys []string, s string) error { return tmux.CheckKeys(keys) }
func (f *fakeTmux) TypeText(pane, text, socket string) error            { return nil }
func (f *fakeTmux) Focus(pane, socket string) error                     { return nil }
func (f *fakeTmux) NewWindow(w tmux.Window) (string, error)             { return f.open() }
func (f *fakeTmux) NewSession(w tmux.Window) (string, error)            { return f.open() }

func (f *fakeTmux) SendText(pane, text, socket string) error {
	f.sent = append(f.sent, pane+"|"+text)
	return nil
}

func (f *fakeTmux) open() (string, error) {
	pane := fmt.Sprintf("%%%d", 100+len(f.panes))
	f.panes[pane] = true
	return pane, nil
}

type fixture struct {
	t    *testing.T
	tmp  string
	url  string
	cfg  *config.Config
	st   *store.Store
	tmux *fakeTmux
	srv  *server.Server
}

// newFixture starts a daemon on a local port, away from the real config, state and Claude Code.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("HC_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("HC_DATA_DIR", filepath.Join(tmp, "data"))
	t.Setenv("HC_CONFIG", filepath.Join(tmp, "config.toml"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	for _, name := range []string{"HC_URL", "HC_PORT", "HC_DB", "HC_TASK", "CLAUDE_CONFIG_DIR", "NO_COLOR",
		"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(name, "")
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Profiles = []config.Profile{{Name: "team", Command: "claude-team"}}
	f := &fixture{t: t, tmp: tmp, cfg: cfg, st: st, tmux: &fakeTmux{panes: map[string]bool{"%1": true}}}
	f.srv = server.New(cfg, st, f.tmux)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(func() { web.Close(); st.Close() })
	f.url = web.URL
	return f
}

// hc runs one command line against the daemon and returns its exit status and what it printed.
func (f *fixture) hc(args ...string) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"--url", f.url}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ok runs a command that must succeed and returns what it printed.
func (f *fixture) ok(args ...string) string {
	f.t.Helper()
	code, out, errOut := f.hc(args...)
	if code != 0 {
		f.t.Fatalf("hc %v exited %d: %s", args, code, errOut)
	}
	return out
}

// hook sends one hook event the way hc-hook.sh would.
func (f *fixture) hook(name, sid, pane string, body map[string]any) {
	f.t.Helper()
	payload := map[string]any{"hook_event_name": name, "session_id": sid}
	for k, v := range body {
		payload[k] = v
	}
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", f.url+"/api/hook", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if pane != "" {
		req.Header.Set("X-Tmux-Pane", pane)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		f.t.Fatalf("hook %s: %v %v", name, resp, err)
	}
	resp.Body.Close()
}

func TestParse(t *testing.T) {
	flags := []flag{{name: "all", short: "a"}, {name: "sort", short: "s", value: true}, {name: "dir", short: "c", value: true, multi: true}}
	p, err := parse([]string{"abc", "-a", "--sort=cost", "x", "-c", "one", "-s", "ctx", "--dir", "two", "--", "-n", "--all"}, flags)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.args, []string{"abc", "x", "-n", "--all"}) {
		t.Errorf("args = %v", p.args)
	}
	if !p.has("all") || p.get("sort", "") != "ctx" || !reflect.DeepEqual(p.flags["dir"], []string{"one", "two"}) || p.get("nope", "d") != "d" {
		t.Errorf("flags = %v", p.flags)
	}
	for _, bad := range [][]string{{"--nope"}, {"-s"}, {"--all=yes"}} {
		if _, err := parse(bad, flags); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestLs(t *testing.T) {
	f := newFixture(t)
	if out := f.ok("ls"); !strings.Contains(out, "No open tasks") {
		t.Errorf("empty board printed %q", out)
	}
	f.hook("UserPromptSubmit", "done0001", "", map[string]any{"prompt": "title done0001"})
	transcript := filepath.Join(f.tmp, "run00001.jsonl")
	os.WriteFile(transcript, []byte(`{"type": "assistant", "message": {"id": "m1", "model": "claude-opus-4-5-20251101", "usage": {"input_tokens": 1}}}`+"\n"), 0o644)
	f.hook("UserPromptSubmit", "run00001", "%1", map[string]any{"prompt": "/developer title run00001", "cwd": f.tmp, "transcript_path": transcript})
	f.hook("SubagentStart", "run00001", "%1", map[string]any{"agent_id": "a1", "agent_type": "Explore"})
	f.hook("PreToolUse", "run00001", "%1", map[string]any{"agent_id": "a1", "tool_name": "Skill", "tool_input": map[string]any{"skill": "reviewer"}})
	f.hook("UserPromptSubmit", "need0001", "", map[string]any{"prompt": "title need0001"})
	f.hook("Stop", "need0001", "", map[string]any{"last_assistant_message": "Shall I?"})
	f.ok("move", "done0001", "done")

	lines := strings.Split(strings.TrimRight(f.ok("ls"), "\n"), "\n")
	want := []string{"TASK", "STATE", "PROFILE", "MODEL", "EFFORT", "PERSONA", "REPO", "TMUX", "AGE", "IDLE", "CTX", "IN", "OUT", "CACHE", "COST", "⏱", "↻", "TITLE"}
	if !reflect.DeepEqual(strings.Fields(lines[0]), want) || len(lines) != 5 { // done is hidden without --all
		t.Fatalf("ls printed %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[1], "need0001  Needs me  team     -      -      -  ") || !strings.HasSuffix(lines[1], "title need0001 (closed)") {
		t.Errorf("needs line = %q", lines[1])
	}
	if strings.TrimSpace(lines[2]) != "? Shall I?" || strings.Index(lines[2], "?") != 158 {
		t.Errorf("question line = %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "run00001  Running   team     opus   -      developer ") || !regexp.MustCompile(` - -  +/developer title run00001$`).MatchString(lines[3]) ||
		!strings.Contains(lines[3], " "+filepath.Base(f.tmp)+" ") {
		t.Errorf("running line = %q", lines[3])
	}
	if !strings.HasSuffix(lines[4], "└─ Explore: ") || !strings.Contains(lines[4], " Running                          reviewer ") {
		t.Errorf("agent line = %q", lines[4])
	}
	// The title starts in the same column on every line, counted in runes: ⏱ and ↻ take 3 bytes.
	at := func(line, s string) int { return utf8.RuneCountInString(line[:strings.Index(line, s)]) }
	if at(lines[0], "TITLE") != at(lines[3], "/developer") || at(lines[4], "└─") != at(lines[0], "TITLE") {
		t.Errorf("columns are not aligned:\n%s", strings.Join(lines, "\n"))
	}

	if out := f.ok("ls", "--all"); !strings.Contains(out, "done0001  Done") {
		t.Errorf("ls --all = %s", out)
	}
	var listed []api.Task
	if err := json.Unmarshal([]byte(f.ok("ls", "--json")), &listed); err != nil || len(listed) != 2 {
		t.Errorf("ls --json gave %d tasks, %v", len(listed), err)
	}
	if code, _, errOut := f.hc("ls", "--sort", "size"); code != 2 || !strings.Contains(errOut, "cannot sort by 'size'") {
		t.Errorf("a bad sort gave %d %q", code, errOut)
	}

	// A pinned task goes on top with its sub-agents, its title marked.
	if out := f.ok("pin", "run00001"); out != "run00001  pinned to the top  /developer title run00001\n" {
		t.Errorf("pin printed %q", out)
	}
	lines = strings.Split(strings.TrimRight(f.ok("ls"), "\n"), "\n")
	if !strings.HasPrefix(lines[1], "run00001") || !strings.HasSuffix(lines[1], "  ▲ /developer title run00001") ||
		!strings.HasSuffix(lines[2], "└─ Explore: ") || !strings.HasPrefix(lines[3], "need0001") {
		t.Errorf("ls with a pin:\n%s", strings.Join(lines, "\n"))
	}
	if out := f.ok("unpin", "run00001"); out != "run00001  unpinned  /developer title run00001\n" {
		t.Errorf("unpin printed %q", out)
	}
	if lines = strings.Split(f.ok("ls"), "\n"); !strings.HasPrefix(lines[1], "need0001") {
		t.Errorf("ls after unpin:\n%s", strings.Join(lines, "\n"))
	}
}

// A reply an agent sends from its session reaches the other task with a label, and show names the sender.
func TestReplyFromAnotherTask(t *testing.T) {
	f := newFixture(t)
	f.tmux.panes["%2"] = true
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "fix it"})
	f.hook("UserPromptSubmit", "c0ffee00-s2", "%2", map[string]any{"prompt": "coordinate"})
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "c0ffee00-s2")
	f.ok("reply", "abcd", "stop", "there")
	if len(f.tmux.sent) != 1 || f.tmux.sent[0] != "%1|[from task c0ffee00] stop there" {
		t.Errorf("sent %q", f.tmux.sent)
	}
	transcript := filepath.Join(f.tmp, "s1.jsonl")
	os.WriteFile(transcript, []byte(`{"type": "user", "message": {"content": "fix it"}}
{"type": "user", "message": {"content": "[from task c0ffee00] stop there"}}
`), 0o644)
	f.hook("Stop", "abcd1234-s1", "%1", map[string]any{"transcript_path": transcript, "last_assistant_message": "Stopped."})
	if out := f.ok("show", "abcd"); !strings.Contains(out, "\nyou   | fix it\ntask  | [from task c0ffee00] stop there\n") {
		t.Errorf("show does not name the sender:\n%s", out)
	}
}

// hc new --parent, hc link and hc unlink put a task under another one, and hc ls prints the tree.
func TestLinks(t *testing.T) {
	f := newFixture(t)
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "coordinate"})
	out := f.ok("new", "-t", "Child", "--parent", "abcd", "do it")
	child := strings.Fields(out)[0]
	f.ok("new", "-t", "Other", "do that")
	ls := f.ok("ls")
	// The tree hangs in the TASK column: the parent says how many tasks it drives, its child branches off.
	if !strings.Contains(ls, "\nabcd1234 ▾ 1 Running ") || !strings.Contains(ls, "\n└─ "+child+"  Backlog ") ||
		!strings.Contains(ls, "  Child\n") || !strings.Contains(ls, "  Other\n") {
		t.Errorf("ls does not nest the child:\n%s", ls)
	}
	if i, j := strings.Index(ls, "abcd1234 ▾ 1"), strings.Index(ls, "└─ "+child); i < 0 || j < i {
		t.Errorf("the child is not under its parent:\n%s", ls)
	}
	if out := f.ok("show", child); !strings.Contains(out, "  parent   abcd1234\n") {
		t.Errorf("show lacks the parent:\n%s", out)
	}
	if out := f.ok("unlink", child); out != child+"  now at the top of the board\n" {
		t.Errorf("unlink printed %q", out)
	}
	if out := f.ok("link", child, "abcd"); out != child+"  now under abcd1234\n" {
		t.Errorf("link printed %q", out)
	}
	if code, _, errOut := f.hc("link", "abcd", child); code == 0 || !strings.Contains(errOut, "cannot be under itself") {
		t.Errorf("a loop: %d %s", code, errOut)
	}
	// From inside the parent's session, self names it.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcd1234-s1")
	out = f.ok("new", "-t", "Mine", "--parent", "self", "mine")
	var detail api.Task
	if err := json.Unmarshal([]byte(f.ok("show", "--json", strings.Fields(out)[0])), &detail); err != nil || detail.Parent != "abcd1234-s1" {
		t.Errorf("--parent self: %+v %v", detail.Parent, err)
	}
}

// hc inbox lists what a task heard from the tasks it drives, and what waits to be said.
func TestInbox(t *testing.T) {
	f := newFixture(t)
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "coordinate"})
	if out := f.ok("inbox", "abcd"); out != "Nothing from the tasks under abcd1234 yet.\n" {
		t.Errorf("empty inbox: %q", out)
	}
	child := strings.Fields(f.ok("new", "-t", "Child", "--parent", "abcd", "do it"))[0]
	f.ok("move", child, "review")
	out := f.ok("inbox", "abcd")
	if !strings.HasPrefix(out, "waiting ") || !strings.Contains(out, child+"  Child: is in Review: Moved by your user\n") {
		t.Errorf("inbox = %q", out)
	}
	if code, _, errOut := f.hc("inbox"); code == 0 || !strings.Contains(errOut, "self names the task") {
		t.Errorf("inbox outside a session: %d %s", code, errOut)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcd1234-s1")
	if got := f.ok("inbox"); got != out {
		t.Errorf("inbox from the parent's session = %q", got)
	}
}

// hc team shows the tasks under a task, where each stands, and what waits on you among them.
func TestTeam(t *testing.T) {
	f := newFixture(t)
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "coordinate"})
	if out := f.ok("team", "abcd"); !strings.Contains(out, "No task under it. Put one there with: hc new --parent abcd1234") {
		t.Errorf("an empty team: %q", out)
	}
	child := strings.Fields(f.ok("new", "-t", "Child", "--parent", "abcd", "do it"))[0]
	grandchild := strings.Fields(f.ok("new", "-t", "Grandchild", "--parent", child, "do that"))[0]
	f.ok("move", grandchild, "review")
	path := strings.TrimSpace(f.ok("handoff", "path", child))
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("## Status\nHalfway.\n## Waiting\n- on you: a go\n## Queue\n- [x] one\n- [ ] two\n"), 0o644)
	out := f.ok("team", "abcd")
	for _, piece := range []string{
		"abcd1234  Running  coordinate\n",
		"Waits on you\n  " + child + "  Child                           a go\n  " + grandchild + "  Grandchild                      in Review: Moved by you\n",
		"\n" + child + " ▾ 1  Backlog    ", "s  1 open, 1 done    Child                                     Halfway.\n",
		"\n└─ " + grandchild + "   Review     ", "s  -                 Grandchild                                Moved by you\n",
	} {
		if !strings.Contains(out, piece) {
			t.Errorf("team lacks %q:\n%s", piece, out)
		}
	}
	// From inside the coordinator's session, it is the default.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcd1234-s1")
	if got := f.ok("team"); got != out {
		t.Errorf("team from the session = %q", got)
	}
	var team api.Team
	if err := json.Unmarshal([]byte(f.ok("team", "--json")), &team); err != nil || len(team.Members) != 2 || team.Members[1].Depth != 1 {
		t.Errorf("--json: %+v %v", team, err)
	}
	// rm takes the tasks under it along, and says how many.
	if out := f.ok("rm", "abcd"); out != "deleted abcd1234 with 2 tasks under it\n" {
		t.Errorf("rm of a parent printed %q", out)
	}
	if out := f.ok("ls"); strings.Contains(out, "Child") {
		t.Errorf("a child outlived its parent:\n%s", out)
	}
}

// hc handoff prints a task's handoff, init writes the template, path says where it goes; show sums it up.
func TestHandoff(t *testing.T) {
	f := newFixture(t)
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "fix it"})
	path := strings.TrimSpace(f.ok("handoff", "path", "abcd"))
	if !strings.HasSuffix(path, "/handoff/abcd1234-s1.md") {
		t.Errorf("path = %q", path)
	}
	if out := f.ok("handoff", "abcd"); !strings.HasPrefix(out, "No handoff yet. Write one at "+path) {
		t.Errorf("no handoff: %q", out)
	}
	if out := f.ok("handoff", "init", "abcd"); out != path+"\n" {
		t.Errorf("init printed %q", out)
	}
	os.WriteFile(path, []byte("# Handoff\n## Status\nHalfway.\n## Waiting\n- on you: a go\n"), 0o644)
	if out := f.ok("handoff", "abcd"); out != "# Handoff\n## Status\nHalfway.\n## Waiting\n- on you: a go\n" {
		t.Errorf("handoff = %q", out)
	}
	out := f.ok("show", "abcd")
	for _, piece := range []string{"\n  handoff  " + path + ", written ", "\n    status   Halfway.\n    waits    on you: a go\n"} {
		if !strings.Contains(out, piece) {
			t.Errorf("show lacks %q:\n%s", piece, out)
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcd1234-s1")
	if got := strings.TrimSpace(f.ok("handoff", "path")); got != path {
		t.Errorf("path from the task's session = %q", got)
	}
	// A reset between two turns needs the prompt on screen; the stand-in tmux shows none.
	f.hook("Stop", "abcd1234-s1", "%1", map[string]any{"last_assistant_message": "Done."})
	if code, _, errOut := f.hc("reset", "abcd"); code == 0 || !strings.Contains(errOut, "prompt is not empty") {
		t.Errorf("reset: %d %s", code, errOut)
	}
}

func TestShow(t *testing.T) {
	f := newFixture(t)
	transcript := filepath.Join(f.tmp, "s1.jsonl")
	os.WriteFile(transcript, []byte(`{"type": "user", "message": {"content": "fix it\nplease"}}
{"type": "assistant", "message": {"id": "m1", "usage": {"input_tokens": 1500, "output_tokens": 20}, "content": [{"type": "text", "text": "Done."}, {"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}]}}
`), 0o644)
	f.hook("UserPromptSubmit", "abcd1234-s1", "%1", map[string]any{"prompt": "fix it", "transcript_path": transcript})
	f.hook("Stop", "abcd1234-s1", "%1", map[string]any{"last_assistant_message": "Ship it?"})
	out := f.ok("show", "abcd", "-n", "2")
	for _, piece := range []string{
		"fix it\n\n  task     abcd1234-s1\n  state    Needs me\n", "  profile  team\n  model    default\n  effort   default\n  persona  -\n  resume   claude-team --resume abcd1234-s1\n",
		"  context  1.5k of 200k (1%)\n", "  tokens   in 1.5k  out 20  cache read 0  cache write 0\n", "\n  needs you: Ship it?\n",
		"\nyou   | fix it\n      | please\nagent | Done.\ntool  | $ ls\n",
	} {
		if !strings.Contains(out, piece) {
			t.Errorf("show lacks %q:\n%s", piece, out)
		}
	}
	// Your last prompt stays, with what is left out between it and the turns asked for.
	if out := f.ok("show", "abcd", "-n", "1"); !strings.Contains(out, "\nyou   | fix it\n      | please\n      | … 1 turn …\ntool  | $ ls\n") {
		t.Errorf("-n 1 does not keep the prompt:\n%s", out)
	}
	if out := f.ok("show", "abcd", "-n", "0"); strings.Contains(out, " | ") {
		t.Errorf("-n 0 printed turns:\n%s", out)
	}
	if out := f.ok("show", "abcd", "--turns=9"); !strings.Contains(out, "you   | fix it\n      | please\n") {
		t.Errorf("a turn of several lines is not indented:\n%s", out)
	}
	var detail api.Task
	if err := json.Unmarshal([]byte(f.ok("show", "--json", "abcd")), &detail); err != nil || detail.ID != "abcd1234-s1" || len(detail.Log) != 3 {
		t.Errorf("show --json = %+v, %v", detail, err)
	}
	if code, _, errOut := f.hc("show", "zzzz"); code != 1 || errOut != "hc: No task matches 'zzzz'\n" {
		t.Errorf("an unknown task gave %d %q", code, errOut)
	}
}

func TestNewDir(t *testing.T) {
	f := newFixture(t)
	workdir := filepath.Join(f.tmp, "code")
	if err := os.WriteFile(os.Getenv("HC_CONFIG"), []byte("workdir = \""+workdir+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	answer := false
	saved := confirmCreate
	confirmCreate = func(e *env, dir string) bool { return answer }
	t.Cleanup(func() { confirmCreate = saved })

	// A missing folder is refused when the prompt says no.
	if code, _, errOut := f.hc("new", "-C", "myidea", "do it"); code != 1 || !strings.Contains(errOut, "does not exist") {
		t.Errorf("refused create gave %d %q", code, errOut)
	}
	// A bare name goes under workdir and the folder is created on yes.
	answer = true
	out := f.ok("new", "-C", "myidea", "do it")
	short := strings.Fields(out)[0]
	want := filepath.Join(workdir, "myidea")
	if task, _ := f.st.Resolve(short); task == nil || task.Cwd != want {
		t.Errorf("task = %+v, want cwd %s", task, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("%s was not created: %v", want, err)
	}
	// An existing folder asks nothing, even when the prompt would say no.
	answer = false
	out = f.ok("new", "-C", "myidea", "again")
	if task, _ := f.st.Resolve(strings.Fields(out)[0]); task == nil || task.Cwd != want {
		t.Errorf("existing folder: task = %+v", task)
	}
	// A path to a file is refused.
	file := filepath.Join(f.tmp, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.hc("new", "-C", file, "do it"); code != 1 || !strings.Contains(errOut, "not a directory") {
		t.Errorf("file dir gave %d %q", code, errOut)
	}
}

func TestSchedule(t *testing.T) {
	f := newFixture(t)
	if out := f.ok("schedule"); !strings.Contains(out, "No schedules") {
		t.Errorf("empty list = %q", out)
	}
	if code, _, _ := f.hc("schedule", "add", "summarise the commits"); code != 2 {
		t.Error("add without --at was accepted")
	}
	out := f.ok("schedule", "add", "--at", "8:00", "--days", "weekdays", "--catch-up", "-t", "Standup", "-C", f.tmp, "summarise", "the", "commits")
	short := strings.Fields(out)[0]
	if !strings.Contains(out, "Standup  next run ") {
		t.Errorf("add printed %q", out)
	}
	list := f.ok("schedule", "ls")
	if !strings.Contains(list, short+"  Standup") || !strings.Contains(list, "08:00 weekdays") || !strings.Contains(list, "yes      -     -      on") {
		t.Errorf("ls =\n%s", list)
	}
	if out := f.ok("schedule", "pause", short); out != short+"  paused\n" {
		t.Errorf("pause printed %q", out)
	}
	if !strings.Contains(f.ok("schedule"), "paused") {
		t.Error("ls does not show the pause")
	}
	f.ok("schedule", "resume", short)
	if out := f.ok("schedule", "run", short); !strings.HasPrefix(out, short+"  started task ") {
		t.Errorf("run printed %q", out)
	}
	if !strings.Contains(f.ok("schedule"), "Running") {
		t.Error("ls does not show how the last run is going")
	}
	if code, _, errOut := f.hc("schedule", "frob"); code != 2 || !strings.Contains(errOut, "not one of") {
		t.Errorf("an unknown action gave %d %q", code, errOut)
	}
	if out := f.ok("schedule", "rm", short); !strings.HasPrefix(out, "deleted "+short) {
		t.Errorf("rm printed %q", out)
	}
	for _, bad := range [][]string{{"--at", "8:00", "--cron", "* * * * *", "x"}, {"--cron", "* * * * *", "--days", "weekdays", "x"},
		{"--at", "0 9 * * *", "--days", "weekdays", "x"}} {
		if code, _, _ := f.hc(append([]string{"schedule", "add"}, bad...)...); code != 2 {
			t.Errorf("add %v was accepted", bad)
		}
	}
	f.ok("schedule", "add", "--cron", "*/15 * * * *", "-t", "Triage", "sort", "new", "issues")
	if list := f.ok("schedule", "ls"); !strings.Contains(list, "Triage                   */15 * * * * -") {
		t.Errorf("ls =\n%s", list)
	}
}

func TestActions(t *testing.T) {
	f := newFixture(t)
	out := f.ok("new", "-t", "A title", "-C", f.tmp, "do", "the", "thing")
	short := strings.Fields(out)[0]
	if !strings.HasSuffix(out, "  Backlog  A title\n") || len(short) != 8 {
		t.Errorf("new printed %q", out)
	}
	if task, _ := f.st.Resolve(short); task == nil || task.Prompt != "do the thing" || task.Cwd != f.tmp {
		t.Errorf("task = %+v", task)
	}
	if out := f.ok("start", short); out != short+"  started in tmux pane %101\n" {
		t.Errorf("start printed %q", out)
	}
	if out := f.ok("reply", short, "hello", "there"); out != short+"  message delivered\n" || !reflect.DeepEqual(f.tmux.sent, []string{"%101|hello there"}) {
		t.Errorf("reply printed %q and sent %v", out, f.tmux.sent)
	}
	if out := f.ok("screen", short); out != "the screen\n" {
		t.Errorf("screen printed %q", out)
	}
	if out := f.ok("keys", short, "Down", "Enter"); out != "the screen\n" {
		t.Errorf("keys printed %q", out)
	}
	if code, _, errOut := f.hc("keys", short, "C-c"); code != 1 || !strings.Contains(errOut, "Keys must be chosen from") {
		t.Errorf("a refused key gave %d %q", code, errOut)
	}
	if out := f.ok("move", short, "done"); out != short+"  Done  A title\n" {
		t.Errorf("move printed %q", out)
	}
	if out := f.ok("move", short, "needs", "me"); out != short+"  Needs me  A title\n" { // the shown name works too, unquoted
		t.Errorf("move by name printed %q", out)
	}
	if out := f.ok("move", short, "DONE"); out != short+"  Done  A title\n" {
		t.Errorf("move in caps printed %q", out)
	}
	if code, _, errOut := f.hc("move", short, "archived"); code != 2 || !strings.Contains(errOut, "Known: Backlog, Running, Needs me") {
		t.Errorf("an unknown state gave %d %q", code, errOut)
	}
	if out := f.ok("rm", short); out != "deleted "+short+"\n" {
		t.Errorf("rm printed %q", out)
	}
	// A task started on the spot, with a prompt that looks like an option.
	if out := f.ok("new", "-s", "--", "--help", "me"); !strings.Contains(out, "  Running  --help me\n") {
		t.Errorf("new -s printed %q", out)
	}
	for _, bad := range [][]string{{"move", short}, {"show"}, {"rm", "a", "b"}, {"nope"}, {"ls", "--nope"}} {
		if code, _, errOut := f.hc(bad...); code != 2 || errOut == "" {
			t.Errorf("hc %v exited %d: %q", bad, code, errOut)
		}
	}
}

func TestAskAndOpen(t *testing.T) {
	f := newFixture(t)
	transcript := filepath.Join(f.tmp, "s1.jsonl")
	os.WriteFile(transcript, []byte(`{"type": "user", "message": {"content": "fix it"}}`+"\n"), 0o644)
	f.hook("UserPromptSubmit", "s1", "%1", map[string]any{"prompt": "fix it", "transcript_path": transcript})
	f.srv.Ask = func(launcher, model, prompt, cwd string) (string, error) {
		if !strings.HasSuffix(prompt, "Question: what is left?") {
			t.Errorf("prompt = %q", prompt)
		}
		return "All good.", nil
	}
	if out := f.ok("ask", "s1", "what", "is", "left?"); out != "All good.\n" {
		t.Errorf("ask printed %q", out)
	}
	var opened string
	old := openBrowser
	defer func() { openBrowser = old }()
	openBrowser = func(url string) { opened = url }
	if out := f.ok("open"); out != f.url+"\n" || opened != f.url {
		t.Errorf("open printed %q and opened %q", out, opened)
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	if out := f.ok("limits"); !strings.Contains(out, "No limits reported yet") {
		t.Errorf("limits printed %q", out)
	}
	// Half of the window used in its first hour: at that pace it runs out before it resets.
	status := fmt.Sprintf(`{"rate_limits": {"five_hour": {"used_percentage": 50, "resets_at": %d}}}`, time.Now().Unix()+4*3600)
	resp, err := http.Post(f.url+"/api/status", "application/json", strings.NewReader(status))
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	out := f.ok("limits")
	if !strings.HasPrefix(out, "team       5h  [##########          ]  50%  resets in 3h59") || !strings.Contains(out, "   ! team 5h limit in about ") {
		t.Errorf("limits printed %q", out)
	}
	f.hook("UserPromptSubmit", "s1", "", map[string]any{"prompt": "x"})
	if ls := f.ok("ls"); !strings.HasSuffix(ls, "\n\n"+out) {
		t.Errorf("ls does not end with the limits:\n%s", ls)
	}
}

func TestReview(t *testing.T) {
	f := newFixture(t)
	if out := f.ok("review"); !strings.HasPrefix(out, "Nothing waits on you in Review.\n\nLast 7 days: ") {
		t.Errorf("empty queue printed %q", out)
	}
	f.hook("UserPromptSubmit", "rev00001", "", map[string]any{"prompt": "title rev00001", "cwd": f.tmp})
	f.hook("Stop", "rev00001", "", map[string]any{"last_assistant_message": "Done."})
	f.ok("move", "rev00001", "Review") // a plain folder finishes in Done
	lines := strings.Split(f.ok("review"), "\n")
	want := []string{"TASK", "WAITED", "REPO", "BRANCH", "CHANGE", "TESTS", "DELIVERY", "REVIEWER", "TITLE"}
	if !reflect.DeepEqual(strings.Fields(lines[0]), want) || !strings.HasPrefix(lines[1], "rev00001 ") ||
		!strings.HasSuffix(lines[1], "  title rev00001") || !strings.HasPrefix(lines[3], "Last 7 days: ") {
		t.Errorf("review printed:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Index(lines[0], "TITLE") != strings.Index(lines[1], "title") {
		t.Errorf("columns are not aligned:\n%s", strings.Join(lines, "\n"))
	}
	var queue api.Review
	if err := json.Unmarshal([]byte(f.ok("review", "--json")), &queue); err != nil || len(queue.Items) != 1 {
		t.Errorf("review --json gave %+v, %v", queue, err)
	}
}

func TestAttach(t *testing.T) {
	f := newFixture(t)
	var ran [][]string
	old, oldAttach := execProgram, runAttach
	defer func() { execProgram, runAttach = old, oldAttach }()
	execProgram = func(argv, environ []string) error {
		for _, kv := range environ {
			if strings.HasPrefix(kv, "TMUX=") || (strings.HasPrefix(kv, "HC_TASK=") && kv != "HC_TASK=") {
				argv = append(argv, kv)
			}
		}
		ran = append(ran, argv)
		return nil
	}
	runAttach = execProgram
	t.Setenv("TMUX", "/tmp/outer,1,0")

	// In the board's own tmux: the session is started, then this terminal attaches to it.
	out := f.ok("new", "-C", f.tmp, "do it")
	short := strings.Fields(out)[0]
	f.ok("attach", short)
	socket := f.cfg.TmuxSocket()
	want := tmux.AttachCommand("%101", socket)
	if len(ran) != 1 || !reflect.DeepEqual(ran[0], want) { // TMUX is not passed on
		t.Fatalf("attach ran %v, want %v", ran, want)
	}

	// In your own tmux: the daemon switches your client to the pane.
	f.hook("UserPromptSubmit", "mine0001", "%1", map[string]any{"prompt": "x"})
	if out := f.ok("attach", "mine"); out != "mine0001  switched tmux to pane %1\n" || len(ran) != 1 {
		t.Errorf("attach printed %q, ran %v", out, ran)
	}

	// Without the board's tmux and without a live session: this terminal resumes the session itself.
	f.cfg.PrivateTmux = false
	delete(f.tmux.panes, "%1")
	f.ok("attach", "mine")
	if want := []string{"/bin/sh", "-c", "claude-team --session-id mine0001", "TMUX=/tmp/outer,1,0", "HC_TASK=mine0001"}; !reflect.DeepEqual(ran[len(ran)-1], want) {
		t.Errorf("attach ran %v, want %v", ran[len(ran)-1], want)
	}
}

// herdrAgents is a Herdr whose every server runs these agents, by pane.
type herdrAgents map[string]herdr.Agent

func (h herdrAgents) Agents(string) map[string]herdr.Agent    { return h }
func (h herdrAgents) Prompt(string, string, string) error     { return nil }
func (h herdrAgents) SendKeys(string, []string, string) error { return nil }
func (h herdrAgents) Capture(string, string) string           { return "" }
func (h herdrAgents) Focus(string, string) error              { return nil }

func TestAttachToASessionInHerdr(t *testing.T) {
	f := newFixture(t)
	f.srv.Herdr = herdrAgents{"w1:p2": {Session: "herd0001"}}
	var ran []string
	old := runHerdrAttach
	defer func() { runHerdrAttach = old }()
	runHerdrAttach = func(argv []string) error { ran = argv; return nil }
	socket := filepath.Join(f.tmp, "herdr.sock")
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "herd0001", "prompt": "x"})
	req, _ := http.NewRequest("POST", f.url+"/api/hook", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Herdr-Pane", "w1:p2")
	req.Header.Set("X-Herdr-Socket", socket)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("hook: %v %v", resp, err)
	}
	f.ok("attach", "herd")
	if want := herdr.AttachCommand("w1:p2", config.RealPath(socket)); !reflect.DeepEqual(ran, want) {
		t.Errorf("attach ran %v, want %v", ran, want)
	}
}

func TestHooksAndConfig(t *testing.T) {
	f := newFixture(t)
	var block struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(f.ok("hooks")), &block); err != nil || len(block.Hooks) != len(install.HookEvents) {
		t.Fatalf("hooks printed %d events, %v", len(block.Hooks), err)
	}
	script := block.Hooks["Stop"][0].Hooks[0].Command
	if info, err := os.Stat(script); err != nil || info.Mode()&0o111 == 0 || filepath.Base(script) != "hc-hook.sh" {
		t.Errorf("the hook script %s is not usable: %v", script, err)
	}

	profile := filepath.Join(f.tmp, "profile")
	t.Setenv("CLAUDE_CONFIG_DIR", profile)
	settings := filepath.Join(profile, "settings.json")
	if out := f.ok("hooks", "install"); out != "hooks installed in "+settings+"\nSessions started from now on report to the board.\n" {
		t.Errorf("install printed %q", out)
	}
	if out := f.ok("hooks", "install", "-c", profile, "-c", filepath.Join(f.tmp, "second")); !strings.HasPrefix(out, "hooks already in "+settings+"\nhooks installed in ") {
		t.Errorf("a second install printed %q", out)
	}
	if out := f.ok("hooks", "uninstall"); out != "hooks removed from "+settings+"\n" {
		t.Errorf("uninstall printed %q", out)
	}
	if raw, _ := os.ReadFile(settings); string(raw) != "{}\n" {
		t.Errorf("settings after uninstall = %q", raw)
	}
	os.WriteFile(settings, []byte("{ not json"), 0o644)
	if code, _, errOut := f.hc("hooks", "install"); code != 1 || !strings.Contains(errOut, "is not valid JSON") {
		t.Errorf("broken settings gave %d %q", code, errOut)
	}
	if code, _, errOut := f.hc("hooks", "remove"); code != 2 || !strings.Contains(errOut, "'remove' is not one of") {
		t.Errorf("an unknown action gave %d %q", code, errOut)
	}

	path := filepath.Join(f.tmp, "config.toml")
	if out := f.ok("config", "init"); out != "wrote "+path+"\n" {
		t.Errorf("config printed %q", out)
	}
	if out := f.ok("config"); out != "kept the existing "+path+"\n" {
		t.Errorf("a second config printed %q", out)
	}
	if cfg, err := config.Load(path); err != nil || cfg.FirstProfile() != "default" || cfg.Port != 7337 {
		t.Errorf("the example config does not load: %v", err)
	}
}

func TestServiceStatusAndAMissingDaemon(t *testing.T) {
	f := newFixture(t)
	out := f.ok("service")
	if !strings.Contains(out, "daemon   hc ") || !strings.Contains(out, " at "+f.url+"\n") {
		t.Errorf("service printed %q", out)
	}
	// Installing over a daemon that something else started would leave two fighting for the port.
	if code, _, errOut := f.hc("service", "install"); code != 1 || !strings.Contains(errOut, "A daemon already runs") {
		if _, err := install.Manager(); err == nil {
			t.Errorf("service install gave %d %q", code, errOut)
		}
	}

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	free := "http://" + listener.Addr().String()
	listener.Close()
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--url", free, "ls"}, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "hc: No daemon at "+free) {
		t.Errorf("a missing daemon gave %d %q", code, stderr.String())
	}
	stdout.Reset()
	if code := Main([]string{"--url=" + free, "service", "status"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "daemon   not running") {
		t.Errorf("status without a daemon gave %d %q", code, stdout.String())
	}
	if v := runningVersion(client.New(free)); v != "" {
		t.Errorf("a missing daemon has version %q", v)
	}
}

func TestVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--version"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "hc ") {
		t.Errorf("--version gave %d %q", code, stdout.String())
	}
	stdout.Reset()
	if code := Main([]string{"ls", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "-s, --sort VALUE") {
		t.Errorf("ls --help gave %d %q", code, stdout.String())
	}
	if code := Main([]string{"--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown option exited %d", code)
	}
}

func TestTimer(t *testing.T) {
	f := newFixture(t)
	short := strings.Fields(f.ok("new", "--timer", "10m", "-C", f.tmp, "triage"))[0]
	task, _ := f.st.Resolve(short)
	if left := task.EndsAt - store.Now(); left < 590 || left > 600 {
		t.Errorf("new --timer 10m left %v seconds", left)
	}
	if out := f.ok("ls"); !strings.Contains(out, " ⏱ ↻ ") || !regexp.MustCompile(` (10m|9m) -  +triage`).MatchString(out) || strings.Contains(out, "⏱ 1") || strings.Contains(out, "⏱ 9") {
		t.Errorf("ls does not show the timer:\n%s", out)
	}
	if out := f.ok("timer", short, "1h30m"); out != short+"  closes in 1h30\n" {
		t.Errorf("timer printed %q", out)
	}
	if out := f.ok("timer", short, "off"); out != short+"  no timer\n" {
		t.Errorf("timer off printed %q", out)
	}
	if task, _ := f.st.Resolve(short); task.EndsAt != 0 {
		t.Errorf("timer off left ends_at %v", task.EndsAt)
	}
	for _, bad := range [][]string{{"timer", short, "10"}, {"timer", short, "0s"}, {"timer", short, "-5m"}, {"timer", short, "8d"},
		{"timer", short, "200h"}, {"new", "--timer", "soon", "x"}, {"timer", short}} {
		if code, _, errOut := f.hc(bad...); code != 2 || errOut == "" {
			t.Errorf("hc %v: exit %d, %q", bad, code, errOut)
		}
	}
}

func TestScheduleTimer(t *testing.T) {
	f := newFixture(t)
	f.ok("schedule", "add", "--cron", "*/15 * * * *", "--timer", "10m", "-t", "Triage", "triage")
	all, _ := f.st.Schedules()
	if len(all) != 1 || all[0].Timer != 600 {
		t.Fatalf("schedules = %+v", all)
	}
	short := all[0].ID[:8]
	if out := f.ok("schedule"); !strings.Contains(out, "⏱ 10m Triage") {
		t.Errorf("ls does not show the timer:\n%s", out)
	}
	if out := f.ok("schedule", "timer", short, "5m"); out != short+"  each run closes after 5m\n" {
		t.Errorf("schedule timer printed %q", out)
	}
	if out := f.ok("schedule", "timer", short, "off"); out != short+"  its runs have no timer\n" {
		t.Errorf("schedule timer off printed %q", out)
	}
	for _, bad := range [][]string{{"schedule", "timer", short}, {"schedule", "timer", short, "soon"},
		{"schedule", "add", "--at", "08:00", "--timer", "0s", "x"}} {
		if code, _, errOut := f.hc(bad...); code != 2 || errOut == "" {
			t.Errorf("hc %v: exit %d, %q", bad, code, errOut)
		}
	}
}

func TestModelAndEffort(t *testing.T) {
	f := newFixture(t)
	short := strings.Fields(f.ok("new", "--model", "sonnet", "--effort", "low", "-C", f.tmp, "triage"))[0]
	if out := f.ok("show", short); !strings.Contains(out, "  model    sonnet\n  effort   low\n") ||
		!strings.Contains(out, "claude-team --model sonnet --effort low --session-id ") {
		t.Errorf("show does not tell the model and the effort:\n%s", out)
	}
	if out := f.ok("ls"); !strings.Contains(out, " sonnet low    ") {
		t.Errorf("ls does not show the model and the effort:\n%s", out)
	}

	f.ok("schedule", "add", "--cron", "*/15 * * * *", "--model", "haiku", "--effort", "low", "-t", "Triage", "triage")
	all, _ := f.st.Schedules()
	if len(all) != 1 || all[0].Model != "haiku" || all[0].Effort != "low" {
		t.Fatalf("schedules = %+v", all)
	}
	id := all[0].ID[:8]
	if out := f.ok("schedule"); !strings.Contains(out, "MODEL EFFORT") || !strings.Contains(out, "no       haiku low    on") {
		t.Errorf("ls does not show the model and the effort:\n%s", out)
	}
	if out := f.ok("schedule", "model", id, "claude-sonnet-4-5"); out != id+"  its runs start with model claude-sonnet-4-5\n" {
		t.Errorf("schedule model printed %q", out)
	}
	if out := f.ok("schedule"); !strings.Contains(out, "no       claude-sonnet-4-5 low    on") {
		t.Errorf("ls does not widen for a long model:\n%s", out)
	}
	if out := f.ok("schedule", "effort", id, "default"); out != id+"  its runs start with effort default\n" {
		t.Errorf("schedule effort default printed %q", out)
	}
	if all, _ := f.st.Schedules(); all[0].Effort != "" || all[0].Model != "claude-sonnet-4-5" {
		t.Errorf("schedule = %+v", all[0])
	}
	f.ok("schedule", "add", "--at", "08:00", "--model", "default", "--effort", "default", "-t", "Standup", "x")
	if all, _ := f.st.Schedules(); len(all) != 2 || all[1].Title != "Standup" || all[1].Model != "" || all[1].Effort != "" {
		t.Errorf("default was kept as a value: %+v", all)
	}
	for _, bad := range [][]string{{"schedule", "model", id}, {"schedule", "effort"}} {
		if code, _, errOut := f.hc(bad...); code != 2 || errOut == "" {
			t.Errorf("hc %v: exit %d, %q", bad, code, errOut)
		}
	}
	for _, bad := range [][]string{{"schedule", "effort", id, "huge"}, {"new", "--effort", "huge", "x"},
		{"new", "--model", "a b", "x"}, {"schedule", "add", "--at", "08:00", "--model", "-x", "y"}} {
		if code, _, errOut := f.hc(bad...); code != 1 || errOut == "" {
			t.Errorf("hc %v: exit %d, %q", bad, code, errOut)
		}
	}
}
