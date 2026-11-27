// Package tui is `hc tui`: the terminal board. One table of tasks and their sub-agents, in the style
// of k9s or top.
package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
	"github.com/rouxlabs/headcoach/internal/client"
	"github.com/rouxlabs/headcoach/internal/config"
	"github.com/rouxlabs/headcoach/internal/format"
	"github.com/rouxlabs/headcoach/internal/herdr"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/tmux"
	"github.com/rouxlabs/headcoach/internal/update"
	"github.com/rouxlabs/headcoach/internal/version"
)

const (
	refresh = 2 * time.Second       // between two polls of the daemon
	frame   = 60 * time.Millisecond // between two frames of the waiting box
	// glowStep is between two frames of a task lit up as it moves to Done.
	glowStep = 250 * time.Millisecond
	askWait  = 150 * time.Second
	// deleteDelay is how long X leaves a task on the board before the daemon deletes it.
	deleteDelay = 10 * time.Minute
	// permission is why a reply cannot answer a permission prompt.
	permission = "The session is asking for a permission. Answer it in the terminal."
)

// view is what the screen shows.
type view int

const (
	viewBoard     view = iota // the table
	viewDetail                // one task in full
	viewPick                  // the state picker of the move action
	viewProfile               // the profile picker of a new task
	viewNote                  // a box of text: the help, an answer, or a y/n question
	viewWaiting               // the loading box while an answer is on its way
	viewSchedules             // the table of schedules
	viewReview                // the review queue
	viewColumns               // the list of the board's columns, to show, hide and move them
	viewTeam                  // the coordinator view of one task
)

// --- messages ------------------------------------------------------------------------

type (
	// tickMsg asks for the next poll of the daemon.
	tickMsg struct{}
	// frameMsg asks for the next frame of the waiting box.
	frameMsg struct{}
	// glowMsg asks for the next frame of the tasks lit up as they move to Done.
	glowMsg struct{}
	// boardMsg carries one poll: the tasks and the limits, or why the daemon did not answer.
	boardMsg struct {
		board api.Tasks
		err   error
	}
	// checkedMsg carries what :update found on GitHub.
	checkedMsg struct {
		rel   update.Release
		newer bool
		err   error
	}
	// binaryMsg says which version the hc binary on disk is, once the daemon reports another one.
	binaryMsg struct{ daemon, binary string }
	// detailMsg carries the task of the detail view.
	detailMsg struct {
		task api.Task
		err  error
	}
	// doneMsg tells how an action went.
	doneMsg struct {
		notice string // what to tell the user on success
		err    error
	}
	// answerMsg carries the answer to a question about a task.
	answerMsg struct {
		asked    int // which question this answers: one given up on is no longer wanted
		taskID   string
		answer   string
		question string
		err      error
	}
	// attachedMsg says the terminal is back from a session.
	attachedMsg struct {
		taskID string
		err    error
	}
	// mkdirMsg says how creating the folder of a new task went.
	mkdirMsg struct {
		dir string
		err error
	}
	// newTaskMsg carries the task just created, so the terminal can drop into its session. When the
	// daemon refused it, it carries the question, the title typed, the folder and the profile, to ask again.
	newTaskMsg struct {
		task                       api.Task
		err                        error
		asked, title, dir, profile string
	}
)

// model is the whole state of the board.
type model struct {
	client   *client.Client
	daemon   string // the daemon's version, once it differs from this board's
	reload   bool   // quit to run the updated binary in place of this one
	checking bool   // a release check is under way: a second u or :update waits for it
	width    int
	height   int
	workdir  string   // the folder a bare name typed into "New task" resolves under
	profiles []string // the configured profiles, in the order of the config file
	newDir   string   // the folder of the task being created
	// newProfile is the profile of the task being created; empty means the daemon's default.
	newProfile  string
	pickProfile int    // the cursor of the profile picker
	creating    string // what the new-item flow builds: "task", or "schedule"
	// sending counts the requests of the new-item flows still on their way: the folder being created
	// and the tasks being sent. The board does not reload under them, their answer would be lost.
	sending int
	// The list of the folders that complete the path being typed, drawn above the input line once
	// tab asked for it.
	listing      bool     // the list is open
	folders      []string // what it holds, as values for the input
	folderNote   string   // what it says when it holds nothing
	folderCursor int      // the folder chosen with the arrows, -1 before any
	folderTop    int      // index of the first folder on screen

	schedules   []api.Schedule
	schedCursor int
	draft       scheduleDraft // the schedule being created

	team        api.Team
	teamID      string // the coordinator the view shows
	teamCursor  int
	teamLoading bool  // a poll of the view is on its way
	teamWidths  []int // the widths of its columns, from every task

	review        api.Review
	reviewCursor  int
	reviewLoading bool  // a poll of the queue is on its way
	reviewWidths  []int // the widths of the queue's columns, from every item

	tasks    []api.Task
	limits   []limits.Entry
	down     string // why the daemon is unreachable, empty when it answers
	showDone bool
	order    string
	needle   string // the filter text

	rows   []format.Row // what the table shows, after filter and order
	widths []int        // the widths of its columns, from every row so they keep still while it scrolls
	keys   []string     // the key of each row: a task id, or "task id/child index"
	cursor int
	top    int    // index of the first row on screen
	jump   string // a task id the cursor should move to at the next redraw

	// The cursor and the scroll a reload saved, of the table and of the detail, put back once their
	// content is in.
	restoreRow, restoreTop, restoreDetail int

	layout     []choice // the columns of the board, in order, and whether each is shown
	layoutPath string   // the file the layout is saved to; empty: not saved
	colCursor  int      // the cursor of the columns view

	selecting bool            // select mode: space ticks tasks, x, m and p act on the ticked ones
	ticked    map[string]bool // the ids of the tasks ticked in select mode

	lit     map[string]time.Time // when the board saw each task move to Done, while it is lit up
	glowing bool                 // a glowMsg is on its way

	notice      string // one line of feedback, shown until it expires
	noticeAlarm bool
	noticeUntil time.Time

	view view

	input    textinput.Model
	asking   string // what the input line is for, such as reply, title, newdir or schedule-at. Empty when it is closed
	replyTo  string
	renameID string // the task the title input renames

	detail   viewport.Model
	detailID string

	pickTask  api.Task
	pickBatch bool // the picker moves the ticked tasks of select mode, not pickTask
	pick      int

	noteTitle  string
	noteText   string
	confirm    func() tea.Cmd // set when the note is a y/n question: what y does
	deny       func() tea.Cmd // what n does, when it does more than close the note
	noteSends  bool           // y starts a request of a new-item flow, counted in sending
	batchKey   string         // the key of select mode the question is for, x, m, p or h; empty for others
	batchState string         // the state m moves the ticked tasks to
	noteBack   view           // the view the note returns to

	waitTitle string
	ticks     int
	asked     int // counts the questions, so that a late answer to an abandoned one is dropped
}

func newModel(c *client.Client) model {
	input := textinput.New()
	input.Prompt = "> "
	input.CharLimit = 0
	return model{client: c, order: "state", input: input, width: 120, height: 30, detail: viewport.New(120, 28),
		layout: defaultLayout()}
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.poll(), tick()}
	switch m.view { // a view a reload put back loads at once, not at the next tick
	case viewDetail:
		cmds = append(cmds, m.loadDetail())
	case viewSchedules:
		cmds = append(cmds, m.loadSchedules())
	case viewReview:
		cmds = append(cmds, m.loadReview())
	case viewTeam:
		cmds = append(cmds, m.loadTeam())
	}
	return tea.Batch(cmds...)
}

