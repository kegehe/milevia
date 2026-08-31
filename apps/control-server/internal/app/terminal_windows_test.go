//go:build windows

package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestWindowsTerminalUsesHiddenConsoleAndStreamsOutput(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("PowerShell is unavailable for the console-window probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := openPlatformTerminal(ctx, TerminalSpec{ProjectID: "project", WorkDir: t.TempDir(), Cols: 120, Rows: 36})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	defer func() { _ = session.Close(); _ = session.Wait() }()
	result := make(chan terminalProbeResult, 1)
	// ConPTY can emit terminal setup sequences before the ready marker. Drain
	// them before waiting for Ready so the session's io.Pipe cannot block it.
	go readTerminalProbe(session, result)
	select {
	case err := <-session.Ready():
		if err != nil {
			t.Fatalf("terminal did not become ready: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("terminal did not become ready before timeout")
	}

	probe := `using System;
using System.Runtime.InteropServices;
public static class NativeConsole {
  [DllImport("kernel32.dll")] public static extern IntPtr GetConsoleWindow();
  [DllImport("user32.dll")] [return: MarshalAs(UnmanagedType.Bool)] public static extern bool IsWindowVisible(IntPtr window);
}`
	script := "Add-Type -TypeDefinition '" + probe + "'; $window = [NativeConsole]::GetConsoleWindow(); if ($window -ne [IntPtr]::Zero -and [NativeConsole]::IsWindowVisible($window)) { Write-Output MILEVIA_CONSOLE_VISIBLE } else { Write-Output MILEVIA_CONSOLE_HIDDEN }"
	command := "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + encodePowerShellCommand(script) + "\r\n"
	if _, err := session.Write([]byte(command)); err != nil {
		t.Fatalf("write terminal probe: %v", err)
	}
	select {
	case output := <-result:
		if output.err != nil {
			t.Fatalf("read terminal probe: %v\noutput: %s", output.err, output.output)
		}
		if strings.Contains(output.output, "MILEVIA_CONSOLE_VISIBLE") {
			t.Fatalf("terminal shell has a visible console window\noutput: %s", output.output)
		}
		if !strings.Contains(output.output, "MILEVIA_CONSOLE_HIDDEN") {
			t.Fatalf("terminal probe result missing\noutput: %s", output.output)
		}
	case <-ctx.Done():
		t.Fatal("terminal probe timed out")
	}
}

type terminalProbeResult struct {
	output string
	err    error
}

func readTerminalProbe(session TerminalSession, result chan<- terminalProbeResult) {
	buffer := make([]byte, 4096)
	var output strings.Builder
	for {
		n, err := session.Read(buffer)
		if n > 0 {
			output.Write(buffer[:n])
			if strings.Contains(output.String(), "MILEVIA_CONSOLE_VISIBLE") || strings.Contains(output.String(), "MILEVIA_CONSOLE_HIDDEN") {
				result <- terminalProbeResult{output: output.String()}
				return
			}
		}
		if err != nil {
			result <- terminalProbeResult{output: output.String(), err: err}
			return
		}
	}
}

func encodePowerShellCommand(script string) string {
	chars := utf16.Encode([]rune(script))
	data := make([]byte, len(chars)*2)
	for index, char := range chars {
		binary.LittleEndian.PutUint16(data[index*2:], char)
	}
	return base64.StdEncoding.EncodeToString(data)
}

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
