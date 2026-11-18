package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A trimmed-down version of what fscarmen's /<UUID>/sing-box returns: a full client
// config whose outbounds mix real nodes, a ShadowTLS detour helper and plumbing.
const fscarmenStyleSubscription = `{
  "dns": {"servers": [{"tag": "remote", "type": "https", "server": "1.1.1.1", "detour": "proxy"}]},
  "inbounds": [{"type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30"], "auto_route": true}],
  "outbounds": [
    {"type": "selector", "tag": "proxy", "outbounds": ["auto", "node-a 节点"], "default": "auto"},
    {"type": "urltest", "tag": "auto", "outbounds": ["node-a 节点", "hy2"]},
    {"type": "direct", "tag": "direct"},
    {"type": "vless", "tag": "node-a 节点", "server": "203.0.113.10", "server_port": 443,
     "uuid": "00000000-0000-4000-8000-000000000001", "flow": "xtls-rprx-vision",
     "tls": {"enabled": true, "server_name": "example.com",
             "utls": {"enabled": true, "fingerprint": "chrome"},
             "reality": {"enabled": true, "public_key": "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0", "short_id": ""}}},
    {"type": "hysteria2", "tag": "hy2", "server": "203.0.113.10", "server_port": 8882,
     "up_mbps": 200, "down_mbps": 1000, "password": "pw",
     "tls": {"enabled": true, "server_name": "example.com",
             "certificate_public_key_sha256": ["47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="], "alpn": ["h3"]}},
    {"type": "shadowsocks", "tag": "ss-over-shadowtls", "method": "2022-blake3-aes-128-gcm",
     "password": "8JCsPssfgS8tiRwiMlhARg==", "detour": "shadowtls-out"},
    {"type": "shadowtls", "tag": "shadowtls-out", "server": "203.0.113.10", "server_port": 8443,
     "version": 3, "password": "pw",
     "tls": {"enabled": true, "server_name": "example.com", "utls": {"enabled": true, "fingerprint": "chrome"}}},
    {"type": "snell", "tag": "snell-node", "server": "203.0.113.10", "server_port": 9000, "version": 6, "psk": "x"}
  ],
  "route": {"final": "proxy"}
}`

func testSubscription(t *testing.T, id, name, body string) *Subscription {
	t.Helper()
	nodes, err := parseSubscription([]byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return &Subscription{ID: id, Name: name, Nodes: nodes}
}

func TestParseSubscriptionKeepsOnlyProxyNodes(t *testing.T) {
	nodes, err := parseSubscription([]byte(fscarmenStyleSubscription))
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	helpers := map[string]bool{}
	for _, node := range nodes {
		tags = append(tags, node.Tag)
		helpers[node.Tag] = node.Helper
	}
	want := "node-a 节点,hy2,ss-over-shadowtls,shadowtls-out,snell-node"
	if got := strings.Join(tags, ","); got != want {
		t.Fatalf("tags = %s, want %s", got, want)
	}
	if !helpers["shadowtls-out"] || helpers["ss-over-shadowtls"] {
		t.Fatalf("helper flags wrong: %v", helpers)
	}
	if nodes[0].Server != "203.0.113.10" || nodes[0].Port != 443 {
		t.Fatalf("server/port not extracted: %+v", nodes[0])
	}
}

func TestParseSubscriptionPreservesLargeNumbers(t *testing.T) {
	body := `{"outbounds":[{"type":"socks","tag":"s","server":"x","server_port":1080,"udp_fragment_default":true,"x":12345678901234567890}]}`
	nodes, err := parseSubscription([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodes[0].Raw), "12345678901234567890") {
		t.Fatalf("number changed: %s", nodes[0].Raw)
	}
}

func TestParseSubscriptionBase64(t *testing.T) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		encoded := encoding.EncodeToString([]byte(fscarmenStyleSubscription))
		nodes, err := parseSubscription([]byte(encoded))
		if err != nil || len(nodes) != 5 {
			t.Fatalf("base64 subscription: nodes=%d err=%v", len(nodes), err)
		}
	}
}

