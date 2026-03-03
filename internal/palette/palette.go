package palette

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/lemonsaurus/agency/internal/agents"
)

var (
	// Catppuccin Mocha palette.
	colorBase    = lipgloss.Color("#1e1e2e")
	colorText    = lipgloss.Color("#cdd6f4")
	colorSubtext = lipgloss.Color("#a6adc8")
	colorBlue    = lipgloss.Color("#89b4fa")
	colorMauve   = lipgloss.Color("#cba6f7")

	titleStyle = lipgloss.NewStyle().
			Foreground(colorMauve).
			Bold(true).
			Padding(0, 1)

	docStyle = lipgloss.NewStyle().
			Background(colorBase).
			Padding(1, 2)
)

// agentItem implements list.Item for the Bubble Tea list.
type agentItem struct {
	name    string
	icon    string
	command string
	custom  bool
}

func (i agentItem) Title() string {
	if i.custom {
		return "⚡ Custom command..."
	}
	return fmt.Sprintf("%s  %s", i.icon, i.name)
}

func (i agentItem) Description() string {
	if i.custom {
		return "Run any command"
	}
	return i.command
}

func (i agentItem) FilterValue() string {
	return i.name
}

type model struct {
	list       list.Model
	agencyBin  string
	choice     string
	quitting   bool
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if item, ok := m.list.SelectedItem().(agentItem); ok {
				if item.custom {
					m.choice = "custom"
				} else {
					m.choice = item.name
				}
			}
			m.quitting = true
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	return docStyle.Render(m.list.View())
}

// Run launches the command palette TUI and spawns the selected agent.
func Run(registry *agents.Registry, agencyBin string) error {
	items := buildItems(registry)

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(colorBlue).
		BorderForeground(colorBlue)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(colorSubtext).
		BorderForeground(colorBlue)
	delegate.Styles.NormalTitle = delegate.Styles.NormalTitle.
		Foreground(colorText)
	delegate.Styles.NormalDesc = delegate.Styles.NormalDesc.
		Foreground(colorSubtext)

	l := list.New(items, delegate, 36, 12)
	l.Title = "Spawn Agent"
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)

	m := model{
		list:      l,
		agencyBin: agencyBin,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("palette: %w", err)
	}

	final := finalModel.(model)
	if final.choice == "" {
		return nil // user cancelled
	}

	if final.choice == "custom" {
		// For custom, just print instructions — the user types in the popup.
		fmt.Println("Use: agency spawn --cmd \"your command\"")
		return nil
	}

	// Chain into the spawn dialog so the user can pick a directory.
	cmd := exec.Command(agencyBin, "spawn-dialog", final.choice)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildItems(registry *agents.Registry) []list.Item {
	var items []list.Item
	for _, name := range registry.Names() {
		agent, _ := registry.Get(name)
		items = append(items, agentItem{
			name:    name,
			icon:    agent.Icon,
			command: agent.Command,
		})
	}
	items = append(items, agentItem{custom: true, name: "custom"})
	return items
}
