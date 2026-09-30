package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mbraak/waitbuild/internal/watch"
)

var stateIcons = map[watch.State]string{
	watch.Running:   "⏳",
	watch.Success:   "✅",
	watch.Failure:   "❌",
	watch.Cancelled: "🚫",
	watch.Error:     "⚠️",
	watch.Stopped:   "⏹",
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	faintStyle    = lipgloss.NewStyle().Faint(true)
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Green)
	failStyle     = lipgloss.NewStyle().Foreground(lipgloss.Red)
)

const helpText = "↑/↓ move · enter expand/open · o open on GitHub · d dismiss · c clear finished · q quit"

// loadedMsg carries the watch files as read at now.
type loadedMsg struct {
	watches []watch.Watch
	err     error
	now     time.Time
}

type tickMsg struct{}

// statusMsg is shown below the list until the next key press.
type statusMsg string

type rowKind int

const (
	buildRow rowKind = iota
	infoRow          // a line of text about the build, without an action
	checkRow
)

// row is one line of the list: a build, or a line of its expanded details.
type row struct {
	kind  rowKind
	watch int // index into model.watches
	index int // index of the info line or check within the build
	text  string
	url   string
}

// rowID identifies a row across reloads, so that the selection stays put
// while builds are added or removed.
type rowID struct {
	key   string
	kind  rowKind
	index int
}

type model struct {
	dir      string
	keep     time.Duration
	interval time.Duration

	watches []watch.Watch // most recently started first
	now     time.Time     // when watches were read
	err     error         // from the last load
	status  string

	// toggled records builds the user expanded or collapsed. Other builds are
	// expanded while they run.
	toggled map[string]bool
	cursor  int   // index into rows()
	sel     rowID // the selected row, to find it again after a reload
	offset  int   // first row shown
	height  int   // terminal height
}

func newModel(dir string, keep, interval time.Duration) model {
	return model{dir: dir, keep: keep, interval: interval, toggled: map[string]bool{}}
}

func watchKey(w watch.Watch) string { return w.Owner + "/" + w.Repo + "@" + w.SHA }

func (m model) Init() tea.Cmd { return tea.Batch(m.load(nil), m.tick()) }

