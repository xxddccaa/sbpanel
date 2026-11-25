package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Updating sbpanel from the web panel.
//
// The service cannot replace its own program file: under systemd it runs as a
// throwaway user on a read-only /usr. So `sbpanel serve` is a small supervisor that
// runs the real panel (the "worker") as a child process. A version installed from
// the panel lives in <data>/panel/sbpanel, and the supervisor runs it instead of
// itself when it is newer. To switch versions the worker exits with
// panelRestartExitCode and the supervisor starts the worker again.
//
// A freshly installed version is on trial (the "pending" marker) until it reports
// that it has started. If it exits first, or stays silent for panelReadyTimeout, the
// supervisor puts the previous version back.
//
// The supervisor keeps running the code it was started with, so the protocol below
// (environment names, fd 3, exit code) must stay the same across versions.

const (
	panelWorkerEnv       = "SBPANEL_PANEL_WORKER" // "1" in the worker
	panelReadyFDEnv      = "SBPANEL_READY_FD"     // fd the worker writes "ready" to
	panelBaseVersionEnv  = "SBPANEL_BASE_VERSION" // and its version
	panelRestartExitCode = 75                     // worker asks to be started again
	panelReadyFD         = 3

	panelDownloadTimeout = 10 * time.Minute
	panelRestartDelay    = 1500 * time.Millisecond // lets the browser see "重启中"
)

var (
	errPanelRestart = errors.New("panel restart requested")

	// Long enough for app.Startup, which may wait for sing-box to come up.
	panelReadyTimeout = 90 * time.Second
)

func panelBinaryPath(dir string) string    { return filepath.Join(dir, "sbpanel") }
func panelPreviousPath(dir string) string  { return filepath.Join(dir, "sbpanel.previous") }
func panelCandidatePath(dir string) string { return filepath.Join(dir, "sbpanel.candidate") }
func panelArchivePath(dir string) string   { return filepath.Join(dir, "download.tar.gz") }
func panelPendingPath(dir string) string   { return filepath.Join(dir, "pending") }
func panelLastErrorPath(dir string) string { return filepath.Join(dir, "last-error") }

// sbpanelBinaryVersion runs `<path> version` and checks that the file is sbpanel.
func sbpanelBinaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kernelVersionTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("无法运行 %s: %w", filepath.Base(path), err)
	}
	line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	got, ok := strings.CutPrefix(line, "sbpanel ")
	if !ok || got == "" {
		return "", errors.New("这个文件不是 sbpanel")
	}
	return got, nil
}

// ---- supervisor ----

type panelSupervisor struct {
	dir          string // <data>/panel
	self         string // the supervisor's own program, run when nothing newer is installed
	selfVersion  string
	args         []string // `serve` arguments, passed on to the worker
	readyTimeout time.Duration
	signals      chan os.Signal
}

