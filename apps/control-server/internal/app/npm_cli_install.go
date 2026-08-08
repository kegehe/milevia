package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type npmCLIInstall struct {
	scope       string
	packageName string
	commandName string
	binFile     string
}

func (install npmCLIInstall) packageRoot(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, "node_modules", install.scope)
	}
	return filepath.Join(prefix, "lib", "node_modules", install.scope)
}

func (install npmCLIInstall) binaryPath(prefix string) string {
	return filepath.Join(install.packageRoot(prefix), install.packageName, "bin", install.binFile)
}

func (install npmCLIInstall) commandPath(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, install.commandName+".cmd")
	}
	return filepath.Join(prefix, "bin", install.commandName)
}

type npmCLIRecovery struct {
	prefix  string
	install npmCLIInstall
}

// prepareNpmCLIRecovery records a rollback target only when the command that
// was healthy before the update resolves to this npm global package.
func prepareNpmCLIRecovery(ctx context.Context, command string, install npmCLIInstall) (npmCLIRecovery, error) {
	commandPath, err := exec.LookPath(command)
	if err != nil {
		return npmCLIRecovery{}, fmt.Errorf("locate CLI command: %w", err)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(lookupCtx, "npm", "prefix", "-g").Output()
	if err != nil {
		return npmCLIRecovery{}, fmt.Errorf("locate npm global prefix: %w", err)
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return npmCLIRecovery{}, errors.New("npm global prefix is empty")
	}
	if runtime.GOOS == "windows" {
		if !sameCleanPath(commandPath, install.commandPath(prefix)) {
			return npmCLIRecovery{}, errors.New("CLI command is not the npm global command shim")
		}
		return npmCLIRecovery{prefix: prefix, install: install}, nil
	}
	actual, err := filepath.EvalSymlinks(commandPath)
	if err != nil {
		return npmCLIRecovery{}, fmt.Errorf("resolve CLI command: %w", err)
	}
	expected, err := filepath.EvalSymlinks(install.binaryPath(prefix))
	if err != nil {
		return npmCLIRecovery{}, fmt.Errorf("resolve npm package command: %w", err)
	}
	if !sameCleanPath(actual, expected) {
		return npmCLIRecovery{}, errors.New("CLI command does not resolve to the npm global package")
	}
	return npmCLIRecovery{prefix: prefix, install: install}, nil
}

func sameCleanPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func npmPackageVersion(packageDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(packageDir, "package.json"))
	if err != nil {
		return "", err
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", err
	}
	if metadata.Version == "" {
		return "", errors.New("npm package has no version")
	}
	return metadata.Version, nil
}

// rollbackInterruptedNpmInstall restores the complete package npm parked next
// to the active package during an interrupted global update. The incomplete
// package is retained for diagnosis or manual recovery.
func rollbackInterruptedNpmInstall(prefix, previous string, install npmCLIInstall) (string, error) {
	packageRoot := install.packageRoot(prefix)
	active := filepath.Join(packageRoot, install.packageName)
	entries, err := os.ReadDir(packageRoot)
	if err != nil {
		return "", fmt.Errorf("read npm package directory: %w", err)
	}
	backup := ""
	backupPrefix := "." + install.packageName + "-"
	interruptedPrefix := "." + install.packageName + "-interrupted-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), backupPrefix) || strings.HasPrefix(entry.Name(), interruptedPrefix) {
			continue
		}
		candidate := filepath.Join(packageRoot, entry.Name())
		version, versionErr := npmPackageVersion(candidate)
		if versionErr != nil || version != previous {
			continue
		}
		binary, statErr := os.Stat(filepath.Join(candidate, "bin", install.binFile))
		if statErr == nil && !binary.IsDir() && (runtime.GOOS == "windows" || binary.Mode()&0o111 != 0) {
			backup = candidate
			break
		}
	}
	if backup == "" {
		return "", fmt.Errorf("no complete npm backup for %s %s", install.commandName, previous)
	}
	interrupted := ""
	if _, err := os.Lstat(active); err == nil {
		interrupted = filepath.Join(packageRoot, fmt.Sprintf(".%s-interrupted-%d", install.packageName, time.Now().UTC().UnixNano()))
		if err := os.Rename(active, interrupted); err != nil {
			return "", fmt.Errorf("preserve interrupted npm package: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect interrupted npm package: %w", err)
	}
	if err := os.Rename(backup, active); err != nil {
		if interrupted != "" {
			_ = os.Rename(interrupted, active)
		}
		return "", fmt.Errorf("restore previous npm package: %w", err)
	}
	if err := ensureNpmCLICommand(prefix, install); err != nil {
		return "", err
	}
	return previous, nil
}

func ensureNpmCLICommand(prefix string, install npmCLIInstall) error {
	if runtime.GOOS == "windows" {
		return ensureWindowsNpmCLICommand(prefix, install)
	}
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create npm global bin directory: %w", err)
	}
	command := filepath.Join(binDir, install.commandName)
	if _, err := os.Lstat(command); err == nil {
		if err := os.Remove(command); err != nil {
			return fmt.Errorf("replace npm global %s command: %w", install.commandName, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect npm global %s command: %w", install.commandName, err)
	}
	target := filepath.Join("..", "lib", "node_modules", install.scope, install.packageName, "bin", install.binFile)
	return os.Symlink(target, command)
}

func ensureWindowsNpmCLICommand(prefix string, install npmCLIInstall) error {
	target := filepath.Join("%~dp0", "node_modules", install.scope, install.packageName, "bin", install.binFile)
	command := filepath.Join(prefix, install.commandName+".cmd")
	invoke := fmt.Sprintf("\"%s\" %%*", target)
	if strings.HasSuffix(strings.ToLower(install.binFile), ".js") {
		invoke = fmt.Sprintf("node \"%s\" %%*", target)
	}
	if err := os.WriteFile(command, []byte("@echo off\r\n"+invoke+"\r\n"), 0o755); err != nil {
		return fmt.Errorf("write npm global %s command shim: %w", install.commandName, err)
	}
	psTarget := "$PSScriptRoot\\" + filepath.Join("node_modules", install.scope, install.packageName, "bin", install.binFile)
	psInvoke := fmt.Sprintf("& \"%s\" @args", psTarget)
	if strings.HasSuffix(strings.ToLower(install.binFile), ".js") {
		psInvoke = fmt.Sprintf("& node \"%s\" @args", psTarget)
	}
	if err := os.WriteFile(filepath.Join(prefix, install.commandName+".ps1"), []byte(psInvoke+"\r\nexit $LASTEXITCODE\r\n"), 0o755); err != nil {
		return fmt.Errorf("write npm global %s PowerShell shim: %w", install.commandName, err)
	}
	return nil
}
