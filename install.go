package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	githubRepo         = "xxddccaa/sbpanel"
	cliDownloadTimeout = 15 * time.Minute
	cliAPITimeout      = 30 * time.Second
)

const unitTemplate = `[Unit]
Description=sbpanel - web panel for sing-box subscriptions
Documentation=` + projectURL + `
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=-%s
ExecStart=%s serve
Restart=on-failure
RestartSec=5
%s
[Install]
WantedBy=multi-user.target
`

// The hardened unit runs as a throwaway user with state in /var/lib/sbpanel. Some
// systems (old kernels, nested containers) cannot set that sandbox up; the plain
// unit is the fallback.
const unitSandbox = `
DynamicUser=yes
StateDirectory=sbpanel
StateDirectoryMode=0700
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
LimitNOFILE=65535
`

const unitPlain = `
Environment=SBPANEL_DATA=` + systemDataDir + `
LimitNOFILE=65535
`

func unitContent(l layout, sandbox bool) string {
	extra := unitPlain
	if sandbox {
		extra = unitSandbox
	}
	return fmt.Sprintf(unitTemplate, l.EnvFile, l.Binary, extra)
}

func writeUnit(l layout) error {
	return writeUnitVariant(l, !unitIsPlain())
}

func writeUnitVariant(l layout, sandbox bool) error {
	return writeFileAtomic(unitPath, []byte(unitContent(l, sandbox)), 0o644)
}

func unitIsPlain() bool {
	data, err := os.ReadFile(unitPath)
	return err == nil && !strings.Contains(string(data), "DynamicUser=yes")
}

// ---- install ----

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	listenFlag := flags.String("listen", "", "面板监听地址，默认 "+defaultPanelListen+"（已安装时保持原来的设置）")
	portFlag := flags.Int("port", 0, "只修改面板端口")
	skipSingBox := flags.Bool("skip-sing-box", false, "不要自动安装 sing-box")
	flags.Parse(args)

	l := currentLayout()
	if err := l.writable(); err != nil {
		if !l.System {
			return fmt.Errorf("没有地方可以安装: %w", err)
		}
		printWarn("系统目录不能写（%v），改为装到 %s", err, filepath.Dir(filepath.Dir(userLayout().Binary)))
		l = userLayout()
		if err := l.writable(); err != nil {
			return fmt.Errorf("没有地方可以安装: %w", err)
		}
	}
	upgrading := l.installed()
	listen, err := chooseListen(l, *listenFlag, *portFlag)
	if err != nil {
		return err
	}

	if err := installSelf(l); err != nil {
		return fmt.Errorf("复制程序失败: %w", err)
	}
	if !*skipSingBox {
		if path, _ := activeKernelPath(l); path == "" {
			printStep("没有找到 sing-box，下载官方最新稳定版…")
			if err := installSingBox(l); err != nil {
				printWarn("sing-box 安装失败: %v", err)
				printWarn("可以稍后在网页面板“设置 → sing-box 内核”里安装，或者设置 GH_PROXY 后重新运行 sbpanel install")
			}
		}
	}
	if err := writeListenToEnvFile(l.EnvFile, listen); err != nil {
		return err
	}
	manager, err := startInstalled(l, listen)
	if err != nil {
		return err
	}

	reportCore(l)
	printInstallSummary(l, manager, listen, upgrading)
	return nil
}

// chooseListen keeps the configured panel address unless asked to change it, and
// makes sure the port is free or held by the panel this install replaces.
func chooseListen(l layout, listenFlag string, portFlag int) (string, error) {
	listen := l.panelListen()
	running := findRunningPanel(l.DataDir)
	if running != nil && !fileExists(l.EnvFile) {
		// Started by hand before sbpanel could run without systemd: keep its address.
		listen = running.listenAddress()
	}
	if listenFlag != "" {
		listen = listenFlag
	}
	if portFlag != 0 {
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			host = "0.0.0.0"
		}
		listen = net.JoinHostPort(host, strconv.Itoa(portFlag))
	}
	if err := validateListen(listen); err != nil {
		return "", err
	}
	// startInstalled stops that panel before starting the new one.
	ours := running != nil && samePort(running.listenAddress(), listen)
	if !ours && !canListen(listen) {
		for _, other := range runningPanels() {
			if !other.uses(l.DataDir) && samePort(other.listenAddress(), listen) {
				return "", otherPanelError(l, other, listen)
			}
		}
		return "", fmt.Errorf("面板端口 %s 已经被别的程序占用了，换一个：sbpanel install --port 8092", listen)
	}
	if running != nil && !l.installed() {
		printStep("发现手动运行的 sbpanel（PID %d），换成新安装的服务", running.PID)
	}
	return listen, nil
}

