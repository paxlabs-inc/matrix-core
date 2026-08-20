package tui

import (
	"fmt"
	"strings"

	"centra/protocol/codegraph-ultra/internal/model"
	"centra/protocol/codegraph-ultra/internal/store"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─────────────────────── palette ───────────────────────

var (
	bgColor     = lipgloss.Color("#0a0a0a")
	cardColor   = lipgloss.Color("#1a1a18")
	accentColor = lipgloss.Color("#fe9a00")
	textColor   = lipgloss.Color("#e3d9d4")
	mutedColor  = lipgloss.Color("#96918e")
	sageColor   = lipgloss.Color("#99bd9c")
)

// ─────────────────────── styles ───────────────────────

var (
	appStyle = lipgloss.NewStyle().
			Background(bgColor)

	headerStyle = lipgloss.NewStyle().
			Background(cardColor).
			Foreground(accentColor).
			Padding(0, 1).
			Bold(true)

	searchLabelStyle = lipgloss.NewStyle().
				Foreground(accentColor).
				Background(cardColor).
				Padding(0, 0, 0, 1)

	searchInputStyle = lipgloss.NewStyle().
				Foreground(textColor).
				Background(cardColor).
				Padding(0, 1)

	listItemStyle = lipgloss.NewStyle().
			Foreground(textColor).
			Background(bgColor).
			Padding(0, 2)

	listItemSelectedStyle = lipgloss.NewStyle().
				Foreground(bgColor).
				Background(accentColor).
				Padding(0, 2)

	listItemKindStyle = lipgloss.NewStyle().
				Foreground(sageColor).
				Width(12)

	listItemNameStyle = lipgloss.NewStyle().
				Foreground(textColor).
				Bold(true)

	listItemFileStyle = lipgloss.NewStyle().
				Foreground(mutedColor)

	detailPanelStyle = lipgloss.NewStyle().
				Background(cardColor).
				Foreground(textColor).
				Padding(1, 2)

	detailTitleStyle = lipgloss.NewStyle().
				Foreground(accentColor).
				Bold(true).
				MarginBottom(1)

	detailLabelStyle = lipgloss.NewStyle().
				Foreground(mutedColor).
				Width(12)

	detailValueStyle = lipgloss.NewStyle().
				Foreground(textColor)

	detailDocStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true).
			MarginTop(1)

	edgeHeaderStyle = lipgloss.NewStyle().
			Foreground(sageColor).
			Bold(true).
			MarginTop(1)

	statusBarStyle = lipgloss.NewStyle().
			Background(cardColor).
			Foreground(mutedColor).
			Padding(0, 1)

	statusKeyStyle = lipgloss.NewStyle().
			Foreground(accentColor).
			Background(cardColor)

	statusValueStyle = lipgloss.NewStyle().
				Foreground(mutedColor).
				Background(cardColor)

	mutedTextStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	treeBranchStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	treeNodeStyle = lipgloss.NewStyle().
			Foreground(textColor)
)

// ─────────────────────── model ───────────────────────

type viewMode int

const (
	viewList viewMode = iota
	viewDetail
	viewGraph
)

type TUIModel struct {
	db          *store.DB
	width       int
	height      int
	searchInput textinput.Model
	query       string
	nodes       []*model.Node
	selected    int
	detail      *model.Node
	edges       map[string][]*model.Node // edge type -> nodes
	mode        viewMode
	err         error
}

func NewModel(db *store.DB) TUIModel {
	ti := textinput.New()
	ti.Placeholder = "search symbols..."
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 60

	return TUIModel{
		db:          db,
		searchInput: ti,
		nodes:       nil,
		selected:    0,
		edges:       make(map[string][]*model.Node),
		mode:        viewList,
	}
}

// ─────────────────────── init ───────────────────────

func (m TUIModel) Init() tea.Cmd {
	return textinput.Blink
}

// ─────────────────────── update ───────────────────────

