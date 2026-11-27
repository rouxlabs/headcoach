// Package store is the SQLite storage. One file, WAL mode, no ORM.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the SQLite driver, registered as "sqlite"
)

// States lists every state a task can be in, in board order.
var States = []string{"backlog", "running", "needs", "review", "parked", "done"}

// StateNames gives the label of each state.
var StateNames = map[string]string{
	"backlog": "Backlog", "running": "Running", "needs": "Needs me",
	"review": "Review", "parked": "Parked", "done": "Done",
}

// StateName is the label of a state, or the state itself when it has no label.
// Sub-agent states go through it too, so the boards never show a bare state key.
func StateName(state string) string {
	if name := StateNames[state]; name != "" {
		return name
	}
	return state
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    prompt     TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'backlog',
    cwd        TEXT NOT NULL DEFAULT '',
    repo       TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL DEFAULT '',
    profile    TEXT NOT NULL DEFAULT 'default',
    source     TEXT NOT NULL DEFAULT 'manual',
    ask        TEXT NOT NULL DEFAULT '',
    ask_kind   TEXT NOT NULL DEFAULT '',
    last       TEXT NOT NULL DEFAULT '',
    remote     TEXT NOT NULL DEFAULT '',
    persona    TEXT NOT NULL DEFAULT '',
    waiting    INTEGER NOT NULL DEFAULT 0,
    base       TEXT NOT NULL DEFAULT '',
    ends_at    REAL NOT NULL DEFAULT 0,
    model      TEXT NOT NULL DEFAULT '',
    effort     TEXT NOT NULL DEFAULT '',
    schedule   TEXT NOT NULL DEFAULT '',
    hidden     INTEGER NOT NULL DEFAULT 0,
    parent     TEXT NOT NULL DEFAULT '',
    pinned_at  REAL NOT NULL DEFAULT 0,
    delete_at  REAL NOT NULL DEFAULT 0,
    seen_at    REAL NOT NULL DEFAULT 0,
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
    session_id      TEXT PRIMARY KEY,
    task_id         TEXT,
    pane            TEXT NOT NULL DEFAULT '',
    socket          TEXT NOT NULL DEFAULT '',
    herdr_pane      TEXT NOT NULL DEFAULT '',
    herdr_socket    TEXT NOT NULL DEFAULT '',
    window          INTEGER NOT NULL DEFAULT 0,
    cost            REAL NOT NULL DEFAULT 0,
    effort          TEXT NOT NULL DEFAULT '',
    config_dir      TEXT NOT NULL DEFAULT '',
    transcript_path TEXT NOT NULL DEFAULT '',
    cwd             TEXT NOT NULL DEFAULT '',
    live            INTEGER NOT NULL DEFAULT 1,
    started_at      REAL NOT NULL,
    ended_at        REAL
);
CREATE INDEX IF NOT EXISTS sessions_task ON sessions(task_id);
CREATE TABLE IF NOT EXISTS history (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    state   TEXT NOT NULL,
    at      REAL NOT NULL,
    by      TEXT NOT NULL DEFAULT 'hook'
);
CREATE INDEX IF NOT EXISTS history_task ON history(task_id);
CREATE TABLE IF NOT EXISTS agents (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL,
    session_id TEXT NOT NULL,
    agent_id   TEXT,
    name       TEXT NOT NULL DEFAULT '',
    descr      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL DEFAULT 'running',
    started_at REAL NOT NULL,
    ended_at   REAL
);
CREATE INDEX IF NOT EXISTS agents_task ON agents(task_id);
CREATE TABLE IF NOT EXISTS schedules (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    prompt     TEXT NOT NULL,
    cwd        TEXT NOT NULL DEFAULT '',
    profile    TEXT NOT NULL DEFAULT '',
    at         TEXT NOT NULL,
    days       TEXT NOT NULL DEFAULT 'daily',
    catch_up   INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_run   REAL NOT NULL DEFAULT 0,
    last_task  TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    timer      INTEGER NOT NULL DEFAULT 0,
    model      TEXT NOT NULL DEFAULT '',
    effort     TEXT NOT NULL DEFAULT '',
    created_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
    name       TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    created_at REAL NOT NULL,
    last_seen  REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS limits (
    profile   TEXT NOT NULL,
    kind      TEXT NOT NULL,
    pct       REAL NOT NULL,
    resets_at REAL NOT NULL,
    at        REAL NOT NULL,
    PRIMARY KEY (profile, kind)
);
CREATE TABLE IF NOT EXISTS limit_samples (
    profile   TEXT NOT NULL,
    kind      TEXT NOT NULL,
    resets_at REAL NOT NULL,
    at        REAL NOT NULL,
    pct       REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS limit_samples_window ON limit_samples(profile, kind, resets_at);
CREATE TABLE IF NOT EXISTS messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL,
    sender      TEXT NOT NULL DEFAULT 'you',
    sender_task TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'message',
    text        TEXT NOT NULL DEFAULT '',
    at          REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_task ON messages(task_id);
CREATE TABLE IF NOT EXISTS inbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id    TEXT NOT NULL,
    child_id     TEXT NOT NULL,
    state        TEXT NOT NULL,
    ask          TEXT NOT NULL DEFAULT '',
    ask_kind     TEXT NOT NULL DEFAULT '',
    last         TEXT NOT NULL DEFAULT '',
    at           REAL NOT NULL,
    delivered_at REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS inbox_parent ON inbox(parent_id);
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         REAL NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT ''
);
`

// eventsKept is how many hook events the log keeps.
const eventsKept = 5000

// ErrAmbiguous is returned when an id prefix matches more than one task.
var ErrAmbiguous = errors.New("matches more than one task")

// Now returns the current time in seconds, the unit every timestamp is stored in.
func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Task is one row of the board.
type Task struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
	State   string `json:"state"`
	Cwd     string `json:"cwd"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Profile string `json:"profile"`
	Source  string `json:"source"`
	Ask     string `json:"ask"`
	AskKind string `json:"ask_kind"`
	Last    string `json:"last"`
	Remote  string `json:"remote"`
	Persona string `json:"persona"`
	// Waiting is set while a running task's main agent only waits on its sub-agents or background
	// jobs. The boards show such a task as Waiting, still in the running column.
	Waiting bool `json:"waiting"`
	// Base is the commit the checkout was on when the task first ran in a repository with a commit:
	// where its change starts. Tasks from before the column take the commit of their next prompt.
	Base string `json:"base"`
	// EndsAt is when the task's timer runs out and the daemon closes it, as a Unix time. Zero: no timer.
	EndsAt float64 `json:"ends_at"`
	// Model and Effort are what its sessions start Claude Code with. Empty: the profile's own.
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// Schedule is the id of the schedule that started the task. Empty: started by hand, or a run
	// from before the column whose schedule ran again since.
	Schedule string `json:"schedule"`
	// Hidden says you hid the task from the board. Only a done task is hidden: a move clears it.
	Hidden bool `json:"hidden"`
	// Parent is the id of the task that drives this one, its coordinator. Empty: none.
	Parent string `json:"parent"`
	// PinnedAt is when you pinned the task to the top of the board, as a Unix time. Zero: not pinned.
	PinnedAt float64 `json:"pinned_at"`
	// DeleteAt is when the daemon deletes the task, as a Unix time. Zero: it stays.
	DeleteAt float64 `json:"delete_at"`
	// SeenAt is when you last went into the task's session, as a Unix time. Zero: never.
	SeenAt    float64 `json:"seen_at"`
	CreatedAt float64 `json:"created_at"`
	UpdatedAt float64 `json:"updated_at"`
}

