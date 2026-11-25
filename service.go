package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	serviceName      = "sbpanel"
	unitPath         = "/etc/systemd/system/sbpanel.service"
	systemBinaryPath = "/usr/local/bin/sbpanel"
	systemConfigDir  = "/etc/sbpanel"
	systemDataDir    = "/var/lib/sbpanel"
	systemLogDir     = "/var/log/sbpanel"
	// With DynamicUser, systemd keeps the real state here and links /var/lib/sbpanel to it.
	systemPrivateDataDir = "/var/lib/private/sbpanel"

	// SBPANEL_SERVICE_MANAGER=direct forces the background-process mode even where
	// systemd runs.
	serviceManagerEnv = "SBPANEL_SERVICE_MANAGER"
	managerDirect     = "direct"

	maxLogBytes    = 10 << 20
	directStopWait = 15 * time.Second
	clockTicks     = 100 // USER_HZ; /proc start times are in these units on Linux
)

// layout says where one installation lives. root installs for the whole machine;
// anyone else gets a per-user installation under their home directory.
type layout struct {
	System    bool
	Binary    string
	ConfigDir string
	EnvFile   string
	DataDir   string
	LogFile   string
	PIDFile   string
}

func systemLayout() layout {
	return layout{
		System:    true,
		Binary:    systemBinaryPath,
		ConfigDir: systemConfigDir,
		EnvFile:   filepath.Join(systemConfigDir, "sbpanel.env"),
		DataDir:   systemDataDir,
		LogFile:   filepath.Join(systemLogDir, "sbpanel.log"),
		PIDFile:   filepath.Join(systemDataDir, "sbpanel.pid"),
	}
}

func userLayout() layout {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	data := filepath.Join(dataHome, "sbpanel")
	config := filepath.Join(configHome, "sbpanel")
	return layout{
		Binary:    filepath.Join(home, ".local", "bin", "sbpanel"),
		ConfigDir: config,
		EnvFile:   filepath.Join(config, "sbpanel.env"),
		DataDir:   data,
		LogFile:   filepath.Join(data, "sbpanel.log"),
		PIDFile:   filepath.Join(data, "sbpanel.pid"),
	}
}

// currentLayout is the installation the current user manages: the system-wide one
// for root, unless root had to fall back to a home-directory install (say, on a
// read-only root filesystem).
func currentLayout() layout {
	if os.Geteuid() != 0 {
		return userLayout()
	}
	system := systemLayout()
	if !system.installed() {
		if user := userLayout(); user.installed() {
			return user
		}
	}
	return system
}

// writable checks that every directory an installation writes to can be created
// and written.
func (l layout) writable() error {
	for _, dir := range []string{filepath.Dir(l.Binary), l.ConfigDir, l.DataDir, filepath.Dir(l.LogFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		probe, err := os.CreateTemp(dir, ".sbpanel-write-test-*")
		if err != nil {
			return err
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	return nil
}

// installed reports whether sbpanel is installed in this layout. The settings file
// alone does not count: a plain uninstall keeps it for the next install.
func (l layout) installed() bool {
	return fileExists(l.Binary) && (fileExists(l.EnvFile) || (l.System && fileExists(unitPath)))
}

func (l layout) singBoxMarker() string {
	return filepath.Join(l.ConfigDir, ".sing-box-installed-by-sbpanel")
}

// systemdUsable reports whether systemd is the running init system, the same test
// as sd_booted(3). Containers often ship systemctl while systemd is not running.
func systemdUsable() bool {
	if os.Getenv(serviceManagerEnv) == managerDirect {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	info, err := os.Stat("/run/systemd/system")
	if err != nil || !info.IsDir() {
		return false
	}
	// Container images built from a full system can carry /run/systemd/system and
	// systemctl while something else is process 1.
	if comm, err := os.ReadFile("/proc/1/comm"); err == nil && strings.TrimSpace(string(comm)) != "systemd" {
		return false
	}
	return true
}

// serviceManager runs the panel in the background: as a systemd service where
// possible, otherwise as a plain detached process.
type serviceManager interface {
	Describe() string
	Start() error
	Stop() error
	Restart() error
	Status() serviceStatus
	Logs(follow bool, lines int) error
	Autostart() (supported, enabled bool)
	SetAutostart(enabled bool) error
}

type serviceStatus struct {
	Running  bool
	Starting bool
	Failed   bool
	PID      int
	Since    time.Time
	MemoryMB float64
}

func managerFor(l layout) serviceManager {
	if l.System && systemdUsable() && fileExists(unitPath) {
		return systemdManager{}
	}
	return directManager{l: l}
}

// ---- systemd ----

type systemdManager struct{}

func (systemdManager) Describe() string { return "systemd 服务" }
func (systemdManager) Start() error     { return systemctl("start", serviceName) }
func (systemdManager) Stop() error      { return systemctl("stop", serviceName) }
func (systemdManager) Restart() error   { return systemctl("restart", serviceName) }

func (systemdManager) Status() serviceStatus {
	unit := queryUnit()
	status := serviceStatus{PID: unit.MainPID, Since: unit.Since, MemoryMB: unit.MemoryMB}
	switch unit.Active {
	case "active":
		status.Running = true
	case "activating", "reloading":
		status.Starting = true
	case "failed":
		status.Failed = true
	}
	return status
}

func (systemdManager) Logs(follow bool, lines int) error {
	args := []string{"-u", serviceName, "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-f")
	}
	return runForeground(follow, "journalctl", args...)
}

func (systemdManager) Autostart() (bool, bool) {
	return true, queryUnit().Enabled == "enabled"
}

func (systemdManager) SetAutostart(enabled bool) error {
	if enabled {
		return systemctl("enable", serviceName)
	}
	return systemctl("disable", serviceName)
}

func systemctl(args ...string) error {
	output, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), message)
	}
	return nil
}

