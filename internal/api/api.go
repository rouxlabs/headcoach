// Package api holds the shapes of the daemon's JSON API. The daemon writes them; the CLI and the TUI
// read them.
package api

import (
	"github.com/rouxlabs/headcoach/internal/handoff"
	"github.com/rouxlabs/headcoach/internal/limits"
	"github.com/rouxlabs/headcoach/internal/review"
	"github.com/rouxlabs/headcoach/internal/store"
	"github.com/rouxlabs/headcoach/internal/transcript"
	"github.com/rouxlabs/headcoach/internal/usage"
)

// Agent is a child row of a task: a sub-agent, or a background command or monitor.
type Agent struct {
	Name    string        `json:"name"`
	Desc    string        `json:"desc"`
	Persona string        `json:"persona"` // the persona skill the sub-agent loaded, if any
	State   string        `json:"state"`
	Age     int           `json:"age"`
	Tokens  *usage.Tokens `json:"tokens"` // nil until the sub-agent has a transcript
}

// Context is how full the context window of a task's latest session is.
type Context struct {
	Tokens int `json:"tokens"`
	Window int `json:"window"`
	Pct    int `json:"pct"`
}

// Link is a reference a task mentions, with a URL when the board knows where it lives.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Move is one line of a task's history.
type Move struct {
	State string  `json:"state"`
	At    float64 `json:"at"`
	By    string  `json:"by"`
}

// Session is one Claude Code session of a task.
type Session struct {
	SessionID string  `json:"session_id"`
	Live      bool    `json:"live"`
	StartedAt float64 `json:"started_at"`
}

// Handoff is a task's handoff file: where the work stands, for a fresh context or the task driving it.
type Handoff struct {
	Path string `json:"path"`
	// Text is the file as it is, empty when there is none; HTML is its Markdown rendered. UpdatedAt is
	// when it last changed, zero when there is no file.
	Text      string          `json:"text"`
	HTML      string          `json:"html"`
	UpdatedAt float64         `json:"updated_at"`
	Summary   handoff.Summary `json:"summary"`
}

// Detail is what only the view of a single task carries.
type Detail struct {
	History  []Move            `json:"history"`
	Sessions []Session         `json:"sessions"`
	Log      []transcript.Turn `json:"log"`
	// Screen is what the terminal shows while a dialog is up, so it can be answered from the board.
	Screen  string   `json:"screen"`
	Handoff *Handoff `json:"handoff"`
}

// Task is a task as the board shows it.
type Task struct {
	store.Task
	Short     string `json:"short"`
	StateName string `json:"state_name"`
	Live      bool   `json:"live"`
	Pane      string `json:"pane"`
	// Tmux says where the live session runs: "hc" in the board's own tmux, "own" in yours, "herdr" in a
	// Herdr pane. Empty when no session is live.
	Tmux string `json:"tmux"`
	// Attach is the command that takes a terminal into the session, when it runs in the board's own tmux or
	// in Herdr.
	Attach    []string `json:"attach"`
	SessionID string   `json:"session_id"`
	Age       int      `json:"age"`
	Idle      int      `json:"idle"`
	CanReply  bool     `json:"can_reply"`
	// Pinned says the task is pinned to the top of the board; PinnedAt says since when.
	Pinned bool `json:"pinned"`
	// TimerLeft is how many seconds the task's timer has left before it closes the task. Zero: no timer.
	TimerLeft int `json:"timer_left"`
	// DeleteLeft is how many seconds are left before the daemon deletes the task. Zero: it stays.
	DeleteLeft int      `json:"delete_left"`
	Tags       []string `json:"tags"`
	Links      []Link   `json:"links"`
	Resume     string   `json:"resume"`
	// Tokens is what the task has consumed so far, sub-agents included. AgentTokens is their share.
	Tokens      usage.Tokens `json:"tokens"`
	AgentTokens usage.Tokens `json:"agent_tokens"`
	// Cost is what Claude Code prices the task at, at API rates. Zero until a status line reports it.
	Cost    float64 `json:"cost"`
	Context Context `json:"context"`
	// SessionModel is the model of the latest session's latest answer. Empty before it answers.
	SessionModel string `json:"session_model"`
	// SessionEffort is the effort level the latest session's status line reports. Empty until it does.
	SessionEffort string  `json:"session_effort"`
	Agents        []Agent `json:"agents"`
	*Detail
}

// Tasks is the answer of GET /api/tasks.
type Tasks struct {
	Tasks  []Task         `json:"tasks"`
	Limits []limits.Entry `json:"limits"`
	// Version is the daemon's, so a board left open across an update can tell it is older.
	Version string `json:"version"`
}

