package main

import (
	"os"
	"path/filepath"
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

func TestUnitTemplateUsesEnvFile(t *testing.T) {
	if !strings.Contains(unitTemplate, "EnvironmentFile=-"+envPath) || strings.Contains(unitTemplate, "--listen") {
		t.Fatal("unit must take the listen address from the env file")
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