func (m TUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.searchInput.Width = msg.Width - 10
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			if m.mode == viewDetail || m.mode == viewGraph {
				m.mode = viewList
				m.detail = nil
				m.edges = make(map[string][]*model.Node)
				return m, nil
			}
			return m, tea.Quit

		case "esc":
			if m.mode == viewDetail || m.mode == viewGraph {
				m.mode = viewList
				m.detail = nil
				m.edges = make(map[string][]*model.Node)
				return m, nil
			}
			m.searchInput.SetValue("")
			m.query = ""
			m.nodes = nil
			m.selected = 0
			return m, nil

		case "/":
			if m.mode == viewList {
				m.searchInput.Focus()
				return m, textinput.Blink
			}

		case "enter":
			if m.mode == viewList && m.searchInput.Focused() {
				m.query = m.searchInput.Value()
				m.searchInput.Blur()
				m.doSearch()
				return m, nil
			}
			if m.mode == viewList && len(m.nodes) > 0 {
				m.drillIn()
				return m, nil
			}

		case "g":
			if !m.searchInput.Focused() && m.mode == viewDetail && m.detail != nil {
				m.mode = viewGraph
				m.loadEdges()
				return m, nil
			}

		case "j", "down":
			if !m.searchInput.Focused() && m.mode == viewList && m.selected < len(m.nodes)-1 {
				m.selected++
			}

		case "k", "up":
			if !m.searchInput.Focused() && m.mode == viewList && m.selected > 0 {
				m.selected--
			}

		case "tab":
			if m.mode == viewDetail {
				m.mode = viewGraph
				if len(m.edges) == 0 {
					m.loadEdges()
				}
			} else if m.mode == viewGraph {
				m.mode = viewDetail
			}
		}
	}

	// Update search input
	if m.searchInput.Focused() {
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

// ─────────────────────── search ───────────────────────

func (m *TUIModel) doSearch() {
	if m.query == "" {
		m.nodes = nil
		m.selected = 0
		return
	}
	nodes := m.db.Search(m.query, 100)
	m.err = nil
	m.nodes = nodes
	m.selected = 0
}

// ─────────────────────── drill in ───────────────────────

func (m *TUIModel) drillIn() {
	if m.selected >= len(m.nodes) {
		return
	}
	node := m.nodes[m.selected]
	m.detail = node
	m.mode = viewDetail
	m.edges = make(map[string][]*model.Node)
}

// ─────────────────────── load edges ───────────────────────

func (m *TUIModel) loadEdges() {
	if m.detail == nil {
		return
	}
	edgeTypes := []model.EdgeType{
		model.EdgeCalls,
		model.EdgeImports,
		model.EdgeReferences,
		model.EdgeContains,
		model.EdgeImplements,
		model.EdgeUses,
	}
	for _, et := range edgeTypes {
		ids := m.db.Forward(m.detail.ID, et)
		if len(ids) == 0 {
			continue
		}
		var nodes []*model.Node
		for _, id := range ids {
			if n := m.db.GetNode(id); n != nil {
				nodes = append(nodes, n)
			}
		}
		if len(nodes) > 0 {
			m.edges[string(et)] = nodes
		}
	}
}

// ─────────────────────── view ───────────────────────

func (m TUIModel) View() string {
	if m.width == 0 {
		return "loading..."
	}

	switch m.mode {
	case viewDetail:
		return m.viewDetail()
	case viewGraph:
		return m.viewGraph()
	default:
		return m.viewList()
	}
}

// ─────────────────────── list view ───────────────────────

func (m TUIModel) viewList() string {
	var b strings.Builder

	// Header
	title := headerStyle.Width(m.width).Render(" cg ")
	b.WriteString(title)
	b.WriteString("\n")

	// Search bar
	var searchBar string
	if m.searchInput.Focused() {
		searchBar = searchLabelStyle.Render("/") + searchInputStyle.Width(m.width-4).Render(m.searchInput.View())
	} else {
		if m.query != "" {
			searchBar = searchLabelStyle.Render("/") + searchInputStyle.Width(m.width-4).Render(m.query)
		} else {
			searchBar = searchLabelStyle.Render("/") + searchInputStyle.Width(m.width-4).Foreground(mutedColor).Render("type to search...")
		}
	}
	b.WriteString(searchBar)
	b.WriteString("\n")

	// Results
	listHeight := m.height - 6
	if listHeight < 3 {
		listHeight = 3
	}

	if len(m.nodes) == 0 && m.query != "" {
		b.WriteString(mutedTextStyle.Render("  no results"))
	} else if len(m.nodes) == 0 {
		b.WriteString(mutedTextStyle.Render("  press / to search"))
	} else {
		start := 0
		if m.selected >= listHeight {
			start = m.selected - listHeight + 1
		}
		end := start + listHeight
		if end > len(m.nodes) {
			end = len(m.nodes)
		}

		for i := start; i < end; i++ {
			node := m.nodes[i]
			kindStr := listItemKindStyle.Render(fmt.Sprintf("[%s]", node.Kind))
			nameStr := listItemNameStyle.Render(node.Name)
			fileStr := listItemFileStyle.Render(node.File)
			line := fmt.Sprintf("%s %s  %s", kindStr, nameStr, fileStr)

			if i == m.selected {
				b.WriteString(listItemSelectedStyle.Width(m.width).Render(line))
			} else {
				b.WriteString(listItemStyle.Width(m.width).Render(line))
			}
			b.WriteString("\n")
		}
	}

	// Status bar
	status := fmt.Sprintf(" %d results ", len(m.nodes))
	help := " / search  enter select  j/k navigate  q quit "
	statusLine := statusBarStyle.Width(m.width).Render(
		statusKeyStyle.Render(status) +
			statusValueStyle.Render(strings.Repeat(" ", m.width-len(status)-len(help)-2)) +
			statusValueStyle.Render(help),
	)
	b.WriteString(statusLine)

	return appStyle.Width(m.width).Height(m.height).Render(b.String())
}

// ─────────────────────── detail view ───────────────────────

func (m TUIModel) viewDetail() string {
	var b strings.Builder

	title := headerStyle.Width(m.width).Render(fmt.Sprintf(" %s ", m.detail.Name))
	b.WriteString(title)
	b.WriteString("\n")

	panel := m.detailPanel()
	b.WriteString(panel)

	help := " g graph  tab switch  q back "
	statusLine := statusBarStyle.Width(m.width).Render(statusValueStyle.Render(help))
	b.WriteString("\n")
	b.WriteString(statusLine)

	return appStyle.Width(m.width).Height(m.height).Render(b.String())
}

func (m TUIModel) detailPanel() string {
	d := m.detail
	if d == nil {
		return ""
	}

	var lines []string
	lines = append(lines, detailTitleStyle.Render(d.Name))
	lines = append(lines, "")
	lines = append(lines, m.detailRow("Kind", string(d.Kind)))
	lines = append(lines, m.detailRow("QName", d.QName))
	lines = append(lines, m.detailRow("Lang", d.Lang))
	lines = append(lines, m.detailRow("File", d.File))
	if d.Sig != "" {
		lines = append(lines, m.detailRow("Sig", d.Sig))
	}
	if d.Doc != "" {
		lines = append(lines, "")
		lines = append(lines, detailDocStyle.Render(d.Doc))
	}
	if d.Enrich.Summary != "" {
		lines = append(lines, "")
		lines = append(lines, detailLabelStyle.Render("Summary")+detailValueStyle.Render(d.Enrich.Summary))
	}
	if d.Enrich.Salience > 0 {
		lines = append(lines, detailLabelStyle.Render("Salience")+detailValueStyle.Render(fmt.Sprintf("%.2f", d.Enrich.Salience)))
	}

	content := strings.Join(lines, "\n")
	return detailPanelStyle.Width(m.width - 2).Render(content)
}

func (m TUIModel) detailRow(label, value string) string {
	if value == "" {
		return ""
	}
	return detailLabelStyle.Render(label) + detailValueStyle.Render(value)
}

// ─────────────────────── graph view ───────────────────────

func (m TUIModel) viewGraph() string {
	var b strings.Builder

	title := headerStyle.Width(m.width).Render(fmt.Sprintf(" graph: %s ", m.detail.Name))
	b.WriteString(title)
	b.WriteString("\n")

	if len(m.edges) == 0 {
		b.WriteString(mutedTextStyle.Render("  no edges found"))
	} else {
		content := m.renderEdgeTree()
		b.WriteString(detailPanelStyle.Width(m.width - 2).Render(content))
	}

	help := " tab switch  q back "
	statusLine := statusBarStyle.Width(m.width).Render(statusValueStyle.Render(help))
	b.WriteString("\n")
	b.WriteString(statusLine)

	return appStyle.Width(m.width).Height(m.height).Render(b.String())
}

func (m TUIModel) renderEdgeTree() string {
	var lines []string

	for edgeType, nodes := range m.edges {
		lines = append(lines, edgeHeaderStyle.Render(fmt.Sprintf("[%s] (%d)", edgeType, len(nodes))))
		for i, n := range nodes {
			prefix := "  +-- "
			if i == len(nodes)-1 {
				prefix = "  \\-- "
			}
			kindTag := treeBranchStyle.Render(prefix)
			name := treeNodeStyle.Render(n.Name)
			file := mutedTextStyle.Render(fmt.Sprintf("  %s", n.File))
			lines = append(lines, fmt.Sprintf("%s%s%s", kindTag, name, file))
		}
		lines = append(lines, "")
	}

	return strings.Join(lines, "\n")
}

// ─────────────────────── run ───────────────────────

func Run(db *store.DB) error {
	m := NewModel(db)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
