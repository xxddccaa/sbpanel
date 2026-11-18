package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The UA contains "sing-box" and "SFA" so that subscription servers which pick the
// format by User-Agent (fscarmen's nginx map, most airport panels) return sing-box JSON.
const defaultUserAgent = "sbpanel (sing-box; SFA compatible)"

const (
	maxSubscriptionBytes = 16 << 20
	fetchTimeout         = 30 * time.Second
)

// Outbound types that are plumbing rather than proxy servers.
var nonProxyOutboundTypes = map[string]bool{
	"direct":   true,
	"block":    true,
	"dns":      true,
	"selector": true,
	"urltest":  true,
}

func fetchSubscription(ctx context.Context, sub *Subscription, proxy *url.URL) ([]byte, error) {
	userAgent := sub.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	var body []byte
	err := fetchOnce(ctx, newHTTPClient(proxy, fetchTimeout), sub.URL, http.Header{"User-Agent": {userAgent}},
		func(response *http.Response) error {
			data, err := readLimited(response.Body, maxSubscriptionBytes)
			body = data
			return err
		})
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	return body, nil
}

// parseSubscription accepts a sing-box JSON config, optionally base64 encoded.
func parseSubscription(body []byte) ([]Node, error) {
	text := bytes.TrimSpace(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")))
	if len(text) == 0 {
		return nil, errors.New("订阅内容为空")
	}
	if text[0] == '{' {
		return parseSingBoxJSON(text)
	}
	if decoded, ok := decodeBase64Loose(text); ok {
		decoded = bytes.TrimSpace(decoded)
		if len(decoded) > 0 && decoded[0] == '{' {
			return parseSingBoxJSON(decoded)
		}
		text = decoded
	}
	return nil, describeUnsupportedFormat(text)
}

func describeUnsupportedFormat(text []byte) error {
	lower := strings.ToLower(string(text[:min(len(text), 4096)]))
	switch {
	case strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype"):
		return errors.New("服务器返回的是网页而不是订阅，请检查订阅地址")
	case strings.Contains(lower, "proxies:"):
		return errors.New("这是 Clash 格式的订阅，目前只支持 sing-box 格式。可以把 User-Agent 设为 sing-box 再试，或换成 sing-box 格式的订阅地址")
	case strings.Contains(lower, "://"):
		return errors.New("这是分享链接（vless:// 等）格式的订阅，目前只支持 sing-box 格式。fscarmen 脚本的订阅请用 /<UUID>/sing-box 地址")
	}
	return errors.New("无法识别的订阅格式，目前只支持 sing-box JSON")
}

func decodeBase64Loose(text []byte) ([]byte, bool) {
	compact := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(text))
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(compact); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

func parseSingBoxJSON(data []byte) ([]Node, error) {
	var document struct {
		Outbounds []json.RawMessage `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("订阅不是合法的 JSON: %w", err)
	}

	var nodes []Node
	seenTags := map[string]bool{}
	detourTargets := map[string]bool{}
	for _, raw := range document.Outbounds {
		outbound, err := decodeObject(raw)
		if err != nil {
			continue
		}
		outboundType, _ := outbound["type"].(string)
		tag, _ := outbound["tag"].(string)
		if outboundType == "" || tag == "" || nonProxyOutboundTypes[outboundType] || seenTags[tag] {
			continue
		}
		seenTags[tag] = true
		if detour, ok := outbound["detour"].(string); ok && detour != "" {
			detourTargets[detour] = true
		}
		compact := &bytes.Buffer{}
		if err := json.Compact(compact, raw); err != nil {
			continue
		}
		server, _ := outbound["server"].(string)
		nodes = append(nodes, Node{
			Tag:    tag,
			Type:   outboundType,
			Server: server,
			Port:   jsonInt(outbound["server_port"]),
			Raw:    compact.Bytes(),
		})
	}

	selectable := 0
	for i := range nodes {
		nodes[i].Helper = detourTargets[nodes[i].Tag]
		if !nodes[i].Helper {
			selectable++
		}
	}
	if selectable == 0 {
		return nil, errors.New("订阅里没有找到可用的节点")
	}
	return nodes, nil
}

// decodeObject keeps numbers as json.Number so re-encoding never changes them.
func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("not an object")
	}
	return object, nil
}

func jsonInt(value any) int {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	parsed, err := number.Int64()
	if err != nil {
		return 0
	}
	return int(parsed)
}

func nodesEqual(a, b []Node) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
