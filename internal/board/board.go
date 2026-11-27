// Package board is the state machine: Claude Code hook events in, task state out.
//
// Rules, in one place:
//
//	UserPromptSubmit      -> running   (creates the task on the first prompt of an unknown session)
//	SessionStart          -> a /clear you type closes the task as done and opens a new one in backlog,
//	                         compaction and hc reset keep the task
//	PreToolUse            -> running   (AskUserQuestion and ExitPlanMode -> needs)
//	PostToolUse           -> running   when the task was waiting on a permission or dialog
//	Notification          -> needs     for permission prompts and input dialogs
//	Stop                  -> running   (waiting) while sub-agents run, or background jobs with no question,
//	                         needs     when the last message ends with a question,
//	                         review    when a plan waits for a go or the checkout holds a change not shipped,
//	                         else done
//	SessionEnd            -> review    when the session closes mid-run
//	SubagentStart / Stop  -> child rows under the task
//	background_tasks      -> child rows for background commands and monitors, on any event that lists them
//
// A child row belongs to its task, not to a session: a session that forks, as one sent to the
// background does, goes on under a new id and takes the running rows of its task with it.
//
// Backlog and parked are only ever set by hand. Parking also closes the session's terminal.
// Any later event from the session wins over a manual move.
package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/review"
	"github.com/rouxlabs/headcoach/internal/store"
)

// Opening is the last activity of a task whose session the board has just started.
const Opening = "Opening the session"

// Cleared is the title of the task a session goes on with after a /clear you typed, until its first prompt.
const Cleared = "Cleared session"

// resetWindow is how long, in seconds, the session hc reset clears has to start again. The daemon
// waits 15 seconds before it briefs it anyway: this leaves room for a slow start.
const resetWindow = 60

// backgroundPrefix marks the child rows that are background jobs. Sub-agents have their own events.
const backgroundPrefix = "bg:"

// autoArmed is in the description of a monitor Claude Code arms by itself, such as the live updates of a
// published artifact. It can outlive the work by hours, so it does not keep a finished turn running.
const autoArmed = "(auto-armed"

var (
	needsNotifications = map[string]string{
		"permission_prompt":      "permission",
		"agent_needs_input":      "dialog",
		"elicitation_dialog":     "dialog",
		"elicitation_url_dialog": "dialog",
	}
	// Background work a session reports in `background_tasks`, and the name of its child row.
	backgroundTypes = map[string]string{"shell": "command", "monitor": "monitor"}
	eventNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// The session id becomes a file name for the status line, so it must be a plain token.
	sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// planRe matches the personas whose turn ends on a plan that waits for your go.
	planRe = regexp.MustCompile(`(^|[-_])(architect|planner)($|[-_])`)
)

// Payload is one decoded hook payload. Its shape belongs to Claude Code, so every field is read defensively.
type Payload map[string]any

// str returns a field when it is a string, else an empty string.
func (p Payload) str(key string) string {
	s, _ := p[key].(string)
	return s
}

// Origin is what the hook script adds to a payload: where the session runs.
type Origin struct {
	Pane      string // tmux pane id, empty outside tmux
	Socket    string // socket of the tmux server that holds the pane
	ConfigDir string // CLAUDE_CONFIG_DIR of the session
	TaskHint  string // id of the task the board started the session for

	HerdrPane   string // Herdr pane id, empty outside Herdr
	HerdrSocket string // socket of the Herdr server that holds the pane
}

// Board applies events to the database. It holds no state of its own.
type Board struct {
	st      *store.Store
	cfg     *config.Config
	persona *regexp.Regexp // nil when the configured pattern does not compile
}

// New builds a board over a store.
func New(st *store.Store, cfg *config.Config) *Board {
	persona, _ := regexp.Compile(cfg.PersonaPattern)
	return &Board{st: st, cfg: cfg, persona: persona}
}

// --- session bookkeeping -------------------------------------------------------------

// Link files a session under a task, and tells the status line about it.
func (b *Board) Link(sessionID, taskID string) error {
	if _, err := b.st.Exec("UPDATE sessions SET task_id = ? WHERE session_id = ?", taskID, sessionID); err != nil {
		return err
	}
	// Best effort: the board works without the status line file.
	dir := b.cfg.SessionsDir()
	if os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(dir, sessionID), []byte(Short(taskID)), 0o644)
	}
	return nil
}

