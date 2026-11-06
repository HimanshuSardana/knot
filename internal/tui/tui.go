// Package tui renders knot's two-pane terminal browser.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/HimanshuSardana/knot/internal/cluster"
	"github.com/HimanshuSardana/knot/internal/node"
	"github.com/HimanshuSardana/knot/internal/ops"
)

var (
	colBorder     = lipgloss.AdaptiveColor{Light: "240", Dark: "240"}
	colBorderHot  = lipgloss.AdaptiveColor{Light: "63", Dark: "111"}
	colDim        = lipgloss.AdaptiveColor{Light: "245", Dark: "243"}
	colDir        = lipgloss.AdaptiveColor{Light: "25", Dark: "111"}
	colErr        = lipgloss.AdaptiveColor{Light: "124", Dark: "203"}
	colOK         = lipgloss.AdaptiveColor{Light: "28", Dark: "114"}
	colCursorBg   = lipgloss.AdaptiveColor{Light: "153", Dark: "24"}
	colSelectedBg = lipgloss.AdaptiveColor{Light: "222", Dark: "53"}
)

// pane is one side of the browser: a node, a directory, and a cursor.
type pane struct {
	nodeIdx  int
	path     string
	entries  []node.Entry
	cursor   int
	offset   int
	selected map[string]bool
	err      string
	loading  bool
}

func newPane(nodeIdx int, path string) pane {
	return pane{nodeIdx: nodeIdx, path: path, selected: map[string]bool{}}
}

// modal tracks transient input modes so keys are not overloaded.
type modal int

const (
	modalNone modal = iota
	modalMkdir
	modalRename
	modalConfirm
)

type loadedMsg struct {
	pane    int
	path    string
	entries []node.Entry
	err     error
}

type opDoneMsg struct {
	status string
	err    error
}

type tickMsg struct{}

// progressState carries live transfer counters from the worker goroutine to the
// render loop without either side blocking on the other.
type progressState struct {
	copied atomic.Int64
	total  atomic.Int64
	label  string
}

type Model struct {
	cl    *cluster.Cluster
	names []string

	panes  [2]pane
	active int

	width, height int
	status        string
	statusErr     bool

	modal      modal
	input      textinput.Model
	confirmMsg string
	confirmAct func() tea.Msg
	busy       bool

	// pendingCursor restores the cursor onto the directory we just left.
	pendingCursor string
	prog          *progressState
}

// New builds the TUI model. Pane one starts on the first node's home, pane two
// on the second node's home so that "move to the other device" is the default.
func New(cl *cluster.Cluster) *Model {
	names := cl.Names()

	m := &Model{cl: cl, names: names}

	ti := textinput.New()
	ti.CharLimit = 512
	ti.Width = 60
	m.input = ti

	left := 0
	right := 0
	if len(names) > 1 {
		right = 1
	}

	m.panes[0] = newPane(left, m.homeOf(left))
	m.panes[1] = newPane(right, m.homeOf(right))
	return m
}

func (m *Model) homeOf(idx int) string {
	names := m.names
	if len(names) == 0 {
		return "/"
	}
	n, err := m.cl.Node(names[idx])
	if err != nil {
		return "/"
	}
	return n.Home()
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.load(0), m.load(1))
}

func (m *Model) nodeFor(p *pane) node.Node {
	if len(m.names) == 0 {
		return nil
	}
	n, err := m.cl.Node(m.names[p.nodeIdx])
	if err != nil {
		return nil
	}
	return n
}

