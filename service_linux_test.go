package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A test binary started with SBPANEL_FAKE_SERVE=1 stands in for a running panel.
func init() {
	if os.Getenv("SBPANEL_FAKE_SERVE") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// startFakeServe runs this test binary as `<dir>/sbpanel serve args...`.
func startFakeServe(t *testing.T, dir string, env []string, args ...string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "sbpanel")
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(self, link); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(link, append([]string{"serve"}, args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), append(env, "SBPANEL_FAKE_SERVE=1")...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for !isServeProcess(command.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return command.Process.Pid
}

func TestFindRunningPanel(t *testing.T) {
	dir := t.TempDir()
	byFlag := startFakeServe(t, dir, nil, "--data", "flagdata", "--listen=127.0.0.1:18093")
	byEnv := startFakeServe(t, dir, []string{"SBPANEL_DATA=" + filepath.Join(dir, "envdata"), "SBPANEL_LISTEN=0.0.0.0:18094"})

	panel := findRunningPanel(filepath.Join(dir, "flagdata"))
	if panel == nil || panel.PID != byFlag || panel.Listen != "127.0.0.1:18093" {
		t.Fatalf("by flag: %+v", panel)
	}
	panel = findRunningPanel(filepath.Join(dir, "envdata"))
	if panel == nil || panel.PID != byEnv || panel.Listen != "0.0.0.0:18094" {
		t.Fatalf("by environment: %+v", panel)
	}
	if panel := findRunningPanel(filepath.Join(dir, "elsewhere")); panel != nil {
		t.Fatalf("matched a panel with another data directory: %+v", panel)
	}
	if !samePort("0.0.0.0:8091", "127.0.0.1:8091") || samePort("0.0.0.0:8091", "0.0.0.0:8092") {
		t.Fatal("samePort")
	}
}

func TestRunningPanelDefaultDataDir(t *testing.T) {
	panel := runningPanel{PID: 1}
	if !panel.uses(defaultDataDir()) || panel.uses(t.TempDir()) {
		t.Fatal("a panel without --data uses the default data directory only")
	}
	panel.OtherUser = true
	if panel.uses(defaultDataDir()) {
		t.Fatal("another user's default is not ours")
	}
}