func samePort(a, b string) bool {
	_, portA, errA := net.SplitHostPort(a)
	_, portB, errB := net.SplitHostPort(b)
	return errA == nil && errB == nil && portA == portB
}

func otherPanelError(l layout, other runningPanel, listen string) error {
	owner := "另一个 sbpanel"
	if other.OtherUser {
		owner = "另一个用户运行的 sbpanel"
	}
	dir := other.DataDir
	if dir == "" {
		dir = "它的默认目录"
	}
	return fmt.Errorf("面板端口 %s 被%s占用了（PID %d，数据目录 %s，和这次安装用的 %s 不同）。\n"+
		"  要继续用它的订阅和设置：先停掉它（kill %d），把 %s 里的文件复制到 %s，再重新安装。\n"+
		"  或者换一个端口安装：sbpanel install --port 8092",
		listen, owner, other.PID, dir, l.DataDir, other.PID, dir, l.DataDir)
}

// startInstalled (re)starts the panel with whatever this machine supports, falling
// back step by step: sandboxed systemd unit, plain systemd unit, background process.
func startInstalled(l layout, listen string) (serviceManager, error) {
	if l.System && systemdUsable() {
		manager, err := startWithSystemd(l, listen)
		if err == nil {
			return manager, nil
		}
		printWarn("没能用 systemd 启动（%v），改用后台进程运行", err)
		removeUnit()
	} else if l.System && fileExists(unitPath) {
		removeUnit() // systemd is gone (say, the disk moved into a container)
	}

	manager := directManager{l: l}
	printStep("以后台进程启动…")
	if err := manager.Restart(); err != nil {
		return nil, err
	}
	if !waitForPanel(listen, panelStartTimeout) {
		fmt.Println()
		_ = manager.Logs(false, 20)
		return nil, errors.New("面板没有正常启动，上面是最近的日志")
	}
	return manager, nil
}

func startWithSystemd(l layout, listen string) (serviceManager, error) {
	// Taking over from a background process started before systemd was available.
	_ = directManager{l: l}.Stop()
	alignOwnership(l.DataDir)

	for _, sandbox := range []bool{!unitIsPlain(), false} {
		if err := writeUnitVariant(l, sandbox); err != nil {
			return nil, err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return nil, err
		}
		_ = systemctl("reset-failed", serviceName)
		printStep("启动 systemd 服务…")
		if err := systemctl("restart", serviceName); err == nil && waitForPanel(listen, panelStartTimeout) {
			if err := systemctl("enable", serviceName); err != nil {
				printWarn("设置开机自启失败: %v", err)
			}
			return systemdManager{}, nil
		}
		if !sandbox || unitExitStatus() < 200 {
			break // the panel itself failed; a plainer unit would not help
		}
		printWarn("这台机器不支持 systemd 的沙箱选项，改用普通方式运行")
	}
	_ = systemctl("stop", serviceName)
	return nil, errors.New("服务没有正常启动，可以运行 journalctl -u sbpanel 查看原因")
}

func removeUnit() {
	if !fileExists(unitPath) {
		return
	}
	if systemdUsable() {
		_ = systemctl("disable", "--now", serviceName)
	}
	_ = os.Remove(unitPath)
	if systemdUsable() {
		_ = systemctl("daemon-reload")
		_ = systemctl("reset-failed", serviceName)
	}
}

