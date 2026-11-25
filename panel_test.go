package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakePanel writes a shell script that answers `version` like sbpanel and otherwise
// appends its name to $FAKE_PANEL_LOG and runs body (fd 3 is the ready pipe).
func fakePanel(t *testing.T, path, version, name, body string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then echo \"sbpanel " + version + "\"; exit 0; fi\n" +
		"echo " + name + " >> \"$FAKE_PANEL_LOG\"\n" + body + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

type supervisorFixture struct {
	t   *testing.T
	dir string // <data>/panel
	log string
	sup *panelSupervisor
}

func newSupervisorFixture(t *testing.T, baseBody string) *supervisorFixture {
	root := t.TempDir()
	f := &supervisorFixture{t: t, dir: filepath.Join(root, "panel"), log: filepath.Join(root, "runs.log")}
	t.Setenv("FAKE_PANEL_LOG", f.log)
	t.Setenv("FAKE_PANEL_ROOT", root)
	base := filepath.Join(root, "bin", "sbpanel")
	fakePanel(t, base, "v1.0.0", "base", baseBody)
	f.sup = &panelSupervisor{
		dir: f.dir, self: base, selfVersion: "v1.0.0",
		readyTimeout: 5 * time.Second, signals: make(chan os.Signal, 1),
	}
	return f
}

func (f *supervisorFixture) runs() string {
	data, _ := os.ReadFile(f.log)
	return strings.Join(strings.Fields(string(data)), ",")
}

func (f *supervisorFixture) run() error {
	f.t.Helper()
	done := make(chan error, 1)
	go func() { done <- f.sup.run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		f.t.Fatal("supervisor did not finish")
		return nil
	}
}

const (
	readyAndExit = "echo ready >&3; exit 0"
	crashEarly   = "exit 3"
)

func TestSupervisorRevertsVersionThatFailsToStart(t *testing.T) {
	f := newSupervisorFixture(t, readyAndExit)
	fakePanel(t, panelBinaryPath(f.dir), "v2.0.0", "new", crashEarly)
	os.WriteFile(panelPendingPath(f.dir), nil, 0o600)

	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "new,base" {
		t.Fatalf("runs = %s", got)
	}
	if fileExists(panelBinaryPath(f.dir)) || fileExists(panelPendingPath(f.dir)) {
		t.Fatal("failed version must be removed together with the trial marker")
	}
	reason, _ := os.ReadFile(panelLastErrorPath(f.dir))
	if !strings.Contains(string(reason), "退出码 3") {
		t.Fatalf("last error = %q", reason)
	}
}

func TestSupervisorRestoresPreviousPanelInstalledVersion(t *testing.T) {
	f := newSupervisorFixture(t, readyAndExit)
	fakePanel(t, panelBinaryPath(f.dir), "v3.0.0", "new", crashEarly)
	fakePanel(t, panelPreviousPath(f.dir), "v2.0.0", "previous", readyAndExit)
	os.WriteFile(panelPendingPath(f.dir), nil, 0o600)

	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "new,previous" {
		t.Fatalf("runs = %s", got)
	}
	if got, _ := sbpanelBinaryVersion(panelBinaryPath(f.dir)); got != "v2.0.0" {
		t.Fatalf("panel-installed version after revert = %q", got)
	}
	if fileExists(panelPreviousPath(f.dir)) {
		t.Fatal("previous should have moved back into place")
	}
}

func TestSupervisorKeepsVersionThatStarts(t *testing.T) {
	f := newSupervisorFixture(t, readyAndExit)
	// Reports ready and exits at once: the supervisor must still see "ready".
	fakePanel(t, panelBinaryPath(f.dir), "v2.0.0", "new", readyAndExit)
	os.WriteFile(panelPendingPath(f.dir), nil, 0o600)

	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "new" {
		t.Fatalf("runs = %s", got)
	}
	if !fileExists(panelBinaryPath(f.dir)) || fileExists(panelPendingPath(f.dir)) {
		t.Fatal("a version that started must stay, and leave its trial")
	}
}

func TestSupervisorRevertsVersionThatHangs(t *testing.T) {
	f := newSupervisorFixture(t, readyAndExit)
	f.sup.readyTimeout = 300 * time.Millisecond
	fakePanel(t, panelBinaryPath(f.dir), "v2.0.0", "new", "exec sleep 30")
	os.WriteFile(panelPendingPath(f.dir), nil, 0o600)

	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "new,base" {
		t.Fatalf("runs = %s", got)
	}
	reason, _ := os.ReadFile(panelLastErrorPath(f.dir))
	if !strings.Contains(string(reason), "内没有启动完成") {
		t.Fatalf("last error = %q", reason)
	}
}

func TestSupervisorRestartsOnRequest(t *testing.T) {
	// First run asks for a restart, the second exits normally.
	f := newSupervisorFixture(t, `if [ ! -e "$FAKE_PANEL_ROOT/restarted" ]; then
  touch "$FAKE_PANEL_ROOT/restarted"; echo ready >&3; exit 75
fi
echo ready >&3; exit 0`)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "base,base" {
		t.Fatalf("runs = %s", got)
	}
}

func TestSupervisorPrefersNewerProgram(t *testing.T) {
	f := newSupervisorFixture(t, readyAndExit)
	// `sbpanel update` installed v1.0.0 after an older version came from the panel.
	fakePanel(t, panelBinaryPath(f.dir), "v0.9.0", "old", readyAndExit)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.runs(); got != "base" {
		t.Fatalf("runs = %s", got)
	}
}

func TestSupervisorReportsCrash(t *testing.T) {
	f := newSupervisorFixture(t, "echo ready >&3; exit 4")
	if err := f.run(); err == nil || !strings.Contains(err.Error(), "4") {
		t.Fatalf("err = %v", err)
	}
}

func TestSupervisorForwardsStop(t *testing.T) {
	f := newSupervisorFixture(t, "echo ready >&3; exec sleep 30")
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.sup.signals <- syscall.SIGTERM
	}()
	started := time.Now()
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatal("stop took too long")
	}
}