// load refreshes one pane from its node.
func (m *Model) load(idx int) tea.Cmd {
	p := m.panes[idx]
	name := m.names[p.nodeIdx]
	path := p.path
	cl := m.cl

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		n, err := cl.Node(name)
		if err != nil {
			return loadedMsg{pane: idx, path: path, err: err}
		}
		entries, err := n.List(ctx, path)
		if err != nil {
			return loadedMsg{pane: idx, path: path, err: err}
		}
		return loadedMsg{pane: idx, path: path, entries: entries}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case loadedMsg:
		p := &m.panes[msg.pane]
		// Ignore a late reply for a directory we have already left.
		if msg.path != p.path {
			return m, nil
		}
		p.loading = false
		if msg.err != nil {
			p.err = friendlyErr(msg.err)
			p.entries = nil
			return m, nil
		}
		p.err = ""
		prev := m.cursorName(msg.pane)
		if m.pendingCursor != "" && msg.pane == m.active {
			prev = m.pendingCursor
			m.pendingCursor = ""
		}
		p.entries = msg.entries
		p.cursor = 0
		if prev != "" {
			for i, e := range p.entries {
				if e.Name == prev {
					p.cursor = i
					break
				}
			}
		}
		m.clampOffset(msg.pane)
		return m, nil

	case opDoneMsg:
		m.busy = false
		m.prog = nil
		if msg.err != nil {
			m.status = friendlyErr(msg.err)
			m.statusErr = true
			return m, nil
		}
		m.status = msg.status
		m.statusErr = false
		for i := range m.panes {
			m.panes[i].selected = map[string]bool{}
		}
		return m, tea.Batch(m.load(0), m.load(1))

	case tickMsg:
		if !m.busy {
			return m, nil
		}
		if m.prog != nil {
			copied := m.prog.copied.Load()
			total := m.prog.total.Load()
			if total > 0 {
				pct := copied * 100 / total
				m.status = fmt.Sprintf("%s %d%% (%s / %s)", m.prog.label, pct, humanSize(copied), humanSize(total))
			} else {
				m.status = fmt.Sprintf("%s %s", m.prog.label, humanSize(copied))
			}
		}
		return m, m.tick()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) cursorName(idx int) string {
	p := &m.panes[idx]
	if p.cursor >= 0 && p.cursor < len(p.entries) {
		return p.entries[p.cursor].Name
	}
	return ""
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Modal input first: it owns every key while open.
	if m.modal != modalNone {
		return m.handleModal(msg)
	}

	p := &m.panes[m.active]

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "tab", "shift+tab", "left", "right":
		m.active = 1 - m.active
		return m, nil

	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
			m.clampOffset(m.active)
		}
		return m, nil

	case "down", "j":
		if p.cursor < len(p.entries)-1 {
			p.cursor++
			m.clampOffset(m.active)
		}
		return m, nil

	case "pgup":
		p.cursor -= m.rows()
		if p.cursor < 0 {
			p.cursor = 0
		}
		m.clampOffset(m.active)
		return m, nil

	case "pgdown":
		p.cursor += m.rows()
		if p.cursor > len(p.entries)-1 {
			p.cursor = max(0, len(p.entries)-1)
		}
		m.clampOffset(m.active)
		return m, nil

	case "home", "g":
		p.cursor = 0
		m.clampOffset(m.active)
		return m, nil

	case "end", "G":
		p.cursor = max(0, len(p.entries)-1)
		m.clampOffset(m.active)
		return m, nil

	case "enter", "l":
		if p.cursor < len(p.entries) && p.entries[p.cursor].IsDir {
			p.path = node.Join(p.path, p.entries[p.cursor].Name)
			p.cursor = 0
			p.offset = 0
			p.selected = map[string]bool{}
			p.loading = true
			return m, m.load(m.active)
		}
		return m, nil

	case "backspace", "h":
		if p.path != "/" {
			// Remember where we came from so the cursor lands back on it.
			m.pendingCursor = node.Base(p.path)
			p.path = node.Parent(p.path)
			p.selected = map[string]bool{}
			p.loading = true
			return m, m.load(m.active)
		}
		return m, nil

	case "n":
		m.cycleNode(1)
		return m, nil
	case "N":
		m.cycleNode(-1)
		return m, nil

	case " ":
		name := m.cursorName(m.active)
		if name != "" {
			if p.selected[name] {
				delete(p.selected, name)
			} else {
				p.selected[name] = true
			}
			if p.cursor < len(p.entries)-1 {
				p.cursor++
				m.clampOffset(m.active)
			}
		}
		return m, nil

	case "r", "ctrl+r":
		p.loading = true
		return m, m.load(m.active)

	case "c":
		return m.transfer(false)
	case "m":
		return m.transfer(true)

	case "d":
		targets := m.targets(m.active)
		if len(targets) == 0 {
			return m, nil
		}
		label := targets[0]
		if len(targets) > 1 {
			label = fmt.Sprintf("%d items", len(targets))
		}
		m.modal = modalConfirm
		m.confirmMsg = "Delete " + label + "?"
		idx := m.active
		m.confirmAct = func() tea.Msg {
			return m.doDelete(idx, targets)
		}
		return m, nil

	case "a":
		m.modal = modalMkdir
		m.input.Placeholder = "directory name"
		m.input.SetValue("")
		m.input.Focus()
		return m, textinput.Blink

	case "R":
		targets := m.targets(m.active)
		if len(targets) != 1 {
			m.status = "select exactly one item to rename"
			m.statusErr = true
			return m, nil
		}
		m.modal = modalRename
		m.input.Placeholder = "new name"
		m.input.SetValue(targets[0])
		m.input.CursorEnd()
		m.input.Focus()
		return m, textinput.Blink
	}

	return m, nil
}