func (b *Board) touchSession(p Payload, o Origin) (*store.Session, error) {
	sid := p.str("session_id")
	transcript, cwd := p.str("transcript_path"), p.str("cwd")
	sess, err := b.st.Session(sid)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		_, err = b.st.Exec(
			"INSERT INTO sessions (session_id, pane, socket, herdr_pane, herdr_socket, config_dir, transcript_path, cwd,"+
				" started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			sid, o.Pane, o.Socket, o.HerdrPane, o.HerdrSocket, o.ConfigDir, transcript, cwd, store.Now(),
		)
	} else {
		_, err = b.st.Exec(
			"UPDATE sessions SET"+
				" socket = CASE WHEN ?1 != '' THEN ?1 ELSE socket END,"+
				" pane = CASE WHEN ?2 != '' THEN ?2 ELSE pane END,"+
				" config_dir = CASE WHEN ?3 != '' THEN ?3 ELSE config_dir END,"+
				" transcript_path = CASE WHEN ?4 != '' THEN ?4 ELSE transcript_path END,"+
				" cwd = CASE WHEN ?5 != '' THEN ?5 ELSE cwd END,"+
				" herdr_socket = CASE WHEN ?6 != '' THEN ?6 ELSE herdr_socket END,"+
				" herdr_pane = CASE WHEN ?7 != '' THEN ?7 ELSE herdr_pane END WHERE session_id = ?8",
			o.Socket, o.Pane, o.ConfigDir, transcript, cwd, o.HerdrSocket, o.HerdrPane, sid,
		)
		// Only a new start or a new prompt proves the session is alive: a late event must not revive it.
		if name := p.str("hook_event_name"); err == nil && (name == "SessionStart" || name == "UserPromptSubmit") {
			_, err = b.st.Exec("UPDATE sessions SET live = 1, ended_at = NULL WHERE session_id = ?", sid)
		}
	}
	if err != nil {
		return nil, err
	}
	if sess, err = b.st.Session(sid); err != nil {
		return nil, err
	}
	// A cleared or compacted session keeps the hint of the one it replaces: onSessionStart decides.
	if sess.TaskID == "" && !replaces(p) {
		hinted, err := b.hinted(sess, o.TaskHint)
		if err != nil || hinted == nil {
			return sess, err
		}
		if err := b.Link(sid, hinted.ID); err != nil {
			return nil, err
		}
		return b.st.Session(sid)
	}
	return sess, nil
}

// hinted is the task the hint names, when the session may belong to it.
func (b *Board) hinted(sess *store.Session, hint string) (*store.Task, error) {
	if hint == "" {
		return nil, nil
	}
	task, err := b.st.Task(hint)
	if err != nil || task == nil {
		return nil, err
	}
	// A claude run nested in the task's session inherits the hint, but one
	// of another profile is not the task's: its id cannot be resumed there.
	if b.cfg.HasProfile(task.Profile) && b.cfg.ProfileFor(sess.ConfigDir) != task.Profile {
		return nil, nil
	}
	return task, nil
}

// replaces tells whether a payload starts a session in place of another, in the same process: a /clear or a compaction.
func replaces(p Payload) bool {
	source := p.str("source")
	return p.str("hook_event_name") == "SessionStart" && (source == "clear" || source == "compact")
}

// fresh tells whether a task is the one a /clear opened, still waiting for its first prompt.
func fresh(t *store.Task) bool { return t.Source == "session" && t.Prompt == "" }

// personaOf is the persona a skill stands for: its name without the plugin prefix, when it matches the pattern.
func (b *Board) personaOf(skill string) string {
	words := strings.Fields(strings.TrimLeft(skill, "/"))
	if len(words) == 0 || b.persona == nil {
		return ""
	}
	name := words[0][strings.LastIndex(words[0], ":")+1:]
	if name == "" || !b.persona.MatchString(name) {
		return ""
	}
	return name
}

func (b *Board) taskOf(sess *store.Session) (*store.Task, error) {
	if sess.TaskID == "" {
		return nil, nil
	}
	return b.st.Task(sess.TaskID)
}

// --- entry point ---------------------------------------------------------------------

