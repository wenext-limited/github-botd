package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/pkg/client"
)

// Exercise the actual entrypoint, signal registration and Unix listener in a
// separate process, using synthetic credentials and no GitHub adapter.
func TestConfigReloadInDaemon(t *testing.T) {
	if path := os.Getenv("BOTD_RELOAD_TEST_CONFIG"); path != "" {
		os.Args = []string{os.Args[0], "-config", path}
		main()
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("/tmp", "botd-reload.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	tokenPath := filepath.Join(dir, "client")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{Version: 1, StateDir: "state", DryRun: true,
		IPC:          config.IPC{Socket: "run/botd.sock"},
		Repositories: map[string]config.Repository{"repo": {FullName: "acme/repo"}},
		Clients:      map[string]config.Client{"agent": {TokenFile: "client", Repositories: []string{"repo"}, Capabilities: []string{"events.read"}}},
	}
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, c)
	logPath := filepath.Join(dir, "daemon.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	process := exec.Command(os.Args[0], "-test.run=^TestConfigReloadInDaemon$")
	process.Env = append(os.Environ(), "BOTD_RELOAD_TEST_CONFIG="+path)
	process.Stdout, process.Stderr = log, log
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			process.Process.Kill()
			<-done
		}
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Log(string(data))
		}
	})
	newClient := func() *client.Client {
		t.Helper()
		c, err := client.New(client.Config{Endpoint: "unix://" + filepath.Join(dir, "run/botd.sock"), TokenFile: tokenPath})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	old := newClient()
	await(t, func() bool { _, err := old.Info(context.Background()); return err == nil })
	// A file-only bearer rotation is detected automatically; no signal or
	// listener reconnect is required and the stable client ID is retained.
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("b", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	updated := newClient()
	await(t, func() bool { _, err := updated.Info(context.Background()); return err == nil })
	if _, err := old.Info(context.Background()); err == nil {
		t.Fatal("old process credential still authorized")
	}
	if err := os.WriteFile(path, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := process.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Contains(string(data), "configuration reload rejected")
	})
	if _, err := updated.Info(context.Background()); err != nil {
		t.Fatal("invalid edit interrupted daemon", err)
	}
	p := c.Clients["agent"]
	p.Capabilities = append(p.Capabilities, "events.replay")
	c.Clients["agent"] = p
	writeConfig(t, path, c)
	await(t, func() bool {
		info, err := updated.Info(context.Background())
		return err == nil && config.Contains(info.Capabilities, "events.replay")
	})
	data, _ := os.ReadFile(logPath)
	before := strings.Count(string(data), "configuration reloaded")
	if err := process.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Count(string(data), "configuration reloaded") > before
	})
	if err := process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal("daemon did not shut down cleanly", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}