func supervisePanel(dataDir string, args []string) error {
	self := executablePath()
	if self == "" {
		return errors.New("找不到 sbpanel 程序自己的路径")
	}
	supervisor := &panelSupervisor{
		dir:          filepath.Join(dataDir, "panel"),
		self:         self,
		selfVersion:  version,
		args:         args,
		readyTimeout: panelReadyTimeout,
		signals:      make(chan os.Signal, 4),
	}
	signal.Notify(supervisor.signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(supervisor.signals)
	return supervisor.run()
}

// binary picks the program to run: the panel-installed one when it is newer.
func (s *panelSupervisor) binary() (path string, managed bool) {
	candidate := panelBinaryPath(s.dir)
	if !fileExists(candidate) {
		return s.self, false
	}
	got, err := sbpanelBinaryVersion(candidate)
	if err != nil {
		log.Printf("面板安装的 sbpanel 无法使用（%v），改用 %s", err, s.self)
		return s.self, false
	}
	if compareSemver(got, s.selfVersion) <= 0 {
		return s.self, false // `sbpanel update` installed something at least as new
	}
	return candidate, true
}

type workerResult struct {
	ready    bool
	stopped  bool // the supervisor was asked to stop
	timedOut bool // a version on trial did not report in time
	exitCode int
	err      error
}

func (s *panelSupervisor) run() error {
	for {
		binary, managed := s.binary()
		trial := managed && fileExists(panelPendingPath(s.dir))
		result := s.runWorker(binary, trial)
		switch {
		case result.stopped:
			return nil
		case trial && !result.ready:
			reason := fmt.Sprintf("新版本没有正常启动（%s），已换回原来的版本", s.describeExit(result))
			log.Print(reason)
			s.revertTrial(reason)
		case result.exitCode == panelRestartExitCode:
			log.Printf("重新启动面板")
		case result.err != nil:
			return result.err
		case result.exitCode != 0:
			return fmt.Errorf("面板退出，退出码 %d", result.exitCode)
		default:
			return nil
		}
	}
}

func (s *panelSupervisor) describeExit(result workerResult) string {
	switch {
	case result.timedOut:
		return fmt.Sprintf("%s 内没有启动完成", s.readyTimeout)
	case result.err != nil:
		return result.err.Error()
	default:
		return fmt.Sprintf("退出码 %d", result.exitCode)
	}
}

// revertTrial drops a version that failed its first start and restores the one it
// replaced (or, with none, falls back to the supervisor's own program).
func (s *panelSupervisor) revertTrial(reason string) {
	failed := panelBinaryPath(s.dir)
	_ = os.Remove(failed)
	if fileExists(panelPreviousPath(s.dir)) {
		if err := os.Rename(panelPreviousPath(s.dir), failed); err != nil {
			log.Printf("恢复上一个版本失败: %v", err)
		}
	}
	_ = os.Remove(panelPendingPath(s.dir))
	_ = os.WriteFile(panelLastErrorPath(s.dir), []byte(reason+"\n"), 0o600)
}

func (s *panelSupervisor) runWorker(binary string, trial bool) workerResult {
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return workerResult{err: err}
	}
	defer readyRead.Close()

	command := exec.Command(binary, append([]string{"serve"}, s.args...)...)
	command.Env = append(os.Environ(),
		panelWorkerEnv+"=1",
		panelReadyFDEnv+"="+strconv.Itoa(panelReadyFD),
		panelBaseVersionEnv+"="+s.selfVersion,
	)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.ExtraFiles = []*os.File{readyWrite} // becomes fd 3
	setParentDeathSignal(command)
	if err := command.Start(); err != nil {
		readyWrite.Close()
		return workerResult{err: fmt.Errorf("启动 %s 失败: %w", binary, err)}
	}
	readyWrite.Close()

	ready := make(chan bool, 1) // the reader's verdict: true once "ready" arrived
	go func() {
		line, _ := bufio.NewReader(readyRead).ReadString('\n')
		ready <- strings.TrimSpace(line) == "ready"
	}()
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	var timeout <-chan time.Time
	if trial {
		timer := time.NewTimer(s.readyTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	result := workerResult{exitCode: -1}
	markReady := func(ok bool) {
		ready = nil
		if !ok {
			return
		}
		result.ready = true
		timeout = nil
		if trial {
			_ = os.Remove(panelPendingPath(s.dir)) // the new version has proven itself
		}
	}
	for {
		select {
		case ok := <-ready:
			markReady(ok)
		case <-timeout:
			log.Printf("新版本 %s 内没有启动完成，结束它", s.readyTimeout)
			result.timedOut = true
			_ = command.Process.Signal(syscall.SIGTERM)
			timeout = nil
			go func() {
				time.Sleep(15 * time.Second)
				_ = command.Process.Kill()
			}()
		case sig := <-s.signals:
			result.stopped = true
			_ = command.Process.Signal(sig)
		case err := <-exited:
			if ready != nil && !result.timedOut {
				// "ready" may still sit in the pipe; the pipe closed when the worker exited.
				select {
				case ok := <-ready:
					markReady(ok)
				case <-time.After(2 * time.Second):
				}
			}
			var exitErr *exec.ExitError
			switch {
			case err == nil:
				result.exitCode = 0
			case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
				result.exitCode = exitErr.ExitCode()
			case errors.As(err, &exitErr):
				result.exitCode = -1 // killed by a signal
				if !result.stopped && !result.timedOut {
					result.err = fmt.Errorf("面板被信号结束: %v", err)
				}
			default:
				result.err = err
			}
			return result
		}
	}
}

// ---- worker side ----

// panelReadySignal tells the supervisor that the worker is up.
type panelReadySignal struct{ file *os.File }

// takePanelReadySignal claims the pipe from the supervisor. It is marked
// close-on-exec so sing-box, started later, does not hold it open.
func takePanelReadySignal() *panelReadySignal {
	fd, err := strconv.Atoi(os.Getenv(panelReadyFDEnv))
	if err != nil || fd < 3 {
		return &panelReadySignal{}
	}
	syscall.CloseOnExec(fd)
	return &panelReadySignal{file: os.NewFile(uintptr(fd), "sbpanel-ready")}
}

func (r *panelReadySignal) supervised() bool { return r.file != nil }

func (r *panelReadySignal) send() error {
	if r.file == nil {
		return nil
	}
	_, err := r.file.WriteString("ready\n")
	closeErr := r.file.Close()
	r.file = nil
	return errors.Join(err, closeErr)
}

type PanelRelease struct {
	Tag       string    `json:"tag"`
	Version   string    `json:"version"`
	URL       string    `json:"url"`
	CheckedAt time.Time `json:"checkedAt"`
	Via       string    `json:"via"`
}

// PanelJob reports a running or finished download, like KernelJob.
type PanelJob = KernelJob

// PanelUpdater is the worker's half: it checks GitHub, downloads and installs a
// release into dir, and asks the supervisor to restart.
type PanelUpdater struct {
	dir         string
	ctx         context.Context
	restart     func()
	supervised  bool
	running     bool // this worker runs the panel-installed program
	baseVersion string

	mu      sync.Mutex
	release *PanelRelease
	job     PanelJob
}

func newPanelUpdater(ctx context.Context, dir string, supervised bool, restart func()) *PanelUpdater {
	baseVersion := os.Getenv(panelBaseVersionEnv)
	if baseVersion == "" {
		baseVersion = version
	}
	return &PanelUpdater{
		dir:         dir,
		ctx:         ctx,
		restart:     restart,
		supervised:  supervised,
		running:     sameFile(executablePath(), panelBinaryPath(dir)),
		baseVersion: baseVersion,
	}
}

// executablePath is this process's program. Go reads it once at startup, so it
// keeps naming the original path after the file is renamed.
func executablePath() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		return resolved
	}
	return self
}

