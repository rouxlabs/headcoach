// Package server is the daemon: a small JSON API plus the web board.
// The web page, the TUI and the CLI are all clients.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/ask"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/review"
	"github.com/rouxlabs/headcoach/internal/sh"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
	"github.com/rouxlabs/headcoach/internal/transcript"
	"github.com/rouxlabs/headcoach/internal/usage"
	"github.com/rouxlabs/headcoach/internal/version"
)

//go:embed static/index.html
var indexPage []byte

var (
	//go:embed static/manifest.webmanifest
	manifest []byte
	//go:embed static/icon.svg
	iconSVG []byte
	//go:embed static/icon-192.png
	icon192 []byte
	//go:embed static/icon-512.png
	icon512 []byte
)

// staticFiles are what a phone needs to install the web board as an app.
var staticFiles = map[string]struct {
	kind string
	data []byte
}{
	"manifest.webmanifest": {"application/manifest+json", manifest},
	"icon.svg":             {"image/svg+xml", iconSVG},
	"icon-192.png":         {"image/png", icon192},
	"icon-512.png":         {"image/png", icon512},
}

const (
	agentLinger    = 300 // seconds a finished sub-agent stays visible
	reconcileEvery = 5   // seconds between two looks at tmux
	openingGrace   = 8   // seconds a new session may stay silent before the board assumes it is stuck at a prompt
	logTurns       = 80
	maxBody        = 32 << 20
)

// Tmux is what the daemon needs from tmux. *tmux.Client is the real one.
type Tmux interface {
	Installed() bool
	Available() bool
	Panes(socket string) map[string]bool
	SendText(pane, text, socket string) error
	RenameWindow(pane, name, socket string)
	Capture(pane, socket string) string
	CaptureStyled(pane, socket string) string
	KillPane(pane, socket string)
	SendKeys(pane string, keys []string, socket string) error
	TypeText(pane, text, socket string) error
	Focus(pane, socket string) error
	NewWindow(w tmux.Window) (string, error)
	NewSession(w tmux.Window) (string, error)
}

// Herdr is what the daemon needs from Herdr. *herdr.Client is the real one.
type Herdr interface {
	Agents(socket string) map[string]herdr.Agent
	Prompt(pane, text, socket string) error
	SendKeys(pane string, keys []string, socket string) error
	Capture(pane, socket string) string
	Focus(pane, socket string) error
}

// Server answers the API. Most requests hold its lock, one at a time: each is a few SQLite statements.
// Those that wait on something slow, such as git, a model or an attached terminal, take it only briefly.
type Server struct {
	cfg   *config.Config
	st    *store.Store
	board *board.Board
	tmux  Tmux
	// Herdr talks to sessions that run in a Herdr pane. Tests replace it.
	Herdr Herdr
	// Ask runs Claude Code to answer a question. Tests replace it.
	Ask func(launcher, model, prompt, cwd string) (string, error)
	// Log reads the end of a task's conversation from its transcripts. Tests replace it.
	Log func(paths []string, limit int) []transcript.Turn
	// AttachProcess builds the process an attached client talks to, from the attach command. Tests replace it.
	AttachProcess func(command []string) *exec.Cmd
	// Inspect reads the change in a task's checkout, for the review queue. Tests replace it.
	Inspect func(dir, base string) *review.Change
	// PRState asks the forge about a pull or merge request. Tests replace it.
	PRState     func(url string) review.PR
	attachCheck time.Duration
	// clearWait is how long a reset waits for the cleared session to come back before it briefs it.
	clearWait time.Duration

	mu            sync.Mutex
	lastReconcile float64
	pair          pairing
}

// New builds the daemon over an open store.
func New(cfg *config.Config, st *store.Store, tm Tmux) *Server {
	return &Server{cfg: cfg, st: st, board: board.New(st, cfg), tmux: tm, Herdr: &herdr.Client{}, Ask: ask.Run,
		Log: transcript.Log, AttachProcess: attachProcess, attachCheck: attachCheck, Inspect: review.InspectCached,
		PRState: review.StateOf, clearWait: 15 * time.Second}
}

// httpError is an error with the status the client should get.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

func fail(status int, format string, args ...any) error {
	return &httpError{status, fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type handler func(r *http.Request) (any, error)

// Handler returns the HTTP handler of the daemon.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// locked handlers run one at a time.
	locked := func(pattern string, h handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			value, err := h(r)
			s.mu.Unlock()
			respond(w, value, err)
		})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexPage)
	})
	locked("POST /api/hook", s.hook)
	locked("POST /api/status", s.status)
	locked("GET /api/limits", s.getLimits)
	locked("GET /api/meta", s.meta)
	locked("GET /api/tasks", s.listTasks)
	locked("POST /api/tasks", s.createTask)
	locked("PATCH /api/tasks/{id}", s.patchTask)
	locked("DELETE /api/tasks/{id}", s.deleteTask)
	locked("POST /api/tasks/{id}/start", s.startTask)
	locked("POST /api/tasks/{id}/reply", s.reply)
	locked("GET /api/tasks/{id}/inbox", s.inbox)
	locked("GET /api/tasks/{id}/team", s.team)
	locked("GET /api/tasks/{id}/handoff", s.getHandoff)
	locked("POST /api/tasks/{id}/handoff", s.initHandoff)
	// A reset waits for the cleared session to come back: it locks only while it reads or types.
	mux.HandleFunc("POST /api/tasks/{id}/reset", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.reset(r)
		respond(w, value, err)
	})
	locked("GET /api/tasks/{id}/screen", s.screen)
	locked("POST /api/tasks/{id}/keys", s.keys)
	locked("POST /api/tasks/{id}/focus", s.focus)
	locked("POST /api/tasks/{id}/adopt", s.adopt)
	locked("POST /api/pair/code", s.pairCode)
	mux.HandleFunc("POST /api/pair", s.pairDevice)
	locked("GET /api/devices", s.listDevices)
	locked("DELETE /api/devices/{name}", s.forgetDevice)
	for name, file := range staticFiles {
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", file.kind)
			_, _ = w.Write(file.data)
		})
	}
	locked("GET /api/schedules", s.listSchedules)
	locked("POST /api/schedules", s.createSchedule)
	locked("PATCH /api/schedules/{id}", s.patchSchedule)
	locked("DELETE /api/schedules/{id}", s.deleteSchedule)
	locked("POST /api/schedules/{id}/run", s.runSchedule)
	// An attached terminal lasts as long as you stay: it locks only while it finds the session.
	mux.HandleFunc("GET /api/tasks/{id}/attach", s.attachTask)
	// The log of a long session takes time to read: the detail locks only while it reads the board.
	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.getTask(r)
		respond(w, value, err)
	})
	// The review queue runs git in each checkout: it locks only while it reads the board.
	mux.HandleFunc("GET /api/review", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.reviewQueue(r)
		respond(w, value, err)
	})
	// Asking takes seconds: it locks only while it reads the board, so the board keeps serving meanwhile.
	mux.HandleFunc("POST /api/tasks/{id}/ask", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.askTask(r)
		respond(w, value, err)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, message := 0, ""
		if remote(r) {
			status, message = s.refuseRemote(w, r)
		} else {
			status, message = s.refuse(r)
		}
		if status != 0 {
			writeError(w, status, message)
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			// No route: let the mux decide between 404 and 405, and answer in the shape of every other error.
			probe := &statusOnly{header: http.Header{}}
			mux.ServeHTTP(probe, r)
			if allow := probe.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			writeError(w, probe.status, http.StatusText(probe.status))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		mux.ServeHTTP(w, r)
	})
}