// Session is one Claude Code session. A task can have several over its life.
type Session struct {
	SessionID      string
	TaskID         string // empty until the session belongs to a task
	Pane           string
	Socket         string
	HerdrPane      string // Herdr pane id, empty outside Herdr
	HerdrSocket    string // socket of the Herdr server that holds the pane
	Window         int
	Cost           float64
	Effort         string // the effort level its status line reports. Empty until it does
	ConfigDir      string
	TranscriptPath string
	Cwd            string
	Live           bool
	StartedAt      float64
	EndedAt        *float64
}

// Fields names the columns of a task to change, with their new values.
type Fields map[string]any

// Store wraps the database. Ad hoc statements go through the embedded *sql.DB.
type Store struct {
	*sql.DB
}

// Open opens the database at path, creating the file and the tables when missing. ":memory:" is for tests.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: the load is a handful of writes per second at most, and a memory database lives in it.
	db.SetMaxOpenConns(1)
	s := &Store{db}
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000", schema} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	hadHidden, err := s.hasColumn("tasks", "hidden")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Boards created before these columns existed.
	for _, c := range []struct{ table, column, decl string }{
		{"tasks", "remote", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "persona", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "waiting", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "base", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "ends_at", "REAL NOT NULL DEFAULT 0"},
		{"tasks", "model", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "effort", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "schedule", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "parent", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "pinned_at", "REAL NOT NULL DEFAULT 0"},
		{"tasks", "delete_at", "REAL NOT NULL DEFAULT 0"},
		{"tasks", "seen_at", "REAL NOT NULL DEFAULT 0"},
		{"schedules", "timer", "INTEGER NOT NULL DEFAULT 0"},
		{"schedules", "model", "TEXT NOT NULL DEFAULT ''"},
		{"schedules", "effort", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "socket", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "window", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "cost", "REAL NOT NULL DEFAULT 0"},
		{"sessions", "effort", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "herdr_pane", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "herdr_socket", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "persona", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.addColumn(c.table, c.column, c.decl); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// Tasks done before the hidden column: the board hid them on its own, they stay out of sight.
	// One transaction, so that a crash between the two cannot leave them all in sight.
	if !hadHidden {
		if err := s.addHidden(); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// Runs from before the schedule column: each schedule still knows its last one.
	if _, err := s.Exec("UPDATE tasks SET schedule = (SELECT id FROM schedules WHERE last_task = tasks.id)" +
		" WHERE schedule = '' AND source = 'schedule' AND id IN (SELECT last_task FROM schedules)"); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	var n int
	err := s.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n)
	return n > 0, err
}

func (s *Store) addHidden() error {
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		"ALTER TABLE tasks ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0",
		"UPDATE tasks SET hidden = 1 WHERE state = 'done'",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) addColumn(table, column, decl string) error {
	has, err := s.hasColumn(table, column)
	if err != nil || has {
		return err
	}
	_, err = s.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// --- tasks ---------------------------------------------------------------------------

const taskColumns = "id, title, prompt, state, cwd, repo, branch, profile, source, ask, ask_kind, last, remote," +
	" persona, waiting, base, ends_at, model, effort, schedule, hidden, parent, pinned_at, delete_at, seen_at, created_at," +
	" updated_at"

type scanner interface{ Scan(dest ...any) error }

func scanTask(row scanner) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Title, &t.Prompt, &t.State, &t.Cwd, &t.Repo, &t.Branch, &t.Profile, &t.Source,
		&t.Ask, &t.AskKind, &t.Last, &t.Remote, &t.Persona, &t.Waiting, &t.Base, &t.EndsAt, &t.Model, &t.Effort, &t.Schedule, &t.Hidden, &t.Parent, &t.PinnedAt, &t.DeleteAt,
		&t.SeenAt, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Task returns the task with that id, or nil when there is none.
func (s *Store) Task(id string) (*Task, error) {
	return scanTask(s.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
}

// Tasks runs a query that selects whole tasks. The clause follows "FROM tasks".
func (s *Store) Tasks(clause string, args ...any) ([]Task, error) {
	rows, err := s.Query("SELECT "+taskColumns+" FROM tasks "+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Resolve finds a task by full id or unique prefix. It returns ErrAmbiguous when the prefix matches several.
func (s *Store) Resolve(prefix string) (*Task, error) {
	exact, err := s.Task(prefix)
	if err != nil || exact != nil || prefix == "" {
		return exact, err
	}
	found, err := s.Tasks("WHERE substr(id, 1, ?) = ? LIMIT 2", len([]rune(prefix)), prefix)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("'%s' %w", prefix, ErrAmbiguous)
	}
	return &found[0], nil
}

// CreateTask inserts a task and the first line of its history. An empty state means backlog.
func (s *Store) CreateTask(t Task, by string) (*Task, error) {
	now := Now()
	if t.State == "" {
		t.State = "backlog"
	}
	if t.Profile == "" {
		t.Profile = "default"
	}
	if t.Source == "" {
		t.Source = "manual"
	}
	_, err := s.Exec(
		"INSERT INTO tasks ("+taskColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.ID, t.Title, t.Prompt, t.State, t.Cwd, t.Repo, t.Branch, t.Profile, t.Source, t.Ask, t.AskKind, t.Last,
		t.Remote, t.Persona, t.Waiting, t.Base, t.EndsAt, t.Model, t.Effort, t.Schedule, false, t.Parent, 0, 0, 0, now, now,
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec("INSERT INTO history (task_id, state, at, by) VALUES (?, ?, ?, ?)", t.ID, t.State, now, by); err != nil {
		return nil, err
	}
	return s.Task(t.ID)
}

// taskFields are the columns UpdateTask may change.
var taskFields = map[string]bool{
	"title": true, "prompt": true, "state": true, "cwd": true, "repo": true, "branch": true, "profile": true,
	"source": true, "ask": true, "ask_kind": true, "last": true, "remote": true, "persona": true,
	"waiting": true, "base": true, "ends_at": true, "hidden": true, "parent": true, "delete_at": true,
}

// UpdateTask changes some columns of a task and its updated_at. Without fields it only touches updated_at.
func (s *Store) UpdateTask(id string, fields Fields) error {
	names := make([]string, 0, len(fields))
	for name := range fields {
		if !taskFields[name] {
			return fmt.Errorf("unknown task column '%s'", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	sets := make([]string, 0, len(names)+1)
	args := make([]any, 0, len(names)+2)
	for _, name := range names {
		sets = append(sets, name+" = ?")
		args = append(args, fields[name])
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, Now(), id)
	_, err := s.Exec("UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

// SetState moves a task. It records history only when the state changes, and reports whether it did.
// Leaving the needs state clears the question the task was waiting on. Any move clears the waiting
// flag too, unless fields set it: whatever moved the task is activity of its own. A move to
// another state shows a hidden task again.
func (s *Store) SetState(id, state, by string, fields Fields) (bool, error) {
	if _, known := StateNames[state]; !known {
		return false, fmt.Errorf("unknown state '%s'", state)
	}
	task, err := s.Task(id)
	if err != nil || task == nil {
		return false, err
	}
	set := Fields{"state": state, "waiting": false}
	if task.State != state {
		set["hidden"] = false
	}
	// A task that runs again is wanted again: a delete set while it was away no longer holds.
	if state == "running" && task.State != state {
		set["delete_at"] = 0
	}
	if state != "needs" {
		set["ask"], set["ask_kind"] = "", ""
	}
	for name, value := range fields {
		set[name] = value
	}
	if err := s.UpdateTask(id, set); err != nil {
		return false, err
	}
	changed := task.State != state
	if changed {
		if _, err = s.Exec("INSERT INTO history (task_id, state, at, by) VALUES (?, ?, ?, ?)", id, state, Now(), by); err != nil {
			return changed, err
		}
		// The task's parent learns of it, unless the parent made the move itself.
		if task.Parent != "" && Notified[state] && by != ByTask(task.Parent) {
			ask, _ := set["ask"].(string)
			askKind, _ := set["ask_kind"].(string)
			last, ok := set["last"].(string)
			if !ok {
				last = task.Last
			}
			err = s.notify(InboxItem{ParentID: task.Parent, ChildID: id, State: state, Ask: ask, AskKind: askKind, Last: last})
		}
	}
	return changed, err
}

// HideTask hides a done task from the board, or shows it again. It leaves updated_at alone:
// hiding a task is not activity of its own.
func (s *Store) HideTask(id string, hidden bool) error {
	_, err := s.Exec("UPDATE tasks SET hidden = ? WHERE id = ?", hidden, id)
	return err
}

// PinTask pins a task to the top of the board, or unpins it. A task pinned already keeps its place
// under those pinned before it. It leaves updated_at alone: pinning a task is not activity of its own.
func (s *Store) PinTask(id string, pinned bool) error {
	if !pinned {
		_, err := s.Exec("UPDATE tasks SET pinned_at = 0 WHERE id = ?", id)
		return err
	}
	_, err := s.Exec("UPDATE tasks SET pinned_at = ? WHERE id = ? AND pinned_at = 0", Now(), id)
	return err
}

// SeeTask notes that you went into a task's session just now. It leaves updated_at alone: looking at
// a task is not activity of its own.
func (s *Store) SeeTask(id string) error {
	_, err := s.Exec("UPDATE tasks SET seen_at = ? WHERE id = ?", Now(), id)
	return err
}

// DeleteTaskAt sets when the daemon deletes a task, as a Unix time, zero to keep it. It leaves
// updated_at alone: a delete to come is not activity of the task.
func (s *Store) DeleteTaskAt(id string, at float64) error {
	_, err := s.Exec("UPDATE tasks SET delete_at = ? WHERE id = ?", at, id)
	return err
}

// Descendants returns the ids of the tasks under a task at any depth: those it drives, those they
// drive, and so on. Empty when it drives none.
func (s *Store) Descendants(id string) ([]string, error) {
	rows, err := s.Query("WITH RECURSIVE under(id) AS (SELECT ?1 UNION SELECT tasks.id FROM tasks JOIN under ON tasks.parent = under.id)"+
		" SELECT id FROM under WHERE id != ?1", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kid string
		if err := rows.Scan(&kid); err != nil {
			return nil, err
		}
		out = append(out, kid)
	}
	return out, rows.Err()
}

// DeleteTask removes one task with its history and child rows. Its sessions stay, unlinked, and the
// tasks it drove go to the top of the board: the server deletes them first, see Descendants.
func (s *Store) DeleteTask(id string) error {
	for _, stmt := range []string{
		"DELETE FROM history WHERE task_id = ?",
		"DELETE FROM inbox WHERE parent_id = ?1 OR child_id = ?1",
		"DELETE FROM agents WHERE task_id = ?",
		"DELETE FROM messages WHERE task_id = ?",
		"UPDATE sessions SET task_id = NULL WHERE task_id = ?",
		"UPDATE tasks SET parent = '' WHERE parent = ?",
		"DELETE FROM tasks WHERE id = ?",
	} {
		if _, err := s.Exec(stmt, id); err != nil {
			return err
		}
	}
	return nil
}

// --- sessions ------------------------------------------------------------------------

const sessionColumns = "session_id, task_id, pane, socket, herdr_pane, herdr_socket, window, cost, effort, config_dir," +
	" transcript_path, cwd, live, started_at, ended_at"

func scanSession(row scanner) (*Session, error) {
	var (
		sess  Session
		task  sql.NullString
		ended sql.NullFloat64
	)
	err := row.Scan(&sess.SessionID, &task, &sess.Pane, &sess.Socket, &sess.HerdrPane, &sess.HerdrSocket, &sess.Window, &sess.Cost,
		&sess.Effort, &sess.ConfigDir, &sess.TranscriptPath, &sess.Cwd, &sess.Live, &sess.StartedAt, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.TaskID = task.String
	if ended.Valid {
		sess.EndedAt = &ended.Float64
	}
	return &sess, nil
}

// Session returns the session with that id, or nil when there is none.
func (s *Store) Session(id string) (*Session, error) {
	return scanSession(s.QueryRow("SELECT "+sessionColumns+" FROM sessions WHERE session_id = ?", id))
}

// Sessions runs a query that selects whole sessions. The clause follows "FROM sessions".
func (s *Store) Sessions(clause string, args ...any) ([]Session, error) {
	rows, err := s.Query("SELECT "+sessionColumns+" FROM sessions "+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// TaskSessions lists the sessions of a task, oldest first.
func (s *Store) TaskSessions(taskID string) ([]Session, error) {
	return s.Sessions("WHERE task_id = ? ORDER BY started_at", taskID)
}

// LatestSession returns the newest session of a task, or nil.
func (s *Store) LatestSession(taskID string) (*Session, error) {
	return scanSession(s.QueryRow(
		"SELECT "+sessionColumns+" FROM sessions WHERE task_id = ? ORDER BY started_at DESC LIMIT 1", taskID))
}

// --- messages ------------------------------------------------------------------------

// Message is one message the board typed into a task's session, and who sent it.
type Message struct {
	ID     int64  `json:"id"`
	TaskID string `json:"task_id"`
	Sender string `json:"sender"` // you, task, agent or hc
	// SenderTask is the id of the task that sent it, when Sender is task.
	SenderTask string `json:"sender_task"`
	// Kind is message for a prompt, answer for the answer to a dialog, keys for keys pressed.
	Kind string  `json:"kind"`
	Text string  `json:"text"`
	At   float64 `json:"at"`
}

// messagesKept is how many messages the board keeps per task.
const messagesKept = 500

// AddMessage records a message the board typed into a task's session, and trims the task's oldest.
func (s *Store) AddMessage(m Message) error {
	if m.At == 0 {
		m.At = Now()
	}
	_, err := s.Exec("INSERT INTO messages (task_id, sender, sender_task, kind, text, at) VALUES (?, ?, ?, ?, ?, ?)",
		m.TaskID, m.Sender, m.SenderTask, m.Kind, m.Text, m.At)
	if err != nil {
		return err
	}
	_, err = s.Exec("DELETE FROM messages WHERE task_id = ? AND id <= (SELECT id FROM messages WHERE task_id = ?"+
		" ORDER BY id DESC LIMIT 1 OFFSET ?)", m.TaskID, m.TaskID, messagesKept)
	return err
}

// Messages lists the messages typed into a task's session, oldest first.
func (s *Store) Messages(taskID string) ([]Message, error) {
	rows, err := s.Query("SELECT id, task_id, sender, sender_task, kind, text, at FROM messages WHERE task_id = ? ORDER BY id",
		taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.TaskID, &m.Sender, &m.SenderTask, &m.Kind, &m.Text, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- events --------------------------------------------------------------------------

// LogEvent keeps one hook payload for debugging, and trims the log now and then.
func (s *Store) LogEvent(sessionID, name, payload string) error {
	res, err := s.Exec("INSERT INTO events (at, session_id, name, payload) VALUES (?, ?, ?, ?)",
		Now(), sessionID, name, payload)
	if err != nil {
		return err
	}
	if id, _ := res.LastInsertId(); id > 0 && id%500 == 0 {
		_, err = s.Exec("DELETE FROM events WHERE id <= ?", id-eventsKept)
	}
	return err
}
