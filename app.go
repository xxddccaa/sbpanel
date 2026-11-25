package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	schedulerTick     = time.Minute
	failedUpdateRetry = time.Hour
	delayTestTimeout  = 5 * time.Second
	delayTestWorkers  = 8
	// Subscriptions update only when asked by default: a fetch on a fixed schedule is
	// a recognisable pattern, and the URL carries the account secret.
	defaultIntervalHour = 0
	// Scheduled updates (when enabled) start at a random point up to this fraction of
	// the interval later, so they do not happen at the same minute every day.
	scheduleJitterFraction = 0.25
)

// App ties the stored state, the generated sing-box config and the running core together.
type App struct {
	store      *Store
	core       *Core
	checker    Checker
	configPath string
	kernel     *Kernel
	panel      *PanelUpdater
	ruleSetDir string
	tempDir    string

	// applyMu serialises every change that rebuilds the config or restarts the core.
	applyMu sync.Mutex

	mu        sync.Mutex
	excluded  map[nodeKey]string
	tags      map[nodeKey]string
	delays    map[nodeKey]delayResult
	updating  map[string]bool
	lastApply string

	ruleSetMu sync.Mutex
}

type delayResult struct {
	Delay int       `json:"delay"`
	At    time.Time `json:"at"`
}

type AppPaths struct {
	Config  string
	RuleSet string
	Temp    string
}

func NewApp(store *Store, core *Core, checker Checker, kernel *Kernel, paths AppPaths) *App {
	return &App{
		store:      store,
		core:       core,
		checker:    checker,
		kernel:     kernel,
		configPath: paths.Config,
		ruleSetDir: paths.RuleSet,
		tempDir:    paths.Temp,
		excluded:   map[nodeKey]string{},
		tags:       map[nodeKey]string{},
		delays:     map[nodeKey]delayResult{},
		updating:   map[string]bool{},
	}
}

type startFailure struct{ err error }

func (e *startFailure) Error() string { return e.err.Error() }
func (e *startFailure) Unwrap() error { return e.err }

// apply regenerates the config from the stored state. When restart is true and the
// core should run, it is restarted if the config changed; otherwise the file is only
// rewritten so the next start picks it up.
func (a *App) apply(ctx context.Context, restart bool) error {
	state := a.store.Snapshot()
	if state.Settings.BypassChina && !ruleSetsPresent(a.ruleSetDir) {
		return errors.New("“绕过大陆”需要的规则文件不在了，请在设置里点“更新规则文件”")
	}
	built, excluded, err := buildChecked(ctx, a.checker, a.buildInput(state))
	a.mu.Lock()
	a.excluded = excluded
	if built != nil {
		a.tags = built.Tags
	}
	a.lastApply = ""
	if err != nil && !errors.Is(err, errNoNodes) {
		a.lastApply = err.Error()
	}
	a.mu.Unlock()

	if errors.Is(err, errNoNodes) {
		// Nothing to proxy through: keep CoreEnabled so sing-box comes back with the next nodes.
		if a.core.Running() {
			_ = a.core.Stop()
		}
		_ = os.Remove(a.configPath)
		return nil
	}
	if err != nil {
		return err
	}

	changed := configChanged(a.configPath, built.JSON)
	if changed {
		if err := writeFileAtomic(a.configPath, built.JSON, 0o600); err != nil {
			return err
		}
	}
	if !state.CoreEnabled {
		return nil
	}
	if !a.core.Running() {
		err = a.core.Start(ctx)
	} else if changed && restart {
		err = a.core.Restart(ctx)
	}
	if err != nil {
		a.mu.Lock()
		a.lastApply = "sing-box 启动失败: " + err.Error()
		a.mu.Unlock()
		return &startFailure{err}
	}
	return nil
}

