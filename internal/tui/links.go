package tui

import (
	"regexp"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rouxlabs/headcoach/internal/api"
	"github.com/rouxlabs/headcoach/internal/board"
)

// The detail of a task finds the task ids it names, such as the tasks a coordinator says it created,
// and links them: tab picks one in turn, enter or a click opens it.

// idPattern finds what may be a task id: a word of 8 hex digits, the short form, or a whole UUID.
var idPattern = regexp.MustCompile(`\b[0-9a-f]{8}(?:-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?\b`)

// escapes are the terminal sequences of styled text, left out where the text is read back off the screen.
var escapes = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]`)

// link is a task id the detail names, and where it shows.
type link struct {
	id    string // the full id of the task it names
	text  string // the id as written
	at    int    // where it starts, counted in the characters of the detail that are not spaces
	spans []span // where it shows: one per line it is wrapped across, none when it could not be placed
}

// span is a piece of a link on screen: a line of the detail, and the columns from and to, to excluded.
type span struct{ line, from, to int }

// knownIDs maps the short and the full id of every task on the board, done ones too, to its full id.
// A short id two tasks share names neither.
func (m model) knownIDs() map[string]string {
	known := make(map[string]string, 2*len(m.tasks))
	for _, t := range m.tasks {
		short := board.Short(t.ID)
		if other, taken := known[short]; taken && other != t.ID {
			known[short] = ""
		} else {
			known[short] = t.ID
		}
		known[t.ID] = t.ID
	}
	return known
}

// solid counts the characters of a text that are not spaces, styles left out. Wrapping a text adds
// and drops spaces only, so this count places a character of the text on the wrapped screen.
func solid(text string) int {
	n := 0
	for _, r := range escapes.ReplaceAllString(text, "") {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// markLinks finds in cells the ids of the tasks in known, but self, and draws each as a link, the one
// at index chosen as picked. It returns the cells to draw and the links, in the order of the text.
func markLinks(cells []cell, known map[string]string, self string, chosen int) ([]cell, []link) {
	var out []cell
	var links []link
	at := 0
	for _, piece := range cells {
		from := 0
		for _, found := range idPattern.FindAllStringIndex(piece.text, -1) {
			text := piece.text[found[0]:found[1]]
			id := known[text]
			if id == "" || id == self {
				continue
			}
			style := linkStyle
			if len(links) == chosen {
				style = linkStyle.Reverse(true)
			}
			before := piece.text[from:found[0]]
			at += solid(before)
			out = append(out, c(before, piece.style), c(text, style))
			links = append(links, link{id: id, text: text, at: at})
			at += len(text)
			from = found[1]
		}
		out = append(out, c(piece.text[from:], piece.style))
		at += solid(piece.text[from:])
	}
	return out, links
}

// placeLinks finds the links on the rendered detail, by the characters that are not spaces. A link
// whose characters on screen are not its id stays without a place: it is not drawn where it seems.
func placeLinks(rendered string, links []link) {
	k, n := 0, 0
	read := make([]strings.Builder, len(links))
	for li, line := range strings.Split(rendered, "\n") {
		line = escapes.ReplaceAllString(line, "")
		for off, r := range line {
			if unicode.IsSpace(r) {
				continue
			}
			for k < len(links) && n >= links[k].at+len(links[k].text) {
				k++
			}
			if k < len(links) && n >= links[k].at {
				col := lipgloss.Width(line[:off])
				end := col + lipgloss.Width(string(r))
				spans := links[k].spans
				if last := len(spans) - 1; last >= 0 && spans[last].line == li && spans[last].to == col {
					spans[last].to = end
				} else {
					spans = append(spans, span{li, col, end})
				}
				links[k].spans = spans
				read[k].WriteRune(r)
			}
			n++
		}
	}
	for i := range links {
		if read[i].String() != links[i].text {
			links[i].spans = nil
		}
	}
}

// setDetail fills the detail with cells that describe the task self, its links marked.
func (m *model) setDetail(cells []cell, self string) {
	m.detailCells, m.detailSelf = cells, self
	m.drawDetail()
}

// drawDetail draws the detail again: after a refresh, or when tab picks another link. The link
// picked stays the same id where it can, though the log moved it.
func (m *model) drawDetail() {
	was := ""
	if m.linking && m.linkAt < len(m.links) {
		was = m.links[m.linkAt].id
	}
	known := m.knownIDs()
	_, links := markLinks(m.detailCells, known, m.detailSelf, -1)
	if m.linking {
		if len(links) == 0 {
			m.linking = false
		} else if m.linkAt = min(m.linkAt, len(links)-1); links[m.linkAt].id != was {
			for i, l := range links {
				if l.id == was {
					m.linkAt = i
					break
				}
			}
		}
	}
	chosen := -1
	if m.linking {
		chosen = m.linkAt
	}
	cells, links := markLinks(m.detailCells, known, m.detailSelf, chosen)
	// Wrapped to the width of the screen: a long line of the log must not run off it.
	rendered := lipgloss.NewStyle().Width(max(20, m.width-2)).Render(renderLines(cells))
	m.detail.SetContent(rendered)
	placeLinks(rendered, links)
	m.links = links
}

// linkKey handles a key of the detail that is for its links, and tells whether the key was one. Tab
// and shift+tab pick the next and the previous link, from the ones on screen; enter opens the link
// picked, escape stops picking.
func (m *model) linkKey(key string) (tea.Cmd, bool) {
	if len(m.links) == 0 {
		return nil, false
	}
	switch {
	case key == "tab" && !m.linking:
		m.linking, m.linkAt = true, 0
		for i, l := range m.links {
			if len(l.spans) > 0 && l.spans[0].line >= m.detail.YOffset {
				m.linkAt = i
				break
			}
		}
	case key == "shift+tab" && !m.linking:
		m.linking, m.linkAt = true, len(m.links)-1
		for i := len(m.links) - 1; i >= 0; i-- {
			if l := m.links[i]; len(l.spans) > 0 && l.spans[0].line < m.detail.YOffset+m.detail.Height {
				m.linkAt = i
				break
			}
		}
	case !m.linking:
		return nil, false
	case key == "tab":
		m.linkAt = (m.linkAt + 1) % len(m.links)
	case key == "shift+tab":
		m.linkAt = (m.linkAt + len(m.links) - 1) % len(m.links)
	case key == "enter":
		return m.jumpTo(m.links[m.linkAt].id), true
	case key == "esc":
		m.linking = false
	default:
		return nil, false
	}
	m.drawDetail()
	if m.linking {
		m.showLink(m.links[m.linkAt])
	}
	return nil, true
}

// showLink scrolls the detail so that the link is on screen, near the middle when it was off it.
func (m *model) showLink(l link) {
	if len(l.spans) == 0 {
		return
	}
	top, bottom := l.spans[0].line, l.spans[len(l.spans)-1].line
	if top < m.detail.YOffset || bottom >= m.detail.YOffset+m.detail.Height {
		m.detail.SetYOffset(top - m.detail.Height/2)
	}
}

// linkAtCell is the link drawn at column x of line y of the screen, or -1.
func (m model) linkAtCell(x, y int) int {
	if y < 0 || y >= m.detail.Height {
		return -1
	}
	line := m.detail.YOffset + y
	for i, l := range m.links {
		for _, s := range l.spans {
			if s.line == line && x >= s.from && x < s.to {
				return i
			}
		}
	}
	return -1
}

// mouse handles a mouse event: a click on a link of the detail opens it, the wheel scrolls the detail.
func (m model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.view != viewDetail {
		return m, nil
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if i := m.linkAtCell(msg.X, msg.Y); i >= 0 {
			return m, m.jumpTo(m.links[i].id)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return m, cmd
}

// taskByID is the task of the board with this id, or nil.
func (m model) taskByID(id string) *api.Task {
	for i := range m.tasks {
		if m.tasks[i].ID == id {
			return &m.tasks[i]
		}
	}
	return nil
}

// jumpTo puts the cursor of the board on a task and opens its detail, so that escape leaves it on its
// row. A hidden done task is shown with the others, a filter that hides the task is cleared.
func (m *model) jumpTo(id string) tea.Cmd {
	t := m.taskByID(id)
	if t == nil {
		return nil // gone from the board since the detail was drawn
	}
	if t.State == "done" && t.Hidden {
		m.showDone = true
	}
	if _, fits := m.fits(t); !fits {
		m.needle = ""
	}
	m.jump = id
	m.rebuild()
	return m.openDetail(t)
}

// openDetail opens the detail of a task at its top, and asks the daemon for it.
func (m *model) openDetail(t *api.Task) tea.Cmd {
	m.view, m.detailID, m.restoreDetail, m.linking = viewDetail, t.ID, 0, false
	m.detail.GotoTop()
	m.setDetail([]cell{c("Loading "+t.Short, dim)}, t.ID)
	return m.loadDetail()
}

// detailFooter is the line under the detail: its keys, and those of its links.
func (m model) detailFooter() string {
	text := " j/k, arrows, pgup/pgdn scroll  g/G top or bottom  escape or q back"
	switch {
	case m.linking && m.linkAt < len(m.links):
		text = " tab/shift+tab next or previous task id  enter opens " + m.links[m.linkAt].text[:8] + "  escape stops"
	case len(m.links) > 0 && m.mouseOn:
		text = " j/k, arrows, pgup/pgdn scroll  g/G top or bottom  tab or a click: task ids  escape or q back"
	case len(m.links) > 0:
		text = " j/k, arrows, pgup/pgdn scroll  g/G top or bottom  tab task ids  escape or q back"
	}
	return dim.MaxWidth(m.width).Render(text)
}
