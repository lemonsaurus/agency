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

var broadcastAccentColor = lipgloss.Color("#f9e2af") // yellow

type broadcastModel struct {
	agencyBin string
	textInput textinput.Model
	submitted bool
	quitting  bool
}

func newBroadcastModel(agencyBin string) broadcastModel {
	ti := textinput.New()
	ti.Placeholder = "/compact, /clear, or any text..."
	ti.Focus()
	ti.Width = 56
	ti.CharLimit = 1024

	return broadcastModel{
		agencyBin: agencyBin,
		textInput: ti,
	}
}

func (m broadcastModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m broadcastModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if strings.TrimSpace(m.textInput.Value()) != "" {
				m.submitted = true
			}
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

func (m broadcastModel) View() string {
	if m.quitting {
		return ""
	}

	title := lipgloss.NewStyle().
		Foreground(broadcastAccentColor).
		Bold(true).
		Render("Send to all agents")
	label := dialogLabelStyle.Render("Command:")
	hint := dialogHintStyle.Render("Enter send · Esc cancel")

	return fmt.Sprintf(
		"\n  %s\n\n  %s\n  %s\n\n  %s\n",
		title,
		label,
		m.textInput.View(),
		hint,
	)
}

// RunBroadcastDialog launches the broadcast TUI and sends text to all agent panes.
func RunBroadcastDialog(agencyBin string) error {
	m := newBroadcastModel(agencyBin)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("broadcast dialog: %w", err)
	}

	final := finalModel.(broadcastModel)
	if !final.submitted {
		return nil // cancelled
	}

	text := strings.TrimSpace(final.textInput.Value())
	if text == "" {
		return nil
	}

	cmd := exec.Command(agencyBin, "broadcast-keys", text)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
