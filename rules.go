package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	ruleSetGeositeCN = "geosite-cn"
	ruleSetGeoIPCN   = "geoip-cn"

	maxRuleSetBytes     = 16 << 20
	ruleSetFetchTimeout = 2 * time.Minute
)

// srsMagic starts every binary sing-box rule-set file.
var srsMagic = []byte("SRS")

type ruleSetSpec struct {
	Tag string
	URL string
}

// The official rule sets generated from v2fly domain-list-community and MaxMind/ipinfo.
var chinaRuleSets = []ruleSetSpec{
	{Tag: ruleSetGeositeCN, URL: "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs"},
	{Tag: ruleSetGeoIPCN, URL: "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"},
}

func ruleSetPath(dir, tag string) string { return filepath.Join(dir, tag+".srs") }

func ruleSetsPresent(dir string) bool {
	for _, spec := range chinaRuleSets {
		info, err := os.Stat(ruleSetPath(dir, spec.Tag))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

// UpdateRuleSets downloads the China rule sets, has sing-box validate them, and only
// then replaces the files in use. sing-box reads them at start, so a running core
// that uses them is restarted.
func (a *App) UpdateRuleSets(ctx context.Context) error {
	a.ruleSetMu.Lock()
	defer a.ruleSetMu.Unlock()
	if err := os.MkdirAll(a.ruleSetDir, 0o700); err != nil {
		return err
	}

	staged := map[string]string{}
	defer func() {
		for _, path := range staged {
			os.Remove(path)
		}
	}()
	header := http.Header{"User-Agent": {"sbpanel/" + version}}
	for _, spec := range chinaRuleSets {
		var data []byte
		_, err := a.fetchWithFallback(ctx, spec.URL, header, ruleSetFetchTimeout, func(response *http.Response) error {
			body, err := readLimited(response.Body, maxRuleSetBytes)
			if err != nil {
				return err
			}
			if !bytes.HasPrefix(body, srsMagic) {
				return errors.New("下载到的不是 sing-box 规则文件")
			}
			data = body
			return nil
		})
		if err != nil {
			hint := ""
			if !a.core.Running() {
				hint = "（代理还没启动，只能直连 GitHub。可以先启动代理再试）"
			}
			return fmt.Errorf("下载规则文件 %s 失败%s: %w", spec.Tag, hint, err)
		}
		path := ruleSetPath(a.ruleSetDir, spec.Tag) + ".new"
		if err := writeFileAtomic(path, data, 0o600); err != nil {
			return err
		}
		staged[spec.Tag] = path
	}

	if err := a.checkRuleSetFiles(ctx, staged); err != nil {
		return fmt.Errorf("规则文件没有通过 sing-box 校验: %w", err)
	}

	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	for tag, path := range staged {
		if err := os.Rename(path, ruleSetPath(a.ruleSetDir, tag)); err != nil {
			return err
		}
		delete(staged, tag)
	}
	if err := a.store.Update(func(state *State) error {
		state.RuleSetsUpdatedAt = time.Now()
		return nil
	}); err != nil {
		return err
	}
	state := a.store.Snapshot()
	if state.Settings.BypassChina && a.core.Running() {
		if err := a.core.Restart(ctx); err != nil {
			return fmt.Errorf("规则文件已更新，但重启 sing-box 失败: %w", err)
		}
	}
	return nil
}

// checkRuleSetFiles loads the given files in a minimal config with `sing-box check`.
func (a *App) checkRuleSetFiles(ctx context.Context, paths map[string]string) error {
	var sets []any
	for tag, path := range paths {
		sets = append(sets, map[string]any{"type": "local", "tag": tag, "format": "binary", "path": path})
	}
	config, err := json.Marshal(map[string]any{"route": map[string]any{"rule_set": sets}})
	if err != nil {
		return err
	}
	return a.checker.Check(ctx, config)
}