// mutate changes the state and applies it. If sing-box fails to start with the new
// config, both the state and the config are rolled back and the old core restarted.
func (a *App) mutate(ctx context.Context, restart bool, change func(*State) error) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	previous := a.store.Snapshot()
	previousConfig, previousConfigErr := os.ReadFile(a.configPath)
	if err := a.store.Update(change); err != nil {
		return err
	}
	err := a.apply(ctx, restart)
	var failure *startFailure
	if !errors.As(err, &failure) {
		return err
	}
	log.Printf("新配置启动失败，回滚: %v", err)
	if restoreErr := a.store.Update(func(state *State) error { *state = *previous; return nil }); restoreErr != nil {
		return fmt.Errorf("%w（回滚状态也失败: %v）", err, restoreErr)
	}
	if previousConfigErr == nil && previous.CoreEnabled {
		if writeErr := writeFileAtomic(a.configPath, previousConfig, 0o600); writeErr == nil {
			if restartErr := a.core.Restart(ctx); restartErr != nil {
				return fmt.Errorf("%w（旧配置也启动失败: %v）", err, restartErr)
			}
		}
	}
	_ = a.apply(ctx, false)
	return fmt.Errorf("%w。已恢复到修改之前的状态", err)
}

func (a *App) buildInput(state *State) buildInput {
	return buildInput{
		Settings:    state.Settings,
		Subs:        state.Subscriptions,
		Selected:    state.Selected,
		ClashAddr:   clashAddress(state.ClashPort),
		ClashSecret: state.ClashSecret,
		RuleSetDir:  a.ruleSetDir,
	}
}

func clashAddress(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

func (a *App) Startup(ctx context.Context) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if err := a.apply(ctx, true); err != nil {
		log.Printf("启动时应用配置失败: %v", err)
	}
}

// ---- subscriptions ----

type SubscriptionInput struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	UserAgent     string `json:"userAgent"`
	IntervalHours *int   `json:"intervalHours"`
	UpdateVia     string `json:"updateVia"`
}

func (input *SubscriptionInput) validate() error {
	switch input.UpdateVia {
	case "":
		input.UpdateVia = updateViaAuto
	case updateViaAuto, updateViaProxy, updateViaDirect:
	default:
		return errors.New("更新方式不对")
	}
	input.Name = strings.TrimSpace(input.Name)
	input.URL = strings.TrimSpace(input.URL)
	input.UserAgent = strings.TrimSpace(input.UserAgent)
	parsed, err := url.Parse(input.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("订阅地址要以 http:// 或 https:// 开头")
	}
	if input.Name == "" {
		input.Name = parsed.Hostname()
	}
	if input.IntervalHours != nil && (*input.IntervalHours < 0 || *input.IntervalHours > 24*30) {
		return errors.New("自动更新间隔要在 0 到 720 小时之间")
	}
	return nil
}