func printInstallSummary(l layout, manager serviceManager, listen string, upgrading bool) {
	fmt.Println()
	verb := "已安装并启动"
	if upgrading {
		verb = "已更新并重新启动"
	}
	printOK("sbpanel %s %s", version, verb)
	row := func(label, value string) { fmt.Printf("  %s  %s\n", padDisplay(label, 8), value) }
	row("运行方式", manager.Describe())
	for i, link := range panelURLs(listen) {
		label := ""
		if i == 0 {
			label = "网页面板"
		}
		row(label, colorBold(link))
	}
	row("访问方式", "免密码，直接打开（仅限可信网络，勿直接暴露到公网）")
	command := "sbpanel"
	if !l.System && !onPath(filepath.Dir(l.Binary)) {
		command = l.Binary
	}
	row("管理命令", colorBold(command)+colorDim("（打开管理菜单）"))
	if supported, enabled := manager.Autostart(); !supported {
		row("开机自启", colorDim("不支持（"+autostartUnsupportedReason(l)+"），机器或容器重启后运行 "+command+" start"))
	} else if enabled {
		row("开机自启", "已开启")
	}
	if !l.System && command != "sbpanel" {
		dir := filepath.Dir(l.Binary)
		fmt.Println(colorDim("  " + dir + " 不在 PATH 里，所以现在要输入完整路径。想直接输入 sbpanel，运行："))
		fmt.Println(colorDim("  echo 'export PATH=\"" + dir + ":$PATH\"' >> ~/.bashrc && export PATH=\"" + dir + ":$PATH\""))
	}
	if host, port, _ := net.SplitHostPort(listen); net.ParseIP(host).IsUnspecified() {
		local := "127.0.0.1:" + port
		fmt.Println()
		fmt.Println(colorDim("  面板是明文 HTTP。在公网服务器上，建议运行 sbpanel listen " + local + "，"))
		fmt.Println(colorDim("  然后用 SSH 隧道访问：ssh -L " + port + ":" + local + " 用户@服务器，再打开 http://" + local))
	}
}

func onPath(dir string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(entry) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// installSelf copies the running binary to the layout's install path.
func installSelf(l layout) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if self == l.Binary {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Binary), 0o755); err != nil {
		return err
	}
	source, err := os.Open(self)
	if err != nil {
		return err
	}
	defer source.Close()
	staged := l.Binary + ".new"
	target, err := os.OpenFile(staged, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(staged)
		return errors.Join(copyErr, closeErr)
	}
	// rename keeps a running old binary intact (it holds the old inode).
	if err := os.Rename(staged, l.Binary); err != nil {
		os.Remove(staged)
		return err
	}
	printStep("已安装到 %s", l.Binary)
	return nil
}

// ---- sing-box for the command line ----

func (l layout) singBoxPath() string { return filepath.Join(filepath.Dir(l.Binary), "sing-box") }

// systemSingBox finds a sing-box outside the panel's data directory: the one the
// installer put next to sbpanel, or any on PATH.
func systemSingBox(l layout) string {
	if path := l.singBoxPath(); fileExists(path) {
		return path
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		return path
	}
	return ""
}

// mirrorURLs prefixes GitHub URLs with $GH_PROXY (for example https://ghfast.top/),
// for servers that cannot reach GitHub directly. HTTPS_PROXY is honoured as usual.
func mirrorURLs(raw string) []string {
	prefix := strings.TrimSpace(os.Getenv("GH_PROXY"))
	if prefix == "" {
		return []string{raw}
	}
	return []string{strings.TrimRight(prefix, "/") + "/" + raw, raw}
}

func cliGet(ctx context.Context, raw string, header http.Header, timeout time.Duration, consume func(*http.Response) error) error {
	transport := http.DefaultTransport.(*http.Transport).Clone() // keeps ProxyFromEnvironment
	client := &http.Client{Transport: transport, Timeout: timeout}
	var failures []error
	for _, candidate := range mirrorURLs(raw) {
		err := fetchRetry(ctx, client, candidate, header, consume)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func cliGitHubRelease(ctx context.Context, repo, tag string) (*githubRelease, error) {
	endpoint := "https://api.github.com/repos/" + repo + "/releases/latest"
	if tag != "" {
		endpoint = "https://api.github.com/repos/" + repo + "/releases/tags/" + tag
	}
	release := &githubRelease{}
	err := cliGet(ctx, endpoint, githubHeader(), cliAPITimeout, func(response *http.Response) error {
		data, err := readLimited(response.Body, maxReleaseListBytes)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, release)
	})
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的版本信息失败（访问不了 GitHub 的话，可以设置 HTTPS_PROXY 或 GH_PROXY 再试）: %w", repo, err)
	}
	return release, nil
}

