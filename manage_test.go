package main

import (
	"bytes"
	"errors"
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

func TestFindManualPanel(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	panels := []runningPanel{
		{PID: 101, DataDir: dirA, Listen: "127.0.0.1:8091"},
		{PID: 102, DataDir: dirB, Listen: "127.0.0.1:8092", OtherUser: true},
	}
	if got := findManualPanel(dirA, panels); got == nil || got.PID != 101 {
		t.Fatalf("exact data dir match = %+v, want PID 101", got)
	}
	// When no panel matches dataDir directly, fall back to a unique same-user panel.
	if got := findManualPanel(filepath.Join(t.TempDir(), "other"), panels); got == nil || got.PID != 101 {
		t.Fatalf("single same-user fallback = %+v, want PID 101", got)
	}
	ambiguous := []runningPanel{
		{PID: 101, DataDir: dirA, Listen: "127.0.0.1:8091"},
		{PID: 103, DataDir: dirB, Listen: "127.0.0.1:8093"},
	}
	if got := findManualPanel(filepath.Join(t.TempDir(), "other"), ambiguous); got != nil {
		t.Fatalf("multiple unrelated same-user panels should not be guessed: %+v", got)
	}
}

func TestResolvePanelTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	l := userLayout()

	// Uninstalled and not running -> error.
	if _, err := resolvePanelTarget(l, nil, false); err == nil {
		t.Fatal("uninstalled and stopped must fail")
	}

	// Uninstalled but manually running -> uses running panel address.
	manual := &runningPanel{PID: 42, DataDir: l.DataDir, Listen: "127.0.0.1:18091"}
	target, err := resolvePanelTarget(l, manual, false)
	if err != nil || target.Installed || !target.Running || target.PID != 42 || target.Listen != "127.0.0.1:18091" {
		t.Fatalf("manual target = %+v, err = %v", target, err)
	}

	// Installed -> uses configured listen address and service running flag.
	if err := writeListenToEnvFile(l.EnvFile, "127.0.0.1:9091"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(l.Binary), 0o755)
	os.WriteFile(l.Binary, []byte("#!/bin/sh\n"), 0o755)
	target, err = resolvePanelTarget(l, nil, false)
	if err != nil || !target.Installed || target.Running || target.Listen != "127.0.0.1:9091" {
		t.Fatalf("installed stopped target = %+v, err = %v", target, err)
	}
}

func TestSSHTunnelHint(t *testing.T) {
	if got := sshTunnelHint("127.0.0.1:8091"); got != "ssh -L 8091:127.0.0.1:8091 用户@服务器" {
		t.Fatalf("ipv4 loopback = %q", got)
	}
	if got := sshTunnelHint("[::1]:8092"); got != "ssh -L 8092:[::1]:8092 用户@服务器" {
		t.Fatalf("ipv6 loopback = %q", got)
	}
	if got := sshTunnelHint("0.0.0.0:8091"); got != "" {
		t.Fatalf("non-loopback should have no tunnel hint, got %q", got)
	}
}

func TestWritePanelURLs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	target := panelTarget{Listen: "127.0.0.1:8091", Running: false, Installed: true}
	if err := writePanelURLs(&stdout, &stderr, target, true); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "http://127.0.0.1:8091\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "sbpanel start") || !strings.Contains(stderr.String(), "ssh -L 8091:127.0.0.1:8091") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	// Non-terminal stdout keeps stderr quiet for running loopback services.
	stdout.Reset()
	stderr.Reset()
	target.Running = true
	if err := writePanelURLs(&stdout, &stderr, target, false); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "http://127.0.0.1:8091\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestBrowserCommand(t *testing.T) {
	found := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	noEnv := func(string) string { return "" }
	hasDisplay := func(key string) string {
		if key == "DISPLAY" {
			return ":0"
		}
		return ""
	}

	cmd, args, ok := browserCommand("darwin", noEnv, found, "http://127.0.0.1:8091")
	if !ok || cmd != "/usr/bin/open" || strings.Join(args, " ") != "http://127.0.0.1:8091" {
		t.Fatalf("darwin = %q %v %v", cmd, args, ok)
	}
	if _, _, ok := browserCommand("linux", noEnv, found, "http://127.0.0.1:8091"); ok {
		t.Fatal("headless linux without DISPLAY/WAYLAND_DISPLAY must not pick a browser")
	}
	cmd, args, ok = browserCommand("linux", hasDisplay, found, "http://127.0.0.1:8091")
	if !ok || cmd != "/usr/bin/xdg-open" || strings.Join(args, " ") != "http://127.0.0.1:8091" {
		t.Fatalf("linux desktop = %q %v %v", cmd, args, ok)
	}
	if _, _, ok := browserCommand("linux", hasDisplay, missing, "http://127.0.0.1:8091"); ok {
		t.Fatal("missing xdg-open must return ok=false")
	}
}

func TestOpenPanelHeadlessAndDesktop(t *testing.T) {
	target := panelTarget{Listen: "127.0.0.1:8091", Running: true, Installed: true}
	var stdout, stderr bytes.Buffer

	// Headless fallback prints URLs and returns nil.
	err := openPanelTarget(target, &stdout, &stderr, "linux", func(string) string { return "" },
		func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func(string, ...string) error { t.Fatal("should not launch browser on headless linux"); return nil })
	if err != nil {
		t.Fatalf("headless err = %v", err)
	}
	if stdout.String() != "http://127.0.0.1:8091\n" || !strings.Contains(stderr.String(), "ssh -L 8091:127.0.0.1:8091") {
		t.Fatalf("headless stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// Desktop invokes browser opener with the panel URL.
	stdout.Reset()
	stderr.Reset()
	var launched string
	err = openPanelTarget(target, &stdout, &stderr, "darwin", func(string) string { return "" },
		func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func(name string, args ...string) error {
			launched = name + " " + strings.Join(args, " ")
			return nil
		})
	if err != nil {
		t.Fatalf("desktop err = %v", err)
	}
	if launched != "/usr/bin/open http://127.0.0.1:8091" {
		t.Fatalf("launched = %q", launched)
	}

	// Stopped service returns an actionable error.
	stopped := panelTarget{Listen: "127.0.0.1:8091", Running: false, Installed: true}
	if err := openPanelTarget(stopped, &stdout, &stderr, "darwin", func(string) string { return "" },
		func(name string) (string, error) { return "/usr/bin/" + name, nil },
		func(string, ...string) error { return nil }); err == nil {
		t.Fatal("stopped panel must return error on open")
	}
}

func TestWriteMenuPanelURLsListsAllAndTunnelHint(t *testing.T) {
	var buf bytes.Buffer
	writeMenuPanelURLs(&buf, "127.0.0.1:8091", []string{"http://127.0.0.1:8091"})
	got := stripANSI(buf.String())
	if !strings.Contains(got, "面板: http://127.0.0.1:8091") || !strings.Contains(got, "隧道: ssh -L 8091:127.0.0.1:8091") {
		t.Fatalf("single loopback menu urls = %q", got)
	}

	buf.Reset()
	writeMenuPanelURLs(&buf, "0.0.0.0:8091", []string{"http://192.168.1.10:8091", "http://10.0.0.5:8091"})
	got = stripANSI(buf.String())
	if !strings.Contains(got, "http://192.168.1.10:8091") || !strings.Contains(got, "http://10.0.0.5:8091") || strings.Contains(got, "隧道:") {
		t.Fatalf("multi urls = %q", got)
	}
}
