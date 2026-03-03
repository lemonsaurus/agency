package palette

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	dialogTitleStyle = lipgloss.NewStyle().
				Foreground(colorMauve).
				Bold(true)

	dialogLabelStyle = lipgloss.NewStyle().
				Foreground(colorSubtext)

	dialogHintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585b70"))
)

type dialogModel struct {
	agentName  string
	agencyBin  string
	textInput  textinput.Model
	submitted  bool
	quitting   bool
}

func newDialogModel(agentName, agencyBin, defaultDir string) dialogModel {
	ti := textinput.New()
	ti.Placeholder = "~/projects/myapp"
	ti.SetValue(defaultDir)
	ti.Focus()
	ti.Width = 44
	ti.CharLimit = 256

	return dialogModel{
		agentName: agentName,
		agencyBin: agencyBin,
		textInput: ti,
	}
}

func (m dialogModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m dialogModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			m.submitted = true
			m.quitting = true
			return m, tea.Quit
		case "esc", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m dialogModel) View() string {
	if m.quitting {
		return ""
	}

	title := dialogTitleStyle.Render(fmt.Sprintf("Spawn %s", m.agentName))
	label := dialogLabelStyle.Render("Directory:")
	hint := dialogHintStyle.Render("Enter confirm · Esc cancel")

	return fmt.Sprintf(
		"\n  %s\n\n  %s\n  %s\n\n  %s\n",
		title,
		label,
		m.textInput.View(),
		hint,
	)
}

// Dir returns the submitted directory, or empty if cancelled.
func (m dialogModel) Dir() string {
	if !m.submitted {
		return ""
	}
	return strings.TrimSpace(m.textInput.Value())
}

// RunSpawnDialog launches the spawn dialog TUI and spawns the agent on confirm.
func RunSpawnDialog(agentName, agencyBin, defaultDir string) error {
	m := newDialogModel(agentName, agencyBin, defaultDir)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("spawn dialog: %w", err)
	}

	final := finalModel.(dialogModel)
	dir := final.Dir()
	if !final.submitted {
		return nil // cancelled
	}

	// Spawn the agent via CLI, passing the directory.
	args := []string{"spawn", agentName}
	if dir != "" {
		args = append(args, dir)
	}
	cmd := exec.Command(agencyBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
