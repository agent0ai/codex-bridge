package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed config.example.toml
var example []byte

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "codex-bridge:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("codex-bridge", flag.ContinueOnError)
	path := fs.String("config", filepath.Join(dir, "codex-bridge", "config.toml"), "TOML config file")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: codex-bridge [--config FILE] init|edit|sync|unsync|start|stop|status|serve\n\ninit     Create an example config\nedit     Open config in $VISUAL / $EDITOR / vi\nsync     Add models to Codex /model (restart Codex afterward)\nunsync   Restore previous Codex provider settings\nstart    Start proxy in background\nstop     Stop proxy gracefully\nstatus   Check the running proxy\nserve    Run in foreground (Ctrl-C stops)")
		fs.PrintDefaults()
	}
	if err = fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("expected one command")
	}
	*path, err = filepath.Abs(*path)
	if err != nil {
		return err
	}
	command := fs.Arg(0)
	if command == "init" {
		if err = os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
			return err
		}
		f, e := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(example)
		cerr := f.Close()
		if e != nil {
			return e
		}
		if cerr != nil {
			return cerr
		}
		fmt.Println(*path)
		return nil
	}
	if command == "edit" {
		cmd := exec.Command("sh", "-c", `exec ${VISUAL:-${EDITOR:-vi}} "$1"`, "sh", *path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("editor failed: %w\nConfig file: %s\nEdit this file manually, or set VISUAL/EDITOR to an installed editor such as micro", err, *path)
		}
		return nil
	}
	if command == "unsync" {
		return withLock(filepath.Join(codexDir(), ".codex-bridge.lock"), unsyncCodex)
	}
	c, err := loadConfig(*path)
	if command == "stop" || command == "status" {
		if running, e := os.ReadFile(*path + ".running"); e == nil && validListen(string(running)) {
			c.Listen, err = string(running), nil
		}
	}
	if err != nil {
		return err
	}
	if command == "sync" {
		return withLock(filepath.Join(codexDir(), ".codex-bridge.lock"), func() error { return syncCodex(*path, c) })
	}
	token, err := localToken(*path)
	if err != nil {
		return err
	}
	switch command {
	case "serve":
		return serve(*path, c, token)
	case "status":
		if err = control(c, token, *path, "GET", "/health"); err != nil {
			return fmt.Errorf("not running or unreachable: %w", err)
		}
		fmt.Println("Running at http://" + c.Listen)
		return nil
	case "stop":
		if err = control(c, token, *path, "POST", "/stop"); err != nil {
			return err
		}
		for i := 0; i < 120; i++ {
			if control(c, token, *path, "GET", "/health") != nil {
				fmt.Println("Stopped")
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("proxy has not stopped yet")
	case "start":
		return withLock(*path+".lock", func() error { return start(*path, c, token) })
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func withLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func control(c Config, token, path, method, endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Listen+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Codex-Bridge-Token", token)
	client := http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bridge HTTP %d", resp.StatusCode)
	}
	if endpoint == "/health" {
		var body object
		if err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
			return err
		}
		if body["service"] != "codex-bridge" || body["config"] != path {
			return fmt.Errorf("port belongs to another process or config")
		}
	}
	return nil
}

func start(path string, c Config, token string) error {
	if running, err := os.ReadFile(path + ".running"); err == nil && validListen(string(running)) && string(running) != c.Listen {
		old := Config{Listen: string(running)}
		if control(old, token, path, "GET", "/health") == nil {
			return fmt.Errorf("proxy is running at %s; run stop before changing the listen address", old.Listen)
		}
	}
	if control(c, token, path, "GET", "/health") == nil {
		fmt.Println("Already running at http://" + c.Listen)
		return nil
	}
	// Fail before forking when the configured keys are unavailable.
	for _, m := range c.Models {
		if _, err := m.key(); err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(executable, "--config", path, "serve")
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for i := 0; i < 150; i++ {
		if control(c, token, path, "GET", "/health") == nil {
			fmt.Printf("Started at http://%s; log: %s.log\n", c.Listen, path)
			return nil
		}
		select {
		case err = <-done:
			return fmt.Errorf("proxy exited (%v); see %s.log", err, path)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("proxy is still starting; inspect %s.log and run status", path)
}