func tick() tea.Cmd { return tea.Tick(refresh, func(time.Time) tea.Msg { return tickMsg{} }) }

func nextFrame() tea.Cmd { return tea.Tick(frame, func(time.Time) tea.Msg { return frameMsg{} }) }

func nextGlow() tea.Cmd { return tea.Tick(glowStep, func(time.Time) tea.Msg { return glowMsg{} }) }

// --- commands: everything that talks to the daemon runs off the UI loop ---------------

func (m model) poll() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		out, err := c.Board()
		return boardMsg{out, err}
	}
}

func (m model) loadDetail() tea.Cmd {
	c, id := m.client, m.detailID
	return func() tea.Msg {
		task, err := c.Task(id)
		return detailMsg{task, err}
	}
}

// act calls the daemon. notice builds the feedback from the task the daemon answered with.
func (m model) act(method, path string, in any, notice func(t api.Task) string) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		t, err := c.Do(method, path, in)
		done := doneMsg{err: err}
		if err == nil && notice != nil {
			done.notice = notice(t)
		}
		return done
	}
}

func (m model) question(taskID, text string) tea.Cmd {
	c, asked := m.client, m.asked
	return func() tea.Msg {
		var out struct {
			Answer   string `json:"answer"`
			Question string `json:"question"`
		}
		err := c.CallFor(askWait, "POST", "/api/tasks/"+taskID+"/ask", map[string]string{"question": text}, &out)
		return answerMsg{asked: asked, taskID: taskID, answer: out.Answer, question: out.Question, err: err}
	}
}

// attach hands the terminal to the session, the way k9s drops you into a shell. Leaving it comes
// back here. fromAgent is set when a sub-agent's row was entered.
func (m model) attach(t api.Task, fromAgent bool) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		command, err := c.Enter(t)
		if err != nil {
			return doneMsg{err: err}
		}
		if len(command) == 0 && t.Live {
			// The session lives in your own tmux: the daemon switches your client to its pane.
			_, err := c.Do("POST", "/api/tasks/"+t.ID+"/focus", nil)
			if fromAgent && err == nil {
				return doneMsg{notice: "Sub-agents run inside the main session: switched to it"}
			}
			return doneMsg{err: err}
		}
		if len(command) > 0 && t.Tmux == "herdr" {
			attach := herdr.NewAttach(command)
			return tea.Exec(attach, func(err error) tea.Msg { return attachedMsg{taskID: t.ID, err: err} })()
		}
		environ := tmux.AttachEnv()
		inTmux := len(command) > 0
		if !inTmux {
			command, environ = []string{"/bin/sh", "-c", t.Resume}, append(os.Environ(), "HC_TASK="+t.ID)
		}
		if inTmux && fromAgent {
			command = mainSessionHint(command)
		}
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Env = environ
		if info, err := os.Stat(t.Cwd); err == nil && info.IsDir() {
			cmd.Dir = t.Cwd
		}
		if inTmux {
			// tmux draws through the terminal and prints why it left on stdout: keep all but "[detached ...]".
			cmd.Stdout = tmux.QuietDetach(os.Stdout)
		}
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{taskID: t.ID, err: err} })()
	}
}

// binaryVersion asks the hc binary on disk for its version.
func binaryVersion(daemon string) tea.Cmd {
	return func() tea.Msg {
		msg := binaryMsg{daemon: daemon}
		if exe, err := os.Executable(); err == nil {
			if out, err := exec.Command(exe, "--version").Output(); err == nil {
				if fields := strings.Fields(string(out)); len(fields) > 0 {
					msg.binary = fields[len(fields)-1]
				}
			}
		}
		return msg
	}
}

// mainSessionHint swaps the hint tmux shows on entry for one that says a sub-agent lives in the
// main session.
func mainSessionHint(command []string) []string {
	last := len(command) - 1
	if last < 0 || command[last] != tmux.DetachHint {
		return command
	}
	return append(append([]string{}, command[:last]...), tmux.MainSessionHint)
}

// --- state ---------------------------------------------------------------------------

func (m *model) say(text string, isAlarm bool) {
	m.notice, m.noticeAlarm = text, isAlarm
	m.noticeUntil = time.Now().Add(5 * time.Second)
	if isAlarm {
		m.noticeUntil = time.Now().Add(10 * time.Second)
	}
	m.clamp() // the notice takes a line from the table, or from the list of folders
}

// light notes the tasks a poll shows in Done that the poll before showed in another state: they
// light up for a moment. A task already done when the board opens does not.
func (m *model) light(tasks []api.Task) {
	was := make(map[string]string, len(m.tasks))
	for _, t := range m.tasks {
		was[t.ID] = t.State
	}
	for _, t := range tasks {
		if state, known := was[t.ID]; known && state != "done" && t.State == "done" {
			if m.lit == nil {
				m.lit = map[string]time.Time{}
			}
			m.lit[t.ID] = time.Now()
		}
	}
}

func (m model) matches(t *api.Task) bool {
	if t.State == "done" && !m.showDone && t.Hidden {
		return false
	}
	_, ok := m.fits(t)
	return ok
}

// fits scores the filter against the title, the repo and the id of a task.
func (m model) fits(t *api.Task) (int, bool) { return fuzzy(m.needle, t.Title, t.Repo, t.Short) }

// toBest puts the cursor on the task the filter fits best, the highest on the board on a tie. An
// empty filter leaves it where it is.
func (m *model) toBest() {
	if strings.TrimSpace(m.needle) == "" {
		return
	}
	best, top := -1, 0
	for i, row := range m.rows {
		if row.Agent != nil {
			continue
		}
		if score, _ := m.fits(row.Task); best < 0 || score > top {
			best, top = i, score
		}
	}
	if best >= 0 {
		m.cursor = best
		m.clamp()
	}
}

// rebuild recomputes the rows, then puts the cursor back on the task it was on.
func (m *model) rebuild() {
	wanted := m.jump
	if wanted == "" && m.cursor < len(m.keys) {
		wanted = m.keys[m.cursor]
	}
	m.jump = ""
	var shown []api.Task
	for i := range m.tasks {
		if m.matches(&m.tasks[i]) {
			shown = append(shown, m.tasks[i])
		}
	}
	m.rows = format.TreeRows(shown, m.order)
	cells := make([][]cell, len(m.rows))
	for i, row := range m.rows {
		cells[i] = rowCells(row)
	}
	m.widths = columnWidths(columns, cells)
	m.keys = m.keys[:0]
	child := 0
	for _, row := range m.rows {
		if row.Agent == nil {
			child = 0
			m.keys = append(m.keys, row.Task.ID)
			continue
		}
		m.keys = append(m.keys, fmt.Sprintf("%s/%d", row.Task.ID, child))
		child++
	}
	// A tick goes with its box: a task gone from the table, a done one hidden by d, is no longer acted on.
	for id := range m.ticked {
		if !slices.Contains(m.keys, id) {
			delete(m.ticked, id)
		}
	}
	if m.view == viewPick && m.pickBatch && len(m.ticked) == 0 {
		m.view, m.pickBatch = viewBoard, false
		m.say("The ticked tasks left the board", true)
	}
	// The row itself, else the task it belonged to, else the same position.
	for _, key := range []string{wanted, strings.SplitN(wanted, "/", 2)[0]} {
		if i := slices.Index(m.keys, key); i >= 0 && key != "" {
			m.cursor = i
			break
		}
	}
	m.clamp()
}