func (a *App) AddSubscription(ctx context.Context, input SubscriptionInput) (string, error) {
	if err := input.validate(); err != nil {
		return "", err
	}
	id := strings.ToLower(randomPassword(10))
	interval := defaultIntervalHour
	if input.IntervalHours != nil {
		interval = *input.IntervalHours
	}
	err := a.store.Update(func(state *State) error {
		for _, sub := range state.Subscriptions {
			if sub.URL == input.URL {
				return fmt.Errorf("这个订阅地址已经添加过了（%s）", sub.Name)
			}
		}
		state.Subscriptions = append(state.Subscriptions, &Subscription{
			ID:            id,
			Name:          uniqueSubscriptionName(state, input.Name, ""),
			URL:           input.URL,
			UserAgent:     input.UserAgent,
			IntervalHours: interval,
			UpdateVia:     input.UpdateVia,
			Nodes:         []Node{},
		})
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, a.UpdateSubscription(ctx, id)
}

func uniqueSubscriptionName(state *State, name, selfID string) string {
	taken := map[string]bool{}
	for _, sub := range state.Subscriptions {
		if sub.ID != selfID {
			taken[sub.Name] = true
		}
	}
	candidate := name
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s %d", name, n)
	}
	return candidate
}

func (a *App) EditSubscription(ctx context.Context, id string, input SubscriptionInput) error {
	if err := input.validate(); err != nil {
		return err
	}
	urlChanged := false
	err := a.mutate(ctx, true, func(state *State) error {
		sub := state.findSubscription(id)
		if sub == nil {
			return errNotFound
		}
		urlChanged = sub.URL != input.URL || sub.UserAgent != input.UserAgent
		sub.Name = uniqueSubscriptionName(state, input.Name, id)
		sub.URL = input.URL
		sub.UserAgent = input.UserAgent
		sub.UpdateVia = input.UpdateVia
		if input.IntervalHours != nil {
			sub.IntervalHours = *input.IntervalHours
		}
		return nil
	})
	if err != nil || !urlChanged {
		return err
	}
	return a.UpdateSubscription(ctx, id)
}

var errNotFound = errors.New("找不到这个订阅")

func (a *App) DeleteSubscription(ctx context.Context, id string) error {
	return a.mutate(ctx, true, func(state *State) error {
		index := -1
		for i, sub := range state.Subscriptions {
			if sub.ID == id {
				index = i
			}
		}
		if index < 0 {
			return errNotFound
		}
		state.Subscriptions = append(state.Subscriptions[:index], state.Subscriptions[index+1:]...)
		if state.Selected != nil && state.Selected.SubID == id {
			state.Selected = nil
		}
		return nil
	})
}

// UpdateSubscription downloads one subscription and applies it. A failed download
// keeps the old nodes and records the error on the subscription.
func (a *App) UpdateSubscription(ctx context.Context, id string) error {
	a.mu.Lock()
	if a.updating[id] {
		a.mu.Unlock()
		return errors.New("这个订阅正在更新中")
	}
	a.updating[id] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.updating, id)
		a.mu.Unlock()
	}()

	state := a.store.Snapshot()
	sub := state.findSubscription(id)
	if sub == nil {
		return errNotFound
	}
	routes, err := a.subscriptionRoutes(sub.UpdateVia)
	if err != nil {
		return a.recordUpdateFailure(id, err)
	}
	var nodes []Node
	var via string
	var failures []error
	for _, route := range routes {
		body, err := fetchSubscription(ctx, sub, route.proxy)
		if err == nil {
			nodes, err = parseSubscription(body)
		}
		if err == nil {
			via = route.name
			break
		}
		if len(routes) > 1 {
			err = fmt.Errorf("通过%s: %w", route.name, err)
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	if via == "" {
		return a.recordUpdateFailure(id, errors.Join(failures...))
	}
	return a.mutate(ctx, true, func(state *State) error {
		current := state.findSubscription(id)
		if current == nil {
			return errNotFound
		}
		current.Nodes = nodes
		current.UpdatedVia = via
		current.UpdatedAt = time.Now()
		current.AttemptedAt = current.UpdatedAt
		current.LastError = ""
		return nil
	})
}

// subscriptionRoutes picks how to reach the subscription server.
func (a *App) subscriptionRoutes(updateVia string) ([]transferRoute, error) {
	proxyRoute := transferRoute{name: routeNameProxy, proxy: localProxyURL(a.store.Snapshot().Settings)}
	directRoute := transferRoute{name: routeNameDirect}
	switch updateVia {
	case updateViaDirect:
		return []transferRoute{directRoute}, nil
	case updateViaProxy:
		if !a.core.Running() {
			return nil, errors.New("这个订阅设置为“只通过代理更新”，但代理没有在运行")
		}
		return []transferRoute{proxyRoute}, nil
	default:
		if a.core.Running() {
			return []transferRoute{proxyRoute, directRoute}, nil
		}
		return []transferRoute{directRoute}, nil
	}
}

func (a *App) recordUpdateFailure(id string, cause error) error {
	_ = a.store.Update(func(state *State) error {
		if sub := state.findSubscription(id); sub != nil {
			sub.AttemptedAt = time.Now()
			sub.LastError = cause.Error()
		}
		return nil
	})
	return cause
}

func localProxyURL(settings Settings) *url.URL {
	host := settings.ListenAddr
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	proxy := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(settings.ListenPort))}
	if settings.ProxyUser != "" {
		proxy.User = url.UserPassword(settings.ProxyUser, settings.ProxyPass)
	}
	return proxy
}