type unitStatus struct {
	Active   string // active, inactive, failed, activating, ...
	Enabled  string // enabled, disabled, ...
	MainPID  int
	Since    time.Time
	MemoryMB float64
}

func queryUnit() unitStatus {
	output, _ := exec.Command("systemctl", "show", serviceName, "--no-pager",
		"-p", "ActiveState,UnitFileState,MainPID,ActiveEnterTimestampMonotonic,MemoryCurrent").Output()
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	status := unitStatus{Active: values["ActiveState"], Enabled: values["UnitFileState"]}
	status.MainPID, _ = strconv.Atoi(values["MainPID"])
	if memory, err := strconv.ParseFloat(values["MemoryCurrent"], 64); err == nil && memory < 1<<50 {
		status.MemoryMB = memory / (1 << 20)
	}
	if micros, err := strconv.ParseInt(values["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && micros > 0 {
		status.Since = bootTime().Add(time.Duration(micros) * time.Microsecond)
	}
	return status
}

// unitExitStatus is the exit code of the service's last run. Codes 200 and up are
// systemd's own: it could not set the process up (user, namespaces, directories...).
func unitExitStatus() int {
	output, err := exec.Command("systemctl", "show", serviceName, "-p", "ExecMainStatus", "--value").Output()
	if err != nil {
		return 0
	}
	code, _ := strconv.Atoi(strings.TrimSpace(string(output)))
	return code
}

// ---- plain background process ----

type directManager struct{ l layout }

func (m directManager) Describe() string {
	return "后台进程（" + autostartUnsupportedReason(m.l) + "）"
}

// pid returns the running panel's process ID, or 0. The PID file alone is not
// trusted: after a reboot or container restart the number may belong to anything.
// When it does not lead to the panel (deleted, stale, or the panel was started by
// hand with `nohup sbpanel serve &`), the process list is searched instead.
func (m directManager) pid() int {
	if data, err := os.ReadFile(m.l.PIDFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && isServeProcess(pid) {
			return pid
		}
	}
	return findServeProcess(m.l.DataDir)
}

// runningPanel is a `sbpanel serve` found in the process list: one started by
// sbpanel itself, by systemd, or by hand (`nohup sbpanel serve &`).
type runningPanel struct {
	PID       int
	DataDir   string // from --data or the environment; "" means the default
	Listen    string // likewise for the panel address
	OtherUser bool   // runs as someone else, so its default data directory is not ours
}

// uses reports whether the panel manages dataDir. A panel started without a data
// directory uses the default, which is ours when it runs as the same user.
func (p runningPanel) uses(dataDir string) bool {
	if p.DataDir == "" {
		return !p.OtherUser && resolvedDir(dataDir) == resolvedDir(defaultDataDir())
	}
	return resolvedDir(p.DataDir) == resolvedDir(dataDir)
}

func (p runningPanel) listenAddress() string {
	if p.Listen == "" {
		return defaultPanelListen
	}
	return p.Listen
}

// runningPanels lists the running panels. Of a supervisor and its worker only the
// supervisor is listed.
func runningPanels() []runningPanel {
	var panels []runningPanel
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() || !isServeProcess(pid) {
			continue
		}
		if fields := procStat(pid); len(fields) > 1 {
			if parent, err := strconv.Atoi(fields[1]); err == nil && isServeProcess(parent) {
				continue
			}
		}
		panel := runningPanel{PID: pid}
		if dir := serveSetting(pid, "data", "SBPANEL_DATA", "STATE_DIRECTORY"); dir != "" {
			panel.DataDir = procPath(pid, dir)
		}
		panel.Listen = serveSetting(pid, "listen", "SBPANEL_LISTEN")
		if info, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			if owner, ok := info.Sys().(*syscall.Stat_t); ok && int(owner.Uid) != os.Geteuid() {
				panel.OtherUser = true
			}
		}
		panels = append(panels, panel)
	}
	return panels
}