// chrome is how many lines of the screen are around the table and the list of folders.
func (m model) chrome() int {
	used := 10 // the header block, the table border and heading, and the tokens line
	if m.asking != "" {
		used += 4
	}
	if m.notice != "" {
		used++
	}
	return used
}

// tableHeight is how many rows fit on screen.
func (m model) tableHeight() int {
	used := m.chrome()
	if rows := m.folderRows(); rows > 0 {
		used += rows + 2
	}
	return max(1, m.height-used)
}

// folderRoom is how many lines the list of folders can take: what the screen has once the last
// row of the table and the two borders of the list are counted.
func (m model) folderRoom() int { return m.height - m.chrome() - 3 }

// folderRows is how many lines the list of folders shows at once. It grows with the folders from
// the input line up to the middle of the screen, then scrolls.
func (m model) folderRows() int {
	if !m.listing {
		return 0
	}
	below := 7 // under the list: its two borders, the input line in its box with its title, and the bottom line
	rows := min(len(m.folders), max(3, m.height/2-below))
	return min(max(1, rows), m.folderRoom()) // one line at least: an empty list says why
}

// clamp keeps the cursor on a row and the row on screen.
func (m *model) clamp() {
	m.cursor = max(0, min(m.cursor, len(m.rows)-1))
	height := m.tableHeight()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+height {
		m.top = m.cursor - height + 1
	}
	m.top = max(0, min(m.top, len(m.rows)-height))
	// The same for the list of folders, where the cursor is on no row until an arrow moves it. A
	// screen with no line left for it has no list.
	if m.listing && m.folderRoom() < 1 {
		m.closeFolders()
	}
	m.folderCursor = max(-1, min(m.folderCursor, len(m.folders)-1))
	rows := m.folderRows()
	if m.folderCursor >= 0 && m.folderCursor < m.folderTop {
		m.folderTop = m.folderCursor
	}
	if m.folderCursor >= m.folderTop+rows {
		m.folderTop = m.folderCursor - rows + 1
	}
	m.folderTop = max(0, min(m.folderTop, len(m.folders)-rows))
}

// current is the task under the cursor. A child row stands for its task.
func (m model) current() *api.Task {
	if m.cursor >= len(m.rows) {
		return nil
	}
	return m.rows[m.cursor].Task
}

func (m *model) ask(kind, title, value string) tea.Cmd {
	m.asking = kind
	m.input.Placeholder = title
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Width = max(10, m.width-8)
	m.closeFolders()
	m.clamp() // the table just got shorter
	return m.input.Focus()
}

// getwd names the folder the board runs in. Tests replace it.
var getwd = os.Getwd

// askFolder starts a new-item flow, for a task or a schedule, with its first step: the folder.
func (m *model) askFolder(creating string) tea.Cmd {
	m.creating, m.newProfile = creating, ""
	dir := m.workdir
	if dir == "" {
		dir, _ = getwd()
	}
	if dir != "" && !strings.HasSuffix(dir, "/") {
		dir += "/" // the first tab lists what is in it
	}
	return m.ask("newdir",
		fmt.Sprintf("%s 1/%d, the folder. Tab lists the folders, a missing one is created", m.flowName(), m.newSteps()), dir)
}

// profileStep is 1 when the new-item flows ask for a profile, which they do only with a choice.
func (m model) profileStep() int {
	if len(m.profiles) > 1 {
		return 1
	}
	return 0
}

// newSteps is how many steps the current new-item flow has.
func (m model) newSteps() int {
	if m.creating == "schedule" {
		if m.cronDraft() {
			return m.profileStep() + scheduleSteps
		}
		return 1 + m.profileStep() + scheduleSteps
	}
	return 2 + m.profileStep()
}

// flowName names the current new-item flow, for the titles of its steps.
func (m model) flowName() string {
	if m.creating == "schedule" {
		return "New schedule"
	}
	return "New task"
}

// afterFolder continues a new-item flow once its folder is settled: the profile picker when
// there is a choice, else the next step.
func (m *model) afterFolder() tea.Cmd {
	if len(m.profiles) > 1 {
		m.view, m.pickProfile = viewProfile, 0
		return nil
	}
	return m.afterProfile()
}

// afterProfile continues a new-item flow once its profile is settled.
func (m *model) afterProfile() tea.Cmd {
	if m.creating == "schedule" {
		m.view = viewSchedules
		return m.askScheduleTime()
	}
	m.view = viewBoard
	return m.askTitle()
}

// askTitle is the last step of the new-task flow.
func (m *model) askTitle() tea.Cmd {
	total := m.newSteps()
	title := fmt.Sprintf("New task %d/%d, the title. Enter drops you into the session. Folder: %s", total, total, m.newDir)
	if m.newProfile != "" {
		title += "  Profile: " + m.newProfile
	}
	return m.ask("new", title, "")
}

// underWorkdir tells whether a typed path is looked up under workdir: one that says nothing else,
// with no leading /, ~, ./ or ../. A name that starts with a dot is a name.
func underWorkdir(path, workdir string) bool {
	path = config.ExpandUser(path)
	return workdir != "" && !filepath.IsAbs(path) && path != ".." && !strings.HasPrefix(path, "./") && !strings.HasPrefix(path, "../")
}

// dirParent splits a typed path at its last slash, and names the folder its first part stands for.
func dirParent(text, workdir string) (head, base, parent string) {
	slash := strings.LastIndex(text, "/")
	head, base = text[:slash+1], text[slash+1:]
	parent = config.ExpandUser(head)
	if underWorkdir(text, workdir) {
		parent = filepath.Join(workdir, parent)
	}
	if parent == "" {
		parent = "."
	}
	return head, base, parent
}

// dirSuggestions returns the folders that complete the typed text, for the "New task" input, or
// why the folder they would be in could not be read. Upper and lower case match each other.
func dirSuggestions(text, workdir string) ([]string, error) {
	head, base, parent := dirParent(text, workdir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		hidden := base == "" && strings.HasPrefix(name, ".") // dot folders only when asked for
		if hidden || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(base)) {
			continue
		}
		// A name with a line break or an escape in it cannot be typed, and would draw over the board.
		if strings.ContainsFunc(name, unicode.IsControl) {
			continue
		}
		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 { // a link to a folder is a folder to work in
			info, err := os.Stat(filepath.Join(parent, name))
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			out = append(out, head+name)
		}
	}
	return out, nil
}

var (
	// listDir and listWait are how the board reads the folders of its list, and how long it waits
	// for them. Tests replace them.
	listDir  = dirSuggestions
	listWait = 300 * time.Millisecond
	// unheard holds the folders that were asked and have not answered yet.
	unheard     sync.Map
	errNoAnswer = errors.New("no answer")
)

// readFolders is dirSuggestions for the board's own loop, which draws the screen and must not
// wait: a folder that does not answer in time, on a dead network mount for one, is given up on,
// and not asked again before it has answered.
func readFolders(text, workdir string) ([]string, error) {
	_, _, parent := dirParent(text, workdir)
	parent = filepath.Clean(parent)
	if _, asked := unheard.LoadOrStore(parent, true); asked {
		return nil, errNoAnswer
	}
	type answer struct {
		found []string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		found, err := listDir(text, workdir)
		unheard.Delete(parent)
		done <- answer{found, err}
	}()
	select {
	case got := <-done:
		return got.found, got.err
	case <-time.After(listWait):
		return nil, errNoAnswer
	}
}