// tick schedules the next progress repaint.
func (m *Model) tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) handleModal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.modal = modalNone
		m.input.Blur()
		return m, nil
	}

	if m.modal == modalConfirm {
		switch msg.String() {
		case "y", "enter":
			m.modal = modalNone
			m.busy = true
			m.status = "working…"
			m.statusErr = false
			return m, m.confirmAct
		case "n":
			m.modal = modalNone
		}
		return m, nil
	}

	switch msg.String() {
	case "enter":
		value := strings.TrimSpace(m.input.Value())
		kind := m.modal
		m.modal = modalNone
		m.input.Blur()
		if value == "" {
			return m, nil
		}
		m.busy = true
		m.status = "working…"
		m.statusErr = false
		return m, m.doModal(kind, value)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) doModal(kind modal, value string) tea.Cmd {
	idx := m.active
	p := m.panes[idx]
	name := m.names[p.nodeIdx]
	path := p.path
	cl := m.cl

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		n, err := cl.Node(name)
		if err != nil {
			return opDoneMsg{err: err}
		}

		switch kind {
		case modalMkdir:
			if err := n.Mkdir(ctx, node.Join(path, value)); err != nil {
				return opDoneMsg{err: err}
			}
			return opDoneMsg{status: "created " + value}
		case modalRename:
			targets := m.targets(idx)
			if len(targets) != 1 {
				return opDoneMsg{err: fmt.Errorf("nothing selected")}
			}
			if err := n.Rename(ctx, node.Join(path, targets[0]), node.Join(path, value)); err != nil {
				return opDoneMsg{err: err}
			}
			return opDoneMsg{status: "renamed to " + value}
		}
		return opDoneMsg{}
	}
}

func (m *Model) doDelete(idx int, targets []string) tea.Msg {
	p := m.panes[idx]
	name := m.names[p.nodeIdx]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	n, err := m.cl.Node(name)
	if err != nil {
		return opDoneMsg{err: err}
	}
	for _, t := range targets {
		if err := n.Remove(ctx, node.Join(p.path, t)); err != nil {
			return opDoneMsg{err: err}
		}
	}
	return opDoneMsg{status: fmt.Sprintf("deleted %d item(s)", len(targets))}
}

// targets returns selected names, falling back to the item under the cursor.
func (m *Model) targets(idx int) []string {
	p := &m.panes[idx]
	if len(p.selected) > 0 {
		out := make([]string, 0, len(p.selected))
		for _, e := range p.entries {
			if p.selected[e.Name] {
				out = append(out, e.Name)
			}
		}
		return out
	}
	if name := m.cursorName(idx); name != "" {
		return []string{name}
	}
	return nil
}

