package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	tagAuto         = "auto"
	tagProxy        = "proxy"
	tagDirect       = "direct"
	tagDNSDirect    = "dns-direct"
	tagDNSBootstrap = "dns-bootstrap"
	tagInbound      = "mixed-in"
)

var errNoNodes = errors.New("还没有可用的节点，请先添加订阅")

type nodeKey struct {
	SubID string
	Tag   string
}

func (r *NodeRef) key() nodeKey { return nodeKey{SubID: r.SubID, Tag: r.Tag} }

type buildInput struct {
	Settings    Settings
	Subs        []*Subscription
	Selected    *NodeRef
	ClashAddr   string
	ClashSecret string
	Excluded    map[nodeKey]string
	// RuleSetDir holds the downloaded China rule-set files used when Settings.BypassChina is on.
	RuleSetDir string
}

type builtConfig struct {
	JSON        []byte
	Entries     []*nodeKey // index-aligned with the outbounds array; nil for built-ins
	Tags        map[nodeKey]string
	SelectedTag string
}

// assignTags gives every node a config-wide unique tag. It only depends on the
// subscriptions, so a node keeps its tag when other nodes are excluded.
func assignTags(subs []*Subscription) map[nodeKey]string {
	reserved := map[string]bool{tagAuto: true, tagProxy: true, tagDirect: true}
	occurrences := map[string]int{}
	for _, sub := range subs {
		for _, node := range sub.Nodes {
			occurrences[node.Tag]++
		}
	}
	tags := map[nodeKey]string{}
	used := map[string]bool{}
	for _, sub := range subs {
		for _, node := range sub.Nodes {
			tag := node.Tag
			if occurrences[tag] > 1 || reserved[tag] {
				tag = sub.Name + " · " + node.Tag
			}
			base := tag
			for n := 2; used[tag] || reserved[tag]; n++ {
				tag = base + " #" + strconv.Itoa(n)
			}
			used[tag] = true
			tags[nodeKey{SubID: sub.ID, Tag: node.Tag}] = tag
		}
	}
	return tags
}

// excludeDependents marks nodes whose detour points at an excluded node.
func excludeDependents(subs []*Subscription, excluded map[nodeKey]string) map[nodeKey]string {
	result := map[nodeKey]string{}
	for key, reason := range excluded {
		result[key] = reason
	}
	for changed := true; changed; {
		changed = false
		for _, sub := range subs {
			for _, node := range sub.Nodes {
				key := nodeKey{SubID: sub.ID, Tag: node.Tag}
				if _, done := result[key]; done {
					continue
				}
				outbound, err := decodeObject(node.Raw)
				if err != nil {
					continue
				}
				detour, _ := outbound["detour"].(string)
				if _, broken := result[nodeKey{SubID: sub.ID, Tag: detour}]; detour != "" && broken {
					result[key] = fmt.Sprintf("依赖的 %s 不可用", detour)
					changed = true
				}
			}
		}
	}
	return result
}

