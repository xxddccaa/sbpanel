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
	systemSingBoxPath  = "/usr/local/bin/sing-box"
	singBoxMarkerPath  = "/etc/sbpanel/.sing-box-installed-by-sbpanel"
	cliDownloadTimeout = 15 * time.Minute
	cliAPITimeout      = 30 * time.Second
	panelStartTimeout  = 20 * time.Second
)

const unitTemplate = `[Unit]
Description=sbpanel - web panel for sing-box subscriptions
Documentation=` + projectURL + `
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=-` + envPath + `
ExecStart=` + installedBinary + ` serve
Restart=on-failure
RestartSec=5

# Runs as a throwaway system user; state lives in /var/lib/sbpanel.
DynamicUser=yes
StateDirectory=sbpanel
StateDirectoryMode=0700
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`

func writeUnit() error {
	return writeFileAtomic(unitPath, []byte(unitTemplate), 0o644)
}

// ---- install ----

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	listenFlag := flags.String("listen", "", "面板监听地址，默认 "+defaultPanelListen+"（已安装时保持原来的设置）")
	portFlag := flags.Int("port", 0, "只修改面板端口")
	skipSingBox := flags.Bool("skip-sing-box", false, "不要自动安装 sing-box")
	flags.Parse(args)
	if err := requireRoot(); err != nil {
		return err
	}
	if err := requireSystemd(); err != nil {
		return err
	}

	upgrading := fileExists(unitPath)
	before := queryUnit()
	oldListen := currentPanelListen()
	listen := oldListen
	if listen == "" {
		listen = defaultPanelListen
	}
	if *listenFlag != "" {
		listen = *listenFlag
	}
	if *portFlag != 0 {
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			host = "0.0.0.0"
		}
		listen = net.JoinHostPort(host, strconv.Itoa(*portFlag))
	}
	if err := validateListen(listen); err != nil {
		return err
	}
	serviceHoldsPort := before.Active == "active" && listen == oldListen
	if !serviceHoldsPort && !canListen(listen) {
		return fmt.Errorf("面板端口 %s 已经被别的程序占用了，换一个：sbpanel install --port 8092", listen)
	}

	if err := installSelf(); err != nil {
		return fmt.Errorf("复制程序失败: %w", err)
	}

	if !*skipSingBox {
		if path, _ := activeKernelPath(); path == "" {
			printStep("没有找到 sing-box，下载官方最新稳定版…")
			if err := installSystemSingBox(); err != nil {
				printWarn("sing-box 安装失败: %v", err)
				printWarn("可以稍后在网页面板“设置 → sing-box 内核”里安装，或者设置 GH_PROXY 后重新运行 sbpanel install")
			}
		}
	}

	if err := writePanelListen(listen); err != nil {
		return err
	}
	if err := writeUnit(); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", serviceName); err != nil {
		return err
	}
	freshAuth := !fileExists(filepath.Join(serviceDataDir, "auth.json"))
	startedAt := time.Now().Add(-time.Second)
	printStep("启动服务…")
	if err := systemctl("restart", serviceName); err != nil {
		return err
	}
	if !waitForPanel(listen, panelStartTimeout) {
		fmt.Println()
		_ = showLogs(false, 20)
		return errors.New("服务没有正常启动，上面是最近的日志")
	}

	password := ""
	if freshAuth {
		password = firstStartPassword(startedAt)
		if password == "" {
			password = randomPassword(12)
			if err := setPanelPassword(serviceDataDir, password); err != nil {
				return err
			}
		}
	}

	fmt.Println()
	if upgrading {
		printOK("sbpanel %s 已更新并重新启动", version)
	} else {
		printOK("sbpanel %s 已安装并启动，已开启开机自启", version)
	}
	urls := panelURLs(listen)
	for i, link := range urls {
		label := "          "
		if i == 0 {
			label = "  网页面板"
		}
		fmt.Printf("%s  %s\n", label, colorBold(link))
	}
	if password != "" {
		fmt.Printf("  登录密码  %s  %s\n", colorBold(password), colorDim("（请记下来；忘了运行 sbpanel passwd 重设）"))
	} else {
		fmt.Println("  登录密码  " + colorDim("没有改变（忘了运行 sbpanel passwd 重设）"))
	}
	fmt.Println("  管理命令  " + colorBold("sbpanel") + colorDim("（打开管理菜单）"))
	if host, port, _ := net.SplitHostPort(listen); net.ParseIP(host).IsUnspecified() {
		local := "127.0.0.1:" + port
		fmt.Println()
		fmt.Println(colorDim("  面板是明文 HTTP。在公网服务器上，建议运行 sbpanel listen " + local + "，"))
		fmt.Println(colorDim("  然后用 SSH 隧道访问：ssh -L " + port + ":" + local + " 用户@服务器，再打开 http://" + local))
	}
	return nil
}

