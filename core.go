package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	logBufferLines    = 500
	coreStartTimeout  = 15 * time.Second
	coreStopTimeout   = 10 * time.Second
	restartBackoffMin = 2 * time.Second
	restartBackoffMax = time.Minute
	healthyRunTime    = 30 * time.Second
	coreStayUpTime    = time.Second
)

// Core supervises one sing-box child process and talks to its clash API.
type Core struct {
	binary     func() string // resolved at every start, so a kernel switch applies on restart
	configPath string
	workDir    string
	clashAddr  string
	secret     string

	mu         sync.Mutex
	command    *exec.Cmd
	done       chan struct{}
	wanted     bool
	startedAt  time.Time
	lastExit   string
	restartGen int

	logs *lineBuffer
}

func NewCore(binary func() string, configPath, workDir, clashAddr, secret string) *Core {
	return &Core{
		binary:     binary,
		configPath: configPath,
		workDir:    workDir,
		clashAddr:  clashAddr,
		secret:     secret,
		logs:       newLineBuffer(logBufferLines),
	}
}

type CoreStatus struct {
	Running   bool       `json:"running"`
	Wanted    bool       `json:"wanted"`
	Pid       int        `json:"pid,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	LastExit  string     `json:"lastExit,omitempty"`
}

func (c *Core) Status() CoreStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := CoreStatus{Wanted: c.wanted, LastExit: c.lastExit}
	if c.command != nil {
		status.Running = true
		status.Pid = c.command.Process.Pid
		startedAt := c.startedAt
		status.StartedAt = &startedAt
	}
	return status
}

func (c *Core) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.command != nil
}

// Start launches sing-box and waits until its API answers. It is a no-op when running.
func (c *Core) Start(ctx context.Context) error {
	c.mu.Lock()
	c.wanted = true
	c.restartGen++
	if c.command != nil {
		c.mu.Unlock()
		return nil
	}
	err := c.spawnLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.waitReadyOrStop(ctx)
}

func (c *Core) Stop() error {
	c.mu.Lock()
	c.wanted = false
	c.restartGen++
	c.mu.Unlock()
	return c.terminate()
}

// Restart stops the running process (if any) and starts it again with the current config.
func (c *Core) Restart(ctx context.Context) error {
	c.mu.Lock()
	c.wanted = true
	c.restartGen++
	c.mu.Unlock()
	if err := c.terminate(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.command != nil {
		c.mu.Unlock()
		return errors.New("sing-box 没有停下来")
	}
	err := c.spawnLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.waitReadyOrStop(ctx)
}

func (c *Core) terminate() error {
	c.mu.Lock()
	command, done := c.command, c.done
	c.mu.Unlock()
	if command == nil {
		return nil
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(coreStopTimeout):
	}
	_ = command.Process.Kill()
	select {
	case <-done:
		return nil
	case <-time.After(coreStopTimeout):
		return errors.New("无法停止 sing-box")
	}
}

func (c *Core) spawnLocked() error {
	if _, err := os.Stat(c.configPath); err != nil {
		return fmt.Errorf("找不到配置文件: %w", err)
	}
	binary := c.binary()
	if binary == "" {
		return errors.New("找不到 sing-box 可执行文件")
	}
	command := exec.Command(binary, "--disable-color", "run", "-c", c.configPath, "-D", c.workDir)
	command.Dir = c.workDir
	command.Stdout = c.logs
	command.Stderr = c.logs
	// Own process group: a Ctrl+C in the terminal reaches only sbpanel, which then stops
	// sing-box itself. Under systemd the whole cgroup is cleaned up if sbpanel dies.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("启动 sing-box 失败: %w", err)
	}
	c.logs.Write([]byte(fmt.Sprintf("[sbpanel] sing-box 已启动 (pid %d)\n", command.Process.Pid)))
	done := make(chan struct{})
	c.command = command
	c.done = done
	c.startedAt = time.Now()
	c.lastExit = ""
	generation := c.restartGen
	go c.wait(command, done, generation)
	return nil
}

func (c *Core) wait(command *exec.Cmd, done chan struct{}, generation int) {
	err := command.Wait()
	exit := "正常退出"
	if err != nil {
		exit = err.Error()
	}
	c.logs.Write([]byte(fmt.Sprintf("[sbpanel] sing-box 已退出: %s\n", exit)))

	c.mu.Lock()
	ranFor := time.Since(c.startedAt)
	c.command = nil
	c.done = nil
	c.lastExit = exit
	shouldRestart := c.wanted && c.restartGen == generation
	c.mu.Unlock()
	close(done)

	if shouldRestart {
		go c.restartAfterCrash(generation, ranFor)
	}
}

// restartAfterCrash restarts an unexpectedly exited core, backing off while it keeps crashing.
func (c *Core) restartAfterCrash(generation int, ranFor time.Duration) {
	delay := restartBackoffMin
	for {
		if ranFor >= healthyRunTime {
			delay = restartBackoffMin
		}
		time.Sleep(delay)
		c.mu.Lock()
		if !c.wanted || c.restartGen != generation || c.command != nil {
			c.mu.Unlock()
			return
		}
		c.logs.Write([]byte("[sbpanel] sing-box 意外退出，自动重启\n"))
		err := c.spawnLocked()
		c.mu.Unlock()
		if err == nil {
			return
		}
		c.logs.Write([]byte(fmt.Sprintf("[sbpanel] %s\n", err)))
		delay = min(delay*2, restartBackoffMax)
		ranFor = 0
	}
}

// waitReadyOrStop leaves the core stopped (and not auto-restarting) if it fails to come up,
// so the caller can roll back to the previous config instead of fighting a restart loop.
func (c *Core) waitReadyOrStop(ctx context.Context) error {
	err := c.waitReady(ctx)
	if err != nil {
		_ = c.Stop()
	}
	return err
}

func (c *Core) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, coreStartTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !c.Running() {
			return fmt.Errorf("sing-box 启动后立即退出了，请看日志: %s", c.lastLogLine())
		}
		var version map[string]any
		if err := c.clashRequest(ctx, http.MethodGet, "/version", nil, &version); err == nil {
			return c.waitStayUp(ctx)
		}
		select {
		case <-ctx.Done():
			return startWaitError(ctx)
		case <-ticker.C:
		}
	}
}

// waitStayUp only reports success once the process has survived a short window, so a
// crash right after the API came up still counts as a failed start (and gets rolled back).
func (c *Core) waitStayUp(ctx context.Context) error {
	c.mu.Lock()
	remaining := coreStayUpTime - time.Since(c.startedAt)
	c.mu.Unlock()
	if remaining > 0 {
		select {
		case <-ctx.Done():
			return startWaitError(ctx)
		case <-time.After(remaining):
		}
	}
	if !c.Running() {
		return fmt.Errorf("sing-box 启动后立即退出了，请看日志: %s", c.lastLogLine())
	}
	return nil
}

func startWaitError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return errors.New("sbpanel 正在退出，启动 sing-box 的过程被中断")
	}
	return errors.New("等待 sing-box 启动超时")
}

func (c *Core) lastLogLine() string {
	lines := c.logs.Lines()
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "[sbpanel]") {
			return lines[i]
		}
	}
	return "(没有输出)"
}

var errCoreNotRunning = errors.New("sing-box 没有在运行")

func (c *Core) clashRequest(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+c.clashAddr+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}}
	response, err := client.Do(request)
	if err != nil {
		return errCoreNotRunning
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode >= 300 {
		var message struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &message)
		if message.Message == "" {
			message.Message = response.Status
		}
		return &clashError{status: response.StatusCode, message: message.Message}
	}
	if result != nil && len(data) > 0 {
		return json.Unmarshal(data, result)
	}
	return nil
}

type clashError struct {
	status  int
	message string
}

func (e *clashError) Error() string { return e.message }

func (c *Core) SelectNode(ctx context.Context, tag string) error {
	return c.clashRequest(ctx, http.MethodPut, "/proxies/"+url.PathEscape(tagProxy), map[string]string{"name": tag}, nil)
}

// Delay measures one node through sing-box. A negative value means the test failed.
func (c *Core) Delay(ctx context.Context, tag, testURL string, timeout time.Duration) (int, error) {
	query := url.Values{}
	query.Set("url", testURL)
	query.Set("timeout", fmt.Sprint(timeout.Milliseconds()))
	var result struct {
		Delay int `json:"delay"`
	}
	err := c.clashRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(tag)+"/delay?"+query.Encode(), nil, &result)
	var apiErr *clashError
	if errors.As(err, &apiErr) && apiErr.status != http.StatusNotFound && apiErr.status != http.StatusUnauthorized {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	return result.Delay, nil
}

type ProxyGroup struct {
	Now string   `json:"now"`
	All []string `json:"all"`
}

func (c *Core) Group(ctx context.Context, tag string) (*ProxyGroup, error) {
	group := &ProxyGroup{}
	if err := c.clashRequest(ctx, http.MethodGet, "/proxies/"+url.PathEscape(tag), nil, group); err != nil {
		return nil, err
	}
	return group, nil
}

func (c *Core) Logs() []string { return c.logs.Lines() }

// lineBuffer is an io.Writer that keeps the last N complete lines.
type lineBuffer struct {
	mu      sync.Mutex
	limit   int
	lines   []string
	partial []byte
}

func newLineBuffer(limit int) *lineBuffer { return &lineBuffer{limit: limit} }

func (b *lineBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, data...)
	for {
		index := bytes.IndexByte(b.partial, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(stripANSI(string(b.partial[:index])), "\r")
		b.partial = b.partial[index+1:]
		b.lines = append(b.lines, line)
		if len(b.lines) > b.limit {
			b.lines = b.lines[len(b.lines)-b.limit:]
		}
	}
	if len(b.partial) > 64<<10 {
		b.partial = b.partial[:0]
	}
	return len(data), nil
}

func (b *lineBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}
