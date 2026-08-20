//go:build darwin

package app

import (
	"context"
	"errors"
)

func openPlatformTerminal(context.Context, TerminalSpec) (TerminalSession, error) {
	return nil, errors.New("local terminal is unavailable on this platform")
}