// installSelf copies the running binary to /usr/local/bin/sbpanel.
func installSelf() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if self == installedBinary {
		return nil
	}
	source, err := os.Open(self)
	if err != nil {
		return err
	}
	defer source.Close()
	staged := installedBinary + ".new"
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
	if err := os.Rename(staged, installedBinary); err != nil {
		os.Remove(staged)
		return err
	}
	printStep("已安装到 %s", installedBinary)
	return nil
}

var firstPasswordPattern = regexp.MustCompile(`已生成网页面板密码: (\S+)`)

// firstStartPassword finds the password the service generated on its first start.
func firstStartPassword(since time.Time) string {
	for range 20 {
		output, _ := exec.Command("journalctl", "-u", serviceName, "--no-pager", "-o", "cat",
			"--since", "@"+strconv.FormatInt(since.Unix(), 10)).Output()
		if match := firstPasswordPattern.FindSubmatch(output); match != nil {
			return string(match[1])
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ""
}

// ---- downloads from GitHub (command line, runs as root) ----

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
		err := fetchOnce(ctx, client, candidate, header, consume)
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
func cliDownloadAsset(ctx context.Context, release *githubRelease, asset *githubAsset, dir string, sums map[string]string) (string, error) {
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
					fmt.Printf("\r  下载 %s  %.1f / %.1f MB ", asset.Name, float64(done)/(1<<20), float64(asset.Size)/(1<<20))
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

// installSystemSingBox puts the latest stable official sing-box at /usr/local/bin.
func installSystemSingBox() error {
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
	archive, err := cliDownloadAsset(ctx, release, asset, dir, nil)
	if err != nil {
		return err
	}
	staged := systemSingBoxPath + ".new"
	if err := extractFromTarball(archive, "sing-box", staged, 0o755); err != nil {
		os.Remove(staged)
		return err
	}
	got, err := binaryVersion(ctx, staged)
	if err != nil {
		os.Remove(staged)
		return err
	}
	if err := os.Rename(staged, systemSingBoxPath); err != nil {
		os.Remove(staged)
		return err
	}
	_ = os.MkdirAll(envDir, 0o755)
	_ = os.WriteFile(singBoxMarkerPath, []byte(systemSingBoxPath+"\n"), 0o644)
	printOK("sing-box %s 已安装到 %s（SHA-256 校验通过）", got, systemSingBoxPath)
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
	if err := requireRoot(); err != nil {
		return err
	}
	if err := requireSystemd(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cliDownloadTimeout)
	defer cancel()
	printStep("检查新版本…")
	release, err := cliGitHubRelease(ctx, githubRepo, *tag)
	if err != nil {
		return err
	}
	if !*force && *tag == "" && compareSemver(release.TagName, version) <= 0 {
		printOK("已经是最新版本 %s", version)
		return nil
	}
	assetName, err := sbpanelAssetName()
	if err != nil {
		return err
	}
	asset, err := findAsset(release, assetName)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sbpanel-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sums := map[string]string{}
	if sumsAsset, err := findAsset(release, "sha256sums.txt"); err == nil {
		_ = cliGet(ctx, sumsAsset.BrowserDownloadURL, nil, cliAPITimeout, func(response *http.Response) error {
			data, err := readLimited(response.Body, 1<<20)
			if err == nil {
				sums = parseSHA256Sums(string(data))
			}
			return err
		})
	}
	printStep("下载 %s %s…", release.TagName, assetName)
	archive, err := cliDownloadAsset(ctx, release, asset, dir, sums)
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
	// The new binary installs itself, so unit-file changes in the new version apply too.
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
	purge := flags.Bool("purge", false, "同时删除订阅、设置、面板密码和面板安装的 sing-box")
	yes := flags.Bool("y", false, "不再确认")
	flags.Parse(args)
	if err := requireRoot(); err != nil {
		return err
	}
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
	return uninstall(*purge)
}

func uninstall(purge bool) error {
	if fileExists(unitPath) {
		_ = systemctl("disable", "--now", serviceName)
		if purge {
			if err := systemctl("clean", "--what=state", serviceName); err != nil {
				_ = os.RemoveAll("/var/lib/private/sbpanel")
				_ = os.Remove(serviceDataDir)
			}
		}
		_ = os.Remove(unitPath)
		_ = systemctl("daemon-reload")
		_ = systemctl("reset-failed", serviceName)
	} else if purge {
		_ = os.RemoveAll("/var/lib/private/sbpanel")
		_ = os.Remove(serviceDataDir)
	}
	removedSingBox := false
	if fileExists(singBoxMarkerPath) {
		removedSingBox = os.Remove(systemSingBoxPath) == nil
	}
	_ = os.RemoveAll(envDir)
	_ = os.Remove(installedBinary)

	printOK("sbpanel 已卸载")
	if removedSingBox {
		printOK("安装时下载的 %s 也已删除", systemSingBoxPath)
	}
	if !purge {
		fmt.Println(colorDim("  订阅和设置保留在 /var/lib/private/sbpanel，重新安装后会继续使用；"))
		fmt.Println(colorDim("  要彻底删除，运行: rm -rf /var/lib/private/sbpanel"))
	}
	return nil
}