// listFolders fills the list with the folders that complete the path typed, or with why there are none.
func (m *model) listFolders() {
	found, err := readFolders(m.input.Value(), m.workdir)
	_, base, parent := dirParent(m.input.Value(), m.workdir)
	m.folders, m.folderCursor, m.folderTop, m.folderNote = found, -1, 0, ""
	switch name := filepath.Base(parent); {
	case errors.Is(err, errNoAnswer):
		m.folderNote = name + " does not answer"
	case errors.Is(err, os.ErrNotExist):
		m.folderNote = name + " does not exist yet"
	case err != nil:
		m.folderNote = "Cannot read " + name
	case len(found) == 0 && base == "":
		m.folderNote = "No folder in " + name
	case len(found) == 0:
		m.folderNote = "No folder starts with " + base
	}
}

func (m *model) closeFolders() {
	m.listing, m.folders, m.folderNote = false, nil, ""
}

// folderKey handles a key of the folder input that is for its list of folders, and tells whether
// the key was one. Tab opens the list, or completes at once when a single folder fits. In the list
// the arrows choose, tab picks the folder chosen or the first one, and escape closes. Enter picks
// the folder chosen, and is left to the input when none is: what is typed is then the folder.
func (m *model) folderKey(msg tea.KeyMsg) bool {
	count := len(m.folders)
	switch {
	case msg.Type == tea.KeyTab && !m.listing:
		m.listing = true
		if m.listFolders(); len(m.folders) == 1 {
			m.pickFolder(m.folders[0])
		}
		if m.listing && m.folderRoom() < 1 {
			m.closeFolders()
			m.say("This terminal is too short to list the folders", false)
		}
	case !m.listing:
		return false
	case msg.Type == tea.KeyEsc:
		m.closeFolders()
	case msg.Type == tea.KeyEnter && m.folderCursor < 0:
		return false
	case msg.Type == tea.KeyEnter:
		m.pickFolder(m.folders[m.folderCursor])
	case msg.Type == tea.KeyTab && count > 0:
		m.pickFolder(m.folders[max(0, m.folderCursor)])
	case msg.Type == tea.KeyDown && count > 0:
		m.folderCursor = (m.folderCursor + 1) % count
	case msg.Type == tea.KeyUp && count > 0:
		m.folderCursor = (max(0, m.folderCursor) + count - 1) % count
	case msg.Type == tea.KeyPgDown && count > 0:
		m.folderCursor = min(count-1, max(0, m.folderCursor)+m.folderRows())
	case msg.Type == tea.KeyPgUp && count > 0:
		m.folderCursor = max(0, m.folderCursor-m.folderRows())
	case msg.Type == tea.KeyTab, msg.Type == tea.KeyDown, msg.Type == tea.KeyUp, msg.Type == tea.KeyPgDown, msg.Type == tea.KeyPgUp:
		// nothing to choose from
	default:
		return false
	}
	m.clamp()
	return true
}

// filterKey moves the cursor of the table with the arrows while the filter is typed, so a task is
// picked without leaving the line. It says whether it took the key.
func (m *model) filterKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyUp:
		m.cursor--
	case tea.KeyDown:
		m.cursor++
	case tea.KeyPgUp:
		m.cursor -= m.tableHeight()
	case tea.KeyPgDown:
		m.cursor += m.tableHeight()
	default:
		return false
	}
	m.clamp()
	return true
}

// pickFolder puts a folder in the input, then lists what is in it: the next level is one tab away.
// The list closes when there is nothing below.
func (m *model) pickFolder(path string) {
	m.input.SetValue(path + "/")
	m.input.CursorEnd()
	if m.listFolders(); len(m.folders) == 0 {
		m.closeFolders()
	}
}

func (m *model) closeLine() {
	m.asking = ""
	m.closeFolders()
	m.input.Blur()
	m.rebuild()
}

// idle tells whether the board can be closed without losing anything: no input line, form or dialog
// open, no select mode, and no request of a new-item flow on its way. Every step of a new task or a new schedule is
// one of those, so the flow needs no mark of its own.
func (m model) idle() bool {
	return m.asking == "" && m.sending == 0 && !m.selecting && (m.view == viewBoard || m.view == viewDetail || m.view == viewSchedules ||
		m.view == viewReview || m.view == viewColumns || m.view == viewTeam)
}

// note shows a box over the current view. With confirm it is a y/n question: y runs confirm, n runs
// deny when one is set, and either goes back to the view the note covered.
func (m *model) note(title, text string, confirm func() tea.Cmd) {
	if m.view != viewNote {
		m.noteBack = m.view
	}
	m.view, m.noteTitle, m.noteText, m.confirm, m.deny, m.noteSends = viewNote, title, text, confirm, nil, false
	m.batchKey = ""
}

// welcome tells a new user how the board works, on the first board opened: until the file at path
// exists. The file is written as the note shows, or in its place when show is off. When writing it
// fails the note shows again next time, which beats never.
func (m *model) welcome(path string, show bool) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		os.WriteFile(path, nil, 0o644)
	}
	if show {
		m.note("Welcome to headcoach", welcome, nil)
	}
}

func (m *model) setDetail(cells []cell) {
	// Wrapped to the width of the screen: a long line of the log must not run off it.
	m.detail.SetContent(lipgloss.NewStyle().Width(max(20, m.width-2)).Render(renderLines(cells)))
}

