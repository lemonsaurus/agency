package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lemonsaurus/agency/internal/budget"
)

const topRefresh = 2 * time.Second

type topModel struct {
	session       string
	snapshot      budget.Snapshot
	live, loaded  bool
	width, height int
}

type topReading struct {
	snapshot budget.Snapshot
	live     bool
}

func (m topModel) read() tea.Cmd {
	return func() tea.Msg {
		snapshot, live := readBudget(m.session)
		return topReading{snapshot, live}
	}
}

func (m topModel) Init() tea.Cmd { return m.read() }

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case topReading:
		m.snapshot, m.live, m.loaded = msg.snapshot, msg.live, true
		return m, tea.Tick(topRefresh, func(time.Time) tea.Msg { return m.read()() })
	}
	return m, nil
}

func (m topModel) View() string {
	if !m.loaded {
		return budgetDim.Render(" reading the box…")
	}
	fixed := 9 + len(m.snapshot.Queue)
	for _, e := range m.snapshot.Entries {
		if e.Kind == budget.KindJob {
			fixed++
		}
	}
	if len(m.snapshot.Queue) > 0 {
		fixed += 2
	}
	rows := max(3, m.height-fixed-2)
	view := renderBudget(m.snapshot, m.live, m.width, rows)
	return view + "\n " + budgetFaint.Render(strings.TrimSpace(fmt.Sprintf("q quit · refreshes every %s", topRefresh)))
}

// runTop shows the budget live until q.
func runTop() {
	cfg := loadConfig()
	if _, err := tea.NewProgram(topModel{session: cfg.Session.Name}, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