// UpdateAll updates every subscription and returns the failures by name.
func (a *App) UpdateAll(ctx context.Context) map[string]string {
	failures := map[string]string{}
	for _, sub := range a.store.Snapshot().Subscriptions {
		if err := a.UpdateSubscription(ctx, sub.ID); err != nil {
			failures[sub.Name] = err.Error()
		}
	}
	return failures
}

func (a *App) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		for _, sub := range a.store.Snapshot().Subscriptions {
			if !updateDue(sub, now) {
				continue
			}
			if err := a.UpdateSubscription(ctx, sub.ID); err != nil {
				log.Printf("自动更新订阅 %s 失败: %v", sub.Name, err)
			} else {
				log.Printf("自动更新订阅 %s 成功", sub.Name)
			}
		}
	}
}

func updateDue(sub *Subscription, now time.Time) bool {
	if sub.IntervalHours <= 0 {
		return false
	}
	interval := time.Duration(sub.IntervalHours) * time.Hour
	interval += time.Duration(float64(interval) * scheduleJitterFraction * jitterFor(sub.ID, sub.AttemptedAt))
	if sub.LastError != "" {
		interval = min(interval, failedUpdateRetry)
	}
	return now.Sub(sub.AttemptedAt) >= interval
}

// ---- selection, core control, delay tests ----

// Select switches the outbound used by the "proxy" selector; ref == nil means automatic.
func (a *App) Select(ctx context.Context, ref *NodeRef) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	state := a.store.Snapshot()
	target := tagAuto
	if ref != nil {
		a.mu.Lock()
		tag, ok := a.tags[ref.key()]
		_, excluded := a.excluded[ref.key()]
		a.mu.Unlock()
		if !ok {
			return errors.New("找不到这个节点")
		}
		if excluded {
			return errors.New("这个节点当前不可用")
		}
		if sub := state.findSubscription(ref.SubID); sub != nil {
			for _, node := range sub.Nodes {
				if node.Tag == ref.Tag && node.Helper {
					return errors.New("这是其他节点的前置节点，不能单独选择")
				}
			}
		}
		target = tag
	}
	if a.core.Running() {
		if err := a.core.SelectNode(ctx, target); err != nil {
			return fmt.Errorf("切换节点失败: %w", err)
		}
	}
	if err := a.store.Update(func(state *State) error { state.Selected = ref; return nil }); err != nil {
		return err
	}
	// Only the selector's default changes, so the running core does not need a restart.
	return a.apply(ctx, false)
}

func (a *App) StartCore(ctx context.Context) error {
	if !hasSelectableNodes(a.store.Snapshot()) {
		return errNoNodes
	}
	return a.mutate(ctx, true, func(state *State) error { state.CoreEnabled = true; return nil })
}

func hasSelectableNodes(state *State) bool {
	for _, sub := range state.Subscriptions {
		for _, node := range sub.Nodes {
			if !node.Helper {
				return true
			}
		}
	}
	return false
}

func (a *App) StopCore(ctx context.Context) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if err := a.store.Update(func(state *State) error { state.CoreEnabled = false; return nil }); err != nil {
		return err
	}
	return a.core.Stop()
}

func (a *App) RestartCore(ctx context.Context) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	if !a.store.Snapshot().CoreEnabled {
		return errors.New("sing-box 没有启用")
	}
	wasRunning := a.core.Running()
	if err := a.apply(ctx, false); err != nil {
		return err
	}
	if !wasRunning {
		return nil // apply has just started it
	}
	return a.core.Restart(ctx)
}