// --- update --------------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.detail.Width, m.detail.Height = msg.Width, max(1, msg.Height-1)
		m.input.Width = max(10, m.width-8)
		// The input picks again which part of its text fits: from the end, then back to the cursor.
		pos := m.input.Position()
		m.input.CursorEnd()
		m.input.SetCursor(pos)
		m.clamp()
		return m, nil

	case tickMsg:
		if m.reload && m.idle() {
			return m, tea.Quit
		}
		if m.notice != "" && time.Now().After(m.noticeUntil) {
			m.notice = ""
			m.clamp()
		}
		cmds := []tea.Cmd{m.poll(), tick()}
		if m.view == viewDetail {
			cmds = append(cmds, m.loadDetail()) // the detail follows the task while it is open
		}
		if m.view == viewSchedules {
			cmds = append(cmds, m.loadSchedules())
		}
		if m.view == viewReview && !m.reviewLoading {
			// The queue can take seconds to answer, while it asks the forge: one poll at a time.
			m.reviewLoading = true
			cmds = append(cmds, m.loadReview())
		}
		if m.view == viewTeam && !m.teamLoading {
			m.teamLoading = true
			cmds = append(cmds, m.loadTeam())
		}
		return m, tea.Batch(cmds...)

	case reviewMsg:
		m.reviewLoading = false
		if msg.err != nil {
			m.say(msg.err.Error(), true)
			return m, nil
		}
		m.review = msg.queue
		cells := make([][]cell, len(m.review.Items))
		for i, it := range m.review.Items {
			cells[i] = reviewCells(it)
		}
		m.reviewWidths = columnWidths(reviewColumns, cells)
		m.reviewCursor = max(0, min(m.reviewCursor, len(m.review.Items)-1))
		return m, nil

	case teamMsg:
		m.teamLoading = false
		if msg.id != m.teamID {
			return m, nil // the view moved on to another coordinator meanwhile
		}
		if msg.err != nil {
			m.say(msg.err.Error(), true)
			return m, nil
		}
		m.team = msg.team
		cells := make([][]cell, len(m.team.Members))
		for i, mem := range m.team.Members {
			cells[i] = teamCells(mem, m.teamColor())
		}
		m.teamWidths = columnWidths(teamColumns, cells)
		m.teamCursor = max(0, min(m.teamCursor, len(m.team.Members)-1))
		return m, nil

	case diffDoneMsg:
		if msg.err != nil {
			m.say("The diff could not be shown: "+msg.err.Error(), true)
		}
		return m, nil

	case schedulesMsg:
		if msg.err != nil {
			m.say(msg.err.Error(), true)
			return m, nil
		}
		m.schedules = msg.list
		m.schedCursor = max(0, min(m.schedCursor, len(m.schedules)-1))
		if msg.notice != "" {
			m.say(msg.notice, false)
		}
		return m, m.poll() // a run or a deletion changes the board too

	case catchUpMsg:
		m.draft.catchUp = msg.on
		return m, m.askScheduleTitle()

	case boardMsg:
		if msg.err != nil {
			m.down = msg.err.Error()
			return m, nil
		}
		m.light(msg.board.Tasks)
		m.down, m.tasks, m.limits = "", msg.board.Tasks, msg.board.Limits
		if m.restoreRow > 0 { // where the cursor stays when its row is gone
			m.cursor, m.restoreRow = m.restoreRow, 0
		}
		m.rebuild()
		if m.restoreTop > 0 {
			m.top, m.restoreTop = m.restoreTop, 0
			m.clamp()
		}
		var cmds []tea.Cmd
		if len(m.lit) > 0 && !m.glowing {
			m.glowing = true
			cmds = append(cmds, nextGlow())
		}
		if v := msg.board.Version; v != "" && v != version.Version && v != m.daemon {
			m.daemon = v
			cmds = append(cmds, binaryVersion(v))
		}
		return m, tea.Batch(cmds...)

	case glowMsg:
		for id, at := range m.lit {
			if time.Since(at) >= glowFor {
				delete(m.lit, id)
			}
		}
		if m.glowing = len(m.lit) > 0; m.glowing {
			return m, nextGlow()
		}
		return m, nil

	case checkedMsg:
		return m.checked(msg)

	case binaryMsg:
		if msg.binary == msg.daemon {
			m.reload = true // the update is installed: open it in place of this board, once idle
			if m.idle() {
				return m, tea.Quit
			}
			return m, nil
		}
		if update.Newer(version.Version, msg.daemon) {
			m.say("This board runs "+version.Version+", the daemon "+msg.daemon+": restart the daemon with hc restart", true)
		} else {
			m.say("The daemon runs "+msg.daemon+", this board "+version.Version+": update hc, then reopen the board", true)
		}
		return m, nil

	case detailMsg:
		if m.view != viewDetail {
			return m, nil
		}
		if msg.err != nil {
			m.setDetail([]cell{c(msg.err.Error(), alarm)})
		} else if msg.task.ID == m.detailID || strings.HasPrefix(msg.task.ID, m.detailID) {
			m.setDetail(describe(&msg.task))
			if m.restoreDetail > 0 {
				m.detail.SetYOffset(m.restoreDetail)
				m.restoreDetail = 0
			}
		}
		return m, nil

	case doneMsg:
		if msg.err != nil {
			m.say(msg.err.Error(), true)
		} else if msg.notice != "" {
			m.say(msg.notice, false)
		}
		return m, m.poll()

	case answerMsg:
		if m.view != viewWaiting || msg.asked != m.asked {
			return m, nil // given up on with escape: the answer is no longer wanted
		}
		m.view = viewBoard
		if msg.err != nil {
			m.say(msg.err.Error(), true)
		} else {
			m.note(board.Short(msg.taskID)+": "+board.Clip(msg.question, 70), msg.answer, nil)
		}
		return m, nil

	case frameMsg:
		if m.view != viewWaiting {
			return m, nil
		}
		m.ticks++
		return m, nextFrame()

	case mkdirMsg:
		m.sending = max(0, m.sending-1)
		if msg.err != nil {
			m.say(msg.err.Error(), true)
			return m, nil
		}
		m.newDir = msg.dir
		return m, m.afterFolder()

	case newTaskMsg:
		m.sending = max(0, m.sending-1)
		if late := (client.NoAnswer{}); errors.As(msg.err, &late) {
			m.say(msg.err.Error()+": the task may be on the board already", true)
			return m, m.poll()
		}
		if msg.err != nil {
			m.say(msg.err.Error(), true)
			// Nothing was created: the title goes back in its input, to send again or to drop with
			// escape. Not over something opened since.
			if m.asking == "" && m.view == viewBoard {
				m.newDir, m.newProfile = msg.dir, msg.profile
				return m, m.ask("new", msg.asked, msg.title)
			}
			return m, nil
		}
		m.jump = msg.task.ID
		return m, m.attach(msg.task, false) // the session opens empty: the prompt is typed in Claude itself

	case attachedMsg:
		if msg.err != nil {
			m.say("The session could not be entered: "+msg.err.Error(), true)
		}
		m.jump = msg.taskID // come back on the row that was entered, wherever it has moved to
		return m, m.poll()

	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Several characters in one event are text: a paste the terminal did not mark, or a slow line
	// that delivers a word at once. Marked as pasted, a word such as enter or down is never taken
	// for the key of that name, here or in the input line.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
		msg.Paste = true
	}
	key := msg.String()
	if key == "ctrl+c" || key == "ctrl+q" {
		return m, tea.Quit
	}
	switch m.view {
	case viewDetail:
		switch key {
		case "esc", "q":
			m.view = viewBoard
			return m, nil
		case "g", "home":
			m.detail.GotoTop()
			return m, nil
		case "G", "end":
			m.detail.GotoBottom()
			return m, nil
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg) // j, k, arrows, page up and down
		return m, cmd

	case viewPick:
		switch key {
		case "esc", "q":
			m.view, m.pickBatch = viewBoard, false
		case "j", "down":
			m.pick = (m.pick + 1) % len(store.States)
		case "k", "up":
			m.pick = (m.pick + len(store.States) - 1) % len(store.States)
		case "enter":
			m.view = viewBoard
			if m.pickBatch {
				m.pickBatch = false
				m.confirmBatch("m", store.States[m.pick])
				return m, nil
			}
			return m, m.act("PATCH", "/api/tasks/"+m.pickTask.ID, map[string]string{"state": store.States[m.pick]}, nil)
		}
		return m, nil

	case viewProfile:
		switch key {
		case "esc", "q":
			m.view = viewBoard
			if m.creating == "schedule" {
				m.view = viewSchedules
			}
		case "j", "down":
			m.pickProfile = (m.pickProfile + 1) % len(m.profiles)
		case "k", "up":
			m.pickProfile = (m.pickProfile + len(m.profiles) - 1) % len(m.profiles)
		case "enter":
			m.newProfile = m.profiles[m.pickProfile]
			return m, m.afterProfile()
		}
		return m, nil

	case viewNote:
		confirm, deny := m.confirm, m.deny
		if confirm != nil && key != "y" && key != "n" && key != "esc" && key != "q" {
			return m, nil // a y/n question waits for an answer
		}
		batch := m.batchKey
		m.view, m.confirm, m.deny, m.batchKey = m.noteBack, nil, nil, ""
		switch {
		case batch != "" && key == "y":
			_, _, cmd := m.planBatch(batch, m.batchState)
			if cmd != nil { // the ticks are spent, else they stay for another try
				m.selecting, m.ticked = false, nil
			}
			return m, cmd
		case confirm != nil && key == "y":
			if m.noteSends {
				m.sending++
			}
			return m, confirm()
		case deny != nil && key == "n":
			return m, deny()
		}
		return m, nil

	case viewSchedules:
		if m.asking != "" {
			return m.typing(msg)
		}
		return m.schedulesKey(key)

	case viewReview:
		return m.reviewKey(key)

	case viewTeam:
		return m.teamKey(key)

	case viewColumns:
		return m.columnsKey(key)

	case viewWaiting:
		if key == "esc" || key == "q" {
			m.view = viewBoard
			m.asked++ // the answer on its way is no longer wanted
		}
		return m, nil
	}
	if m.asking != "" {
		return m.typing(msg)
	}
	if m.selecting {
		return m.selectKey(key)
	}
	return m.boardKey(key)
}