// Apply applies one hook payload. It returns the id of the task it touched, or an empty string.
func (b *Board) Apply(p Payload, o Origin) (string, error) {
	name, sid := p.str("hook_event_name"), p.str("session_id")
	if !eventNameRe.MatchString(name) || !sessionIDRe.MatchString(sid) {
		return "", nil
	}
	raw, _ := json.Marshal(p)
	if err := b.st.LogEvent(sid, name, cut(string(raw), 20000)); err != nil {
		return "", err
	}
	sess, err := b.touchSession(p, o)
	if err != nil {
		return "", err
	}
	if err := b.syncBackground(p, sess); err != nil {
		return "", err
	}
	switch name {
	case "SessionStart":
		err = b.onSessionStart(p, sess, o)
	case "UserPromptSubmit":
		err = b.onUserPromptSubmit(p, sess)
	case "PreToolUse":
		err = b.onPreToolUse(p, sess)
	case "PostToolUse":
		err = b.onPostToolUse(p, sess)
	case "Notification":
		err = b.onNotification(p, sess)
	case "Stop":
		err = b.onStop(p, sess)
	case "SubagentStart":
		err = b.onSubagentStart(p, sess)
	case "SubagentStop":
		err = b.onSubagentStop(p, sess)
	case "SessionEnd":
		err = b.CloseSession(sess.SessionID)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if sess, err = b.st.Session(sid); err != nil || sess == nil {
		return "", err
	}
	return sess.TaskID, nil
}

// cut shortens a string to at most n bytes without splitting a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// text renders a JSON value that should be an id: a string, or a number written without a fraction.
func text(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// clipJob shortens the description of a background job, keeping the mark of a monitor Claude Code armed.
func clipJob(descr string) string {
	const room = 120
	i := strings.Index(descr, autoArmed)
	tail := Clip(descr[max(i, 0):], room/2)
	if i < 0 || !strings.HasPrefix(tail, autoArmed) {
		return Clip(descr, room)
	}
	return strings.TrimSpace(Clip(descr[:i], room-utf8.RuneCountInString(tail)-1) + " " + tail)
}

// syncBackground mirrors the session's background commands and monitors as child rows of its task.
//
// Nothing fires when one of them ends, so the list on the next event is the only way to close a row.
func (b *Board) syncBackground(p Payload, sess *store.Session) error {
	listed, ok := p["background_tasks"].([]any)
	task, err := b.taskOf(sess)
	if err != nil || task == nil || !ok {
		return err
	}
	type job struct{ name, descr string }
	running := map[string]job{}
	for _, item := range listed {
		entry, _ := item.(map[string]any)
		id := text(entry["id"])
		kind, _ := entry["type"].(string)
		if id == "" || backgroundTypes[kind] == "" || entry["status"] != "running" {
			continue
		}
		descr, _ := entry["description"].(string)
		if descr == "" {
			command, _ := entry["command"].(string)
			descr = FirstLine(command)
		}
		running[backgroundPrefix+id] = job{backgroundTypes[kind], clipJob(descr)}
	}
	rows, err := b.st.Query(
		"SELECT id, agent_id FROM agents WHERE session_id = ? AND state = 'running' AND agent_id LIKE ?",
		sess.SessionID, backgroundPrefix+"%",
	)
	if err != nil {
		return err
	}
	known := map[string]int64{}
	for rows.Next() {
		var (
			id      int64
			agentID string
		)
		if err := rows.Scan(&id, &agentID); err != nil {
			rows.Close()
			return err
		}
		known[agentID] = id
	}
	if err := rows.Close(); err != nil {
		return err
	}
	changed := false
	for agentID, id := range known {
		if _, still := running[agentID]; still {
			continue
		}
		if _, err := b.st.Exec("UPDATE agents SET state = 'done', ended_at = ? WHERE id = ?", store.Now(), id); err != nil {
			return err
		}
		changed = true
	}
	for agentID, j := range running {
		if _, seen := known[agentID]; seen {
			continue
		}
		_, err := b.st.Exec(
			"INSERT INTO agents (task_id, session_id, agent_id, name, descr, started_at) VALUES (?, ?, ?, ?, ?, ?)",
			task.ID, sess.SessionID, agentID, j.name, j.descr, store.Now(),
		)
		if err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return b.st.UpdateTask(task.ID, nil)
	}
	return nil
}

// --- handlers ------------------------------------------------------------------------

func (b *Board) onSessionStart(p Payload, sess *store.Session, o Origin) error {
	source := p.str("source")
	if source == "fork" && sess.TaskID != "" {
		// The fork is where the sub-agents and jobs now run, and the only session that will report them.
		_, err := b.st.Exec(
			"UPDATE agents SET session_id = ? WHERE task_id = ? AND state = 'running'", sess.SessionID, sess.TaskID)
		return err
	}
	if sess.TaskID != "" || !replaces(p) {
		return nil
	}
	// /clear and compaction start a new session id in the same pane and process. The pane says which task
	// it was on: the hint stays the one the board started the process for, even after a /clear.
	task, err := b.paneTask(sess)
	if err == nil && task == nil {
		task, err = b.hinted(sess, o.TaskHint)
	}
	if err != nil || task == nil {
		return err
	}
	if source == "clear" {
		reset, err := b.resetting(task)
		if err != nil {
			return err
		}
		if reset {
			return b.Link(sess.SessionID, task.ID)
		}
		return b.cleared(p, sess, task)
	}
	if task.State == "done" {
		// A task you, or another agent, closed by hand stays closed: what follows is a new task. One the
		// board closed goes on.
		var by string
		err = b.st.QueryRow("SELECT by FROM history WHERE task_id = ? ORDER BY id DESC LIMIT 1", task.ID).Scan(&by)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if by == "you" || by == FromAgent || store.TaskOf(by) != "" {
			return nil
		}
	}
	return b.Link(sess.SessionID, task.ID)
}

// paneTask is the task of the latest other session in the same tmux or Herdr pane, or nil.
func (b *Board) paneTask(sess *store.Session) (*store.Task, error) {
	var where string
	var args []any
	switch {
	case sess.Pane != "":
		where, args = "pane = ?", []any{sess.Pane}
	case sess.HerdrPane != "":
		where, args = "herdr_socket = ? AND herdr_pane = ?", []any{sess.HerdrSocket, sess.HerdrPane}
	default:
		return nil, nil
	}
	var prev sql.NullString
	err := b.st.QueryRow(
		"SELECT task_id FROM sessions WHERE "+where+" AND session_id != ? ORDER BY started_at DESC, rowid DESC LIMIT 1",
		append(args, sess.SessionID)...,
	).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && prev.String == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b.st.Task(prev.String)
}

// resetting tells whether hc reset typed the /clear that cleared a task's session. A reset counts once:
// it must be newer than every session of the task, so a /clear you type after it is yours.
func (b *Board) resetting(task *store.Task) (bool, error) {
	var n int
	err := b.st.QueryRow(
		"SELECT count(*) FROM messages WHERE task_id = ?1 AND kind = 'reset' AND at >= ?2"+
			" AND at > (SELECT coalesce(max(started_at), 0) FROM sessions WHERE task_id = ?1)",
		task.ID, store.Now()-resetWindow,
	).Scan(&n)
	return n > 0, err
}

// cleared closes the task of a session you cleared and opens a new one for what comes next in it.
func (b *Board) cleared(p Payload, sess *store.Session, task *store.Task) error {
	if fresh(task) {
		return b.Link(sess.SessionID, task.ID) // cleared again before its first prompt: nothing to close
	}
	if _, err := b.st.SetState(task.ID, "done", FromYou, nil); err != nil {
		return err
	}
	if sess.Pane != "" || sess.HerdrPane != "" {
		// Ended before its SessionEnd arrives, if it ever does: a reply to the old task must not reach the pane.
		_, err := b.st.Exec("UPDATE sessions SET live = 0, ended_at = ? WHERE task_id = ? AND live = 1"+
			" AND pane = ? AND herdr_socket = ? AND herdr_pane = ?",
			store.Now(), task.ID, sess.Pane, sess.HerdrSocket, sess.HerdrPane)
		if err != nil {
			return err
		}
	}
	taskID, err := b.newTaskID(sess.SessionID)
	if err != nil {
		return err
	}
	cwd := p.str("cwd")
	if cwd == "" {
		cwd = sess.Cwd
	}
	repo, branch := GitInfo(cwd)
	_, err = b.st.CreateTask(store.Task{
		ID: taskID, Title: Cleared, Cwd: cwd, Repo: repo, Branch: branch, Source: "session", Remote: GitRemote(cwd),
		Profile: b.cfg.ProfileFor(sess.ConfigDir), Last: "Context cleared",
	}, "hook")
	if err != nil {
		return err
	}
	return b.Link(sess.SessionID, taskID)
}

// newTaskID is the id of a task a session starts: the session's id, unless a deleted task once had it.
func (b *Board) newTaskID(sid string) (string, error) {
	taken, err := b.st.Task(sid)
	if err != nil {
		return "", err
	}
	if taken != nil {
		return NewID(), nil
	}
	return sid, nil
}

func (b *Board) onUserPromptSubmit(p Payload, sess *store.Session) error {
	prompt := p.str("prompt")
	persona := ""
	if strings.HasPrefix(prompt, "/") {
		persona = b.personaOf(prompt)
	}
	last := Said(prompt)
	task, err := b.taskOf(sess)
	if err != nil {
		return err
	}
	if task != nil {
		fields := store.Fields{"last": last}
		if persona != "" {
			fields["persona"] = persona
		}
		if task.Base == "" {
			// A task added to the backlog starts from where the checkout is when it runs.
			fields["base"] = GitHead(task.Cwd)
		}
		if fresh(task) {
			fields["title"], fields["prompt"] = TitleFromPrompt(prompt), prompt
		}
		_, err = b.st.SetState(task.ID, "running", "hook", fields)
		return err
	}
	sid := sess.SessionID
	taskID, err := b.newTaskID(sid)
	if err != nil {
		return err
	}
	cwd := p.str("cwd")
	if cwd == "" {
		cwd = sess.Cwd
	}
	repo, branch := GitInfo(cwd)
	_, err = b.st.CreateTask(store.Task{
		ID: taskID, Title: TitleFromPrompt(prompt), State: "running", Prompt: prompt, Cwd: cwd, Repo: repo,
		Branch: branch, Source: "session", Remote: GitRemote(cwd), Profile: b.cfg.ProfileFor(sess.ConfigDir),
		Persona: persona, Last: last, Base: GitHead(cwd),
	}, "hook")
	if err != nil {
		return err
	}
	return b.Link(sid, taskID)
}

func (b *Board) onPreToolUse(p Payload, sess *store.Session) error {
	task, err := b.taskOf(sess)
	if err != nil || task == nil {
		return err
	}
	if agentID := text(p["agent_id"]); agentID != "" {
		return b.agentPersona(p, sess, task, agentID) // activity inside a sub-agent does not describe the task itself
	}
	tool := p.str("tool_name")
	input, _ := p["tool_input"].(map[string]any)
	field := func(key string) string {
		s, _ := input[key].(string)
		return s
	}
	switch tool {
	case "AskUserQuestion":
		ask := ""
		if questions, _ := input["questions"].([]any); len(questions) > 0 {
			first, _ := questions[0].(map[string]any)
			ask, _ = first["question"].(string)
		}
		if ask == "" {
			ask = "The agent has a question"
		}
		_, err = b.st.SetState(task.ID, "needs", "hook", store.Fields{"ask": Clip(ask, 300), "ask_kind": "dialog"})
		return err
	case "ExitPlanMode":
		_, err = b.st.SetState(task.ID, "needs", "hook",
			store.Fields{"ask": "A plan is ready for your approval", "ask_kind": "dialog"})
		return err
	case "Agent", "Task":
		name := field("subagent_type")
		if name == "" {
			name = "agent"
		}
		_, err = b.st.Exec(
			"INSERT INTO agents (task_id, session_id, name, descr, started_at) VALUES (?, ?, ?, ?, ?)",
			task.ID, sess.SessionID, name, Clip(field("description"), 120), store.Now(),
		)
		if err != nil {
			return err
		}
	}
	fields := store.Fields{"last": SummarizeTool(tool, p["tool_input"])}
	if tool == "Agent" || tool == "Task" {
		// A foreground sub-agent blocks the main agent until it returns; a background one does not.
		background, _ := input["run_in_background"].(bool)
		fields["waiting"] = !background
	}
	if tool == "Skill" {
		if persona := b.personaOf(field("skill")); persona != "" {
			fields["persona"] = persona
		}
	}
	_, err = b.st.SetState(task.ID, "running", "hook", fields)
	return err
}

// agentPersona records the persona a sub-agent takes on when it loads a skill that matches the pattern.
func (b *Board) agentPersona(p Payload, sess *store.Session, task *store.Task, agentID string) error {
	if p.str("tool_name") != "Skill" {
		return nil
	}
	input, _ := p["tool_input"].(map[string]any)
	skill, _ := input["skill"].(string)
	persona := b.personaOf(skill)
	if persona == "" {
		return nil
	}
	res, err := b.st.Exec("UPDATE agents SET persona = ? WHERE task_id = ? AND agent_id = ?", persona, task.ID, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return b.st.UpdateTask(task.ID, nil)
	}
	// Its SubagentStart has not arrived yet.
	if err := b.trackAgent(p, sess, task, agentID); err != nil {
		return err
	}
	_, err = b.st.Exec("UPDATE agents SET persona = ? WHERE task_id = ? AND agent_id = ?", persona, task.ID, agentID)
	return err
}

func (b *Board) onPostToolUse(p Payload, sess *store.Session) error {
	task, err := b.taskOf(sess)
	if err != nil || task == nil || task.State != "needs" {
		return err
	}
	// A permission prompt can come from a sub-agent, so any finished tool call clears it.
	// Answering a sub-agent's prompt does not wake the main agent: it keeps waiting.
	if task.AskKind == "permission" || p["agent_id"] == nil || p["agent_id"] == "" {
		fromAgent := p["agent_id"] != nil && p["agent_id"] != ""
		_, err = b.st.SetState(task.ID, "running", "hook", store.Fields{"waiting": fromAgent && task.Waiting})
	}
	return err
}

func (b *Board) onNotification(p Payload, sess *store.Session) error {
	task, err := b.taskOf(sess)
	if err != nil || task == nil {
		return err
	}
	// A question or a plan put up by its own hook is the dialog this notification is about: its text says more.
	if task.State == "needs" && task.AskKind == "dialog" {
		return nil
	}
	message := p.str("message")
	kind := needsNotifications[p.str("notification_type")]
	if kind == "" && p.str("notification_type") == "" && strings.Contains(strings.ToLower(message), "permission") {
		kind = "permission"
	}
	if kind == "" {
		return nil
	}
	if message == "" {
		message = "The agent is waiting for you"
	}
	_, err = b.st.SetState(task.ID, "needs", "hook", store.Fields{"ask": Clip(message, 300), "ask_kind": kind})
	return err
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (b *Board) onStop(p Payload, sess *store.Session) error {
	task, err := b.taskOf(sess)
	if err != nil || task == nil {
		return err
	}
	// A spawn that never reported an agent id cannot be tracked past the end of the turn.
	_, err = b.st.Exec(
		"UPDATE agents SET state = 'done', ended_at = ? WHERE session_id = ? AND agent_id IS NULL AND state = 'running'",
		store.Now(), sess.SessionID,
	)
	if err != nil {
		return err
	}
	cwd := p.str("cwd")
	if cwd == "" {
		cwd = task.Cwd
	}
	fields := store.Fields{}
	if _, branch := GitInfo(cwd); branch != "" {
		fields["branch"] = branch
	}
	var jobs, total int
	err = b.st.QueryRow(
		"SELECT coalesce(sum(agent_id LIKE ?), 0), count(*) FROM agents WHERE task_id = ? AND state = 'running'"+
			" AND NOT (name = 'monitor' AND instr(descr, ?) > 0)",
		backgroundPrefix+"%", task.ID, autoArmed,
	).Scan(&jobs, &total)
	if err != nil {
		return err
	}
	message := p.str("last_assistant_message")
	ask := ClosingQuestion(message)
	if ask == "" && !planRe.MatchString(task.Persona) {
		// A plan persona that asks for your go leaves its plan in Review, below.
		ask = ClosingAsk(message)
	}
	state := "done"
	switch running := total - jobs; {
	case running > 0:
		state, fields["last"], fields["waiting"] = "running", "Waiting for "+plural(running, "sub-agent"), true
	case jobs > 0 && ask == "":
		// A background job can run for hours: a question to you must not hide behind it.
		state, fields["last"], fields["waiting"] = "running", "Waiting for "+plural(jobs, "background job"), true
	case ask != "":
		state, fields["ask"], fields["ask_kind"] = "needs", ask, "question"
	default:
		fields["last"] = Clip(markupRe.ReplaceAllString(FirstLine(message), ""), 140)
		if fields["last"] == "" {
			fields["last"] = "Turn finished"
		}
		// Done needs nothing from you; Review has something for you to read first.
		if planRe.MatchString(task.Persona) || review.Unshipped(cwd) {
			state = "review"
		}
	}
	_, err = b.st.SetState(task.ID, state, "hook", fields)
	return err
}

func (b *Board) onSubagentStart(p Payload, sess *store.Session) error {
	task, err := b.taskOf(sess)
	agentID := text(p["agent_id"])
	if err != nil || task == nil || agentID == "" {
		return err
	}
	return b.trackAgent(p, sess, task, agentID)
}

// trackAgent gives a sub-agent its row, once: the row its spawn left when it was seen as a tool
// call, or a new one. Hooks arrive in any order, so a sub-agent's first tool call can come first.
func (b *Board) trackAgent(p Payload, sess *store.Session, task *store.Task, agentID string) error {
	sid := sess.SessionID
	var id int64
	err := b.st.QueryRow("SELECT id FROM agents WHERE task_id = ? AND agent_id = ?", task.ID, agentID).Scan(&id)
	if err == nil {
		return nil // already known: delivered twice, its tool call came first, or a fork announces it again
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	agentType := p.str("agent_type")
	if agentType == "" {
		agentType = "agent"
	}
	// The spawn was seen as a tool call first: adopt that row, by type when one matches.
	const untracked = "SELECT id FROM agents WHERE session_id = ? AND agent_id IS NULL AND state = 'running'"
	err = b.st.QueryRow(untracked+" AND name = ? ORDER BY id LIMIT 1", sid, agentType).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = b.st.QueryRow(untracked+" ORDER BY id LIMIT 1", sid).Scan(&id)
	}
	switch {
	case err == nil:
		_, err = b.st.Exec("UPDATE agents SET agent_id = ?, name = ? WHERE id = ?", agentID, agentType, id)
	case errors.Is(err, sql.ErrNoRows):
		_, err = b.st.Exec(
			"INSERT INTO agents (task_id, session_id, agent_id, name, started_at) VALUES (?, ?, ?, ?, ?)",
			task.ID, sid, agentID, agentType, store.Now(),
		)
	}
	if err != nil {
		return err
	}
	return b.st.UpdateTask(task.ID, nil)
}

func (b *Board) onSubagentStop(p Payload, sess *store.Session) error {
	agentID := text(p["agent_id"])
	if agentID == "" || sess.TaskID == "" {
		return nil
	}
	// By task: after a fork, the session that reports the end may not be the one the row carries.
	_, err := b.st.Exec(
		"UPDATE agents SET state = 'done', ended_at = ? WHERE task_id = ? AND agent_id = ? AND state = 'running'",
		store.Now(), sess.TaskID, agentID,
	)
	if err != nil {
		return err
	}
	return b.st.UpdateTask(sess.TaskID, nil)
}

// CloseSession marks a session as ended. Its task moves to review when it was running and no other
// session of the task is alive.
func (b *Board) CloseSession(sessionID string) error {
	now := store.Now()
	sess, err := b.st.Session(sessionID)
	if err != nil || sess == nil {
		return err
	}
	if _, err := b.st.Exec("UPDATE sessions SET live = 0, ended_at = ? WHERE session_id = ?", now, sessionID); err != nil {
		return err
	}
	_, err = b.st.Exec(
		"UPDATE agents SET state = 'done', ended_at = ? WHERE session_id = ? AND state = 'running'", now, sessionID)
	if err != nil {
		return err
	}
	task, err := b.taskOf(sess)
	if err != nil || task == nil || task.State != "running" {
		return err
	}
	var other int
	err = b.st.QueryRow(
		"SELECT count(*) FROM sessions WHERE task_id = ? AND live = 1 AND session_id != ?", task.ID, sessionID,
	).Scan(&other)
	if err != nil || other > 0 {
		return err
	}
	_, err = b.st.SetState(task.ID, "review", "hook", store.Fields{"last": "Session closed"})
	return err
}