func findAsset(release *githubRelease, name string) (*githubAsset, error) {
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			return &release.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("版本 %s 里没有 %s", release.TagName, name)
}

// cliDownloadAsset downloads one release asset into dir and checks its SHA-256.
func cliDownloadAsset(ctx context.Context, asset *githubAsset, dir string, sums map[string]string) (string, error) {
	want, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok {
		want = sums[asset.Name]
	}
	if want == "" {
		return "", fmt.Errorf("拿不到 %s 的 SHA-256，无法校验，已取消", asset.Name)
	}
	path := filepath.Join(dir, asset.Name)
	lastPrint := time.Time{}
	err := cliGet(ctx, asset.BrowserDownloadURL, http.Header{"User-Agent": {"sbpanel/" + version}}, cliDownloadTimeout,
		func(response *http.Response) error {
			return downloadVerified(response.Body, path, want, func(done int64) {
				if useColor && time.Since(lastPrint) > 500*time.Millisecond {
					lastPrint = time.Now()
					size := asset.Size
					if size == 0 {
						size = response.ContentLength
					}
					if size > 0 {
						fmt.Printf("\r  下载 %s  %.1f / %.1f MB ", asset.Name, float64(done)/(1<<20), float64(size)/(1<<20))
					} else {
						fmt.Printf("\r  下载 %s  %.1f MB ", asset.Name, float64(done)/(1<<20))
					}
				}
			})
		})
	if useColor && !lastPrint.IsZero() {
		fmt.Print("\r\033[K")
	}
	if err != nil {
		return "", fmt.Errorf("下载 %s 失败: %w", asset.Name, err)
	}
	return path, nil
}

// installSingBox puts the latest stable official sing-box next to sbpanel.
func installSingBox(l layout) error {
	ctx, cancel := context.WithTimeout(context.Background(), cliDownloadTimeout)
	defer cancel()
	release, err := cliGitHubRelease(ctx, "SagerNet/sing-box", "")
	if err != nil {
		return err
	}
	assetName, err := releaseAssetName(release.TagName)
	if err != nil {
		return err
	}
	asset, err := findAsset(release, assetName)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sbpanel-sing-box-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	archive, err := cliDownloadAsset(ctx, asset, dir, nil)
	if err != nil {
		return err
	}
	target := l.singBoxPath()
	staged := target + ".new"
	if err := extractFromTarball(archive, "sing-box", staged, 0o755); err != nil {
		os.Remove(staged)
		return err
	}
	got, err := binaryVersion(ctx, staged)
	if err != nil {
		os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		os.Remove(staged)
		return err
	}
	_ = os.MkdirAll(l.ConfigDir, 0o755)
	_ = os.WriteFile(l.singBoxMarker(), []byte(target+"\n"), 0o644)
	printOK("sing-box %s 已安装到 %s（SHA-256 校验通过）", got, target)
	return nil
}

// ---- update ----