// selectKey handles a key in select mode. x, m, p and h act on every ticked task, with the meaning they
// have on one. Every other key is ignored, so that a stray one does not act on the task under the
// cursor while tasks are ticked.
func (m model) selectKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.selecting, m.ticked = false, nil
		return m, nil
	case "j", "down":
		m.cursor++
	case "k", "up":
		m.cursor--
	case " ":
		if t := m.current(); t != nil && tickable(m.rows[m.cursor]) {
			if m.ticked[t.ID] {
				delete(m.ticked, t.ID)
			} else {
				m.ticked[t.ID] = true
			}
		}
	case "x", "p", "h":
		m.confirmBatch(key, "parked")
		return m, nil
	case "m":
		if tasks := m.tickedTasks(); len(tasks) == 0 {
			m.confirmBatch(key, "") // says nothing is ticked
		} else {
			m.view, m.pickBatch, m.pick = viewPick, true, slices.Index(store.States, tasks[0].State)
		}
		return m, nil
	}
	m.clamp()
	return m, nil
}

// tickedTasks lists the ticked tasks, in board order.
func (m model) tickedTasks() []api.Task {
	var out []api.Task
	for i, key := range m.keys {
		if m.ticked[key] {
			out = append(out, *m.rows[i].Task)
		}
	}
	return out
}

// confirmBatch asks once before key acts on every ticked task: x deletes them, p parks them, h hides
// them, m moves them to state. A task the key does not apply to is skipped, and the question names it.
func (m *model) confirmBatch(key, state string) {
	title, question, _ := m.planBatch(key, state)
	if question == "" {
		return
	}
	// y plans the batch again: a poll may have changed the ticked tasks while the question was open.
	m.note(title, question+"  y / n", func() tea.Cmd { return nil })
	m.batchKey, m.batchState = key, state
}

// planBatch says what key does to the ticked tasks: the title and the question to ask, and the
// command that does it. With nothing to do it says why, and the question is empty.
func (m *model) planBatch(key, state string) (title, question string, cmd tea.Cmd) {
	tasks := m.tickedTasks()
	if len(tasks) == 0 {
		m.say("Nothing is ticked: space ticks the task under the cursor", true)
		return "", "", nil
	}
	title, verb, method, body := "Delete", "deleted", "DELETE", any(nil)
	switch key {
	case "h":
		title, verb, method, body = "Hide", "hidden", "PATCH", map[string]bool{"hidden": true}
	case "p", "m":
		title, verb, method, body = "Park", "parked", "PATCH", map[string]string{"state": state}
	}
	if key == "m" {
		title, verb = "Move", "moved to "+store.StateNames[state]
	}
	var ids, skipped []string
	running := 0
	for _, t := range tasks {
		why := ""
		switch {
		case key == "p" && t.State == "done":
			why = "is done"
		case key == "p" && t.State == state:
			why = "is already parked"
		case key == "m" && t.State == state:
			why = "is already in " + store.StateNames[state]
		case key == "h" && t.State != "done":
			why = "is not done"
		case key == "h" && t.Hidden:
			why = "is already hidden"
		}
		if why != "" {
			skipped = append(skipped, t.Short+" "+why)
			continue
		}
		if t.State == "running" {
			running++
		}
		ids = append(ids, t.ID)
	}
	skip := ""
	if len(skipped) > 0 {
		skip = " Skipped: " + strings.Join(skipped, ", ") + "."
	}
	if len(ids) == 0 {
		m.say("Nothing to "+strings.ToLower(title)+"."+skip, true)
		return "", "", nil
	}
	question = title + " " + format.Counted(len(ids), "task")
	if key == "m" {
		question += " to " + store.StateNames[state]
	}
	if key == "x" {
		question += m.deleteOrder(ids)
	}
	question += "?"
	if running > 0 && key != "m" {
		question += fmt.Sprintf(" %d running.", running)
	}
	c, skips := m.client, len(skipped)
	return title, question + skip, func() tea.Msg {
		var failed []string
		for _, id := range ids {
			if err := c.Call(method, "/api/tasks/"+id, body, nil); err != nil {
				failed = append(failed, board.Short(id)+": "+err.Error())
			}
		}
		done := format.Counted(len(ids)-len(failed), "task") + " " + verb
		if len(failed) > 0 {
			done = fmt.Sprintf("%d of %s %s", len(ids)-len(failed), format.Counted(len(ids), "task"), verb)
		}
		if skips > 0 {
			done += fmt.Sprintf(", %d skipped", skips)
		}
		if len(failed) > 0 {
			return doneMsg{err: fmt.Errorf("%s. %s", done, strings.Join(failed, "; "))}
		}
		return doneMsg{notice: done}
	}
}