// statusOnly is a response writer that keeps the status and drops the body.
type statusOnly struct {
	header http.Header
	status int
}

func (p *statusOnly) Header() http.Header         { return p.header }
func (p *statusOnly) WriteHeader(status int)      { p.status = status }
func (p *statusOnly) Write(b []byte) (int, error) { return len(b), nil }

func respond(w http.ResponseWriter, value any, err error) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.message)
	case err != nil:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "Internal error: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, value)
	}
}

// refuse rejects what a web page on another origin could send: the API can type into terminals.
// It returns a zero status when the request may go on.
func (s *Server) refuse(r *http.Request) (int, string) {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "[::1]", s.cfg.Host:
	default:
		return http.StatusForbidden, "Unknown host"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if _, rest, found := strings.Cut(origin, "://"); !found || rest != r.Host {
			return http.StatusForbidden, "Requests from other origins are refused"
		}
	}
	// A page on another origin cannot send JSON without asking first, and nothing here answers that question.
	if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return http.StatusUnsupportedMediaType, "Send application/json"
	}
	return 0, ""
}

// body decodes a request body that must be a JSON object.
func body(r *http.Request) (map[string]any, error) {
	var data any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return nil, fail(400, "The request body is not valid JSON")
	}
	object, ok := data.(map[string]any)
	if !ok {
		return nil, fail(400, "The request body must be a JSON object")
	}
	return object, nil
}

// str reads a field as text: a string as it is, a number or a boolean as written, anything else empty.
func str(data map[string]any, key string) string {
	switch v := data[key].(type) {
	case string:
		return v
	case float64, bool:
		return fmt.Sprint(v)
	}
	return ""
}

func truthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	}
	return v != nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// --- helpers -------------------------------------------------------------------------

func (s *Server) find(r *http.Request) (*store.Task, error) {
	if id := r.PathValue("id"); id != "self" {
		return s.resolve(id)
	}
	return s.self(r)
}

// resolve finds a task by its id or a unique prefix of it.
func (s *Server) resolve(id string) (*store.Task, error) {
	task, err := s.st.Resolve(id)
	switch {
	case errors.Is(err, store.ErrAmbiguous):
		return nil, fail(409, "%v", err)
	case err != nil:
		return nil, err
	case task == nil:
		return nil, fail(404, "No task matches '%s'", id)
	}
	return task, nil
}

// liveSession returns the newest session of a task whose pane still exists, or nil.
func (s *Server) liveSession(taskID string) (*store.Session, error) {
	sessions, err := s.st.TaskSessions(taskID)
	if err != nil {
		return nil, err
	}
	for i := len(sessions) - 1; i >= 0; i-- {
		sess := &sessions[i]
		if sess.Live && sess.Pane != "" && s.tmux.Panes(sess.Socket)[sess.Pane] {
			return sess, nil
		}
	}
	return nil, nil
}

// errHerdrUnsure is a session Herdr may still run in one of several panes: nothing reaches it, and resuming
// it could start a second claude on the same conversation.
var errHerdrUnsure = fail(409, "More than one Herdr pane may run this session, all in its directory: answer it in Herdr")

// herdrSession returns the newest session of a task that Herdr still runs, or nil, with HerdrPane set to the
// pane it runs in now and no tmux pane. errHerdrUnsure when it cannot tell which pane that is.
func (s *Server) herdrSession(taskID string) (*store.Session, error) {
	sessions, err := s.st.TaskSessions(taskID)
	if err != nil {
		return nil, err
	}
	for i := len(sessions) - 1; i >= 0; i-- {
		sess := &sessions[i]
		if sess.HerdrPane == "" {
			continue
		}
		pane, err := s.herdrPane(sess)
		if err != nil {
			return nil, err
		}
		if pane != "" {
			sess.Pane, sess.Socket, sess.HerdrPane = "", "", pane
			return sess, nil
		}
	}
	return nil, nil
}

// herdrPane returns the Herdr pane a session runs in now, or "", or errHerdrUnsure. A pane moved to another workspace changes
// its id, and without its Claude Code integration Herdr knows no session id: then the session is the agent
// in its own pane, or else the only agent in its directory, as long as no other session claims that pane.
func (s *Server) herdrPane(sess *store.Session) (string, error) {
	agents := s.Herdr.Agents(sess.HerdrSocket)
	for pane, agent := range agents {
		if agent.Session == sess.SessionID {
			return pane, nil
		}
	}
	if agent, ok := agents[sess.HerdrPane]; ok && agent.Session == "" {
		// One agent runs in a pane at a time: a session started there after this one replaced it.
		newer, err := s.st.Sessions("WHERE herdr_socket = ? AND herdr_pane = ? AND started_at > ? LIMIT 1",
			sess.HerdrSocket, sess.HerdrPane, sess.StartedAt)
		if err != nil || len(newer) == 0 {
			return sess.HerdrPane, err
		}
		return "", nil
	}
	if sess.Cwd == "" {
		return "", nil
	}
	found := ""
	for pane, agent := range agents {
		if agent.Session != "" || agent.Cwd != sess.Cwd {
			continue
		}
		// A pane id another session holds, or held after this one started, is not where this one moved.
		other, err := s.st.Sessions("WHERE herdr_socket = ? AND herdr_pane = ? AND session_id != ?"+
			" AND (live = 1 OR started_at > ?) LIMIT 1", sess.HerdrSocket, pane, sess.SessionID, sess.StartedAt)
		if err != nil {
			return "", err
		}
		if len(other) > 0 {
			continue
		}
		if found != "" {
			return "", errHerdrUnsure
		}
		found = pane
	}
	return found, nil
}