// findRunningPanel returns the running panel that manages dataDir, if any.
func findRunningPanel(dataDir string) *runningPanel {
	for _, panel := range runningPanels() {
		if panel.uses(dataDir) {
			return &panel
		}
	}
	return nil
}

func findServeProcess(dataDir string) int {
	if panel := findRunningPanel(dataDir); panel != nil {
		return panel.PID
	}
	return 0
}

// serveSetting reads a `sbpanel serve` option from its command line (--flag value or
// --flag=value), then from its environment, the same order serve itself uses.
func serveSetting(pid int, flagName string, envNames ...string) string {
	args := procCmdline(pid)
	for i := 2; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if name != flagName {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		return value
	}
	if environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		for _, name := range envNames {
			for _, variable := range strings.Split(string(environ), "\x00") {
				if value, ok := strings.CutPrefix(variable, name+"="); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	return ""
}

// resolvedDir resolves symlinks in a directory path, also when it does not exist.
func resolvedDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return resolvedFile(path)
}

// procPath resolves a path a process was given relative to its working directory.
func procPath(pid int, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		return filepath.Join(cwd, path)
	}
	return path
}

func (m directManager) Start() error {
	if m.pid() != 0 {
		return nil
	}
	if dataDirLocked(m.l.DataDir) {
		return fmt.Errorf("数据目录 %s 正被另一个 sbpanel 使用（是不是手动运行了 sbpanel serve？）", m.l.DataDir)
	}
	if err := os.MkdirAll(m.l.DataDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.l.LogFile), 0o755); err != nil {
		return err
	}
	rotateLog(m.l.LogFile)
	logFile, err := os.OpenFile(m.l.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	settings, err := readEnvFile(m.l.EnvFile)
	if err != nil {
		return err
	}

	command := exec.Command(m.l.Binary, "serve", "--data", m.l.DataDir)
	command.Env = append(os.Environ(), settings...) // the settings file wins
	command.Dir = "/"
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive this terminal
	if err := command.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	return writeFileAtomic(m.l.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func (m directManager) Stop() error {
	if pid := m.pid(); pid != 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM) // the panel stops sing-box itself
		if !waitProcessExit(pid, directStopWait) {
			// Start used setsid, so the process group holds the panel and its children.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitProcessExit(pid, 3*time.Second)
		}
	}
	killStaleCores(m.l.DataDir)
	_ = os.Remove(m.l.PIDFile)
	return nil
}

func (m directManager) Restart() error {
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}

func (m directManager) Status() serviceStatus {
	pid := m.pid()
	if pid == 0 {
		return serviceStatus{}
	}
	return serviceStatus{
		Running:  true,
		PID:      pid,
		Since:    processStartTime(pid),
		MemoryMB: float64(processTreeRSS(pid)) / (1 << 20),
	}
}

func (m directManager) Logs(follow bool, lines int) error {
	if !fileExists(m.l.LogFile) {
		return errors.New("还没有日志")
	}
	if _, err := exec.LookPath("tail"); err != nil {
		return printLastLines(m.l.LogFile, lines)
	}
	args := []string{"-n", strconv.Itoa(lines)}
	if follow {
		args = append(args, "-F")
	}
	return runForeground(follow, "tail", append(args, m.l.LogFile)...)
}

func (m directManager) Autostart() (bool, bool) { return false, false }

func (m directManager) SetAutostart(bool) error {
	return errors.New(autostartHint(m.l))
}

func autostartUnsupportedReason(l layout) string {
	switch {
	case !l.System:
		return "只对当前用户安装"
	case systemdUsable():
		return "systemd 没能启动服务，已改用后台进程"
	default:
		return "这个环境没有 systemd"
	}
}