func (a *App) TestDelays(ctx context.Context) error {
	if !a.core.Running() {
		return errors.New("先启动 sing-box 才能测速")
	}
	state := a.store.Snapshot()
	a.mu.Lock()
	type job struct {
		key nodeKey
		tag string
	}
	var jobs []job
	for _, sub := range state.Subscriptions {
		for _, node := range sub.Nodes {
			key := nodeKey{SubID: sub.ID, Tag: node.Tag}
			if _, skip := a.excluded[key]; skip || node.Helper {
				continue
			}
			jobs = append(jobs, job{key: key, tag: a.tags[key]})
		}
	}
	a.mu.Unlock()

	queue := make(chan job)
	var wait sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for range min(delayTestWorkers, max(len(jobs), 1)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for item := range queue {
				delay, err := a.core.Delay(ctx, item.tag, state.Settings.TestURL, delayTestTimeout)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				a.mu.Lock()
				a.delays[item.key] = delayResult{Delay: delay, At: time.Now()}
				a.mu.Unlock()
			}
		}()
	}
	for _, item := range jobs {
		queue <- item
	}
	close(queue)
	wait.Wait()
	return firstErr
}

// ---- settings ----

var (
	validLogLevels     = []string{"trace", "debug", "info", "warn", "error", "fatal", "panic"}
	validDNSStrategies = []string{"", "prefer_ipv4", "prefer_ipv6", "ipv4_only", "ipv6_only"}
)

func (a *App) UpdateSettings(ctx context.Context, settings Settings) error {
	settings.ListenAddr = strings.TrimSpace(settings.ListenAddr)
	settings.ProxyUser = strings.TrimSpace(settings.ProxyUser)
	settings.DNSServer = strings.TrimSpace(settings.DNSServer)
	settings.TestURL = strings.TrimSpace(settings.TestURL)
	if net.ParseIP(settings.ListenAddr) == nil {
		return errors.New("监听地址要填 IP，例如 127.0.0.1 或 0.0.0.0")
	}
	if settings.ListenPort < 1 || settings.ListenPort > 65535 {
		return errors.New("代理端口要在 1 到 65535 之间")
	}
	if (settings.ProxyUser == "") != (settings.ProxyPass == "") {
		return errors.New("代理用户名和密码要么都填，要么都不填")
	}
	if _, err := dnsServerOptions(settings.DNSServer); err != nil {
		return err
	}
	if !contains(validDNSStrategies, settings.DNSStrategy) {
		return errors.New("DNS 策略不对")
	}
	if !contains(validLogLevels, settings.LogLevel) {
		return errors.New("日志级别不对")
	}
	testURL, err := url.Parse(settings.TestURL)
	if err != nil || (testURL.Scheme != "http" && testURL.Scheme != "https") || testURL.Host == "" {
		return errors.New("测速地址要以 http:// 或 https:// 开头")
	}
	// Turning on China bypass needs the rule-set files first.
	if settings.BypassChina && !ruleSetsPresent(a.ruleSetDir) {
		if err := a.UpdateRuleSets(ctx); err != nil {
			return fmt.Errorf("开启“绕过大陆”需要先下载规则文件，但下载失败了: %w", err)
		}
	}
	return a.mutate(ctx, true, func(state *State) error {
		if settings.ListenPort == state.ClashPort {
			return errors.New("代理端口和内部 API 端口冲突，请换一个")
		}
		state.Settings = settings
		return nil
	})
}

// ---- view model for the web UI ----

type NodeView struct {
	SubID    string `json:"subId"`
	Tag      string `json:"tag"`
	Type     string `json:"type"`
	Server   string `json:"server,omitempty"`
	Port     int    `json:"port,omitempty"`
	Helper   bool   `json:"helper,omitempty"`
	Disabled string `json:"disabled,omitempty"`
	Delay    *int   `json:"delay,omitempty"`
}

