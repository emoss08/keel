// Package term detects whether keelcli's streams are terminals and how wide they are.
package term

import (
	xterm "github.com/charmbracelet/x/term"
)

type fder interface {
	Fd() uintptr
}

// IsTerminal reports whether stream is a file attached to a terminal.
func IsTerminal(stream any) bool {
	f, ok := stream.(fder)
	return ok && xterm.IsTerminal(f.Fd())
}

// Width is the terminal's width in cells, or 0 when stream is not a terminal.
func Width(stream any) int {
	f, ok := stream.(fder)
	if !ok || !xterm.IsTerminal(f.Fd()) {
		return 0
	}
	w, _, err := xterm.GetSize(f.Fd())
	if err != nil {
		return 0
	}
	return w
}