func sbpanelAssetName() (string, error) {
	arch, ok := releaseArch[runtime.GOARCH]
	if !ok || runtime.GOOS != "linux" {
		return "", fmt.Errorf("暂不支持 %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return "sbpanel-linux-" + arch + ".tar.gz", nil
}

func runUpdate(args []string) error {
	flags := flag.NewFlagSet("update", flag.ExitOnError)
	tag := flags.String("version", "", "安装指定版本，例如 v0.1.0（默认最新版）")
	force := flags.Bool("force", false, "版本相同也重新安装")
	flags.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), cliDownloadTimeout)
	defer cancel()
	fetch := func(ctx context.Context, rawURL string, header http.Header, timeout time.Duration, consume func(*http.Response) error) (string, error) {
		return "", cliGet(ctx, rawURL, header, timeout, consume)
	}
	target := *tag
	if target == "" {
		printStep("检查新版本…")
		latest, _, err := latestSbpanelTag(ctx, fetch)
		if err != nil {
			return fmt.Errorf("%w（访问不了 GitHub 的话，可以设置 HTTPS_PROXY 或 GH_PROXY 再试）", err)
		}
		if !*force && compareSemver(latest, version) <= 0 {
			printOK("已经是最新版本 %s", version)
			return nil
		}
		target = latest
	} else if !releaseTagPattern.MatchString(target) {
		return fmt.Errorf("版本号格式不对: %s（例如 v0.3.0）", target)
	}
	asset, err := sbpanelReleaseAsset(ctx, fetch, target)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sbpanel-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	printStep("下载 %s %s…", target, asset.Name)
	archive, err := cliDownloadAsset(ctx, asset, dir, nil)
	if err != nil {
		return err
	}
	binary := filepath.Join(dir, "sbpanel")
	if err := extractFromTarball(archive, "sbpanel", binary, 0o755); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil || !strings.HasPrefix(string(output), "sbpanel ") {
		return errors.New("下载的文件不是 sbpanel，已取消")
	}
	printOK("下载完成，SHA-256 校验通过：%s", strings.TrimSpace(string(output)))
	// The new binary installs itself, so install-logic changes in the new version apply too.
	command := exec.Command(binary, "install")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func parseSHA256Sums(text string) map[string]string {
	sums := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && len(fields[0]) == 64 {
			sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	return sums
}

var semverPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?`)

// compareSemver orders versions like v1.2.3 and v1.2.3-rc.1; unparseable ones
// (such as "dev") sort first so any release counts as newer.
func compareSemver(a, b string) int {
	left, right := semverPattern.FindStringSubmatch(a), semverPattern.FindStringSubmatch(b)
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(left[i])
		y, _ := strconv.Atoi(right[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case left[4] == right[4]:
		return 0
	case left[4] == "":
		return 1
	case right[4] == "":
		return -1
	}
	return strings.Compare(left[4], right[4])
}

// ---- uninstall ----

func runUninstall(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := flags.Bool("purge", false, "同时删除订阅、设置和面板安装的程序")
	yes := flags.Bool("y", false, "不再确认")
	flags.Parse(args)
	l := currentLayout()
	if !*yes && stdinIsTerminal() {
		what := "服务和程序（订阅和设置会保留）"
		if *purge {
			what = "服务、程序以及所有订阅和设置"
		}
		fmt.Printf("将要删除%s。确定吗？[y/N] ", what)
		var answer string
		fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("已取消")
			return nil
		}
	}
	return uninstall(l, *purge)
}

func uninstall(l layout, purge bool) error {
	_ = directManager{l: l}.Stop()
	if l.System {
		removeUnit()
	}
	removedSingBox := false
	if fileExists(l.singBoxMarker()) {
		removedSingBox = os.Remove(l.singBoxPath()) == nil
		_ = os.Remove(l.singBoxMarker())
	}
	_ = os.Remove(l.Binary)
	if purge {
		_ = os.RemoveAll(l.ConfigDir)
		removeDataDir(l)
	} else {
		_ = os.Remove(l.PIDFile)
	}

	printOK("sbpanel 已卸载")
	if removedSingBox {
		printOK("安装时下载的 %s 也已删除", l.singBoxPath())
	}
	if !purge {
		kept := []string{dataLocation(l), l.ConfigDir}
		if logDir := filepath.Dir(l.LogFile); l.System && fileExists(l.LogFile) {
			kept = append(kept, logDir)
		}
		fmt.Println(colorDim("  订阅和设置保留了下来，重新安装后会继续使用。要彻底删除，运行："))
		fmt.Println(colorDim("  rm -rf " + strings.Join(kept, " ")))
	}
	return nil
}

// dataLocation is where the data really is: with systemd's DynamicUser,
// /var/lib/sbpanel is only a link into /var/lib/private.
func dataLocation(l layout) string {
	if l.System && fileExists(filepath.Join(systemPrivateDataDir, "state.json")) {
		return systemPrivateDataDir
	}
	return l.DataDir
}

func removeDataDir(l layout) {
	if l.System {
		_ = os.RemoveAll(systemPrivateDataDir)
		_ = os.RemoveAll(systemLogDir)
	}
	if info, err := os.Lstat(l.DataDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(l.DataDir)
	} else {
		_ = os.RemoveAll(l.DataDir)
	}
	_ = os.Remove(l.LogFile)
	_ = os.Remove(l.LogFile + ".1")
}
