package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"v0.2.0", "v0.1.9", 1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v0.1.0", "dev", 1},
		{"dev", "v0.1.0", -1},
		{"v0.10.0", "v0.9.0", 1},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseSHA256Sums(t *testing.T) {
	sum := strings.Repeat("a", 64)
	sums := parseSHA256Sums(sum + "  sbpanel-linux-amd64.tar.gz\n" + sum + " *sbpanel-linux-arm64.tar.gz\nnot a line\n")
	if sums["sbpanel-linux-amd64.tar.gz"] != sum || sums["sbpanel-linux-arm64.tar.gz"] != sum || len(sums) != 2 {
		t.Fatalf("sums = %v", sums)
	}
}

func TestValidateListen(t *testing.T) {
	for _, ok := range []string{"0.0.0.0:8091", "127.0.0.1:1", "[::]:8091"} {
		if err := validateListen(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"8091", "localhost:8091", "0.0.0.0:0", "0.0.0.0:70000", "0.0.0.0:abc"} {
		if err := validateListen(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestWriteListenToEnvFileKeepsOtherLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sbpanel.env")
	if err := writeListenToEnvFile(path, "0.0.0.0:8091"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "SBPANEL_LISTEN=0.0.0.0:8091") || !strings.HasPrefix(string(data), "#") {
		t.Fatalf("new file = %q", data)
	}
	os.WriteFile(path, []byte("SBPANEL_SECURE_COOKIE=1\nSBPANEL_LISTEN=0.0.0.0:8091\n"), 0o644)
	if err := writeListenToEnvFile(path, "127.0.0.1:9000"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "SBPANEL_SECURE_COOKIE=1\nSBPANEL_LISTEN=127.0.0.1:9000\n" {
		t.Fatalf("rewritten = %q", data)
	}
	if match := envListenPattern.FindSubmatch(data); match == nil || string(match[1]) != "127.0.0.1:9000" {
		t.Fatal("env pattern did not read the value back")
	}
}

func TestLegacyUnitListenIsUnderstood(t *testing.T) {
	unit := "ExecStart=/usr/local/bin/sbpanel serve --listen 0.0.0.0:8091\n"
	if match := unitListenPattern.FindStringSubmatch(unit); match == nil || match[1] != "0.0.0.0:8091" {
		t.Fatalf("match = %v", match)
	}
}

func TestPanelURLsForSpecificAddress(t *testing.T) {
	urls := panelURLs("127.0.0.1:8091")
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:8091" {
		t.Fatalf("urls = %v", urls)
	}
	if urls := panelURLs("0.0.0.0:8091"); len(urls) == 0 {
		t.Fatal("unspecified address should list at least one URL")
	}
}

func TestMirrorURLs(t *testing.T) {
	t.Setenv("GH_PROXY", "")
	if urls := mirrorURLs("https://github.com/x"); len(urls) != 1 {
		t.Fatalf("urls = %v", urls)
	}
	t.Setenv("GH_PROXY", "https://mirror.example/")
	urls := mirrorURLs("https://github.com/x")
	if len(urls) != 2 || urls[0] != "https://mirror.example/https://github.com/x" || urls[1] != "https://github.com/x" {
		t.Fatalf("urls = %v", urls)
	}
}

func TestUnitContentVariants(t *testing.T) {
	l := systemLayout()
	sandboxed := unitContent(l, true)
	plain := unitContent(l, false)
	for _, unit := range []string{sandboxed, plain} {
		if !strings.Contains(unit, "EnvironmentFile=-"+l.EnvFile) || !strings.Contains(unit, "ExecStart="+l.Binary+" serve") || strings.Contains(unit, "--listen") {
			t.Fatalf("unit must take its settings from the env file:\n%s", unit)
		}
	}
	if !strings.Contains(sandboxed, "DynamicUser=yes") || strings.Contains(plain, "DynamicUser") {
		t.Fatal("only the sandboxed unit uses DynamicUser")
	}
	if !strings.Contains(plain, "SBPANEL_DATA="+systemDataDir) {
		t.Fatal("the plain unit must point at the same data directory")
	}
}

func TestLayouts(t *testing.T) {
	system := systemLayout()
	if !system.System || system.Binary != "/usr/local/bin/sbpanel" || system.DataDir != "/var/lib/sbpanel" {
		t.Fatalf("system layout = %+v", system)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	user := userLayout()
	if user.System || user.Binary != filepath.Join(home, ".local/bin/sbpanel") ||
		user.DataDir != filepath.Join(home, ".local/share/sbpanel") || user.EnvFile != filepath.Join(home, ".config/sbpanel/sbpanel.env") {
		t.Fatalf("user layout = %+v", user)
	}
	if user.installed() {
		t.Fatal("empty home must not count as installed")
	}
	if err := writeListenToEnvFile(user.EnvFile, "127.0.0.1:9100"); err != nil {
		t.Fatal(err)
	}
	if user.installed() || user.panelListen() != "127.0.0.1:9100" {
		t.Fatal("settings kept after an uninstall must not count as installed, but must still be read")
	}
	os.MkdirAll(filepath.Dir(user.Binary), 0o755)
	os.WriteFile(user.Binary, []byte("#!/bin/sh\n"), 0o755)
	if !user.installed() {
		t.Fatal("binary plus settings file is an installation")
	}
}

func TestReadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sbpanel.env")
	os.WriteFile(path, []byte("# comment\n\nSBPANEL_LISTEN=0.0.0.0:8091\n  SBPANEL_SECURE_COOKIE = \"1\"\nbroken line\n"), 0o644)
	settings, err := readEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(settings, ";") != "SBPANEL_LISTEN=0.0.0.0:8091;SBPANEL_SECURE_COOKIE=1" {
		t.Fatalf("settings = %v", settings)
	}
	if missing, err := readEnvFile(filepath.Join(t.TempDir(), "none")); err != nil || missing != nil {
		t.Fatal("a missing env file is not an error")
	}
}

func TestSystemdUsableHonoursOverride(t *testing.T) {
	t.Setenv(serviceManagerEnv, managerDirect)
	if systemdUsable() {
		t.Fatal("SBPANEL_SERVICE_MANAGER=direct must disable systemd")
	}
}

func TestCoreArgsUseConfig(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(dir, link)
	config := resolvedFile(filepath.Join(link, "config.json"))
	args := []string{"/usr/bin/sing-box", "--disable-color", "run", "-c", filepath.Join(dir, "config.json"), "-D", dir}
	if !coreArgsUseConfig(args, config) {
		t.Fatal("same file through a symlinked directory must match")
	}
	if coreArgsUseConfig([]string{"sing-box", "run", "-c", "/etc/sing-box/config.json"}, config) {
		t.Fatal("other configs must not match")
	}
}

func TestPIDFileIsVerified(t *testing.T) {
	dir := t.TempDir()
	m := directManager{l: layout{PIDFile: filepath.Join(dir, "sbpanel.pid"), DataDir: dir}}
	os.WriteFile(m.l.PIDFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	if m.pid() != 0 {
		t.Fatal("a live PID that is not `sbpanel serve` must be ignored")
	}
	os.WriteFile(m.l.PIDFile, []byte("999999999\n"), 0o644)
	if m.pid() != 0 || m.Status().Running {
		t.Fatal("a dead PID must count as stopped")
	}
}

func TestPadDisplayCountsWideCharacters(t *testing.T) {
	if got := padDisplay("代理", 8); got != "代理    " {
		t.Fatalf("got %q", got)
	}
	if got := padDisplay("sing-box", 8); got != "sing-box" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultProxyPort(t *testing.T) {
	if defaultSettings().ListenPort != 10826 {
		t.Fatalf("default proxy port = %d, want 10826", defaultSettings().ListenPort)
	}
}

func TestAlignOwnershipLeavesMatchingFilesAlone(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600)
	alignOwnership(dir) // same owner already: must be a no-op and not fail
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
}
