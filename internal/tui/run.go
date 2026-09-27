package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"
)

func Run(ctx context.Context, api API, options Options) error {
	program := tea.NewProgram(NewModel(api, options), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := program.Run()
	if errors.Is(err, context.Canceled) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}