func autostartHint(l layout) string {
	if l.System && systemdUsable() {
		return "现在是后台进程方式运行（安装时 systemd 没能启动服务），没有开机自启。\n" +
			"  排除问题后重新运行 sbpanel install，会自动改回 systemd 服务并开启开机自启。"
	}
	return autostartUnsupportedReason(l) + "，sbpanel 不能自己设置开机自启。\n" +
		"  机器或容器重启后运行一次 sbpanel start 即可；也可以把 sbpanel start 加到启动脚本里，\n" +
		"  或者让容器、supervisord 直接运行: " + l.Binary + " serve --data " + l.DataDir
}

// readEnvFile returns the KEY=VALUE lines of an EnvironmentFile-style file.
func readEnvFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var settings []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		settings = append(settings, key+"="+value)
	}
	return settings, scanner.Err()
}

func rotateLog(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
}

func printLastLines(path string, count int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	return nil
}

// runForeground runs a viewer such as journalctl or tail attached to the terminal.
// When following, Ctrl+C ends the viewer, not the menu that started it.
func runForeground(follow bool, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	err := command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && follow {
		return nil
	}
	return err
}

// dataDirLocked reports whether a running panel holds the data directory lock.
func dataDirLocked(dir string) bool {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// alignOwnership gives every file under dir the owner of dir itself. Running as root
// in background-process mode leaves root-owned files behind, which the unprivileged
// systemd service could not read after switching back.
func alignOwnership(dir string) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entryInfo, err := entry.Info(); err == nil {
			if stat, ok := entryInfo.Sys().(*syscall.Stat_t); ok && (stat.Uid != owner.Uid || stat.Gid != owner.Gid) {
				_ = os.Lchown(path, int(owner.Uid), int(owner.Gid))
			}
		}
		return nil
	})
}

// ---- /proc ----

func procCmdline(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

// procStat returns the /proc/<pid>/stat fields after the command name, so index 0
// is the state, 1 the parent PID and 19 the start time.
func procStat(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(text[end+1:])
}

func processAlive(pid int) bool {
	fields := procStat(pid)
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

func isServeProcess(pid int) bool {
	args := procCmdline(pid)
	return len(args) >= 2 && strings.HasPrefix(filepath.Base(args[0]), "sbpanel") && args[1] == "serve" && processAlive(pid)
}

func waitProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

func processStartTime(pid int) time.Time {
	fields := procStat(pid)
	if len(fields) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return bootTime().Add(time.Duration(ticks) * time.Second / clockTicks)
}

// processTreeRSS adds up the resident memory of pid and its descendants (the panel
// worker and sing-box).
func processTreeRSS(pid int) int64 {
	children := map[int][]int{}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		child, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if fields := procStat(child); len(fields) > 1 {
			if parent, err := strconv.Atoi(fields[1]); err == nil {
				children[parent] = append(children[parent], child)
			}
		}
	}
	var total int64
	queue := []int{pid}
	for len(queue) > 0 && len(queue) < 1000 {
		current := queue[0]
		queue = queue[1:]
		total += processRSS(current)
		queue = append(queue, children[current]...)
	}
	return total
}

func processRSS(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(fields[1], 10, 64)
	return pages * int64(os.Getpagesize())
}

// bootTime derives the boot time from /proc/uptime, so monotonic timestamps from
// systemd and /proc can be shown as durations.
func bootTime() time.Time {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Now()
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return time.Now()
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Now()
	}
	return time.Now().Add(-time.Duration(seconds * float64(time.Second)))
}

// killStaleCores ends sing-box processes still running this data directory's config.
// Without systemd nothing cleans them up when the panel is killed outright, and they
// would keep the proxy port busy.
func killStaleCores(dataDir string) int {
	config := resolvedFile(filepath.Join(dataDir, "config.json"))
	entries, _ := os.ReadDir("/proc")
	killed := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		args := procCmdline(pid)
		if len(args) < 2 || filepath.Base(args[0]) != "sing-box" || !coreArgsUseConfig(args, config) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if !waitProcessExit(pid, 5*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		killed++
	}
	return killed
}

func coreArgsUseConfig(args []string, config string) bool {
	for i := 0; i+1 < len(args); i++ {
		if (args[i] == "-c" || args[i] == "--config") && resolvedFile(args[i+1]) == config {
			return true
		}
	}
	return false
}

// resolvedFile resolves symlinks in the directory part, which works even when the
// file itself is gone (/var/lib/sbpanel is a link under systemd's DynamicUser).
func resolvedFile(path string) string {
	dir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	} else if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	return filepath.Join(dir, filepath.Base(path))
}
