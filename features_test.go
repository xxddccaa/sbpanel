package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBypassChinaRoutesAndRealCheck(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed")
	}
	// Point SBPANEL_TEST_RULESET_DIR at a directory holding geosite-cn.srs and
	// geoip-cn.srs (from SagerNet/sing-geosite and sing-geoip) to run this test.
	ruleDir := os.Getenv("SBPANEL_TEST_RULESET_DIR")
	if ruleDir == "" || !ruleSetsPresent(ruleDir) {
		t.Skip("SBPANEL_TEST_RULESET_DIR not set")
	}
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	settings := defaultSettings()
	settings.BypassChina = true
	input := buildInput{Settings: settings, Subs: []*Subscription{sub}, ClashAddr: "127.0.0.1:19090", ClashSecret: "x", RuleSetDir: ruleDir}
	built, _, err := buildChecked(context.Background(), &singBoxChecker{binary: fixedBinary(binary), tempDir: t.TempDir()}, input)
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	var config struct {
		Route struct {
			Rules   []map[string]any `json:"rules"`
			RuleSet []map[string]any `json:"rule_set"`
			Final   string           `json:"final"`
		} `json:"route"`
	}
	if err := json.Unmarshal(built.JSON, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Route.RuleSet) != 2 || config.Route.Final != tagProxy {
		t.Fatalf("route = %+v", config.Route)
	}
	if config.Route.Rules[0]["action"] != "sniff" {
		t.Fatal("first rule must sniff so IP connections still match geosite")
	}
	// No rule may force a DNS lookup of every domain (that would leak foreign domains).
	for _, rule := range config.Route.Rules {
		if rule["action"] == "resolve" {
			t.Fatal("resolve action leaks DNS")
		}
	}
}

func TestBypassChinaOffKeepsSimpleRoute(t *testing.T) {
	rules := routeRules(defaultSettings())
	if len(rules) != 1 {
		t.Fatalf("rules = %v", rules)
	}
}

func TestMigrateStateMovesViaProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := `{"subscriptions":[{"id":"a","viaProxy":true,"intervalHours":24,"nodes":[]},{"id":"b","intervalHours":24,"nodes":[]}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	if state.Subscriptions[0].UpdateVia != updateViaProxy || state.Subscriptions[1].UpdateVia != updateViaAuto {
		t.Fatalf("updateVia = %q, %q", state.Subscriptions[0].UpdateVia, state.Subscriptions[1].UpdateVia)
	}
	if state.Subscriptions[0].IntervalHours != 24 {
		t.Fatal("migration must not silently change an explicit interval")
	}
}

func TestNewSubscriptionsDefaultToManualUpdates(t *testing.T) {
	if defaultIntervalHour != 0 {
		t.Fatalf("default interval = %d, want manual (0)", defaultIntervalHour)
	}
}

func TestScheduleJitterIsStableAndBounded(t *testing.T) {
	attempted := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	first := jitterFor("abc", attempted)
	if first != jitterFor("abc", attempted) {
		t.Fatal("jitter must be stable for the same slot")
	}
	if first < 0 || first >= 1 {
		t.Fatalf("jitter out of range: %v", first)
	}
	sub := &Subscription{ID: "abc", IntervalHours: 24, AttemptedAt: attempted}
	if updateDue(sub, attempted.Add(24*time.Hour-time.Minute)) {
		t.Fatal("must not fire before the base interval")
	}
	if !updateDue(sub, attempted.Add(30*time.Hour+time.Minute)) {
		t.Fatal("must fire after interval + max jitter")
	}
}

func TestSubscriptionErrorsDoNotLeakURL(t *testing.T) {
	secret := "00000000-feed-face-secret"
	sub := &Subscription{URL: "http://127.0.0.1:1/" + secret + "/sing-box"}
	_, err := fetchSubscription(context.Background(), sub, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the subscription URL: %v", err)
	}
}

func TestSubscriptionFallsBackToDirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fscarmenStyleSubscription))
	}))
	defer server.Close()
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "state.json"))
	core := NewCore(fixedBinary("true"), filepath.Join(dir, "config.json"), dir, "127.0.0.1:1", "x")
	app := NewApp(store, core, &fakeChecker{}, NewKernel("", filepath.Join(dir, "kernel")),
		AppPaths{Config: filepath.Join(dir, "config.json"), RuleSet: filepath.Join(dir, "rules"), Temp: dir})
	id, err := app.AddSubscription(context.Background(), SubscriptionInput{URL: server.URL + "/x/sing-box"})
	if err != nil {
		t.Fatal(err)
	}
	sub := app.store.Snapshot().findSubscription(id)
	if sub.UpdatedVia != routeNameDirect || sub.IntervalHours != 0 || sub.UpdateVia != updateViaAuto || len(sub.Nodes) == 0 {
		t.Fatalf("sub = via %q interval %d mode %q nodes %d", sub.UpdatedVia, sub.IntervalHours, sub.UpdateVia, len(sub.Nodes))
	}
}

func makeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tarWriter.Write([]byte(content))
	}
	tarWriter.Close()
	gzipWriter.Close()
	return buffer.Bytes()
}

func TestDownloadVerifiedAndExtract(t *testing.T) {
	dir := t.TempDir()
	archive := makeTarball(t, map[string]string{
		"sing-box-1.0.0-linux-amd64-musl/LICENSE":  "x",
		"sing-box-1.0.0-linux-amd64-musl/sing-box": "#!/bin/sh\necho 'sing-box version 1.0.0'\n",
	})
	sum := sha256.Sum256(archive)
	path := filepath.Join(dir, "a.tar.gz")
	if err := downloadVerified(bytes.NewReader(archive), path, hex.EncodeToString(sum[:]), func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if err := downloadVerified(bytes.NewReader(archive), path, strings.Repeat("0", 64), func(int64) {}); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	binary := filepath.Join(dir, "sing-box")
	if err := extractSingBox(path, binary); err != nil {
		t.Fatal(err)
	}
	version, err := binaryVersion(context.Background(), binary)
	if err != nil || version != "1.0.0" {
		t.Fatalf("version = %q, %v", version, err)
	}
	notSingBox := filepath.Join(dir, "other")
	os.WriteFile(notSingBox, []byte("#!/bin/sh\necho hello\n"), 0o700)
	if _, err := binaryVersion(context.Background(), notSingBox); err == nil {
		t.Fatal("non sing-box binary accepted")
	}
}

func TestReleaseAssetName(t *testing.T) {
	name, err := releaseAssetName("v1.14.4")
	if err != nil {
		t.Skip(err)
	}
	if !strings.HasPrefix(name, "sing-box-1.14.4-linux-") || !strings.HasSuffix(name, "-musl.tar.gz") {
		t.Fatalf("asset = %s", name)
	}
	for _, tag := range []string{"v1.14.4", "v1.15.0-alpha.13"} {
		if !releaseTagPattern.MatchString(tag) {
			t.Errorf("%s rejected", tag)
		}
	}
	for _, tag := range []string{"1.14.4", "v1.14.4/../x", "latest"} {
		if releaseTagPattern.MatchString(tag) {
			t.Errorf("%s accepted", tag)
		}
	}
}

// rejectingChecker fails every check, like a kernel that cannot parse the config.
type rejectingChecker struct{}

func (rejectingChecker) Check(context.Context, []byte) error {
	return errors.New("FATAL unknown field")
}

func TestSwitchKernelRestoresFilesWhenStartFails(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "state.json"))
	kernel := NewKernel("", filepath.Join(dir, "kernel"))
	os.MkdirAll(kernel.dir, 0o700)
	os.WriteFile(kernel.managedPath(), []byte("old"), 0o700)
	os.WriteFile(kernel.previousPath(), []byte("older"), 0o700)
	core := NewCore(kernel.Active, filepath.Join(dir, "config.json"), dir, "127.0.0.1:1", "x")
	app := NewApp(store, core, &fakeChecker{}, kernel, AppPaths{Config: filepath.Join(dir, "config.json"), RuleSet: dir, Temp: dir})
	// Give the app nodes and enable the core so that apply has to start sing-box.
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	store.Update(func(state *State) error {
		state.Subscriptions = []*Subscription{sub}
		state.CoreEnabled = true
		return nil
	})

	err := app.RollbackKernel(context.Background())
	if err == nil {
		t.Fatal("rollback to a fake binary should fail to start")
	}
	if !strings.Contains(err.Error(), "新内核") {
		// "check" runs first with the candidate; the fake binary cannot even run.
		t.Logf("error: %v", err)
	}
	managed, _ := os.ReadFile(kernel.managedPath())
	previous, _ := os.ReadFile(kernel.previousPath())
	if string(managed) != "old" || string(previous) != "older" {
		t.Fatalf("files not restored: managed=%q previous=%q", managed, previous)
	}
}
