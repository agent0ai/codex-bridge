package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const installerURL = "https://raw.githubusercontent.com/agent0ai/codex-bridge/main/install.sh"

func updateBridge(path string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	fetch := exec.Command("curl", "-fsSL", "--connect-timeout", "15", "--max-time", "60", installerURL)
	fetch.Stderr = os.Stderr
	script, err := fetch.Output()
	if err != nil {
		return fmt.Errorf("download installer: %w", err)
	}
	if len(bytes.TrimSpace(script)) == 0 {
		return fmt.Errorf("downloaded installer is empty")
	}
	install := exec.Command("sh", "-s", "--", executable)
	install.Stdin = bytes.NewReader(script)
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	if err = install.Run(); err != nil {
		return fmt.Errorf("install update: %w", err)
	}
	restart := exec.Command(executable, "--config", path, "restart")
	restart.Stdin, restart.Stdout, restart.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err = restart.Run(); err != nil {
		return fmt.Errorf("binary updated, but proxy restart failed: %w", err)
	}
	return nil
}