func TestParseSubscriptionExplainsUnsupportedFormats(t *testing.T) {
	cases := map[string]string{
		"proxies:\n  - name: a\n":                                                  "Clash",
		"vless://abc@host:443?security=reality#n\n":                                "分享链接",
		"<!doctype html><html></html>":                                             "网页",
		base64.StdEncoding.EncodeToString([]byte("vless://a@b:1#x\nss://c@d:2#y")): "分享链接",
		`{"outbounds":[{"type":"direct","tag":"direct"}]}`:                         "没有找到可用的节点",
	}
	for body, want := range cases {
		_, err := parseSubscription([]byte(body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("body %.30q: err = %v, want mention of %q", body, err, want)
		}
	}
}

func TestAssignTagsResolvesCollisionsAndReservedNames(t *testing.T) {
	first := &Subscription{ID: "a", Name: "机场A", Nodes: []Node{{Tag: "香港"}, {Tag: "auto"}, {Tag: "only-a"}}}
	second := &Subscription{ID: "b", Name: "机场B", Nodes: []Node{{Tag: "香港"}}}
	tags := assignTags([]*Subscription{first, second})
	expect := map[nodeKey]string{
		{"a", "香港"}:     "机场A · 香港",
		{"a", "auto"}:   "机场A · auto",
		{"a", "only-a"}: "only-a",
		{"b", "香港"}:     "机场B · 香港",
	}
	for key, want := range expect {
		if tags[key] != want {
			t.Errorf("tag for %v = %q, want %q", key, tags[key], want)
		}
	}
}

func TestGenerateConfigRewiresDetourAndSelection(t *testing.T) {
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	other := testSubscription(t, "s2", "other", fscarmenStyleSubscription)
	built, err := generateConfig(buildInput{
		Settings:    defaultSettings(),
		Subs:        []*Subscription{sub, other},
		Selected:    &NodeRef{SubID: "s2", Tag: "hy2"},
		ClashAddr:   "127.0.0.1:19090",
		ClashSecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(built.JSON, &config); err != nil {
		t.Fatal(err)
	}
	byTag := map[string]map[string]any{}
	for _, outbound := range config.Outbounds {
		byTag[outbound["tag"].(string)] = outbound
	}
	if detour := byTag["other · ss-over-shadowtls"]["detour"]; detour != "other · shadowtls-out" {
		t.Fatalf("detour not rewritten: %v", detour)
	}
	if built.SelectedTag != "other · hy2" || byTag["proxy"]["default"] != "other · hy2" {
		t.Fatalf("selection = %s / %v", built.SelectedTag, byTag["proxy"]["default"])
	}
	for _, member := range byTag["auto"]["outbounds"].([]any) {
		if strings.Contains(member.(string), "shadowtls-out") {
			t.Fatal("helper node must not be selectable")
		}
	}
	if len(built.Entries) != len(config.Outbounds) {
		t.Fatalf("entries %d != outbounds %d", len(built.Entries), len(config.Outbounds))
	}
}

func TestGenerateConfigFallsBackToAutoForUnknownSelection(t *testing.T) {
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	built, err := generateConfig(buildInput{
		Settings: defaultSettings(), Subs: []*Subscription{sub},
		Selected:  &NodeRef{SubID: "s1", Tag: "gone"},
		ClashAddr: "127.0.0.1:1", ClashSecret: "x",
	})
	if err != nil || built.SelectedTag != tagAuto {
		t.Fatalf("selected = %v, err = %v", built, err)
	}
}

func TestExcludingDetourTargetExcludesDependents(t *testing.T) {
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	excluded := excludeDependents([]*Subscription{sub}, map[nodeKey]string{{"s1", "shadowtls-out"}: "bad"})
	if _, ok := excluded[nodeKey{"s1", "ss-over-shadowtls"}]; !ok {
		t.Fatal("dependent node should be excluded too")
	}
}

func TestDNSServerOptions(t *testing.T) {
	cases := map[string]string{
		"local":                            `[{"tag":"dns-direct","type":"local"}]`,
		"223.5.5.5":                        `[{"server":"223.5.5.5","tag":"dns-direct","type":"udp"}]`,
		"tls://1.1.1.1:853":                `[{"server":"1.1.1.1","server_port":853,"tag":"dns-direct","type":"tls"}]`,
		"https://dns.alidns.com/dns-query": `[{"domain_resolver":"dns-bootstrap","server":"dns.alidns.com","tag":"dns-direct","type":"https"},{"tag":"dns-bootstrap","type":"local"}]`,
	}
	for input, want := range cases {
		servers, err := dnsServerOptions(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		got, _ := json.Marshal(servers)
		if string(got) != want {
			t.Errorf("%s:\n got  %s\n want %s", input, got, want)
		}
	}
	if _, err := dnsServerOptions("ftp://x"); err == nil {
		t.Error("ftp scheme should be rejected")
	}
}

func TestFriendlyCheckError(t *testing.T) {
	got := friendlyCheckError("FATAL[0000] decode config at /tmp/check-1.json: outbounds[7]: unknown outbound type: snell")
	if got != "当前 sing-box 版本不支持 snell 协议" {
		t.Fatalf("got %q", got)
	}
	if index, ok := outboundIndexFromError("initialize outbound[3]: TLS required"); !ok || index != 3 {
		t.Fatalf("index = %d, %v", index, ok)
	}
}

// fakeChecker rejects outbounds of the given types, the way sing-box check would.
type fakeChecker struct{ reject map[string]bool }

func (f *fakeChecker) Check(_ context.Context, config []byte) error {
	var document struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(config, &document); err != nil {
		return err
	}
	for i, outbound := range document.Outbounds {
		if f.reject[outbound["type"].(string)] {
			return &checkError{message: "FATAL[0000] decode config at x.json: outbounds[" + strconv.Itoa(i) + "]: unknown outbound type: " + outbound["type"].(string)}
		}
	}
	return nil
}

func TestBuildCheckedDropsRejectedNodesOnly(t *testing.T) {
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	built, excluded, err := buildChecked(context.Background(), &fakeChecker{reject: map[string]bool{"snell": true, "shadowtls": true}},
		buildInput{Settings: defaultSettings(), Subs: []*Subscription{sub}, ClashAddr: "127.0.0.1:1", ClashSecret: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"snell-node", "shadowtls-out", "ss-over-shadowtls"} {
		if _, ok := excluded[nodeKey{"s1", tag}]; !ok {
			t.Errorf("%s should be excluded, got %v", tag, excluded)
		}
	}
	if !strings.Contains(excluded[nodeKey{"s1", "snell-node"}], "不支持 snell") {
		t.Errorf("reason = %q", excluded[nodeKey{"s1", "snell-node"}])
	}
	if !strings.Contains(string(built.JSON), `"node-a 节点"`) || !strings.Contains(string(built.JSON), `"hy2"`) {
		t.Fatal("good nodes missing from config")
	}
}

// TestGeneratedConfigPassesRealSingBoxCheck runs the installed sing-box binary.
func TestGeneratedConfigPassesRealSingBoxCheck(t *testing.T) {
	binary, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box not installed")
	}
	sub := testSubscription(t, "s1", "home", fscarmenStyleSubscription)
	settings := defaultSettings()
	settings.DNSServer = "https://dns.alidns.com/dns-query"
	settings.ProxyUser, settings.ProxyPass = "u", "p"
	built, excluded, err := buildChecked(context.Background(), &singBoxChecker{binary: fixedBinary(binary), tempDir: t.TempDir()},
		buildInput{Settings: settings, Subs: []*Subscription{sub}, ClashAddr: "127.0.0.1:19090", ClashSecret: "x"})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	t.Logf("excluded by this sing-box build: %v", excluded)
	if _, ok := excluded[nodeKey{"s1", "node-a 节点"}]; ok {
		t.Fatal("vless reality node should be accepted")
	}
	if !strings.Contains(string(built.JSON), "clash_api") {
		t.Fatal("clash_api missing")
	}
}

func TestPBKDF2KnownVector(t *testing.T) {
	// RFC 7914 section 11 test vector for PBKDF2-HMAC-SHA256.
	got := pbkdf2SHA256([]byte("passwd"), []byte("salt"), 1, 64)
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if hex.EncodeToString(got) != want {
		t.Fatalf("pbkdf2 = %x", got)
	}
}

func TestAuthSessionLifecycle(t *testing.T) {
	auth := NewAuth(t.TempDir() + "/auth.json")
	if err := auth.SetPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := auth.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if !auth.Verify("correct horse") || auth.Verify("wrong password") {
		t.Fatal("verify mismatch")
	}
	now := time.Now()
	token, err := auth.IssueSession(now)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckSession(token, now) {
		t.Fatal("fresh session rejected")
	}
	if auth.CheckSession(token, now.Add(sessionLifetime+time.Minute)) {
		t.Fatal("expired session accepted")
	}
	if auth.CheckSession(token+"x", now) {
		t.Fatal("tampered session accepted")
	}
	time.Sleep(10 * time.Millisecond) // make sure the file's mtime moves
	if err := auth.SetPassword("another password"); err != nil {
		t.Fatal(err)
	}
	if auth.CheckSession(token, now) {
		t.Fatal("password change must sign out old sessions")
	}
}

func TestUpdateDue(t *testing.T) {
	now := time.Now()
	sub := &Subscription{IntervalHours: 24, AttemptedAt: now.Add(-2 * time.Hour)}
	if updateDue(sub, now) {
		t.Fatal("not due yet")
	}
	sub.LastError = "boom"
	if !updateDue(sub, now) {
		t.Fatal("failed update should retry after an hour")
	}
	sub.IntervalHours = 0
	if updateDue(sub, now) {
		t.Fatal("interval 0 disables updates")
	}
}

func TestViewOfEmptyStateHasSubscriptionArray(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	core := NewCore(fixedBinary("sing-box"), dir+"/config.json", dir, "127.0.0.1:1", "x")
	app := NewApp(store, core, &fakeChecker{}, NewKernel("", dir+"/kernel"), AppPaths{Config: dir + "/config.json", RuleSet: dir + "/rules", Temp: dir})
	data, err := json.Marshal(app.View(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"subscriptions":[]`) {
		t.Fatalf("empty state must serialise subscriptions as []: %s", data)
	}
}