func (m model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// load returns a command that runs change, if any, then rereads the watch
// files and deletes those of builds that finished longer than keep ago.
func (m model) load(change func()) tea.Cmd {
	dir, keep := m.dir, m.keep
	return func() tea.Msg {
		if change != nil {
			change()
		}
		watches, err := watch.Load(dir)
		now := time.Now()
		kept := watches[:0]
		for _, w := range watches {
			if w.State() != watch.Running && now.Sub(w.Updated) > keep {
				_ = watch.Remove(dir, w)
				continue
			}
			kept = append(kept, w)
		}
		return loadedMsg{watches: kept, err: err, now: now}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
	case tickMsg:
		return m, tea.Batch(m.load(nil), m.tick())
	case loadedMsg:
		m.watches, m.err, m.now = msg.watches, msg.err, msg.now
		m.restoreSelection()
	case statusMsg:
		m.status = string(msg)
	case tea.KeyPressMsg:
		m.status = ""
		return m.key(msg.String())
	}
	m.scroll()
	return m, nil
}

func (m model) key(key string) (tea.Model, tea.Cmd) {
	rows := m.rows()
	var cur *row
	if m.cursor < len(rows) {
		cur = &rows[m.cursor]
	}
	var cmd tea.Cmd
	switch key {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(rows) - 1
	case "pgup":
		m.cursor -= max(1, m.bodyHeight()-1)
	case "pgdown":
		m.cursor += max(1, m.bodyHeight()-1)
	case "right", "l":
		if cur != nil && cur.kind == buildRow {
			m.toggled[watchKey(m.watches[cur.watch])] = true
		}
	case "left", "h":
		if cur != nil {
			m.toggled[watchKey(m.watches[cur.watch])] = false
			m.cursor = m.buildRowIndex(cur.watch)
		}
	case "enter", "space":
		switch {
		case cur == nil:
		case cur.kind == buildRow:
			w := m.watches[cur.watch]
			m.toggled[watchKey(w)] = !m.expanded(w)
		case cur.url != "":
			cmd = open(cur.url)
		}
	case "o":
		switch {
		case cur == nil:
		case cur.url != "":
			cmd = open(cur.url)
		default:
			cmd = open(m.watches[cur.watch].URL)
		}
	case "d", "backspace", "delete":
		if cur != nil {
			if w := m.watches[cur.watch]; w.State() != watch.Running {
				dir := m.dir
				cmd = m.load(func() { _ = watch.Remove(dir, w) })
			}
		}
	case "c":
		dir := m.dir
		cmd = m.load(func() {
			watches, _ := watch.Load(dir)
			for _, w := range watches {
				if w.State() != watch.Running {
					_ = watch.Remove(dir, w)
				}
			}
		})
	}
	m.cursor = max(0, min(m.cursor, len(m.rows())-1))
	m.sel = m.idOf(m.cursor)
	m.scroll()
	return m, cmd
}

func open(url string) tea.Cmd {
	return func() tea.Msg {
		if err := openURL(url); err != nil {
			return statusMsg(fmt.Sprintf("opening %s: %v", url, err))
		}
		return nil
	}
}

func (m model) expanded(w watch.Watch) bool {
	if v, ok := m.toggled[watchKey(w)]; ok {
		return v
	}
	return w.State() == watch.Running
}

// rows returns the lines of the list: every build, followed by its details
// when it is expanded.
func (m model) rows() []row {
	var rows []row
	for i, w := range m.watches {
		state := w.State()
		rows = append(rows, row{kind: buildRow, watch: i, text: buildTitle(w, state, m.now)})
		if !m.expanded(w) {
			continue
		}
		var info []string
		switch state {
		case watch.Error:
			info = append(info, "Gave up: "+w.Error)
		case watch.Stopped:
			info = append(info, "waitbuild exited before the build finished")
		}
		if len(w.Checks) == 0 && state == watch.Running {
			info = append(info, "Waiting for checks to appear…")
		}
		for j, s := range info {
			rows = append(rows, row{kind: infoRow, watch: i, index: j, text: s})
		}
		for j, c := range w.Checks {
			rows = append(rows, row{kind: checkRow, watch: i, index: j, text: checkTitle(c), url: c.URL})
		}
	}
	return rows
}

func (m model) idOf(i int) rowID {
	rows := m.rows()
	if i < 0 || i >= len(rows) {
		return rowID{}
	}
	r := rows[i]
	return rowID{key: watchKey(m.watches[r.watch]), kind: r.kind, index: r.index}
}

// buildRowIndex returns the index of the row of build w.
func (m model) buildRowIndex(w int) int {
	for i, r := range m.rows() {
		if r.kind == buildRow && r.watch == w {
			return i
		}
	}
	return 0
}

// restoreSelection moves the cursor to the previously selected row, or to its
// build when the row is gone. When the build is gone too, the cursor stays at
// the same position.
func (m *model) restoreSelection() {
	rows := m.rows()
	build := -1
	for i, r := range rows {
		key := watchKey(m.watches[r.watch])
		if key != m.sel.key {
			continue
		}
		if r.kind == m.sel.kind && r.index == m.sel.index {
			m.cursor = i
			return
		}
		if r.kind == buildRow {
			build = i
		}
	}
	if build >= 0 {
		m.cursor = build
	}
	m.cursor = max(0, min(m.cursor, len(rows)-1))
	m.sel = m.idOf(m.cursor)
}

// bodyHeight is the number of rows that fit on the screen, below the header
// and above the help line.
func (m model) bodyHeight() int {
	if m.height == 0 {
		return 1 << 30 // size not known yet
	}
	return max(1, m.height-4)
}

// scroll adjusts offset so that the cursor is visible.
func (m *model) scroll() {
	h := m.bodyHeight()
	n := len(m.rows())
	m.offset = max(0, min(m.offset, n-h))
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
}

func (m model) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(header(m.watches)))
	b.WriteString("\n\n")

	rows := m.rows()
	if len(rows) == 0 {
		b.WriteString(faintStyle.Render("  No builds"))
		b.WriteString("\n")
	}
	end := min(len(rows), m.offset+m.bodyHeight())
	for i := m.offset; i < end; i++ {
		r := rows[i]
		line := r.text
		switch r.kind {
		case buildRow:
			marker := "▸ "
			if m.expanded(m.watches[r.watch]) {
				marker = "▾ "
			}
			line = marker + line
		case infoRow:
			line = "    " + line
		case checkRow:
			line = "    " + line
		}
		switch {
		case i == m.cursor:
			line = selectedStyle.Render(line)
		case r.kind == infoRow:
			line = faintStyle.Render(line)
		case r.kind == checkRow:
			c := m.watches[r.watch].Checks[r.index]
			switch {
			case !c.Done(), c.Cancelled():
			case c.OK():
				line = okStyle.Render(line)
			default:
				line = failStyle.Render(line)
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch {
	case m.status != "":
		b.WriteString(failStyle.Render(m.status))
	case m.err != nil:
		b.WriteString(failStyle.Render(m.err.Error()))
	default:
		b.WriteString(faintStyle.Render(helpText))
	}

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = header(m.watches)
	return v
}

// header counts the running builds, or shows the result of the most recent
// build when none is running.
func header(watches []watch.Watch) string {
	running := 0
	for _, w := range watches {
		if w.State() == watch.Running {
			running++
		}
	}
	switch {
	case running > 0:
		return fmt.Sprintf("waitbuild %s %d running", stateIcons[watch.Running], running)
	case len(watches) > 0:
		s := watches[0].State()
		return fmt.Sprintf("waitbuild %s last build %s", stateIcons[s], s)
	}
	return "waitbuild: no builds"
}

func buildTitle(w watch.Watch, state watch.State, now time.Time) string {
	title := fmt.Sprintf("%s %s/%s · %s · %s", stateIcons[state], w.Owner, w.Repo, w.Branch, w.ShortSHA())
	if state == watch.Running {
		done := 0
		for _, c := range w.Checks {
			if c.Done() {
				done++
			}
		}
		elapsed := "just started"
		if d := short(now.Sub(w.Started)); d != "" {
			elapsed = "running " + d
		}
		return title + fmt.Sprintf(" — %d/%d, %s", done, len(w.Checks), elapsed)
	}
	finished := "just now"
	if d := short(now.Sub(w.Updated)); d != "" {
		finished = d + " ago"
	}
	return title + " — " + finished
}

func checkTitle(c watch.Check) string {
	switch {
	case !c.Done():
		return fmt.Sprintf("⏳ %s — %s", c.Name, c.Status)
	case c.OK():
		return fmt.Sprintf("✔ %s — %s", c.Name, c.Conclusion)
	case c.Cancelled():
		return fmt.Sprintf("⊘ %s — %s", c.Name, c.Conclusion)
	default:
		return fmt.Sprintf("✘ %s — %s", c.Name, c.Conclusion)
	}
}

// short formats d coarsely ("5m", "2h", "3d"), or returns "" when it is less
// than a minute.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