// transfer copies or moves the active pane's selection into the other pane.
func (m *Model) transfer(move bool) (tea.Model, tea.Cmd) {
	targets := m.targets(m.active)
	if len(targets) == 0 {
		return m, nil
	}

	src := m.panes[m.active]
	dst := m.panes[1-m.active]
	srcName := m.names[src.nodeIdx]
	dstName := m.names[dst.nodeIdx]

	cl := m.cl
	srcPath, dstPath := src.path, dst.path

	verb := "copying"
	if move {
		verb = "moving"
	}

	prog := &progressState{label: verb}

	m.busy = true
	m.prog = prog
	m.status = verb + "…"
	m.statusErr = false

	return m, tea.Batch(func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		s, err := cl.Node(srcName)
		if err != nil {
			return opDoneMsg{err: err}
		}
		d, err := cl.Node(dstName)
		if err != nil {
			return opDoneMsg{err: err}
		}

		var base int64
		for _, t := range targets {
			if err := ctx.Err(); err != nil {
				return opDoneMsg{err: err}
			}
			if total, terr := ops.Total(ctx, s, node.Join(srcPath, t)); terr == nil {
				prog.total.Add(total)
			}
			start := base
			report := func(copied, _ int64) {
				prog.copied.Store(start + copied)
			}
			if move {
				err = ops.Move(ctx, s, node.Join(srcPath, t), d, node.Join(dstPath, t), report)
			} else {
				err = ops.Copy(ctx, s, node.Join(srcPath, t), d, node.Join(dstPath, t), report)
			}
			if err != nil {
				return opDoneMsg{err: err}
			}
			if total, terr := ops.Total(ctx, d, node.Join(dstPath, t)); terr == nil {
				base += total
			}
		}

		return opDoneMsg{status: fmt.Sprintf("%s %d item(s) to %s", verb, len(targets), dstName)}
	}, m.tick())
}

func (m *Model) cycleNode(delta int) {
	p := &m.panes[m.active]
	if len(m.names) == 0 {
		return
	}
	next := (p.nodeIdx + delta + len(m.names)) % len(m.names)
	p.nodeIdx = next
	p.path = m.homeOf(next)
	p.cursor, p.offset = 0, 0
	p.selected = map[string]bool{}
	p.loading = true
	p.err = ""
}

// viewWidth and viewHeight fall back to a sane size when the terminal has not
// reported one yet. Some pseudo-terminals report 0x0, and without a fallback
// the UI would render nothing at all.
func (m *Model) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

func (m *Model) viewHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 24
}

// rows is the number of list lines each pane can show.
func (m *Model) rows() int {
	h := m.viewHeight() - 7 // header, borders, footer
	if h < 1 {
		return 1
	}
	return h
}

func (m *Model) clampOffset(idx int) {
	p := &m.panes[idx]
	rows := m.rows()
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+rows {
		p.offset = p.cursor - rows + 1
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

func (m *Model) View() string {
	paneW := (m.viewWidth() - 3) / 2
	if paneW < 20 {
		paneW = 20
	}

	left := m.viewPane(0, paneW)
	right := m.viewPane(1, paneW)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)

	return strings.Join([]string{
		m.viewHeader(),
		body,
		m.viewFooter(),
	}, "\n")
}

func (m *Model) viewHeader() string {
	title := lipgloss.NewStyle().Bold(true).Render("knot")
	tail := lipgloss.NewStyle().Foreground(colDim).Render("tailnet file browser")
	return title + "  " + tail
}

func (m *Model) viewPane(idx, width int) string {
	p := &m.panes[idx]
	active := idx == m.active

	borderCol := colBorder
	if active {
		borderCol = colBorderHot
	}

	name := "?"
	if len(m.names) > 0 {
		name = m.names[p.nodeIdx]
	}
	label := name + ":" + p.path
	innerW := width - 4
	if innerW < 8 {
		innerW = 8
	}

	var lines []string
	if p.err != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colErr).Render(clip(p.err, innerW)))
	} else if p.loading && len(p.entries) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colDim).Render("loading…"))
	} else if len(p.entries) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colDim).Render("empty"))
	}

	rows := m.rows()
	end := p.offset + rows
	if end > len(p.entries) {
		end = len(p.entries)
	}
	for i := p.offset; i < end; i++ {
		lines = append(lines, m.viewEntry(p, i, innerW))
	}

	// Pad so both panes keep the same height.
	for len(lines) < rows+1 {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")

	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderCol).
		Width(width-2).
		Height(rows+1).
		Padding(0, 1)

	header := lipgloss.NewStyle().
		Foreground(borderCol).
		Bold(active).
		Render(clip(label, innerW))

	// Replace the top border line with one carrying the label.
	boxed := style.Render(content)
	boxLines := strings.Split(boxed, "\n")
	if len(boxLines) > 0 {
		top := "┌─ " + header + " "
		pad := width - lipgloss.Width(top) - 1
		if pad < 0 {
			pad = 0
		}
		boxLines[0] = lipgloss.NewStyle().Foreground(borderCol).Render(top + strings.Repeat("─", pad) + "┐")
	}
	return strings.Join(boxLines, "\n")
}