// typing handles a key while the input line is open.
func (m model) typing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.asking == "newdir" && m.folderKey(msg) || m.asking == "filter" && m.filterKey(msg) {
		return m, nil
	}
	switch msg.String() {
	case "esc":
		if m.asking == "filter" {
			m.needle = ""
		}
		m.closeLine()
		return m, nil
	case "enter":
		kind, text := m.asking, strings.TrimSpace(m.input.Value())
		m.closeLine()
		switch {
		case kind == "command" && text != "":
			return m.command(text)
		case kind == "question":
			m.asked++
			m.view, m.ticks, m.waitTitle = viewWaiting, 1, "Asking about "+board.Short(m.replyTo)
			return m, tea.Batch(m.question(m.replyTo, text), nextFrame())
		case strings.HasPrefix(kind, "schedule-"):
			return m.scheduleStep(kind, text)
		case kind == "title" && text != "":
			return m, m.act("PATCH", "/api/tasks/"+m.renameID, map[string]string{"title": text},
				func(t api.Task) string { return t.Short + " renamed" })
		case kind == "reply" && text != "":
			return m, m.act("POST", "/api/tasks/"+m.replyTo+"/reply", map[string]string{"text": text},
				func(t api.Task) string { return "Message delivered to " + t.Short })
		case kind == "newdir":
			dir := text
			if dir == "" {
				dir, _ = getwd()
			}
			if dir == "" {
				m.say("Type a folder: the one the board runs in is gone", true)
				return m, m.askFolder(m.creating)
			}
			under := underWorkdir(dir, m.workdir)
			if dir = config.ExpandUser(dir); under {
				dir = filepath.Join(m.workdir, dir)
			}
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
			info, err := os.Stat(dir)
			switch {
			case err == nil && info.IsDir():
				m.newDir = dir
				return m, m.afterFolder()
			case err == nil:
				m.say(dir+" is not a directory", true)
				return m, nil
			}
			m.note("New folder", dir+" does not exist. Create it?  y / n", func() tea.Cmd {
				return func() tea.Msg { return mkdirMsg{dir, os.MkdirAll(dir, 0o755)} }
			})
			m.noteSends = true
			return m, nil
		case kind == "new" && text != "":
			task := map[string]string{"title": text, "cwd": m.newDir}
			if m.newProfile != "" {
				task["profile"] = m.newProfile
			}
			c, asked, dir, profile := m.client, m.input.Placeholder, m.newDir, m.newProfile
			m.sending++
			return m, func() tea.Msg {
				t, err := c.Do("POST", "/api/tasks", task)
				return newTaskMsg{t, err, asked, text, dir, profile}
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	before := m.input.Value()
	m.input, cmd = m.input.Update(msg)
	if m.listing && m.input.Value() != before { // the list follows the path as it is typed
		m.listFolders()
		m.clamp()
	}
	if m.asking == "filter" && m.input.Value() != m.needle { // the table follows the filter as it is typed
		m.needle = m.input.Value()
		m.rebuild()
		m.toBest()
	}
	return m, cmd
}

// boardKey handles a key on the table.
func (m model) boardKey(key string) (tea.Model, tea.Cmd) {
	page := m.tableHeight()
	switch key {
	case "Q":
		return m, tea.Quit
	case "j", "down":
		m.cursor++
	case "k", "up":
		m.cursor--
	case "pgdown", " ":
		m.cursor += page
	case "pgup":
		m.cursor -= page
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = len(m.rows) - 1
	case "esc":
		m.needle = ""
		m.rebuild()
	case "d":
		m.showDone = !m.showDone
		m.rebuild()
	case "o":
		m.order = format.Sorts[(slices.Index(format.Sorts, m.order)+1)%len(format.Sorts)]
		m.rebuild()
	case "/":
		return m, m.ask("filter", "Fuzzy filter by title, repo or task id", m.needle)
	case ":":
		return m, m.ask("command", "Command: "+strings.Join(commandNames, ", "), "")
	case "u":
		return m.command("update")
	case "n":
		return m, m.askFolder("task")
	case "S":
		m.view = viewSchedules
		return m, m.loadSchedules()
	case "R":
		m.view, m.reviewLoading = viewReview, true
		return m, m.loadReview()
	case "w":
		t := m.current()
		if t == nil {
			return m, nil
		}
		if id := m.teamOf(t); id != m.teamID {
			m.teamID, m.team, m.teamCursor, m.teamWidths = id, api.Team{}, 0, nil
		}
		m.view, m.teamLoading = viewTeam, true
		return m, m.loadTeam()
	case "C":
		m.view, m.colCursor = viewColumns, 0
		return m, nil
	case "?":
		m.note("Keys (any key closes)", help, nil)
	case "v":
		// Not under a new task on its way: its answer reopens an input, or enters its session.
		if len(m.rows) > 0 && m.sending == 0 {
			m.selecting, m.ticked = true, map[string]bool{}
		}
	}
	m.clamp()
	t := m.current()
	if t == nil {
		return m, nil
	}
	path := "/api/tasks/" + t.ID
	switch key {
	case "enter":
		m.view, m.detailID, m.restoreDetail = viewDetail, t.ID, 0 // another task's detail opens at its top
		m.detail.GotoTop()
		m.setDetail([]cell{c("Loading "+t.Short, dim)})
		return m, m.loadDetail()
	case "r":
		if t.AskKind == "permission" {
			m.say(permission, true)
			return m, nil
		}
		m.replyTo = t.ID
		if !t.CanReply {
			// A question takes a typed answer in its free text option; the daemon says when it has none.
			return m, m.ask("reply", "Answer the question "+t.Short+" shows, in its free text option", "")
		}
		return m, m.ask("reply", "Reply to "+t.Short+": "+board.Clip(t.Title, 50), "")
	case "q":
		m.replyTo = t.ID
		return m, m.ask("question", "Ask about "+t.Short+" (enter alone for a summary): "+board.Clip(t.Title, 40), "")
	case "t":
		m.renameID = t.ID
		return m, m.ask("title", "Rename "+t.Short, t.Title)
	case "m":
		m.view, m.pickTask, m.pick = viewPick, *t, slices.Index(store.States, t.State)
	case "p":
		if t.State == "parked" {
			return m, nil
		}
		park := func() tea.Cmd {
			return m.act("PATCH", path, map[string]string{"state": "parked"},
				func(t api.Task) string { return t.Short + " parked" })
		}
		if t.State != "running" {
			return m, park()
		}
		// A stray key must not kill a session that is working.
		m.note("Park", t.Short+" is running. Park it anyway?  y / n", park)
	case "s":
		return m, m.act("POST", path+"/start", nil,
			func(t api.Task) string { return t.Short + " started in tmux pane " + t.Pane })
	case "c":
		// A copy is a new task where this one lives: same folder, same profile, a title of its own.
		// What would make the daemon refuse it is said now, not once the title is typed.
		where := t.Cwd
		if info, err := os.Stat(t.Cwd); t.Cwd == "" {
			where = "no folder"
		} else if err != nil || !info.IsDir() {
			m.say("The folder of "+t.Short+" is gone: "+t.Cwd, true)
			return m, nil
		}
		// The profile is the daemon's to judge: the list this board read at its start can be out of date.
		m.newDir, m.newProfile = t.Cwd, t.Profile
		title := "Copy of %s in %s as %s: its title. Enter drops you into the session"
		room := m.width - 1 - lipgloss.Width(fmt.Sprintf(title, t.Short, "", t.Profile))
		return m, m.ask("new", fmt.Sprintf(title, t.Short, clipStart(where, max(4, room)), t.Profile), "")
	case "h":
		if t.State != "done" {
			m.say("Only a done task can be hidden", true)
			return m, nil
		}
		if t.Hidden {
			return m, m.act("PATCH", path, map[string]bool{"hidden": false},
				func(t api.Task) string { return t.Short + " shown again" })
		}
		return m, m.act("PATCH", path, map[string]bool{"hidden": true},
			func(t api.Task) string { return t.Short + " hidden, d shows it" })
	case "P":
		c, id, pin := m.client, t.ID, t.PinnedAt == 0
		return m, func() tea.Msg {
			t, err := c.Pin(id, pin)
			if err != nil {
				return doneMsg{err: err}
			}
			if pin {
				return doneMsg{notice: t.Short + " pinned to the top"}
			}
			return doneMsg{notice: t.Short + " unpinned"}
		}
	case "x":
		id, question := t.ID, "Delete "+t.Short+" "+board.Clip(t.Title, 50)
		if n := len(m.under(t.ID)); n > 0 {
			question += " with " + format.Counted(n, "task") + " under it"
		}
		m.note("Delete", question+"?  y / n", func() tea.Cmd {
			c := m.client
			return func() tea.Msg { return doneMsg{err: c.Call("DELETE", "/api/tasks/"+id, nil, nil)} }
		})
	case "X":
		// No question: the task stays for a while, and X again keeps it.
		if t.DeleteLeft > 0 {
			return m, m.act("PATCH", path, map[string]int{"delete_in": 0},
				func(t api.Task) string { return t.Short + " kept" })
		}
		return m, m.act("PATCH", path, map[string]int{"delete_in": int(deleteDelay.Seconds())},
			func(t api.Task) string {
				return t.Short + " deleted in " + format.Human(t.DeleteLeft) + ", X again keeps it"
			})
	case "a":
		// A sub-agent has no terminal of its own: its row enters the main session, and says so.
		return m, m.attach(*t, m.rows[m.cursor].Agent != nil)
	case "A":
		if t.Tmux != "own" {
			m.say("Only a session living in your own tmux can be adopted", true)
			return m, nil
		}
		return m, m.act("POST", path+"/adopt", nil,
			func(t api.Task) string { return t.Short + " now runs in the board's tmux, a attaches to it" })
	}
	return m, nil
}

// --- view ----------------------------------------------------------------------------

func (m model) View() string {
	switch m.view {
	case viewDetail:
		return m.detail.View() + "\n" + dim.Render(" j/k, arrows, pgup/pgdn scroll  g/G top or bottom  escape or q back")
	case viewPick:
		var lines []string
		for i, state := range store.States {
			line := "  " + stateStyles[state].Render(store.StateNames[state])
			if i == m.pick {
				line = accent.Render("> ") + stateStyles[state].Bold(true).Render(store.StateNames[state])
			}
			lines = append(lines, line)
		}
		what := m.pickTask.Short
		if m.pickBatch {
			what = format.Counted(len(m.ticked), "task")
		}
		return m.centered(titled("Move "+what+" to", strings.Join(lines, "\n")))
	case viewProfile:
		var lines []string
		for i, name := range m.profiles {
			line := "  " + name
			if i == m.pickProfile {
				line = accent.Render("> ") + bold.Render(name)
			}
			lines = append(lines, line)
		}
		return m.centered(titled(fmt.Sprintf("%s 2/%d, the profile", m.flowName(), m.newSteps()), strings.Join(lines, "\n")))
	case viewNote:
		text := lipgloss.NewStyle().Width(min(106, max(20, m.width-6))).Render(m.noteText)
		return m.centered(titled(m.noteTitle, text))
	case viewWaiting:
		return m.centered(titled(m.waitTitle, pitchFrame(m.ticks)))
	case viewSchedules:
		return m.schedulesView()
	case viewReview:
		return m.reviewView()
	case viewTeam:
		return m.teamView()
	case viewColumns:
		return m.columnsView()
	}

	height := m.tableHeight()
	var cells [][]cell
	shown, widths := m.shownColumns(), m.shownWidths()
	if m.selecting {
		shown = append([]column{{"", false}}, shown...)
		widths = append([]int{lipgloss.Width("[ ]")}, widths...)
	}
	for _, row := range m.rows[m.top:min(len(m.rows), m.top+height)] {
		if m.selecting {
			cells = append(cells, append([]cell{m.checkbox(row)}, m.shownCells(m.glow(row, rowCells(row)))...))
			continue
		}
		cells = append(cells, m.shownCells(m.glow(row, rowCells(row))))
	}
	table := tableLines(shown, widths, cells, m.cursor-m.top, max(10, m.width-2))
	return m.frame(m.boxTitle(), table, tokensLine(m.tasks))
}

// frame draws a full screen around a table: the header, the table in its box, the input line when it
// is open, the notice, and a bottom line.
func (m model) frame(title []cell, table []string, bottom []cell) string {
	cut := plain.MaxWidth(m.width)
	lines := m.header()
	for len(table) < m.tableHeight()+1 { // the lines below stay at the bottom of the screen
		table = append(table, "")
	}
	lines = append(lines, boxed(title, table, m.width)...)
	if m.folderRows() > 0 {
		lines = append(lines, boxed(m.folderTitle(), m.folderLines(), m.width)...)
	}
	if m.asking != "" {
		lines = append(lines, cut.Render(accent.Render(" "+m.input.Placeholder)),
			box.Width(max(10, m.width-2)).Render(m.inputLine()))
	}
	if m.notice != "" {
		style := levelStyles["ok"]
		if m.noticeAlarm {
			style = alarm
		}
		lines = append(lines, cut.Render(style.Render(" "+m.notice)))
	}
	lines = append(lines, cut.Render(" "+render(bottom)))
	return strings.Join(lines, "\n")
}

// folderTitle names the list of folders: where they are, how many, which one is chosen, and the keys.
func (m model) folderTitle() []cell {
	head, _, parent := dirParent(m.input.Value(), m.workdir)
	where := head
	if typed := config.ExpandUser(head); underWorkdir(m.input.Value(), m.workdir) || typed == "" {
		where = strings.TrimSuffix(parent, "/") + "/" // not where it is typed from: under workdir, or where the board runs
	}
	count, keys := fmt.Sprintf("[%d]", len(m.folders)), "  ↑↓ choose, tab picks, esc closes"
	if m.folderCursor >= 0 {
		count = fmt.Sprintf("[%d/%d]", m.folderCursor+1, len(m.folders))
	}
	// A title wider than its border is not drawn at all: the keys go first, then the start of the path.
	room := max(10, m.width-2) - 2 - lipgloss.Width("folders()"+count)
	if lipgloss.Width(where+keys) > room {
		keys = ""
	}
	return []cell{c("folders", stateStyles["parked"].Bold(true)), c("(", plain), c(clipStart(where, room), stateStyles["review"]),
		c(")", plain), c(count, plain), c(keys, dim)}
}

// folderLines draws the folders on screen, one per line, each cut to the width of the list. The
// first and the last say how many more lie above and below, so that the arrows are known to scroll.
func (m model) folderLines() []string {
	rows, inner := m.folderRows(), max(10, m.width-2)
	cut := plain.MaxWidth(inner)
	if len(m.folders) == 0 {
		return []string{cut.Render(dim.Render("   " + m.folderNote))}
	}
	var out []string
	last := min(len(m.folders), m.folderTop+rows) - 1
	for i := m.folderTop; i <= last; i++ {
		name := filepath.Base(m.folders[i]) + "/"
		line := "   " + name
		if i == m.folderCursor {
			line = accent.Render(" > ") + bold.Render(name)
		}
		var more []string
		if i == m.folderTop && i > 0 {
			more = append(more, fmt.Sprintf("↑ %d more", i))
		}
		if i == last && i < len(m.folders)-1 {
			more = append(more, fmt.Sprintf("↓ %d more", len(m.folders)-1-i))
		}
		mark := strings.Join(more, "  ") + " "
		if gap := inner - lipgloss.Width(line) - lipgloss.Width(mark); len(more) > 0 && gap > 0 {
			line += strings.Repeat(" ", gap) + dim.Render(mark)
		}
		out = append(out, cut.Render(line))
	}
	return out
}

// inputLine draws the input without its placeholder: the title above the box says what it is for.
func (m model) inputLine() string {
	input := m.input
	input.Placeholder = ""
	return input.View()
}

func (m model) centered(content string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

// Run opens the terminal board, drawn in the named theme, and returns when it is closed.
// workdir is where a bare folder name typed into "New task" lands, and profiles are the
// configured profiles that "New task" offers.
// The columns of the board are read from, and saved to, columnsPath.
func Run(c *client.Client, theme, workdir string, profiles []string, columnsPath, welcomePath string) error {
	if err := setTheme(theme); err != nil {
		return err
	}
	// Ctrl-C belongs to the session while the terminal is handed over, not to the board: the signal
	// is caught and dropped here. Caught, not ignored, so that the session itself still gets it.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT)
	defer signal.Stop(interrupts)
	go func() {
		for range interrupts {
		}
	}()

	m := newModel(c)
	m.workdir, m.profiles = workdir, profiles
	m.layout, m.layoutPath = readLayout(columnsPath), columnsPath
	// A reload after an update hands over the view of the board it replaces. It is for this board
	// only, not for the sessions it opens.
	saved := os.Getenv(stateEnv)
	m.restoreState(saved)
	os.Unsetenv(stateEnv)
	// A board reloaded after an update was in use already: it is past its welcome.
	m.welcome(welcomePath, saved == "")
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithoutSignalHandler())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	go func() {
		if _, ok := <-stop; ok {
			program.Quit() // leave the terminal as it was found
		}
	}()
	final, err := program.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(model); ok && fm.reload {
		// The binary was updated under this board: run the new one, with the same arguments.
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return syscall.Exec(exe, os.Args, fm.reloadEnv())
	}
	return nil
}
