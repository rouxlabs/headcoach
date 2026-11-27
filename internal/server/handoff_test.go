package server

import (
	"os"
	"strings"
	"testing"

	"github.com/rouxlabs/headcoach/internal/handoff"
)

const filled = "# Handoff\n## Status\nHalfway.\n## Waiting\n- on you: a go\n## Queue\n- [x] one\n- [ ] two\n"

func TestHandoff(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	path := f.cfg.HandoffPath(id)
	eq(t, path, f.tmp+"/state/handoff/"+id+".md")
	h := f.ok(f.get("/api/tasks/" + id + "/handoff")).data
	eq(t, []any{h["path"], h["text"], h["updated_at"]}, []any{path, "", 0.0})

	// init writes the template once, and never over a handoff already there.
	h = f.ok(f.post("/api/tasks/"+id+"/handoff", nil)).data
	eq(t, h["text"], handoff.Template)
	os.WriteFile(path, []byte(filled), 0o644)
	h = f.ok(f.post("/api/tasks/"+id+"/handoff", nil)).data
	eq(t, h["text"], filled)
	eq(t, h["summary"].(map[string]any)["status"], "Halfway.")

	// The detail of the task carries it, from inside its session too.
	detail := f.ok(f.do("GET", "/api/tasks/self", nil, "X-HC-Session", "s1")).data["handoff"].(map[string]any)
	eq(t, []any{detail["text"], detail["updated_at"] != 0.0}, []any{filled, true})
	if !strings.Contains(detail["html"].(string), "<h2>Status</h2>") {
		t.Errorf("html = %q", detail["html"])
	}

	// It goes with its task.
	f.ok(f.do("DELETE", "/api/tasks/"+id, nil))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the handoff outlived its task: %v", err)
	}
}

func TestReset(t *testing.T) {
	f := newFixture(t)
	id := f.live()
	path := "/api/tasks/" + id + "/reset"
	f.tmux.screen = idleScreen

	// Nothing to start again from: refused, nothing typed.
	f.wantError(f.post(path, nil), 409)
	f.ok(f.post("/api/tasks/"+id+"/handoff", nil))
	f.wantError(f.post(path, nil), 409)
	os.WriteFile(f.cfg.HandoffPath(id), []byte(filled), 0o644)
	// Mid-turn: refused.
	f.wantError(f.post(path, nil), 409)
	f.hook("Stop", "s1", obj{"last_assistant_message": "Done."})
	// Words in the prompt, or a dialog: refused.
	f.tmux.screen = rule + "\n❯\u00a0half\n" + rule + "\n"
	f.wantError(f.post(path, nil), 409)
	eq(t, len(f.tmux.sent), 0)

	// A suggestion Claude Code draws in the prompt is nothing typed.
	f.tmux.screen = rule + "\n❯\u00a0\x1b[2msay more\x1b[0m\n" + rule + "\n"
	out := f.ok(f.do("POST", path, nil, "X-HC-Session", "s1"))
	brief := "[from hc] Your context was cleared. Read your handoff file, " + f.cfg.HandoffPath(id) + ", then go on from where it says."
	eq(t, f.tmux.sent, []string{"%1|/clear|", "%1|" + brief + "|"})
	if !strings.HasPrefix(out.str("last"), "From hc: Your context was cleared. Read your handoff file, ") {
		t.Errorf("last = %q", out.str("last"))
	}
	eq(t, f.sent(id), []string{"task s1 reset /clear", "hc  message " + brief})
	// The cleared session comes back in its pane as the same task, where a /clear you type opens a new one.
	eq(t, f.hook("SessionStart", "s2", obj{"source": "clear"}), id)
	eq(t, f.hook("SessionStart", "s3", obj{"source": "clear"}), "s3")
	eq(t, f.task(id).State, "done")

	// No live session: refused.
	f.hook("SessionEnd", "s1", nil)
	delete(f.tmux.known, "%1")
	f.wantError(f.post(path, nil), 409)
}