// reachable is liveSession, or else the session Herdr runs, which has no tmux pane: what a message reaches.
func (s *Server) reachable(taskID string) (*store.Session, error) {
	sess, err := s.liveSession(taskID)
	if err != nil || sess != nil {
		return sess, err
	}
	return s.herdrSession(taskID)
}

// transcripts lists the transcript of each session, in the order given.
func transcripts(sessions []store.Session) []string {
	paths := make([]string, len(sessions))
	for i := range sessions {
		paths[i] = sessions[i].TranscriptPath
	}
	return paths
}

// WaitingName is the label of a running task whose main agent only waits on its sub-agents.
const WaitingName = "Waiting"

// private tells whether a session runs in the board's own tmux server, where nobody sees it until they attach.
func (s *Server) private(sess *store.Session) bool {
	return sess != nil && s.cfg.PrivateTmux && sess.Socket == s.cfg.TmuxSocket()
}

func (s *Server) canOpen() bool {
	if s.cfg.PrivateTmux {
		return s.tmux.Installed()
	}
	return s.tmux.Available()
}

// sessionTarget returns the session id to open for a task, and whether that session already exists on disk.
// A session of another profile, or whose transcript is gone, is skipped: claude cannot resume it.
func (s *Server) sessionTarget(task *store.Task) (string, bool, error) {
	sessions, err := s.st.Sessions("WHERE task_id = ? ORDER BY started_at DESC", task.ID)
	if err != nil {
		return "", false, err
	}
	for _, sess := range sessions {
		if s.cfg.HasProfile(task.Profile) && s.cfg.ProfileFor(sess.ConfigDir) != task.Profile {
			continue
		}
		if sess.TranscriptPath != "" && !isFile(sess.TranscriptPath) {
			continue
		}
		if sess.TranscriptPath != "" {
			return sess.SessionID, true, nil
		}
		break
	}
	return task.ID, false, nil
}

func (s *Server) resumeCommand(task *store.Task) (string, error) {
	sid, exists, err := s.sessionTarget(task)
	flag := "--session-id"
	if exists {
		flag = "--resume"
	}
	return s.cfg.Launcher(task.Profile) + launchFlags(task) + " " + flag + " " + sid, err
}

// firstPrompt is the prompt to open a window with: the task's own prompt until a session has really started.
func (s *Server) firstPrompt(task *store.Task) (string, error) {
	_, exists, err := s.sessionTarget(task)
	if exists {
		return "", err
	}
	return task.Prompt, err
}

// reconcile notices sessions whose tmux pane is gone without a SessionEnd, for example after a kill.
func (s *Server) reconcile() error {
	now := store.Now()
	if now-s.lastReconcile < reconcileEvery {
		return nil
	}
	s.lastReconcile = now
	live, err := s.st.Sessions("WHERE live = 1 AND pane != ''")
	if err != nil {
		return err
	}
	for i := range live {
		sess := &live[i]
		known := s.tmux.Panes(sess.Socket)
		// Your own tmux answering nothing means it cannot be reached, not that every session died.
		// The board's server is different: it exits with its last session.
		if len(known) == 0 && !s.private(sess) {
			continue
		}
		if !known[sess.Pane] {
			if err := s.board.CloseSession(sess.SessionID); err != nil {
				return err
			}
		}
	}
	// A session the board opened that has sent no event is waiting at a prompt no hook reports.
	stuck, err := s.st.Tasks("WHERE state = 'running' AND last = ? AND updated_at < ?", board.Opening, now-openingGrace)
	if err != nil {
		return err
	}
	for i := range stuck {
		sess, err := s.liveSession(stuck[i].ID)
		if err != nil {
			return err
		}
		if sess == nil {
			continue
		}
		shown := strings.ToLower(s.tmux.Capture(sess.Pane, sess.Socket))
		question := "The session opened but has not started. Check its terminal."
		if strings.Contains(shown, "trust this folder") || strings.Contains(shown, "trust the files") {
			question = "Waiting at the folder trust prompt."
		}
		_, err = s.st.SetState(stuck[i].ID, "needs", "board", store.Fields{"ask": question, "ask_kind": "dialog"})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) screenText(sess *store.Session) string {
	shown := s.tmux.Capture(sess.Pane, sess.Socket)
	if sess.Pane == "" {
		shown = s.Herdr.Capture(sess.HerdrPane, sess.HerdrSocket)
	}
	lines := strings.Split(shown, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

func isDialog(task *store.Task) bool {
	return task.State == "needs" && (task.AskKind == "permission" || task.AskKind == "dialog")
}

// freeTextRe finds the option of a question dialog that takes a typed answer, its number, and the cursor
// when it is already on it. chatRe finds the option every question dialog ends with.
var (
	freeTextRe = regexp.MustCompile(`^\s*(❯\s*)?([1-9])\.\s+Type something\.?$`)
	chatRe     = regexp.MustCompile(`^\s*(❯\s*)?[1-9]\.\s+Chat about this$`)
	optionRe   = regexp.MustCompile(`^\s*(❯\s*)?[1-9]\.\s`)
)

// freeTextOption finds the free text option of the question dialog at the bottom of a screen: the one
// right above its "Chat about this", the last option on the screen, so that text above another dialog,
// such as a plan, never counts.
func freeTextOption(screen string) []string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !optionRe.MatchString(lines[i]) {
			continue
		}
		if !chatRe.MatchString(lines[i]) {
			return nil
		}
		for j := i - 1; j >= max(0, i-3); j-- {
			if found := freeTextRe.FindStringSubmatch(lines[j]); found != nil {
				return found
			}
		}
		return nil
	}
	return nil
}

// oneLine puts an answer on one line of plain words: a control character would act as a key, Escape cancels.
func oneLine(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " ")
}