func TestPanelSwapInAndRollback(t *testing.T) {
	dir := t.TempDir()
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		data, _ := os.ReadFile(path)
		return string(data)
	}
	restarted := make(chan struct{}, 4)
	p := &PanelUpdater{dir: dir, supervised: true, restart: func() { restarted <- struct{}{} }, baseVersion: "v1.0.0"}

	// Running the base program: stale panel-installed files are dropped.
	write(panelBinaryPath(dir), "stale")
	write(panelPreviousPath(dir), "stale previous")
	write(panelLastErrorPath(dir), "old failure")
	write(panelCandidatePath(dir), "v2")
	if err := p.swapIn(panelCandidatePath(dir)); err != nil {
		t.Fatal(err)
	}
	if read(panelBinaryPath(dir)) != "v2" || fileExists(panelPreviousPath(dir)) || fileExists(panelLastErrorPath(dir)) || !fileExists(panelPendingPath(dir)) {
		t.Fatal("swap from the base program")
	}

	// Running the panel-installed v2: it becomes the rollback target.
	p.running = true
	os.Remove(panelPendingPath(dir))
	write(panelCandidatePath(dir), "v3")
	if err := p.swapIn(panelCandidatePath(dir)); err != nil {
		t.Fatal(err)
	}
	if read(panelBinaryPath(dir)) != "v3" || read(panelPreviousPath(dir)) != "v2" || !fileExists(panelPendingPath(dir)) {
		t.Fatal("swap from a panel-installed program")
	}

	app := &App{panel: p}
	if err := app.RollbackPanel(); err != nil {
		t.Fatal(err)
	}
	if read(panelBinaryPath(dir)) != "v2" || fileExists(panelPreviousPath(dir)) || fileExists(panelPendingPath(dir)) {
		t.Fatal("rollback to the previous panel-installed program")
	}
	<-restarted

	p.job = PanelJob{}
	if err := app.RollbackPanel(); err != nil {
		t.Fatal(err)
	}
	if fileExists(panelBinaryPath(dir)) {
		t.Fatal("rollback without a previous version goes back to the base program")
	}
	<-restarted

	p.running = false
	p.job = PanelJob{}
	if err := app.RollbackPanel(); err == nil {
		t.Fatal("nothing to roll back from the base program")
	}
}