// sameFile compares paths after resolving symlinks: with systemd's DynamicUser the
// data directory /var/lib/sbpanel is a link into /var/lib/private.
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	} else if absolute, err := filepath.Abs(b); err == nil {
		b = absolute
	}
	return a == b
}

// managed reports whether this worker runs the panel-installed program.
func (p *PanelUpdater) managed() bool { return p.running }

func (p *PanelUpdater) updateJob(change func(*PanelJob)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.job)
}

type PanelView struct {
	Version     string        `json:"version"`
	Managed     bool          `json:"managed"`
	BaseVersion string        `json:"baseVersion,omitempty"`
	Previous    string        `json:"previous,omitempty"` // version a rollback goes back to
	Release     *PanelRelease `json:"release,omitempty"`
	Job         PanelJob      `json:"job"`
	LastError   string        `json:"lastError,omitempty"`
	Supported   bool          `json:"supported"`
	Unsupported string        `json:"unsupported,omitempty"`
}

func (p *PanelUpdater) View() PanelView {
	view := PanelView{Version: version, Managed: p.managed(), BaseVersion: p.baseVersion}
	if view.Managed {
		view.Previous = p.baseVersion
		if fileExists(panelPreviousPath(p.dir)) {
			if previous, err := sbpanelBinaryVersion(panelPreviousPath(p.dir)); err == nil && compareSemver(previous, p.baseVersion) > 0 {
				view.Previous = previous
			}
		}
	}
	if data, err := os.ReadFile(panelLastErrorPath(p.dir)); err == nil {
		view.LastError = strings.TrimSpace(string(data))
	}
	if _, err := sbpanelAssetName(); err != nil {
		view.Unsupported = "这台机器的系统或架构暂不支持在面板里升级"
	} else if !p.supervised {
		view.Unsupported = "面板不是通过 sbpanel serve 启动的，不能在网页里升级"
	}
	view.Supported = view.Unsupported == ""
	p.mu.Lock()
	if p.release != nil {
		release := *p.release
		view.Release = &release
	}
	view.Job = p.job
	p.mu.Unlock()
	return view
}

