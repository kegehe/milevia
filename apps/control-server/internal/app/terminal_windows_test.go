//go:build windows

package app

import "testing"

func TestQuoteWindowsEscapesTrailingBackslashAndQuotes(t *testing.T) {
	if got := quoteWindows(`C:\work dir\`); got != `"C:\work dir\\"` {
		t.Fatalf("trailing backslash quote=%q", got)
	}
	if got := quoteWindows(`a"b`); got != `"a\"b"` {
		t.Fatalf("embedded quote=%q", got)
	}
}

func TestWindowsTerminalCommandUsesWSLPathAndDistro(t *testing.T) {
	command, err := windowsTerminalCommand(TerminalSpec{
		RunnerID:  "wsl-local",
		WorkDir:   `\\wsl$\Ubuntu\home\dev\project`,
		WSLDistro: "Ubuntu",
	}, "__READY__")
	if err != nil {
		t.Fatalf("build WSL terminal command: %v", err)
	}
	want := `wsl.exe -d Ubuntu --cd /home/dev/project --exec /bin/sh -lc "printf '\n__READY__\n'; exec \"${SHELL:-/bin/sh}\" -l"`
	if command != want {
		t.Fatalf("command=%q, want %q", command, want)
	}
}

func TestWindowsTerminalCommandRejectsNonWSLPath(t *testing.T) {
	if _, err := windowsTerminalCommand(TerminalSpec{RunnerID: "wsl-local", WorkDir: `C:\project`, WSLDistro: "Ubuntu"}, "__READY__"); err == nil {
		t.Fatal("accepted a non-WSL working directory")
	}
}