func (m *Model) viewEntry(p *pane, i, width int) string {
	e := p.entries[i]
	isCursor := i == p.cursor
	_, isSel := p.selected[e.Name]

	sizeCol := 8
	dateCol := 8
	nameW := width - sizeCol - dateCol - 3
	if nameW < 6 {
		nameW = 6
	}

	name := e.Name
	if e.IsDir {
		name += "/"
	}
	name = clip(name, nameW)
	pad := nameW - lipgloss.Width(name)

	size := humanSize(e.Size)
	if e.IsDir {
		size = "-"
	}

	line := name + strings.Repeat(" ", max(0, pad)) + " " +
		padLeft(size, sizeCol) + " " +
		padLeft(shortAge(e.ModTime), dateCol)

	if isCursor {
		marker := " "
		if isSel {
			marker = "*"
		}
		return lipgloss.NewStyle().
			Background(colCursorBg).
			Width(width).
			Render(marker + line)
	}

	style := lipgloss.NewStyle()
	if isSel {
		style = style.Background(colSelectedBg)
	}
	if e.IsDir {
		style = style.Foreground(colDir)
	}
	return style.Render(" " + line)
}

func (m *Model) viewFooter() string {
	keys := "tab switch · n node · enter open · h up · space select · c copy · m move · d delete · R rename · a mkdir · r refresh · q quit"
	help := lipgloss.NewStyle().Foreground(colDim).Render(clip(keys, m.viewWidth()))

	if m.modal == modalConfirm {
		return lipgloss.NewStyle().Foreground(colErr).Bold(true).
			Render(m.confirmMsg+" [y/n]") + "\n" + help
	}
	if m.modal == modalMkdir || m.modal == modalRename {
		return m.input.View() + "\n" + help
	}

	status := m.status
	if status == "" {
		status = "ready"
	}
	style := lipgloss.NewStyle().Foreground(colOK)
	if m.statusErr {
		style = lipgloss.NewStyle().Foreground(colErr)
	}
	return style.Render(clip(status, m.viewWidth())) + "\n" + help
}

// friendlyErr shortens the noisy errors SSH and SFTP produce.
func friendlyErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "unable to authenticate"), strings.Contains(s, "no supported methods"):
		return "auth failed — needs a password (press n to switch node, or set up a key)"
	case strings.Contains(s, "no such file"):
		return "no such file or directory"
	case strings.Contains(s, "connection refused"):
		return "connection refused — is sshd running?"
	case strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return "timed out — device offline?"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return "unreachable — is the device on the tailnet?"
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTP"[exp])
}

func shortAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}

func padLeft(s string, w int) string {
	d := w - lipgloss.Width(s)
	if d <= 0 {
		return s
	}
	return strings.Repeat(" ", d) + s
}

// clip truncates to w display cells, adding an ellipsis when shortened.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > w-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