// CheckPanelRelease looks up the newest sbpanel release, through the proxy first.
func (a *App) CheckPanelRelease(ctx context.Context) error {
	tag, via, err := latestSbpanelTag(ctx, a.fetchWithFallback)
	if err != nil {
		return err
	}
	a.panel.mu.Lock()
	a.panel.release = &PanelRelease{
		Tag:       tag,
		Version:   strings.TrimPrefix(tag, "v"),
		URL:       sbpanelWebURL + "/releases/tag/" + tag,
		CheckedAt: time.Now(),
		Via:       via,
	}
	a.panel.mu.Unlock()
	return nil
}

// ---- release lookup without the GitHub API ----
//
// The API allows 60 anonymous requests an hour per IP address. Cloud machines and
// notebooks (DSW and the like) share their address with many others and often find
// the allowance used up. github.com itself has no such limit, so the newest tag is
// read from where /releases/latest redirects to, and the SHA-256 comes from the
// sha256sums.txt every release carries.

// fetchFunc fetches a URL; it matches App.fetchWithFallback.
type fetchFunc func(ctx context.Context, rawURL string, header http.Header, timeout time.Duration,
	consume func(*http.Response) error) (via string, err error)

var (
	sbpanelWebURL        = projectURL // replaced in tests
	releaseTagURLPattern = regexp.MustCompile(`/releases/tag/([^/?#]+)$`)
)

func sbpanelDownloadURL(tag, name string) string {
	return sbpanelWebURL + "/releases/download/" + tag + "/" + name
}

func plainHeader() http.Header { return http.Header{"User-Agent": {"sbpanel/" + version}} }

// latestSbpanelTag returns the newest release's tag.
func latestSbpanelTag(ctx context.Context, fetch fetchFunc) (tag, via string, err error) {
	via, err = fetch(ctx, sbpanelWebURL+"/releases/latest", plainHeader(), releaseAPITimeout, func(response *http.Response) error {
		match := releaseTagURLPattern.FindStringSubmatch(response.Request.URL.Path)
		if match == nil {
			return errors.New("GitHub 没有跳转到最新版本")
		}
		tag = match[1]
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("获取 sbpanel 最新版本失败: %w", err)
	}
	if !releaseTagPattern.MatchString(tag) {
		return "", "", fmt.Errorf("最新版本号 %q 格式不对", tag)
	}
	return tag, via, nil
}

// sbpanelReleaseAsset returns this machine's package of a release, with the SHA-256
// from the release's sha256sums.txt.
func sbpanelReleaseAsset(ctx context.Context, fetch fetchFunc, tag string) (*githubAsset, error) {
	name, err := sbpanelAssetName()
	if err != nil {
		return nil, err
	}
	var sums map[string]string
	_, err = fetch(ctx, sbpanelDownloadURL(tag, "sha256sums.txt"), plainHeader(), releaseAPITimeout, func(response *http.Response) error {
		data, err := readLimited(response.Body, 1<<20)
		if err == nil {
			sums = parseSHA256Sums(string(data))
		}
		return err
	})
	var status *httpStatusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil, fmt.Errorf("版本 %s 不存在", tag)
	}
	if err != nil {
		return nil, fmt.Errorf("下载 %s 的 sha256sums.txt 失败: %w", tag, err)
	}
	if sums[name] == "" {
		return nil, fmt.Errorf("版本 %s 的 sha256sums.txt 里没有 %s，无法校验，已取消", tag, name)
	}
	return &githubAsset{Name: name, BrowserDownloadURL: sbpanelDownloadURL(tag, name), Digest: "sha256:" + sums[name]}, nil
}

// StartPanelInstall downloads a release in the background, then restarts into it.
func (a *App) StartPanelInstall(tag string) error {
	p := a.panel
	if view := p.View(); !view.Supported {
		return errors.New(view.Unsupported)
	}
	if !releaseTagPattern.MatchString(tag) {
		return errors.New("版本号格式不对")
	}
	if compareSemver(tag, version) <= 0 {
		return fmt.Errorf("%s 不比现在的 %s 新", tag, version)
	}
	p.mu.Lock()
	if p.job.Running {
		p.mu.Unlock()
		return errors.New("已经有一个升级任务在进行")
	}
	p.job = PanelJob{Running: true, Tag: tag, Stage: "准备中"}
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(p.ctx, panelDownloadTimeout)
		defer cancel()
		message, err := a.installPanel(ctx, tag)
		if err != nil {
			now := time.Now()
			p.updateJob(func(job *PanelJob) {
				*job = PanelJob{Tag: tag, Error: err.Error(), FinishedAt: &now}
			})
			log.Printf("升级 sbpanel 到 %s 失败: %v", tag, err)
			return
		}
		log.Print(message)
		p.updateJob(func(job *PanelJob) { job.Stage = "重启中"; job.Message = message })
		time.Sleep(panelRestartDelay)
		p.restart()
	}()
	return nil
}

