package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rouxlabs/headcoach/internal/store"
)

func TestTimerIsSetClearedAndChecked(t *testing.T) {
	f := newFixture(t)
	out := f.newTask(obj{"timer": 600})
	id := out.str("id")
	eq(t, out.data["timer_left"], float64(600))
	if ends := f.task(id).EndsAt; ends < store.Now()+590 || ends > store.Now()+600 {
		t.Errorf("ends_at %v is not ten minutes from now", ends)
	}
	eq(t, f.newTask(nil).data["timer_left"], float64(0))

	path := "/api/tasks/" + id
	eq(t, f.ok(f.patch(path, obj{"timer": 90})).data["timer_left"], float64(90))
	eq(t, f.ok(f.patch(path, obj{"timer": 0})).data["timer_left"], float64(0))
	eq(t, f.task(id).EndsAt, float64(0))
	for _, bad := range []any{-1, 1.5, "10m", nil, true, maxTimer + 1} {
		f.wantError(f.patch(path, obj{"timer": bad}), 400)
		f.wantError(f.post("/api/tasks", obj{"prompt": "x", "cwd": f.tmp, "timer": bad}), 400)
	}
	eq(t, f.count("tasks"), 2)
}

func TestExpireClosesWhateverTheTaskDoes(t *testing.T) {
	f := newFixture(t)
	running := f.live()
	f.ok(f.patch("/api/tasks/"+running, obj{"timer": 60}))
	later := f.newTask(obj{"timer": 3600}).str("id")
	// A task you moved to Done, its session still open in pane %2.
	f.tmux.known["%2"] = true
	done := f.hook("UserPromptSubmit", "s2", obj{"prompt": "keep me"}, "X-Tmux-Pane", "%2")
	f.ok(f.patch("/api/tasks/"+done, obj{"timer": 60}))
	f.ok(f.patch("/api/tasks/"+done, obj{"state": "done"}))

	// Not yet: nothing moves.
	if err := f.srv.Expire(time.Now()); err != nil {
		t.Fatal(err)
	}
	eq(t, f.task(running).State, "running")

	if err := f.srv.Expire(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	task := f.task(running)
	eq(t, []any{task.State, task.EndsAt, task.Last}, []any{"done", float64(0), "Closed when its timer ran out"})
	eq(t, f.tmux.killed, []string{"%1|"})
	eq(t, f.session("s1").Live, false)
	eq(t, f.history(running), []string{"running hook", "done timer"})
	// A task already in Done only loses its timer, its session left open; one with time left keeps it.
	eq(t, []any{f.task(done).State, f.task(done).EndsAt, f.session("s2").Live}, []any{"done", float64(0), true})
	eq(t, f.history(done)[len(f.history(done))-1], "done you")
	eq(t, f.task(later).State, "backlog")
	if f.task(later).EndsAt == 0 {
		t.Error("the task with time left lost its timer")
	}

	// The session's own end, once its pane is gone, leaves the task in Done.
	f.hook("SessionEnd", "s1", obj{"reason": "other"})
	eq(t, f.task(running).State, "done")
}

func TestTheScheduleLoopClosesExpiredTasks(t *testing.T) {
	f := newFixture(t)
	id := f.newTask(obj{"timer": 1}).str("id")
	if err := f.st.UpdateTask(id, store.Fields{"ends_at": store.Now() - 1}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	f.srv.RunSchedules(stop) // one pass, then it sees stop
	eq(t, f.task(id).State, "done")
}

func TestAScheduleTimesEachRun(t *testing.T) {
	f := newFixture(t)
	sc := f.ok(f.post("/api/schedules", obj{"prompt": "triage", "at": "*/15 * * * *", "timer": 600}))
	id := sc.str("id")
	eq(t, sc.data["timer"], float64(600))
	f.wantError(f.patch("/api/schedules/"+id, obj{"timer": -5}), 400)
	f.wantError(f.post("/api/schedules", obj{"prompt": "x", "at": "08:00", "timer": "10m"}), 400)

	f.runDue(monday(0, 9, 0))
	run := f.task(f.schedule(id).LastTask)
	if left := run.EndsAt - store.Now(); left < 590 || left > 600 {
		t.Errorf("the run's timer has %v seconds left", left)
	}
	eq(t, f.ok(f.patch("/api/schedules/"+id, obj{"timer": 0})).data["timer"], float64(0))
	f.ok(f.patch("/api/tasks/"+run.ID, obj{"state": "done"}))
	f.runDue(monday(0, 9, 15))
	eq(t, f.task(f.schedule(id).LastTask).EndsAt, float64(0))
}

func TestADeleteIsSetCancelledAndDone(t *testing.T) {
	f := newFixture(t)
	id := f.newTask(nil).str("id")
	kept := f.newTask(nil).str("id")
	path := "/api/tasks/" + id
	before := f.task(id).UpdatedAt

	eq(t, f.ok(f.patch(path, obj{"delete_in": 600})).data["delete_left"], float64(600))
	if at := f.task(id).DeleteAt; at < store.Now()+590 || at > store.Now()+600 {
		t.Errorf("delete_at %v is not ten minutes from now", at)
	}
	// A delete to come is not activity: the task does not look busy.
	eq(t, f.task(id).UpdatedAt, before)
	eq(t, f.ok(f.patch(path, obj{"delete_in": 0})).data["delete_left"], float64(0))
	eq(t, f.task(id).DeleteAt, float64(0))
	for _, bad := range []any{-1, 1.5, "10m", nil, true, maxTimer + 1} {
		f.wantError(f.patch(path, obj{"delete_in": bad}), 400)
	}
	eq(t, f.task(id).DeleteAt, float64(0))

	// Not yet due: the task stays. Due: it goes with its handoff and the tasks under it, the other one
	// stays. A task under it due too is gone by its turn.
	f.ok(f.patch(path, obj{"delete_in": 60}))
	kid := f.newTask(obj{"parent": id}).str("id")
	f.ok(f.patch("/api/tasks/"+kid, obj{"delete_in": 60}))
	f.newTask(obj{"parent": kid})
	if err := os.MkdirAll(filepath.Dir(f.cfg.HandoffPath(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cfg.HandoffPath(id), []byte("# Handoff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Expire(time.Now()); err != nil {
		t.Fatal(err)
	}
	eq(t, f.count("tasks"), 4)
	if err := f.srv.Expire(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	eq(t, f.count("tasks"), 1)
	if f.task(kept) == nil {
		t.Error("the task without a delete went too")
	}
	if _, err := os.Stat(f.cfg.HandoffPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the handoff of the deleted task stayed: %v", err)
	}
	f.wantError(f.get(path), 404)
}

func TestATimerAndADeleteDueTogether(t *testing.T) {
	f := newFixture(t)
	running := f.live()
	f.ok(f.patch("/api/tasks/"+running, obj{"timer": 60, "delete_in": 60}))
	if err := f.srv.Expire(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The timer is a hard limit: the session is closed before its task goes.
	eq(t, f.tmux.killed, []string{"%1|"})
	eq(t, f.session("s1").Live, false)
	eq(t, f.count("tasks"), 0)
}

func TestATaskThatRunsAgainIsNotDeleted(t *testing.T) {
	f := newFixture(t)
	running := f.live()
	path := "/api/tasks/" + running
	// A delete set on a running task holds while it keeps running.
	f.ok(f.patch(path, obj{"delete_in": 600}))
	f.hook("UserPromptSubmit", "s1", obj{"prompt": "more"}, "X-Tmux-Pane", "%1")
	eq(t, f.task(running).State, "running")
	if f.task(running).DeleteAt == 0 {
		t.Error("a prompt to a running task cancelled its delete")
	}
	// Set on a done task, it goes once the task runs again.
	f.ok(f.patch(path, obj{"state": "done"}))
	f.ok(f.patch(path, obj{"delete_in": 600}))
	f.hook("UserPromptSubmit", "s1", obj{"prompt": "one more thing"}, "X-Tmux-Pane", "%1")
	eq(t, f.task(running).State, "running")
	eq(t, f.task(running).DeleteAt, float64(0))

	// A refused delete changes nothing else.
	f.wantError(f.patch(path, obj{"title": "renamed", "delete_in": "10m"}), 400)
	if f.task(running).Title == "renamed" {
		t.Error("a refused request renamed the task")
	}
}
