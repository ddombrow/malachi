// Package tui is malachi's interactive terminal frontend.
package tui

import (
	"errors"

	"github.com/ddombrow/malachi/coding"
)

// Run starts the interactive UI. initialPrompt, if non-empty, is sent first.
func Run(s *coding.Session, initialPrompt string) error {
	return errors.New("interactive mode is not implemented yet; use -p")
}