// answerDialog types a line in the free text option of the question a session shows: it presses the
// option's number, types the line, and submits it. False when the screen shows no such option.
func (s *Server) answerDialog(sess *store.Session, line string) (bool, error) {
	screen := s.screenText(sess)
	if screen == "" {
		return true, errors.New("the session's terminal could not be read")
	}
	option := freeTextOption(screen)
	if option == nil {
		return false, nil
	}
	// On the option, its number would be typed in the answer; anywhere else, it moves there.
	if option[1] == "" {
		if err := s.tmux.SendKeys(sess.Pane, []string{option[2]}, sess.Socket); err != nil {
			return true, err
		}
	}
	if err := s.tmux.TypeText(sess.Pane, line, sess.Socket); err != nil {
		return true, err
	}
	time.Sleep(250 * time.Millisecond) // the text lands before Enter, or the dialog takes both as one paste
	return true, s.tmux.SendKeys(sess.Pane, []string{"Enter"}, sess.Socket)
}

// view builds what the board shows of a task.
func (s *Server) view(task *store.Task, detail bool) (*api.Task, error) {
	now := store.Now()
	sess, err := s.reachable(task.ID)
	if errors.Is(err, errHerdrUnsure) {
		sess, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	sessions, err := s.st.TaskSessions(task.ID)
	if err != nil {
		return nil, err
	}
	latest := sess
	if latest == nil && len(sessions) > 0 {
		if latest, err = s.st.LatestSession(task.ID); err != nil {
			return nil, err
		}
	}
	var own, sub usage.Tokens
	byAgent := map[string]usage.Tokens{}
	cost := 0.0
	for i := range sessions {
		cost += sessions[i].Cost
		mine, theirs := usage.OfSession(sessions[i].TranscriptPath)
		own = own.Add(mine)
		for id, used := range theirs {
			byAgent[id] = used
		}
	}
	for _, used := range byAgent {
		sub = sub.Add(used)
	}
	// The status line reports the real window. Without it, fall back on the configured size.
	used, window := 0, 0
	if latest != nil {
		used, window = usage.ContextOf(latest.TranscriptPath), latest.Window
	}
	if window == 0 {
		window = max(s.cfg.ContextWindow, 1)
	}
	if used > window {
		window = max(window, 1_000_000) // more than fits: the session runs a larger window than configured
	}
	resume, err := s.resumeCommand(task)
	if err != nil {
		return nil, err
	}
	tags := []string{task.Profile}
	if task.Source == "schedule" {
		tags = append(tags, "schedule")
	}
	if task.PinnedAt > 0 {
		tags = append(tags, "pinned")
	}
	out := &api.Task{
		Task:        *task,
		Short:       board.Short(task.ID),
		StateName:   store.StateNames[task.State],
		Live:        sess != nil,
		Attach:      []string{},
		Age:         int(now - task.CreatedAt),
		Idle:        int(now - task.UpdatedAt),
		CanReply:    !isDialog(task),
		Pinned:      task.PinnedAt > 0,
		TimerLeft:   timerLeft(task, now),
		DeleteLeft:  left(task.DeleteAt, now),
		Tags:        tags,
		Links:       []api.Link{},
		Resume:      resume,
		Tokens:      own.Add(sub),
		AgentTokens: sub,
		Cost:        math.Round(cost*100) / 100,
		Context:     api.Context{Tokens: used, Window: window, Pct: min(100, int(math.RoundToEven(100*float64(used)/float64(window))))},
		Agents:      []api.Agent{},
	}
	if sess != nil && sess.Pane == "" {
		out.Pane, out.Tmux = sess.HerdrPane, "herdr"
		out.Attach = herdr.AttachCommand(sess.HerdrPane, sess.HerdrSocket)
	} else if sess != nil {
		out.Pane, out.Tmux = sess.Pane, "own"
		if s.private(sess) {
			out.Tmux = "hc"
			out.Attach = tmux.AttachCommand(sess.Pane, sess.Socket)
		}
	}
	if latest != nil {
		out.SessionID = latest.SessionID
		out.SessionModel = usage.ModelOf(latest.TranscriptPath)
		out.SessionEffort = latest.Effort
	}
	// Jira and merge request references, with a URL when the board knows where they live.
	if key := board.JiraKey(task); key != "" {
		link := api.Link{Label: key}
		if s.cfg.JiraURL != "" {
			link.URL = strings.TrimRight(s.cfg.JiraURL, "/") + "/" + key
		}
		out.Tags, out.Links = append(out.Tags, "jira"), append(out.Links, link)
	}
	if mr := board.MRNumber(task); mr != "" {
		link := api.Link{Label: "!" + mr}
		if task.Remote != "" {
			link.URL = task.Remote + "/-/merge_requests/" + mr
		}
		out.Tags, out.Links = append(out.Tags, "mr"), append(out.Links, link)
	}

	rows, err := s.st.Query(
		"SELECT coalesce(agent_id, ''), name, descr, persona, state, started_at, coalesce(ended_at, 0) FROM agents"+
			" WHERE task_id = ? AND (state = 'running' OR ended_at > ?) ORDER BY id",
		task.ID, now-agentLinger,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			agent          api.Agent
			id             string
			started, ended float64
		)
		if err := rows.Scan(&id, &agent.Name, &agent.Desc, &agent.Persona, &agent.State, &started, &ended); err != nil {
			rows.Close()
			return nil, err
		}
		if ended == 0 {
			ended = now
		}
		agent.Age = int(ended - started)
		if tokens, ok := byAgent[id]; ok {
			agent.Tokens = &tokens
		}
		out.Agents = append(out.Agents, agent)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// The flag can outlive what it waited on: only a sub-agent or job still running keeps it.
	busy := false
	for _, a := range out.Agents {
		busy = busy || a.State == "running"
	}
	out.Waiting = task.State == "running" && task.Waiting && busy
	if out.Waiting {
		out.StateName = WaitingName
	}
	if !detail {
		return out, nil
	}

	d := &api.Detail{History: []api.Move{}, Sessions: []api.Session{}, Log: []transcript.Turn{}}
	out.Detail = d
	rows, err = s.st.Query("SELECT state, at, by FROM history WHERE task_id = ? ORDER BY id", task.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var move api.Move
		if err := rows.Scan(&move.State, &move.At, &move.By); err != nil {
			rows.Close()
			return nil, err
		}
		if id := store.TaskOf(move.By); id != "" {
			move.By = "task " + board.Short(id)
		}
		d.History = append(d.History, move)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range sessions {
		d.Sessions = append(d.Sessions, api.Session{
			SessionID: sessions[i].SessionID, Live: sessions[i].Live, StartedAt: sessions[i].StartedAt,
		})
	}
	// While a dialog is up, show what the terminal shows so it can be answered from here.
	if sess != nil && !out.CanReply {
		d.Screen = s.screenText(sess)
	}
	d.Handoff = s.readHandoff(task.ID)
	return out, nil
}

// refreshed returns the view of a task after an action changed it.
func (s *Server) refreshed(id string) (any, error) {
	task, err := s.st.Task(id)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fail(404, "No task matches '%s'", id)
	}
	return s.view(task, false)
}

// openWindow starts or resumes the task's session in a new tmux window. It returns the pane id.
func (s *Server) openWindow(task *store.Task, prompt string) (string, error) {
	return s.openWindowBy(task, prompt, "you")
}

// openWindowBy is openWindow with who started it, for the task's history.
func (s *Server) openWindowBy(task *store.Task, prompt, by string) (string, error) {
	if !s.canOpen() {
		if s.cfg.PrivateTmux {
			return "", fail(503, "tmux is not installed, so there is nowhere to open the session")
		}
		return "", fail(503, "tmux is not running, so there is nowhere to open the session")
	}
	command, err := s.resumeCommand(task)
	if err != nil {
		return "", err
	}
	if prompt != "" {
		command += " -- " + sh.Quote(prompt) // after --, so a prompt that starts with a dash is not an option
	}
	cwd := task.Cwd
	if !isDir(cwd) {
		cwd, _ = os.UserHomeDir()
	}
	window := tmux.Window{
		Cwd: cwd, Command: command, Name: board.WindowName(task),
		Env: []string{"HC_TASK=" + task.ID, "HC_URL=" + s.cfg.URL(), "HC_HANDOFF=" + s.cfg.HandoffPath(task.ID)},
	}
	var pane string
	if s.cfg.PrivateTmux {
		window.Socket, window.Conf = s.cfg.TmuxSocket(), s.cfg.TmuxConf()
		pane, err = s.tmux.NewSession(window)
	} else {
		window.Session = s.cfg.TmuxSession
		pane, err = s.tmux.NewWindow(window)
	}
	if err != nil {
		return "", fail(502, "tmux could not open a window: %v", err)
	}
	// Record the pane now so the task is reachable before its first hook arrives.
	sid, _, err := s.sessionTarget(task)
	if err != nil {
		return "", err
	}
	known, err := s.st.Session(sid)
	if err != nil {
		return "", err
	}
	if known != nil {
		_, err = s.st.Exec("UPDATE sessions SET pane = ?, socket = ?, live = 1, ended_at = NULL WHERE session_id = ?",
			pane, s.cfg.TmuxSocket(), sid)
	} else {
		_, err = s.st.Exec(
			"INSERT INTO sessions (session_id, task_id, pane, socket, cwd, started_at) VALUES (?, ?, ?, ?, ?, ?)",
			sid, task.ID, pane, s.cfg.TmuxSocket(), cwd, store.Now())
	}
	if err != nil {
		return "", err
	}
	if err := s.board.Link(sid, task.ID); err != nil {
		return "", err
	}
	_, err = s.st.SetState(task.ID, "running", by, store.Fields{"last": board.Opening})
	return pane, err
}

// closeSessions marks every session of a task as ended, after its terminal was closed on purpose.
func (s *Server) closeSessions(taskID string) error {
	sessions, err := s.st.TaskSessions(taskID)
	if err != nil {
		return err
	}
	for i := range sessions {
		if sessions[i].Live {
			if err := s.board.CloseSession(sessions[i].SessionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- hooks ---------------------------------------------------------------------------

func (s *Server) hook(r *http.Request) (any, error) {
	payload, err := body(r)
	if err != nil {
		return nil, err
	}
	origin := board.Origin{
		Pane:      r.Header.Get("X-Tmux-Pane"),
		ConfigDir: r.Header.Get("X-Config-Dir"),
		TaskHint:  r.Header.Get("X-HC-Task"),
	}
	if socket := r.Header.Get("X-Tmux-Socket"); origin.Pane != "" && socket != "" {
		origin.Socket = config.RealPath(socket)
	}
	// Herdr takes TMUX out of its panes: in a tmux pane, the Herdr variables are those tmux was started with.
	if pane, socket := r.Header.Get("X-Herdr-Pane"), r.Header.Get("X-Herdr-Socket"); pane != "" && socket != "" &&
		origin.Pane == "" {
		origin.HerdrPane, origin.HerdrSocket = pane, config.RealPath(socket)
	}
	hadTask := false
	if sid, ok := payload["session_id"].(string); ok {
		before, err := s.st.Session(sid)
		if err != nil {
			return nil, err
		}
		hadTask = before != nil && before.TaskID != ""
	}
	taskID, err := s.board.Apply(payload, origin)
	if err != nil {
		return nil, err
	}
	if taskID == "" {
		return map[string]any{"task": nil}, nil
	}
	// A session you started yourself gets its window named the moment it becomes a task.
	if origin.Pane != "" && !hadTask {
		task, err := s.st.Task(taskID)
		if err != nil {
			return nil, err
		}
		if task != nil {
			s.tmux.RenameWindow(origin.Pane, board.WindowName(task), origin.Socket)
		}
	}
	return map[string]any{"task": taskID}, nil
}

// status takes one status line update of a session: the size of its context window, its cost, its
// effort level and the usage limits.
func (s *Server) status(r *http.Request) (any, error) {
	payload, err := body(r)
	if err != nil {
		return nil, err
	}
	sid, hasSession := payload["session_id"].(string)
	window, _ := payload["context_window"].(map[string]any)
	if size, ok := window["context_window_size"].(float64); ok && hasSession && size > 0 && size == math.Trunc(size) {
		_, err := s.st.Exec("UPDATE sessions SET window = ? WHERE session_id = ? AND window != ?", int(size), sid, int(size))
		if err != nil {
			return nil, err
		}
	}
	cost, _ := payload["cost"].(map[string]any)
	if spent, ok := cost["total_cost_usd"].(float64); ok && hasSession && spent > 0 {
		// Never let a figure go down: a resumed session may start counting from zero again.
		_, err := s.st.Exec("UPDATE sessions SET cost = ? WHERE session_id = ? AND cost < ?", spent, sid, spent)
		if err != nil {
			return nil, err
		}
	}
	// No effort level, or one claude does not take: the session's model has none, so it shows none.
	effort, _ := payload["effort"].(map[string]any)
	level, _ := effort["level"].(string)
	if !slices.Contains(efforts, level) {
		level = ""
	}
	if hasSession {
		_, err := s.st.Exec("UPDATE sessions SET effort = ? WHERE session_id = ? AND effort != ?", level, sid, level)
		if err != nil {
			return nil, err
		}
	}
	profile := s.cfg.ProfileFor(r.Header.Get("X-Config-Dir"))
	return map[string]any{}, limits.Record(s.st, profile, payload["rate_limits"], store.Now())
}

func (s *Server) getLimits(r *http.Request) (any, error) {
	report, err := limits.Report(s.st, store.Now())
	return map[string]any{"limits": report}, err
}

// --- tasks ---------------------------------------------------------------------------

func (s *Server) meta(r *http.Request) (any, error) {
	seen := map[string]bool{}
	for _, root := range s.cfg.RepoRoots {
		for _, pattern := range []string{"*/.git", "*/*/.git", "*/*/*/.git"} {
			found, _ := filepath.Glob(filepath.Join(root, pattern))
			for _, git := range found {
				seen[filepath.Dir(git)] = true
			}
		}
	}
	rows, err := s.st.Query("SELECT DISTINCT cwd FROM tasks WHERE cwd != ''")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			rows.Close()
			return nil, err
		}
		seen[cwd] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	meta := api.Meta{
		Version: version.Version, Profiles: s.cfg.ProfileNames(), Dirs: dirs,
		Tmux: s.canOpen(), PrivateTmux: s.cfg.PrivateTmux,
	}
	for _, id := range store.States {
		meta.States = append(meta.States, api.State{ID: id, Name: store.StateNames[id]})
	}
	return meta, nil
}

func (s *Server) listTasks(r *http.Request) (any, error) {
	if err := s.reconcile(); err != nil {
		return nil, err
	}
	tasks, err := s.st.Tasks("ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}
	out := api.Tasks{Tasks: []api.Task{}, Version: version.Version}
	for i := range tasks {
		view, err := s.view(&tasks[i], false)
		if err != nil {
			return nil, err
		}
		out.Tasks = append(out.Tasks, *view)
	}
	out.Limits, err = limits.Report(s.st, store.Now())
	return out, err
}

func (s *Server) createTask(r *http.Request) (any, error) {
	data, err := body(r)
	if err != nil {
		return nil, err
	}
	timer := 0
	if _, ok := data["timer"]; ok {
		if timer, err = timerField(data); err != nil {
			return nil, err
		}
	}
	launch := store.Fields{}
	if err := launchFields(data, launch); err != nil {
		return nil, err
	}
	model, _ := launch["model"].(string)
	effort, _ := launch["effort"].(string)
	from, err := s.caller(r)
	if err != nil {
		return nil, err
	}
	// The prompt of a task another agent adds is its first message: it carries a label, the title does not.
	parent, err := s.parentFor(r, str(data, "parent"), "")
	if err != nil {
		return nil, err
	}
	title, prompt := strings.TrimSpace(str(data, "title")), strings.TrimSpace(str(data, "prompt"))
	label := from.label(&store.Task{Parent: parent})
	if label != "" && prompt != "" {
		if title == "" {
			title = board.Clip(board.FirstLine(prompt), 90)
		}
		prompt = board.Labeled(label, prompt)
	}
	task, err := s.addTask(title, prompt, str(data, "cwd"), str(data, "profile"), model, effort,
		str(data, "source"), "", from.by(), truthy(data["start"]), timer)
	if task != nil && parent != "" {
		if err := s.st.UpdateTask(task.ID, store.Fields{"parent": parent}); err != nil {
			return nil, err
		}
	}
	if task != nil && label != "" && prompt != "" {
		if err := s.record(task, from, "message", prompt); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	return s.refreshed(task.ID)
}

// addTask creates a task, and starts its session when start is set. A timer, in seconds, closes it once
// it runs out. A model and an effort, when set, replace the profile's. A schedule creates its tasks here
// too, with its id, so they are checked the same way as one typed in.
func (s *Server) addTask(title, prompt, cwd, profile, model, effort, source, schedule, by string, start bool, timer int) (*store.Task, error) {
	prompt = strings.TrimSpace(prompt)
	title = strings.TrimSpace(title)
	if title == "" {
		title = board.Clip(board.FirstLine(prompt), 90)
	}
	if title == "" {
		return nil, fail(400, "A task needs a title or a prompt")
	}
	cwd = config.ExpandUser(strings.TrimSpace(cwd))
	if cwd != "" && !isDir(cwd) {
		return nil, fail(400, "'%s' is not a directory", cwd)
	}
	profile, err := s.checkProfile(profile)
	if err != nil {
		return nil, err
	}
	if start && !s.canOpen() {
		return nil, fail(503, "tmux is not available, so the task cannot be started. Nothing was added.")
	}
	// No prompt stays no prompt: a title-only task opens an empty session to type the prompt into.
	repo, branch := board.GitInfo(cwd)
	task, err := s.st.CreateTask(store.Task{
		ID: board.NewID(), Title: title, Prompt: prompt, Cwd: cwd, Repo: repo, Branch: branch, Profile: profile,
		Source: source, Last: "Not started", Remote: board.GitRemote(cwd), EndsAt: endsAt(timer),
		Model: model, Effort: effort, Schedule: schedule,
	}, by)
	if err != nil {
		return nil, err
	}
	if start {
		if _, err := s.openWindowBy(task, task.Prompt, by); err != nil {
			return task, err // the task exists, in the backlog: the caller may still point at it
		}
	}
	return task, nil
}

// checkProfile returns the profile to use: the one named, or the first configured one.
func (s *Server) checkProfile(profile string) (string, error) {
	if profile == "" {
		profile = s.cfg.FirstProfile()
	}
	if !s.cfg.HasProfile(profile) {
		return "", fail(400, "Unknown profile '%s'. Known: %s", profile, strings.Join(s.cfg.ProfileNames(), ", "))
	}
	return profile, nil
}

// getTask answers with the detail of a task: what the board knows, then the end of its sessions' log.
func (s *Server) getTask(r *http.Request) (any, error) {
	s.mu.Lock()
	out, paths, err := s.detail(r)
	var sent []store.Message
	if err == nil {
		sent, err = s.st.Messages(out.ID)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out.Log = append(out.Log, s.Log(paths, logTurns)...)
	senders(out.Log, sent)
	for i := range out.Log {
		if out.Log[i].Who == "agent" {
			out.Log[i].HTML = transcript.Render(out.Log[i].Text)
		}
	}
	return out, nil
}

// detail is the part of getTask that reads the board: the task without its log, and the transcripts to read.
func (s *Server) detail(r *http.Request) (*api.Task, []string, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, nil, err
	}
	out, err := s.view(task, true)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := s.st.TaskSessions(task.ID)
	if err != nil {
		return nil, nil, err
	}
	return out, transcripts(sessions), nil
}

func (s *Server) patchTask(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	data, err := body(r)
	if err != nil {
		return nil, err
	}
	hidden, hide := data["hidden"].(bool)
	if _, ok := data["hidden"]; ok && !hide {
		return nil, fail(400, "hidden is true or false")
	}
	pinned, pin := data["pinned"].(bool)
	if _, ok := data["pinned"]; ok && !pin {
		return nil, fail(400, "pinned is true or false")
	}
	// Checked before any change: a refused request changes nothing.
	if state, ok := data["state"].(string); hidden && (ok && state != "done" || !ok && task.State != "done") {
		return nil, fail(400, "Only a done task can be hidden")
	}
	deleteIn := 0
	if _, ok := data["delete_in"]; ok {
		if deleteIn, err = seconds(data, "delete_in", "delete_in"); err != nil {
			return nil, err
		}
	}
	from, err := s.caller(r)
	if err != nil {
		return nil, err
	}
	_, relink := data["parent"]
	parent, err := s.parentFor(r, str(data, "parent"), task.ID)
	if err != nil {
		return nil, err
	}
	if relink {
		if err := s.st.UpdateTask(task.ID, store.Fields{"parent": parent}); err != nil {
			return nil, err
		}
	}
	if _, ok := data["title"]; ok {
		title := strings.TrimSpace(str(data, "title"))
		if title == "" {
			return nil, fail(400, "The title cannot be empty")
		}
		if err := s.st.UpdateTask(task.ID, store.Fields{"title": board.Clip(title, 200)}); err != nil {
			return nil, err
		}
	}
	if _, ok := data["timer"]; ok {
		timer, err := timerField(data)
		if err != nil {
			return nil, err
		}
		if err := s.st.UpdateTask(task.ID, store.Fields{"ends_at": endsAt(timer)}); err != nil {
			return nil, err
		}
	}
	if _, ok := data["delete_in"]; ok {
		if err := s.st.DeleteTaskAt(task.ID, endsAt(deleteIn)); err != nil {
			return nil, err
		}
	}
	if _, ok := data["state"]; ok {
		state := str(data, "state")
		if _, known := store.StateNames[state]; !known {
			return nil, fail(400, "Unknown state '%s'. Known: %s", state, strings.Join(store.States, ", "))
		}
		live, err := s.reachable(task.ID)
		if errors.Is(err, errHerdrUnsure) && state == "parked" {
			live, err = nil, nil // parking leaves Herdr panes alone anyway
		}
		if err != nil {
			return nil, err
		}
		_, started, err := s.sessionTarget(task)
		if err != nil {
			return nil, err
		}
		switch {
		case state == "running" && task.State == "backlog" && !started && live == nil:
			_, err = s.openWindow(task, task.Prompt)
		case state == "running" && task.State == "parked" && live == nil:
			var prompt string
			if prompt, err = s.firstPrompt(task); err == nil {
				_, err = s.openWindow(task, prompt)
			}
		case state == "parked" && task.State != "parked":
			// Parking closes the terminal, but not a Herdr pane. The session stays on disk and resumes from Start.
			if live != nil && live.Pane != "" {
				s.tmux.KillPane(live.Pane, live.Socket)
			}
			if err = s.closeSessions(task.ID); err == nil {
				_, err = s.st.SetState(task.ID, "parked", from.by(), store.Fields{"last": "Parked by " + from.name()})
			}
		case state != task.State:
			_, err = s.st.SetState(task.ID, state, from.by(), store.Fields{"last": "Moved by " + from.name()})
		}
		if err != nil {
			return nil, err
		}
	}
	if hide {
		// After the move: a move to Done shows the task again.
		if err := s.st.HideTask(task.ID, hidden); err != nil {
			return nil, err
		}
	}
	if pin {
		if err := s.st.PinTask(task.ID, pinned); err != nil {
			return nil, err
		}
	}
	return s.refreshed(task.ID)
}

func (s *Server) deleteTask(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	return map[string]string{"deleted": task.ID}, s.removeTask(task.ID)
}

// removeTask deletes a task, and its handoff with it.
func (s *Server) removeTask(id string) error {
	_ = os.Remove(s.cfg.HandoffPath(id)) // there may be none
	return s.st.DeleteTask(id)
}

func (s *Server) startTask(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	if live, err := s.reachable(task.ID); err != nil {
		return nil, err
	} else if live != nil {
		return nil, fail(409, "This task already has a live session")
	}
	prompt, err := s.firstPrompt(task)
	if err != nil {
		return nil, err
	}
	if _, err := s.openWindow(task, prompt); err != nil {
		return nil, err
	}
	return s.refreshed(task.ID)
}

func (s *Server) reply(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	data, err := body(r)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(str(data, "text"))
	if text == "" {
		return nil, fail(400, "The message is empty")
	}
	from, err := s.caller(r)
	if err != nil {
		return nil, err
	}
	label := from.label(task)
	sess, err := s.reachable(task.ID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		// No terminal left: resume the session with the message as its next prompt.
		text = board.Labeled(label, text)
		if _, err := s.openWindow(task, text); err != nil {
			return nil, err
		}
		if err := s.record(task, from, "message", text); err != nil {
			return nil, err
		}
		return s.refreshed(task.ID)
	}
	kind := "message"
	switch {
	case sess.Pane == "" && isDialog(task):
		// Herdr takes no prompt while a dialog shows.
		return nil, fail(409, "The session runs in Herdr: answer its dialog with its keys, or in Herdr.")
	case sess.Pane == "":
		text = board.Labeled(label, text)
		if err := s.Herdr.Prompt(sess.HerdrPane, text, sess.HerdrSocket); err != nil {
			return nil, fail(502, "herdr could not deliver the message: %v", err)
		}
	case isDialog(task):
		if text = oneLine(text); text == "" {
			return nil, fail(400, "The message is empty")
		}
		text, kind = board.Labeled(label, text), "answer"
		// A permission prompt's own typed option rejects the tool: it is never picked for a reply.
		answered := false
		if task.AskKind != "permission" {
			answered, err = s.answerDialog(sess, text)
		}
		if !answered {
			return nil, fail(409, "The dialog takes no typed answer. Answer it with its keys, or in the terminal.")
		}
		if err != nil {
			return nil, fail(502, "tmux could not deliver the answer: %v", err)
		}
	default:
		text = board.Labeled(label, text)
		if err := s.tmux.SendText(sess.Pane, text, sess.Socket); err != nil {
			return nil, fail(502, "tmux could not deliver the message: %v", err)
		}
	}
	if err := s.record(task, from, kind, text); err != nil {
		return nil, err
	}
	if err := s.st.UpdateTask(task.ID, store.Fields{"last": board.Said(text)}); err != nil {
		return nil, err
	}
	return s.refreshed(task.ID)
}

// askTask answers a question about a task's session without sending anything to the session.
func (s *Server) askTask(r *http.Request) (any, error) {
	s.mu.Lock()
	task, question, paths, err := s.askInput(r)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	turns := s.Log(paths, logTurns)
	if len(turns) == 0 {
		return nil, fail(409, "This task has no session to ask about yet")
	}
	// The helper run needs a directory that exists and is not a project: the state directory will do.
	cwd := filepath.Dir(s.cfg.DB)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	answer, err := s.Ask(s.cfg.Launcher(task.Profile), s.cfg.AskModel, ask.BuildPrompt(task, turns, question), cwd)
	if err != nil {
		return nil, fail(502, "%v", err)
	}
	if question = strings.TrimSpace(question); question == "" {
		question = ask.DefaultQuestion
	}
	return map[string]string{"answer": answer, "question": question}, nil
}

// askInput reads the board for askTask: the task, the question and the transcripts to read.
func (s *Server) askInput(r *http.Request) (*store.Task, string, []string, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, "", nil, err
	}
	data, err := body(r)
	if err != nil {
		return nil, "", nil, err
	}
	sessions, err := s.st.TaskSessions(task.ID)
	if err != nil {
		return nil, "", nil, err
	}
	return task, str(data, "question"), transcripts(sessions), nil
}

func (s *Server) screen(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	sess, err := s.reachable(task.ID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, fail(409, "This task has no live session to look at")
	}
	return map[string]string{"screen": s.screenText(sess)}, nil
}

func (s *Server) keys(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	data, err := body(r)
	if err != nil {
		return nil, err
	}
	list, ok := data["keys"].([]any)
	pressed := make([]string, 0, len(list))
	for _, item := range list {
		key, isString := item.(string)
		ok = ok && isString
		pressed = append(pressed, key)
	}
	if !ok {
		return nil, fail(400, "Send a list of key names")
	}
	sess, err := s.reachable(task.ID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, fail(409, "This task has no live session to send keys to")
	}
	if sess.Pane == "" {
		err = s.Herdr.SendKeys(sess.HerdrPane, pressed, sess.HerdrSocket)
	} else {
		err = s.tmux.SendKeys(sess.Pane, pressed, sess.Socket)
	}
	if err != nil {
		return nil, fail(400, "%v", err)
	}
	// Keys carry no label: the record is the only trace of who pressed them.
	from, err := s.caller(r)
	if err != nil {
		return nil, err
	}
	if err := s.record(task, from, "keys", strings.Join(pressed, " ")); err != nil {
		return nil, err
	}
	time.Sleep(300 * time.Millisecond) // let the terminal redraw before reading it back
	return map[string]string{"screen": s.screenText(sess)}, nil
}

func (s *Server) focus(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	sess, err := s.reachable(task.ID)
	if err != nil {
		return nil, err
	}
	if sess != nil && sess.Pane == "" {
		if err := s.Herdr.Focus(sess.HerdrPane, sess.HerdrSocket); err != nil {
			return nil, fail(502, "herdr could not switch to the session: %v", err)
		}
		return s.refreshed(task.ID)
	}
	if s.private(sess) {
		return nil, fail(409, "This session runs in the board's own tmux. Enter it from a terminal with: hc attach %s",
			board.Short(task.ID))
	}
	if sess == nil {
		prompt, err := s.firstPrompt(task)
		if err != nil {
			return nil, err
		}
		if _, err := s.openWindow(task, prompt); err != nil {
			return nil, err
		}
		if sess, err = s.liveSession(task.ID); err != nil {
			return nil, err
		}
	}
	if sess != nil && !s.private(sess) {
		if err := s.tmux.Focus(sess.Pane, sess.Socket); err != nil {
			return nil, fail(502, "tmux could not switch to the session: %v", err)
		}
	}
	return s.refreshed(task.ID)
}

// adopt moves a task's session out of your tmux into the board's own: close it there, resume it here.
func (s *Server) adopt(r *http.Request) (any, error) {
	task, err := s.find(r)
	if err != nil {
		return nil, err
	}
	if !s.cfg.PrivateTmux {
		return nil, fail(409, "private_tmux is off, so there is no board tmux to move the session to")
	}
	if _, started, err := s.sessionTarget(task); err != nil {
		return nil, err
	} else if !started {
		return nil, fail(409, "This task has no session yet. Start it instead.")
	}
	live, err := s.reachable(task.ID)
	if err != nil {
		return nil, err
	}
	if s.private(live) {
		return nil, fail(409, "This session already runs in the board's own tmux")
	}
	if live != nil && live.Pane == "" {
		return nil, fail(409, "This session runs in Herdr: it stays there")
	}
	if live != nil {
		s.tmux.KillPane(live.Pane, live.Socket)
	}
	if err := s.closeSessions(task.ID); err != nil {
		return nil, err
	}
	if _, err := s.openWindow(task, ""); err != nil {
		return nil, err
	}
	return s.refreshed(task.ID)
}