type SubscriptionView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	UserAgent     string     `json:"userAgent"`
	IntervalHours int        `json:"intervalHours"`
	UpdateVia     string     `json:"updateVia"`
	UpdatedVia    string     `json:"updatedVia,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	Updating      bool       `json:"updating"`
	Nodes         []NodeView `json:"nodes"`
}

type StateView struct {
	Settings      Settings           `json:"settings"`
	Subscriptions []SubscriptionView `json:"subscriptions"`
	Selected      *NodeRef           `json:"selected"`
	Current       *NodeRef           `json:"current"`
	AutoPick      *NodeRef           `json:"autoPick"`
	Core          CoreStatus         `json:"core"`
	CoreEnabled   bool               `json:"coreEnabled"`
	ProxyURL      string             `json:"proxyUrl"`
	ConfigError   string             `json:"configError,omitempty"`
	RuleSets      RuleSetsView       `json:"ruleSets"`
}

type RuleSetsView struct {
	Present   bool       `json:"present"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

func (a *App) View(ctx context.Context) StateView {
	state := a.store.Snapshot()
	a.mu.Lock()
	tagToKey := map[string]nodeKey{}
	for key, tag := range a.tags {
		tagToKey[tag] = key
	}
	excluded := a.excluded
	delays := map[nodeKey]delayResult{}
	for key, result := range a.delays {
		delays[key] = result
	}
	updating := map[string]bool{}
	for id := range a.updating {
		updating[id] = true
	}
	configError := a.lastApply
	a.mu.Unlock()

	view := StateView{
		// Always an array, never null: the web UI reads .length on it.
		Subscriptions: []SubscriptionView{},
		Settings:      state.Settings,
		Selected:      state.Selected,
		Core:          a.core.Status(),
		CoreEnabled:   state.CoreEnabled,
		ConfigError:   configError,
		RuleSets:      RuleSetsView{Present: ruleSetsPresent(a.ruleSetDir)},
	}
	if !state.RuleSetsUpdatedAt.IsZero() {
		updatedAt := state.RuleSetsUpdatedAt
		view.RuleSets.UpdatedAt = &updatedAt
	}
	proxy := localProxyURL(state.Settings)
	proxy.User = nil
	view.ProxyURL = proxy.String()

	for _, sub := range state.Subscriptions {
		subView := SubscriptionView{
			ID: sub.ID, Name: sub.Name, URL: sub.URL, UserAgent: sub.UserAgent,
			IntervalHours: sub.IntervalHours, UpdateVia: sub.UpdateVia, UpdatedVia: sub.UpdatedVia,
			LastError: sub.LastError, Updating: updating[sub.ID], Nodes: []NodeView{},
		}
		if !sub.UpdatedAt.IsZero() {
			updatedAt := sub.UpdatedAt
			subView.UpdatedAt = &updatedAt
		}
		for _, node := range sub.Nodes {
			key := nodeKey{SubID: sub.ID, Tag: node.Tag}
			nodeView := NodeView{
				SubID: sub.ID, Tag: node.Tag, Type: node.Type, Server: node.Server,
				Port: node.Port, Helper: node.Helper, Disabled: excluded[key],
			}
			if result, ok := delays[key]; ok {
				delay := result.Delay
				nodeView.Delay = &delay
			}
			subView.Nodes = append(subView.Nodes, nodeView)
		}
		view.Subscriptions = append(view.Subscriptions, subView)
	}
	if view.Core.Running {
		lookup := func(tag string) *NodeRef {
			if key, ok := tagToKey[tag]; ok {
				return &NodeRef{SubID: key.SubID, Tag: key.Tag}
			}
			return nil
		}
		requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if group, err := a.core.Group(requestCtx, tagProxy); err == nil {
			view.Current = lookup(group.Now)
			if group.Now == tagAuto {
				if auto, err := a.core.Group(requestCtx, tagAuto); err == nil {
					view.AutoPick = lookup(auto.Now)
				}
			}
		}
	}
	return view
}

func (a *App) GeneratedConfig() ([]byte, error) {
	return os.ReadFile(a.configPath)
}

// jitterFor returns a stable pseudo-random number in [0,1) for one schedule slot, so
// the delay does not change every minute the scheduler looks at it.
func jitterFor(id string, attempted time.Time) float64 {
	sum := sha256.Sum256([]byte(id + "|" + attempted.UTC().Format(time.RFC3339Nano)))
	return float64(binary.BigEndian.Uint64(sum[:8])>>11) / float64(1<<53)
}
