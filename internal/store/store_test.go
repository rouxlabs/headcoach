package store

import (
	"path/filepath"
	"testing"
)

func TestOpenFillsTheScheduleOfLastRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hc.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := st.CreateSchedule(Schedule{ID: "sched0001", Title: "Standup", Prompt: "summarise", At: "08:00"})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{{ID: "last0001", Title: "last", Source: "schedule"}, {ID: "old00001", Title: "old", Source: "schedule"},
		{ID: "hand0001", Title: "by hand"}} {
		if _, err := st.CreateTask(task, "you"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateSchedule(sc.ID, Fields{"last_task": "last0001"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, want := range map[string]string{"last0001": "sched0001", "old00001": "", "hand0001": ""} {
		if task, err := st.Task(id); err != nil || task.Schedule != want {
			t.Errorf("task %s has the schedule %q, want %q (%v)", id, task.Schedule, want, err)
		}
	}
}

func TestOpenHidesTheTasksDoneBeforeTheHiddenColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hc.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{{ID: "done0001", Title: "done", State: "done"}, {ID: "open0001", Title: "open", State: "running"}} {
		if _, err := st.CreateTask(task, "you"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Exec("ALTER TABLE tasks DROP COLUMN hidden"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// The board hid a done task on its own before: it stays hidden.
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"done0001": true, "open0001": false} {
		if task, err := st.Task(id); err != nil || task.Hidden != want {
			t.Errorf("task %s hidden %v, want %v (%v)", id, task.Hidden, want, err)
		}
	}
	if err := st.HideTask("done0001", false); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Only once: a task shown again stays in sight at the next open.
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if task, _ := st.Task("done0001"); task.Hidden {
		t.Error("a task shown again is hidden at the next open")
	}
}

// A task keeps its newest messages only, and loses them all when it is deleted.
func TestMessagesAreTrimmedAndDeleted(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(Task{ID: "t1", Title: "t"}, "you"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < messagesKept+2; i++ {
		if err := st.AddMessage(Message{TaskID: "t1", Sender: "task", SenderTask: "t2", Kind: "message", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddMessage(Message{TaskID: "t3", Sender: "you", Kind: "message", Text: "other"}); err != nil {
		t.Fatal(err)
	}
	kept, err := st.Messages("t1")
	if err != nil || len(kept) != messagesKept || kept[0].ID != 3 || kept[0].SenderTask != "t2" || kept[0].At == 0 {
		t.Fatalf("kept %d, first %+v, %v", len(kept), kept[0], err)
	}
	if err := st.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if kept, _ = st.Messages("t1"); len(kept) != 0 {
		t.Errorf("a deleted task keeps %d messages", len(kept))
	}
	if other, _ := st.Messages("t3"); len(other) != 1 {
		t.Errorf("another task lost its messages: %v", other)
	}
}

// A child's stop reaches its parent's inbox, one item per child; a move the parent made does not.
func TestInboxItems(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, task := range []Task{{ID: "boss", Title: "boss"}, {ID: "kid", Title: "kid", Parent: "boss", State: "running"}, {ID: "solo", Title: "solo"}} {
		if _, err := st.CreateTask(task, "you"); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() []string {
		items, err := st.Inbox("WHERE delivered_at = 0 ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ParentID+" "+it.ChildID+" "+it.State+" "+it.AskKind+" "+it.Ask+" "+it.Last)
		}
		return out
	}
	st.SetState("solo", "done", "hook", nil)
	st.SetState("kid", "needs", "hook", Fields{"ask": "A or B?", "ask_kind": "question"})
	if got := pending(); len(got) != 1 || got[0] != "boss kid needs question A or B? " {
		t.Fatalf("pending = %q", got)
	}
	st.SetState("kid", "running", "hook", nil)
	st.SetState("kid", "review", "hook", Fields{"last": "Pushed"})
	if got := pending(); len(got) != 1 || got[0] != "boss kid review   Pushed" {
		t.Fatalf("pending = %q", got)
	}
	st.SetState("kid", "done", ByTask("boss"), nil)
	if got := pending(); len(got) != 1 || got[0] != "boss kid review   Pushed" {
		t.Fatalf("a move by the parent itself: %q", got)
	}
	if TaskOf(ByTask("boss")) != "boss" || TaskOf("you") != "" {
		t.Error("TaskOf does not undo ByTask")
	}
	if err := st.DeleteTask("kid"); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 0 {
		t.Errorf("a deleted child leaves %q", got)
	}
}

func TestPinTask(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"first001", "second01"} {
		if _, err := st.CreateTask(Task{ID: id, Title: id}, "you"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Exec("UPDATE tasks SET updated_at = 1 WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	pinned := func(id string) float64 {
		t.Helper()
		task, err := st.Task(id)
		if err != nil || task == nil {
			t.Fatalf("task %s: %v", id, err)
		}
		if task.UpdatedAt != 1 {
			t.Errorf("pinning %s counted as activity: updated_at %v", id, task.UpdatedAt)
		}
		return task.PinnedAt
	}
	if pinned("first001") != 0 {
		t.Error("a new task is pinned")
	}
	for _, id := range []string{"first001", "second01"} {
		if err := st.PinTask(id, true); err != nil {
			t.Fatal(err)
		}
	}
	first, second := pinned("first001"), pinned("second01")
	if first == 0 || second < first {
		t.Errorf("pinned at %v then %v", first, second)
	}
	// Pinned again, the first keeps its place above the second.
	if err := st.PinTask("first001", true); err != nil {
		t.Fatal(err)
	}
	if again := pinned("first001"); again != first {
		t.Errorf("pinned again, the first moved from %v to %v", first, again)
	}
	if err := st.PinTask("first001", false); err != nil {
		t.Fatal(err)
	}
	if pinned("first001") != 0 || pinned("second01") != second {
		t.Error("unpinning the first changed the pins")
	}
}

func TestSeeTask(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTask(Task{ID: "seen0001", Title: "seen"}, "you"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Exec("UPDATE tasks SET updated_at = 1 WHERE id = 'seen0001'"); err != nil {
		t.Fatal(err)
	}
	if task, _ := st.Task("seen0001"); task.SeenAt != 0 {
		t.Errorf("a new task was seen at %v", task.SeenAt)
	}
	before := Now()
	if err := st.SeeTask("seen0001"); err != nil {
		t.Fatal(err)
	}
	task, err := st.Task("seen0001")
	if err != nil {
		t.Fatal(err)
	}
	if task.SeenAt < before {
		t.Errorf("seen at %v, before %v", task.SeenAt, before)
	}
	if task.UpdatedAt != 1 {
		t.Errorf("seeing a task counted as activity: updated_at %v", task.UpdatedAt)
	}
}