func (a *App) installPanel(ctx context.Context, tag string) (string, error) {
	p := a.panel
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return "", err
	}
	p.updateJob(func(job *PanelJob) { job.Stage = "获取校验值" })
	asset, err := sbpanelReleaseAsset(ctx, a.fetchWithFallback, tag)
	if err != nil {
		return "", err
	}
	want := strings.TrimPrefix(asset.Digest, "sha256:")

	p.updateJob(func(job *PanelJob) { job.Stage = "下载中" })
	archive := panelArchivePath(p.dir)
	defer os.Remove(archive)
	via, err := a.fetchWithFallback(ctx, asset.BrowserDownloadURL, plainHeader(),
		panelDownloadTimeout, func(response *http.Response) error {
			if response.ContentLength > 0 {
				p.updateJob(func(job *PanelJob) { job.Total = response.ContentLength })
			}
			return downloadVerified(response.Body, archive, want, func(done int64) {
				p.updateJob(func(job *PanelJob) { job.Done = done })
			})
		})
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}

	p.updateJob(func(job *PanelJob) { job.Stage = "解压与校验" })
	candidate := panelCandidatePath(p.dir)
	defer os.Remove(candidate)
	if err := extractFromTarball(archive, "sbpanel", candidate, 0o700); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}
	got, err := sbpanelBinaryVersion(candidate)
	if err != nil {
		return "", err
	}
	if got != tag {
		return "", fmt.Errorf("安装包里的版本是 %s，和 %s 不一致", got, tag)
	}

	p.updateJob(func(job *PanelJob) { job.Stage = "替换程序" })
	if err := p.swapIn(candidate); err != nil {
		return "", fmt.Errorf("替换程序失败: %w", err)
	}
	return fmt.Sprintf("sbpanel %s 已下载（通过%s，SHA-256 校验通过），正在重启", tag, via), nil
}

// swapIn makes candidate the panel-installed program and marks it as on trial. The
// version running now becomes the rollback target if it was panel-installed too.
func (p *PanelUpdater) swapIn(candidate string) error {
	current, previous := panelBinaryPath(p.dir), panelPreviousPath(p.dir)
	if p.managed() {
		if err := os.Rename(current, previous); err != nil {
			return err
		}
	} else {
		// Running the supervisor's own program: older panel-installed files are stale.
		_ = os.Remove(current)
		_ = os.Remove(previous)
	}
	if err := os.Rename(candidate, current); err != nil {
		if p.managed() {
			_ = os.Rename(previous, current)
		}
		return err
	}
	_ = os.Remove(panelLastErrorPath(p.dir))
	return os.WriteFile(panelPendingPath(p.dir), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

// RollbackPanel goes back to the version that ran before the panel-installed one.
func (a *App) RollbackPanel() error {
	p := a.panel
	if !p.supervised {
		return errors.New("面板不是通过 sbpanel serve 启动的，不能在网页里切换版本")
	}
	if !p.managed() {
		return errors.New("现在用的就是安装时的版本，没有可以回退的")
	}
	p.mu.Lock()
	if p.job.Running {
		p.mu.Unlock()
		return errors.New("升级任务还在进行")
	}
	p.job = PanelJob{Running: true, Stage: "重启中"}
	p.mu.Unlock()

	current, previous := panelBinaryPath(p.dir), panelPreviousPath(p.dir)
	var err error
	if fileExists(previous) {
		err = os.Rename(previous, current)
	} else {
		err = os.Remove(current)
	}
	if err != nil {
		p.updateJob(func(job *PanelJob) { *job = PanelJob{Error: err.Error()} })
		return fmt.Errorf("回退失败: %w", err)
	}
	_ = os.Remove(panelPendingPath(p.dir))
	_ = os.Remove(panelLastErrorPath(p.dir))
	log.Printf("回退 sbpanel，正在重启")
	go func() {
		time.Sleep(panelRestartDelay)
		p.restart()
	}()
	return nil
}