func TestStartPanelInstallRejects(t *testing.T) {
	original := version
	version = "v1.0.0"
	defer func() { version = original }()
	app := &App{panel: &PanelUpdater{dir: t.TempDir(), ctx: context.Background(), supervised: false}}
	if err := app.StartPanelInstall("v99.0.0"); err == nil {
		t.Fatal("an unsupervised panel cannot restart into a new version")
	}
	app.panel.supervised = true
	if _, err := sbpanelAssetName(); err != nil {
		t.Skip("platform has no release build")
	}
	for _, tag := range []string{"latest", "../../x", "v0.9.0", "v1.0.0"} {
		if err := app.StartPanelInstall(tag); err == nil {
			t.Fatalf("tag %q accepted", tag)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allowed := parseAllowedHosts("Panel.Example.com, other.example.com")
	for host, want := range map[string]bool{
		"192.168.1.10:8091":           true,
		"[::1]:8091":                  true,
		"localhost:8091":              true,
		"app.localhost":               true,
		"":                            true,
		"panel.example.com":           true,
		"panel.example.com.:443":      true,
		"attacker.example:8091":       false,
		"192.168.1.10.nip.io:8091":    false,
		"other.example.com.evil.test": false,
	} {
		if got := hostAllowed(host, allowed); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	if !hostAllowed("anything.test", parseAllowedHosts("*")) {
		t.Error("* allows every host")
	}
}

func TestHandlerOpenWithoutLogin(t *testing.T) {
	handler := (&Server{}).Handler()
	do := func(method, host string, header map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://"+host+"/api/health", nil)
		for key, value := range header {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if code := do(http.MethodGet, "192.168.1.10:8091", nil).Code; code != http.StatusOK {
		t.Fatalf("health by IP = %d", code)
	}
	if code := do(http.MethodGet, "rebind.attacker.test:8091", nil).Code; code != http.StatusForbidden {
		t.Fatalf("health by unknown name = %d", code)
	}
	cross := map[string]string{"Origin": "http://evil.test", "X-SBPanel": "1"}
	if code := do(http.MethodGet, "192.168.1.10:8091", cross).Code; code != http.StatusForbidden {
		t.Fatalf("cross-origin request = %d", code)
	}
	if code := do(http.MethodGet, "192.168.1.10:8091", map[string]string{"Sec-Fetch-Site": "cross-site"}).Code; code != http.StatusForbidden {
		t.Fatalf("cross-site request = %d", code)
	}
}

// TestInstallPanelFromRelease runs the whole download against a fake GitHub: release
// lookup, SHA-256 from the asset digest or from sha256sums.txt, extraction, the
// version check, and the swap into place.
func TestInstallPanelFromRelease(t *testing.T) {
	assetName, err := sbpanelAssetName()
	if err != nil {
		t.Skip("platform has no release build")
	}
	for _, viaSums := range []bool{false, true} {
		t.Run(map[bool]string{false: "digest", true: "sha256sums"}[viaSums], func(t *testing.T) {
			script := "#!/bin/sh\necho 'sbpanel v9.9.9'\n"
			archive := tarGz(t, "sbpanel", script)
			sum := sha256Hex(archive)

			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asset := map[string]any{"name": assetName, "size": len(archive), "browser_download_url": server.URL + "/dl/" + assetName}
				if !viaSums {
					asset["digest"] = "sha256:" + sum
				}
				switch r.URL.Path {
				case "/releases/latest", "/releases/tags/v9.9.9":
					writeJSON(w, http.StatusOK, map[string]any{
						"tag_name": "v9.9.9", "published_at": time.Now(),
						"assets": []any{asset, map[string]any{"name": "sha256sums.txt", "browser_download_url": server.URL + "/dl/sums"}},
					})
				case "/dl/" + assetName:
					w.Write(archive)
				case "/dl/sums":
					w.Write([]byte(sum + "  " + assetName + "\n"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			original := sbpanelReleasesAPI
			sbpanelReleasesAPI = server.URL + "/releases"
			defer func() { sbpanelReleasesAPI = original }()

			dir := t.TempDir()
			app := &App{core: &Core{}, panel: &PanelUpdater{dir: dir, ctx: context.Background(), supervised: true}}
			if err := app.CheckPanelRelease(context.Background()); err != nil {
				t.Fatal(err)
			}
			if release := app.panel.View().Release; release == nil || release.Tag != "v9.9.9" || release.Via != routeNameDirect {
				t.Fatalf("release = %+v", release)
			}
			message, err := app.installPanel(context.Background(), "v9.9.9")
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := sbpanelBinaryVersion(panelBinaryPath(dir)); got != "v9.9.9" || !fileExists(panelPendingPath(dir)) {
				t.Fatalf("installed %q, pending %v (%s)", got, fileExists(panelPendingPath(dir)), message)
			}
			if fileExists(panelArchivePath(dir)) || fileExists(panelCandidatePath(dir)) {
				t.Fatal("temporary files left behind")
			}
		})
	}
}

func TestInstallPanelRejectsBadChecksum(t *testing.T) {
	assetName, err := sbpanelAssetName()
	if err != nil {
		t.Skip("platform has no release build")
	}
	archive := tarGz(t, "sbpanel", "#!/bin/sh\necho 'sbpanel v9.9.9'\n")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl" {
			w.Write(archive)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tag_name": "v9.9.9", "assets": []any{map[string]any{
			"name": assetName, "browser_download_url": server.URL + "/dl", "digest": "sha256:" + strings.Repeat("0", 64)}}})
	}))
	defer server.Close()
	original := sbpanelReleasesAPI
	sbpanelReleasesAPI = server.URL + "/releases"
	defer func() { sbpanelReleasesAPI = original }()

	dir := t.TempDir()
	app := &App{core: &Core{}, panel: &PanelUpdater{dir: dir, ctx: context.Background(), supervised: true}}
	if _, err := app.installPanel(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
	if fileExists(panelBinaryPath(dir)) {
		t.Fatal("unverified download must not be installed")
	}
}

func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buffer.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