// State is one state of the board, with its label.
type State struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Meta is the answer of GET /api/meta: what a client needs to know about the daemon.
type Meta struct {
	Version     string   `json:"version"`
	States      []State  `json:"states"`
	Profiles    []string `json:"profiles"`
	Dirs        []string `json:"dirs"`
	Tmux        bool     `json:"tmux"`
	PrivateTmux bool     `json:"private_tmux"`
	// Theme is the configured color theme, which the web board starts with too.
	Theme string `json:"theme"`
}

// Schedule is a schedule as the boards show it.
type Schedule struct {
	store.Schedule
	Short string `json:"short"`
	// Next is when it runs next, as a Unix time. Zero while it is paused.
	Next float64 `json:"next"`
	// LastState is the state of the task its last run started. Empty when there is none any more.
	LastState string `json:"last_state"`
}

// InboxItem is one thing a task heard, or will hear, from one of its children.
type InboxItem struct {
	store.InboxItem
	ChildShort string `json:"child_short"`
	ChildTitle string `json:"child_title"` // empty when the child is gone
	// News is the line the parent reads about it.
	News string `json:"news"`
}

// Inbox is the answer of GET /api/tasks/{id}/inbox: what waits to be said first, then the newest.
type Inbox struct {
	Task  string      `json:"task"`
	Items []InboxItem `json:"items"`
}

// Member is one task under a coordinator, as the coordinator view shows it.
type Member struct {
	Task
	// Depth is how far under the coordinator it is: 0 for a task it drives itself. Tree draws the
	// branches down to it from there, and Kids counts the tasks it drives in turn.
	Depth int    `json:"depth"`
	Tree  string `json:"tree"`
	Kids  int    `json:"kids"`
	// Handoff is what its handoff file says, HandoffAt when that changed: zero without a file.
	Handoff   handoff.Summary `json:"handoff"`
	HandoffAt float64         `json:"handoff_at"`
}

// OnYou is one thing that waits on you, in the coordinator view.
type OnYou struct {
	Task  string `json:"task"`
	Short string `json:"short"`
	Title string `json:"title"`
	// Kind is needs for a task in Needs me, review for one in Review, handoff for a line of a handoff
	// file that says it waits on you.
	Kind string `json:"kind"`
	What string `json:"what"`
}

// Team is the answer of GET /api/tasks/{id}/team: a coordinator, the tasks it drives at any depth,
// and what waits on you among them.
type Team struct {
	Task    Task            `json:"task"`
	Handoff handoff.Summary `json:"handoff"`
	Members []Member        `json:"members"`
	OnYou   []OnYou         `json:"on_you"`
}

// ReviewItem is one task of the review queue: what waits on you, and how big it is.
type ReviewItem struct {
	Task
	// Waited is how long the task has been in Review, in seconds.
	Waited int `json:"waited"`
	// Change is what its checkout holds. Nil outside a git checkout.
	Change *review.Change `json:"change"`
	// Tests is "passed" or "failed" for the last test command the task ran, empty when it ran none.
	Tests   string `json:"tests"`
	TestCmd string `json:"test_cmd"`
	// Verdict is the gist of the last report of a reviewer sub-agent, empty when none ran.
	Verdict string `json:"verdict"`
	// PR is the pull or merge request the task opened or mentions, empty when there is none.
	PR string `json:"pr"`
	// PRState is "open", "merged" or "closed", when gh or glab could tell.
	PRState string `json:"pr_state"`
	// Delivery is how the change ships: "pr" through a pull or merge request, "push" straight to the
	// default branch, "branch" on a branch with no pull request yet, empty with no change in sight.
	Delivery string `json:"delivery"`
}

// ReviewMetrics is how the review queue has been doing lately.
type ReviewMetrics struct {
	// Days is the period the other fields cover, ending now.
	Days int `json:"days"`
	// Reviews counts the times a task left Review, and AvgWait is how long it had waited, on average, in seconds.
	Reviews int `json:"reviews"`
	AvgWait int `json:"avg_wait"`
	// Delivered counts the tasks moved to Done, and CostPerChange is their average cost.
	Delivered     int     `json:"delivered"`
	CostPerChange float64 `json:"cost_per_change"`
}

// Review is the answer of GET /api/review: the tasks in Review, the one waiting longest first.
type Review struct {
	Items   []ReviewItem  `json:"items"`
	Metrics ReviewMetrics `json:"metrics"`
}