func generateConfig(input buildInput) (*builtConfig, error) {
	tags := assignTags(input.Subs)
	excluded := excludeDependents(input.Subs, input.Excluded)

	var outbounds []any
	var entries []*nodeKey
	var selectable []string
	for _, sub := range input.Subs {
		subTags := map[string]string{}
		for _, node := range sub.Nodes {
			subTags[node.Tag] = tags[nodeKey{SubID: sub.ID, Tag: node.Tag}]
		}
		for _, node := range sub.Nodes {
			key := nodeKey{SubID: sub.ID, Tag: node.Tag}
			if _, skip := excluded[key]; skip {
				continue
			}
			outbound, err := decodeObject(node.Raw)
			if err != nil {
				return nil, fmt.Errorf("节点 %s 数据损坏: %w", node.Tag, err)
			}
			outbound["tag"] = tags[key]
			if detour, ok := outbound["detour"].(string); ok {
				if renamed, found := subTags[detour]; found {
					outbound["detour"] = renamed
				}
			}
			// Server names are resolved by route.default_domain_resolver; a resolver tag
			// copied from the subscription would point at a DNS server we do not define.
			delete(outbound, "domain_resolver")
			outbounds = append(outbounds, outbound)
			entries = append(entries, &key)
			if !node.Helper {
				selectable = append(selectable, tags[key])
			}
		}
	}
	if len(selectable) == 0 {
		return nil, errNoNodes
	}

	selectedTag := tagAuto
	if input.Selected != nil {
		key := input.Selected.key()
		if tag, ok := tags[key]; ok {
			if _, skip := excluded[key]; !skip && contains(selectable, tag) {
				selectedTag = tag
			}
		}
	}

	settings := input.Settings
	outbounds = append(outbounds,
		map[string]any{
			"type":      "urltest",
			"tag":       tagAuto,
			"outbounds": selectable,
			"url":       settings.TestURL,
			"interval":  "10m",
		},
		map[string]any{
			"type":      "selector",
			"tag":       tagProxy,
			"outbounds": append([]string{tagAuto}, selectable...),
			"default":   selectedTag,
		},
		map[string]any{"type": "direct", "tag": tagDirect},
	)
	entries = append(entries, nil, nil, nil)

	dnsServers, err := dnsServerOptions(settings.DNSServer)
	if err != nil {
		return nil, err
	}
	dns := map[string]any{"servers": dnsServers}
	if settings.DNSStrategy != "" {
		dns["strategy"] = settings.DNSStrategy
	}

	inbound := map[string]any{
		"type":        "mixed",
		"tag":         tagInbound,
		"listen":      settings.ListenAddr,
		"listen_port": settings.ListenPort,
	}
	if settings.ProxyUser != "" {
		inbound["users"] = []any{map[string]any{
			"username": settings.ProxyUser,
			"password": settings.ProxyPass,
		}}
	}

	route := map[string]any{
		"rules":                   routeRules(settings),
		"final":                   tagProxy,
		"default_domain_resolver": tagDNSDirect,
	}
	if settings.BypassChina {
		route["rule_set"] = localRuleSets(input.RuleSetDir)
	}

	config := map[string]any{
		"log":       map[string]any{"level": settings.LogLevel, "timestamp": true},
		"dns":       dns,
		"inbounds":  []any{inbound},
		"outbounds": outbounds,
		"route":     route,
		"experimental": map[string]any{
			"clash_api": map[string]any{
				"external_controller": input.ClashAddr,
				"secret":              input.ClashSecret,
			},
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return &builtConfig{JSON: data, Entries: entries, Tags: tags, SelectedTag: selectedTag}, nil
}

// routeRules sends private addresses direct, and with BypassChina also mainland China
// domains (geosite-cn) and addresses (geoip-cn). Everything else falls through to "proxy".
//
// Domains are never resolved locally just to test geoip-cn: that would leak every
// foreign domain to the local DNS. geoip-cn only matches connections whose destination
// is already an IP (or that sniffing could not turn into a domain).
func routeRules(settings Settings) []any {
	if !settings.BypassChina {
		return []any{map[string]any{"ip_is_private": true, "outbound": tagDirect}}
	}
	return []any{
		// Recover the domain (TLS SNI / HTTP Host) when a client connects by IP.
		map[string]any{"action": "sniff"},
		map[string]any{"ip_is_private": true, "outbound": tagDirect},
		map[string]any{"rule_set": []string{ruleSetGeositeCN}, "outbound": tagDirect},
		map[string]any{"rule_set": []string{ruleSetGeoIPCN}, "outbound": tagDirect},
	}
}

func localRuleSets(dir string) []any {
	var sets []any
	for _, spec := range chinaRuleSets {
		sets = append(sets, map[string]any{
			"type":   "local",
			"tag":    spec.Tag,
			"format": "binary",
			"path":   ruleSetPath(dir, spec.Tag),
		})
	}
	return sets
}

var dnsTypes = map[string]bool{"udp": true, "tcp": true, "tls": true, "https": true, "quic": true, "h3": true}

// dnsServerOptions turns "local", "223.5.5.5", "tls://1.1.1.1" or
// "https://dns.alidns.com/dns-query" into sing-box (1.12+) DNS server objects.
func dnsServerOptions(value string) ([]any, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "local" {
		return []any{map[string]any{"type": "local", "tag": tagDNSDirect}}, nil
	}
	if !strings.Contains(value, "://") {
		value = "udp://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || !dnsTypes[parsed.Scheme] {
		return nil, fmt.Errorf("DNS 服务器格式不对: %s", value)
	}
	server := map[string]any{"type": parsed.Scheme, "tag": tagDNSDirect, "server": parsed.Hostname()}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("DNS 端口不对: %s", port)
		}
		server["server_port"] = number
	}
	if (parsed.Scheme == "https" || parsed.Scheme == "h3") && parsed.Path != "" && parsed.Path != "/dns-query" {
		server["path"] = parsed.Path
	}
	if net.ParseIP(parsed.Hostname()) == nil {
		// A DNS server given by name needs another resolver to find its own address.
		server["domain_resolver"] = tagDNSBootstrap
		return []any{server, map[string]any{"type": "local", "tag": tagDNSBootstrap}}, nil
	}
	return []any{server}, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// Checker validates a config with `sing-box check`.
type Checker interface {
	Check(ctx context.Context, config []byte) error
}

type singBoxChecker struct {
	binary  func() string // read on every check, so a kernel switch takes effect at once
	tempDir string
}

func fixedBinary(path string) func() string { return func() string { return path } }

var errNoSingBox = errors.New("还没有安装 sing-box，请在“设置 → sing-box 内核”里点“检查更新”后安装")

func (c *singBoxChecker) Check(ctx context.Context, config []byte) error {
	binary := c.binary()
	if binary == "" {
		return errNoSingBox
	}
	file, err := os.CreateTemp(c.tempDir, "check-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(config); err != nil {
		file.Close()
		return err
	}
	file.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--disable-color", "check", "-c", file.Name())
	command.Dir = c.tempDir
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(stripANSI(string(output)))
	if message == "" {
		message = err.Error()
	}
	return &checkError{message: strings.ReplaceAll(message, file.Name(), filepath.Base(file.Name()))}
}

type checkError struct{ message string }

func (e *checkError) Error() string { return e.message }

var (
	outboundIndexPattern = regexp.MustCompile(`outbounds?\[(\d+)\]`)
	ansiPattern          = regexp.MustCompile("\x1b\\[[0-9;]*m")
	fatalPrefixPattern   = regexp.MustCompile(`^(FATAL|ERROR)\[\d+\]\s*`)
	decodePrefixPattern  = regexp.MustCompile(`^decode config at [^:]+:\s*`)
	unknownTypePattern   = regexp.MustCompile(`unknown outbound type: (\S+)`)
)

func stripANSI(text string) string { return ansiPattern.ReplaceAllString(text, "") }

func outboundIndexFromError(message string) (int, bool) {
	match := outboundIndexPattern.FindStringSubmatch(message)
	if match == nil {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil
}

// friendlyCheckError turns a sing-box check failure about one outbound into a short reason.
func friendlyCheckError(message string) string {
	line := strings.TrimSpace(strings.SplitN(message, "\n", 2)[0])
	line = fatalPrefixPattern.ReplaceAllString(line, "")
	line = decodePrefixPattern.ReplaceAllString(line, "")
	if match := unknownTypePattern.FindStringSubmatch(line); match != nil {
		return "当前 sing-box 版本不支持 " + match[1] + " 协议"
	}
	line = regexp.MustCompile(`(initialize |parse )?outbounds?\[\d+\]:?\s*`).ReplaceAllString(line, "")
	return "sing-box 拒绝了这个节点: " + line
}

// buildChecked generates a config and drops nodes that sing-box rejects, one at a time,
// so a single unsupported node cannot take the whole subscription down with it.
func buildChecked(ctx context.Context, checker Checker, input buildInput) (*builtConfig, map[nodeKey]string, error) {
	excluded := map[nodeKey]string{}
	for key, reason := range input.Excluded {
		excluded[key] = reason
	}
	totalNodes := 0
	for _, sub := range input.Subs {
		totalNodes += len(sub.Nodes)
	}
	for attempt := 0; attempt <= totalNodes; attempt++ {
		input.Excluded = excluded
		built, err := generateConfig(input)
		if err != nil {
			return nil, excludeDependents(input.Subs, excluded), err
		}
		checkErr := checker.Check(ctx, built.JSON)
		if checkErr == nil {
			return built, excludeDependents(input.Subs, excluded), nil
		}
		index, ok := outboundIndexFromError(checkErr.Error())
		if !ok || index >= len(built.Entries) || built.Entries[index] == nil {
			return nil, excludeDependents(input.Subs, excluded), fmt.Errorf("sing-box 配置检查失败: %w", checkErr)
		}
		excluded[*built.Entries[index]] = friendlyCheckError(checkErr.Error())
	}
	return nil, excluded, errors.New("配置检查次数过多")
}

func configChanged(path string, data []byte) bool {
	current, err := os.ReadFile(path)
	return err != nil || !bytes.Equal(current, data)
}
