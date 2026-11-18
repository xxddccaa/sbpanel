package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	serviceName        = "sbpanel"
	unitPath           = "/etc/systemd/system/sbpanel.service"
	envDir             = "/etc/sbpanel"
	envPath            = "/etc/sbpanel/sbpanel.env"
	installedBinary    = "/usr/local/bin/sbpanel"
	serviceDataDir     = "/var/lib/sbpanel"
	defaultPanelListen = "0.0.0.0:8091"
	connectivityURL    = "https://cp.cloudflare.com/"
)

// ---- terminal output ----

var useColor = func() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}()

func paint(code, text string) string {
	if !useColor {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func colorRed(text string) string    { return paint("31", text) }
func colorGreen(text string) string  { return paint("32", text) }
func colorYellow(text string) string { return paint("33", text) }
func colorBold(text string) string   { return paint("1", text) }
func colorDim(text string) string    { return paint("2", text) }

func printOK(format string, args ...any) {
	fmt.Println(colorGreen("✓ ") + fmt.Sprintf(format, args...))
}

func printWarn(format string, args ...any) {
	fmt.Println(colorYellow("! ") + fmt.Sprintf(format, args...))
}

func printStep(format string, args ...any) {
	fmt.Println(colorDim("→ ") + fmt.Sprintf(format, args...))
}

// ---- preconditions ----

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限，请用 sudo 运行（例如 sudo sbpanel）")
	}
	return nil
}

func requireSystemd() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("这台机器没有 systemd，暂不支持自动安装；可以手动运行 sbpanel serve")
	}
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("systemd 没有在运行（容器里？），暂不支持自动安装；可以手动运行 sbpanel serve")
	}
	return nil
}

func requireInstalled() error {
	if err := requireRoot(); err != nil {
		return err
	}
	if !fileExists(unitPath) {
		return errors.New("sbpanel 还没有安装成服务，先运行 sbpanel install")
	}
	return nil
}

// ---- systemd ----

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
	Loaded   bool
	Active   string // active, inactive, failed, activating, ...
	Enabled  string // enabled, disabled, ...
	MainPID  int
	Since    time.Time
	MemoryMB float64
}

func queryUnit() unitStatus {
	output, _ := exec.Command("systemctl", "show", serviceName, "--no-pager",
		"-p", "LoadState,ActiveState,UnitFileState,MainPID,ActiveEnterTimestampMonotonic,MemoryCurrent").Output()
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	status := unitStatus{
		Loaded:  values["LoadState"] == "loaded",
		Active:  values["ActiveState"],
		Enabled: values["UnitFileState"],
	}
	status.MainPID, _ = strconv.Atoi(values["MainPID"])
	if memory, err := strconv.ParseFloat(values["MemoryCurrent"], 64); err == nil && memory < 1<<50 {
		status.MemoryMB = memory / (1 << 20)
	}
	if micros, err := strconv.ParseInt(values["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && micros > 0 {
		status.Since = bootTime().Add(time.Duration(micros) * time.Microsecond)
	}
	return status
}

// bootTime derives the boot time from /proc/uptime, so systemd's monotonic
// timestamps can be shown as a duration without parsing localized dates.
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

func describeActive(status unitStatus) string {
	switch status.Active {
	case "active":
		text := colorGreen("● 运行中")
		var extra []string
		if !status.Since.IsZero() {
			if uptime := time.Since(status.Since); uptime < time.Minute {
				extra = append(extra, "刚刚启动")
			} else {
				extra = append(extra, "已运行 "+humanDuration(uptime))
			}
		}
		if status.MemoryMB > 0 {
			extra = append(extra, fmt.Sprintf("内存 %.0f MB", status.MemoryMB))
		}
		if len(extra) > 0 {
			text += colorDim("（" + strings.Join(extra, "，") + "）")
		}
		return text
	case "activating", "reloading":
		return colorYellow("● 正在启动")
	case "failed":
		return colorRed("● 启动失败") + colorDim("（sbpanel logs 查看原因）")
	case "":
		return colorDim("● 未安装")
	default:
		return colorDim("● 已停止")
	}
}

func describeEnabled(status unitStatus) string {
	if status.Enabled == "enabled" {
		return colorGreen("已开启")
	}
	return colorYellow("未开启")
}

// padDisplay pads text to width terminal columns; CJK characters take two columns.
func padDisplay(text string, width int) string {
	columns := 0
	for _, r := range text {
		if r >= 0x2E80 {
			columns += 2
		} else {
			columns++
		}
	}
	if columns >= width {
		return text
	}
	return text + strings.Repeat(" ", width-columns)
}

func humanDuration(duration time.Duration) string {
	switch {
	case duration < time.Minute:
		return "不到 1 分钟"
	case duration < time.Hour:
		return fmt.Sprintf("%d 分钟", int(duration.Minutes()))
	case duration < 48*time.Hour:
		return fmt.Sprintf("%d 小时", int(duration.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(duration.Hours()/24))
	}
}

// ---- panel listen address (/etc/sbpanel/sbpanel.env) ----

var (
	envListenPattern  = regexp.MustCompile(`(?m)^[ \t]*SBPANEL_LISTEN[ \t]*=[ \t]*"?([^"\s]+)"?[ \t]*$`)
	unitListenPattern = regexp.MustCompile(`--listen[ =](\S+)`)
)

// currentPanelListen returns the configured panel address, also understanding the
// pre-0.1 unit files that passed it as a command-line flag.
func currentPanelListen() string {
	if data, err := os.ReadFile(envPath); err == nil {
		if match := envListenPattern.FindSubmatch(data); match != nil {
			return string(match[1])
		}
	}
	if data, err := os.ReadFile(unitPath); err == nil {
		if match := unitListenPattern.FindSubmatch(data); match != nil {
			return string(match[1])
		}
	}
	return ""
}

func validateListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("地址格式不对: %s（要写成 IP:端口，例如 0.0.0.0:8091）", address)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("地址里要写 IP，不能写域名: %s", host)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("端口不对: %s", port)
	}
	return nil
}

const envFileHeader = `# sbpanel 服务设置。修改后运行 sbpanel restart 生效。
#
# 网页面板监听地址：
#   0.0.0.0:8091    局域网和公网都能访问（面板有密码，但走的是明文 HTTP）
#   127.0.0.1:8091  只有本机能访问，远程用 SSH 隧道：ssh -L 8091:127.0.0.1:8091 用户@服务器
`

// writePanelListen rewrites SBPANEL_LISTEN and keeps any other lines the user added.
func writePanelListen(address string) error {
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return err
	}
	return writeListenToEnvFile(envPath, address)
}

func writeListenToEnvFile(path, address string) error {
	line := "SBPANEL_LISTEN=" + address
	data, err := os.ReadFile(path)
	var content string
	switch {
	case err != nil:
		content = envFileHeader + line + "\n"
	case envListenPattern.Match(data):
		content = envListenPattern.ReplaceAllString(string(data), line)
	default:
		content = strings.TrimRight(string(data), "\n") + "\n" + line + "\n"
	}
	return writeFileAtomic(path, []byte(content), 0o644)
}

// panelURLs lists the addresses a browser can use to reach the panel.
func panelURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsUnspecified() {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	var urls []string
	for _, address := range hostIPv4Addresses() {
		urls = append(urls, "http://"+net.JoinHostPort(address, port))
	}
	if len(urls) == 0 {
		urls = append(urls, "http://"+net.JoinHostPort("127.0.0.1", port))
	}
	return urls
}

// Interfaces created by container runtimes and VPNs are not how a person reaches the box.
var virtualInterfacePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "podman", "lxc", "tun", "tap", "wg", "zt"}

func hostIPv4Addresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addresses []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		virtual := false
		for _, prefix := range virtualInterfacePrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				virtual = true
			}
		}
		if virtual {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if network, ok := addr.(*net.IPNet); ok && network.IP.To4() != nil && !network.IP.IsLinkLocalUnicast() {
				addresses = append(addresses, network.IP.String())
			}
		}
	}
	return addresses
}

// waitForPanel waits until the panel answers HTTP on its configured address.
func waitForPanel(listen string, timeout time.Duration) bool {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/api/auth")
		if err == nil {
			response.Body.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// ---- status ----

func runStatus(args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	printStatus()
	return nil
}

func printStatus() {
	unit := queryUnit()
	fmt.Println(colorBold("sbpanel " + version))
	if !fileExists(unitPath) {
		fmt.Println("  服务       " + colorDim("● 未安装") + "（运行 sbpanel install 安装）")
		return
	}
	row := func(label, value string) { fmt.Printf("  %s  %s\n", padDisplay(label, 8), value) }
	row("服务", describeActive(unit))
	row("开机自启", describeEnabled(unit))

	listen := currentPanelListen()
	if listen == "" {
		listen = defaultPanelListen
	}
	for i, address := range panelURLs(listen) {
		label := ""
		if i == 0 {
			label = "网页面板"
		}
		row(label, address)
	}

	state, stateErr := readServiceState()
	kernelPath, kernelSource := activeKernelPath()
	if kernelPath == "" {
		row("sing-box", colorYellow("未安装")+"（网页面板“设置”里可以一键安装）")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		kernelVersion, _ := binaryVersion(ctx, kernelPath)
		cancel()
		row("sing-box", fmt.Sprintf("%s %s", kernelVersion, colorDim("（"+kernelSource+" "+kernelPath+"）")))
	}
	if stateErr != nil {
		row("代理", colorDim("还没有数据（面板第一次启动后才会生成）"))
		return
	}

	proxy := localProxyURL(state.Settings)
	proxyAddress := net.JoinHostPort(state.Settings.ListenAddr, strconv.Itoa(state.Settings.ListenPort))
	row("代理", proxyAddress+colorDim("  HTTP 和 SOCKS5 都行"))
	bypass := "关"
	if state.Settings.BypassChina {
		bypass = "开"
	}
	row("绕过大陆", bypass)

	nodes := 0
	for _, sub := range state.Subscriptions {
		for _, node := range sub.Nodes {
			if !node.Helper {
				nodes++
			}
		}
	}
	row("订阅", fmt.Sprintf("%d 个，共 %d 个节点", len(state.Subscriptions), nodes))

	if unit.Active != "active" {
		return
	}
	if current := currentNodeName(state); current != "" {
		row("当前节点", current)
	}
	if !state.CoreEnabled {
		row("连通性", colorDim("代理没有启动（在网页面板里点“启动代理”）"))
		return
	}
	latency, err := testProxy(proxy)
	if err != nil {
		row("连通性", colorRed("✗ 代理不通")+colorDim("（"+err.Error()+"）"))
	} else {
		row("连通性", colorGreen("✓ 代理可用")+colorDim(fmt.Sprintf("（cp.cloudflare.com %d ms）", latency.Milliseconds())))
	}
}

func readServiceState() (*State, error) {
	data, err := os.ReadFile(filepath.Join(serviceDataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	state := &State{Settings: defaultSettings()}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, err
	}
	return state, nil
}

// activeKernelPath mirrors Kernel.Active for the installed service.
func activeKernelPath() (string, string) {
	managed := filepath.Join(serviceDataDir, "kernel", "sing-box")
	if fileExists(managed) {
		return managed, "面板安装"
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		return path, "系统"
	}
	return "", ""
}

// currentNodeName asks the running sing-box (clash API) which node is in use.
func currentNodeName(state *State) string {
	if state.ClashPort == 0 {
		return ""
	}
	core := &Core{clashAddr: clashAddress(state.ClashPort), secret: state.ClashSecret}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	group, err := core.Group(ctx, tagProxy)
	if err != nil {
		return ""
	}
	if group.Now != tagAuto {
		return group.Now + colorDim("（手动选择）")
	}
	if auto, err := core.Group(ctx, tagAuto); err == nil && auto.Now != "" {
		return auto.Now + colorDim("（自动选择）")
	}
	return "自动选择"
}

func testProxy(proxy *url.URL) (time.Duration, error) {
	if host, _, err := net.SplitHostPort(proxy.Host); err == nil && net.ParseIP(host).IsUnspecified() {
		proxy.Host = net.JoinHostPort("127.0.0.1", proxy.Port())
	}
	client := newHTTPClient(proxy, 10*time.Second)
	started := time.Now()
	response, err := client.Get(connectivityURL)
	if err != nil {
		return 0, stripURL(err)
	}
	response.Body.Close()
	if response.StatusCode >= 400 {
		return 0, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return time.Since(started), nil
}

// ---- start / stop / enable ----

func serviceAction(action string) func([]string) error {
	return func(args []string) error {
		if err := requireInstalled(); err != nil {
			return err
		}
		return doServiceAction(action)
	}
}

func doServiceAction(action string) error {
	switch action {
	case "start", "restart":
		if err := systemctl(action, serviceName); err != nil {
			return err
		}
		listen := currentPanelListen()
		if listen == "" {
			listen = defaultPanelListen
		}
		if !waitForPanel(listen, 15*time.Second) {
			return errors.New("服务没有正常启动，运行 sbpanel logs 查看原因")
		}
		if action == "start" {
			printOK("已启动")
		} else {
			printOK("已重启")
		}
	case "stop":
		if err := systemctl("stop", serviceName); err != nil {
			return err
		}
		printOK("已停止（代理也一起停了）")
	case "enable":
		if err := systemctl("enable", serviceName); err != nil {
			return err
		}
		printOK("已开启开机自启")
	case "disable":
		if err := systemctl("disable", serviceName); err != nil {
			return err
		}
		printOK("已关闭开机自启（现在运行的服务不受影响）")
	default:
		return fmt.Errorf("unknown action %s", action)
	}
	return nil
}

// ---- logs ----

func runLogs(args []string) error {
	flags := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := flags.Bool("f", false, "持续输出新日志（Ctrl+C 退出）")
	lines := flags.Int("n", 100, "显示最近多少行")
	flags.Parse(args)
	if err := requireInstalled(); err != nil {
		return err
	}
	return showLogs(*follow, *lines)
}

func showLogs(follow bool, lines int) error {
	arguments := []string{"-u", serviceName, "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(lines)}
	if follow {
		arguments = append(arguments, "-f")
	}
	command := exec.Command("journalctl", arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl+C should end journalctl, not the menu that started it.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	err := command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && follow {
		return nil
	}
	return err
}

// ---- listen ----

func runListen(args []string) error {
	if err := requireInstalled(); err != nil {
		return err
	}
	if len(args) != 1 {
		fmt.Println("当前面板地址: " + currentPanelListen())
		fmt.Println("修改: sbpanel listen 0.0.0.0:8091   或   sbpanel listen 127.0.0.1:8091")
		return nil
	}
	return changePanelListen(args[0])
}

func changePanelListen(address string) error {
	address = strings.TrimSpace(address)
	if !strings.Contains(address, ":") {
		address = "0.0.0.0:" + address // just a port
	}
	if err := validateListen(address); err != nil {
		return err
	}
	current := currentPanelListen()
	if address == current {
		printOK("面板地址没有变化: %s", address)
		return nil
	}
	if !canListen(address) {
		return fmt.Errorf("%s 已经被别的程序占用了，换一个端口", address)
	}
	if err := writePanelListen(address); err != nil {
		return err
	}
	// Older installs passed --listen in the unit; rewrite it so the env file wins.
	if err := writeUnit(); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if queryUnit().Active == "active" {
		if err := doServiceAction("restart"); err != nil {
			return err
		}
	}
	printOK("面板地址已改为 %s", address)
	for _, link := range panelURLs(address) {
		fmt.Println("  " + link)
	}
	return nil
}

func canListen(address string) bool {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// ---- interactive menu ----

func runMenu() error {
	if err := requireRoot(); err != nil {
		return err
	}
	input := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		line, err := input.ReadString('\n')
		if err != nil && line == "" {
			fmt.Println()
			os.Exit(0) // Ctrl+D
		}
		return strings.TrimSpace(line)
	}
	pause := func() { ask(colorDim("\n按回车返回菜单…")) }
	report := func(err error) {
		if err != nil {
			fmt.Println(colorRed("错误: ") + err.Error())
		}
	}

	for {
		fmt.Println()
		fmt.Println(colorBold("  sbpanel 管理菜单") + colorDim("  "+version))
		fmt.Println(colorDim("  ─────────────────────────────────"))
		if !fileExists(unitPath) {
			fmt.Println("  还没有安装成服务。")
			fmt.Println()
			fmt.Println("   1. 安装")
			fmt.Println("   0. 退出")
			switch ask("\n请选择: ") {
			case "1":
				report(runInstall(nil))
				pause()
			case "0", "q", "":
				return nil
			}
			continue
		}

		unit := queryUnit()
		listen := currentPanelListen()
		if listen == "" {
			listen = defaultPanelListen
		}
		urls := panelURLs(listen)
		fmt.Printf("  服务: %s   开机自启: %s\n", describeActive(unit), describeEnabled(unit))
		if len(urls) > 0 {
			fmt.Println("  面板: " + urls[0])
		}
		autostart := "开启开机自启"
		if unit.Enabled == "enabled" {
			autostart = "关闭开机自启"
		}
		fmt.Println()
		fmt.Println("   1. 查看详细状态")
		fmt.Println("   2. 启动              3. 停止              4. 重启")
		fmt.Println("   5. " + autostart)
		fmt.Println("   6. 查看最近日志      7. 持续查看日志（Ctrl+C 返回）")
		fmt.Println("   8. 重设面板密码")
		fmt.Println("   9. 修改面板地址/端口")
		fmt.Println("  10. 更新 sbpanel")
		fmt.Println("  11. 卸载")
		fmt.Println("   0. 退出")

		switch ask("\n请选择: ") {
		case "1":
			fmt.Println()
			printStatus()
			pause()
		case "2":
			report(doServiceAction("start"))
		case "3":
			if strings.EqualFold(ask("停止后代理也会断开，确定吗？[y/N] "), "y") {
				report(doServiceAction("stop"))
			}
		case "4":
			report(doServiceAction("restart"))
		case "5":
			if unit.Enabled == "enabled" {
				report(doServiceAction("disable"))
			} else {
				report(doServiceAction("enable"))
			}
		case "6":
			report(showLogs(false, 60))
			pause()
		case "7":
			report(showLogs(true, 30))
		case "8":
			password := readSecret("新密码（至少 8 位，直接回车随机生成）: ")
			generated := password == ""
			if generated {
				password = randomPassword(12)
			}
			if err := setPanelPassword(serviceDataDir, password); err != nil {
				report(err)
			} else if generated {
				printOK("新密码: %s", colorBold(password))
			} else {
				printOK("密码已更新")
			}
			pause()
		case "9":
			fmt.Println("  当前: " + listen)
			fmt.Println(colorDim("  可以只填端口（如 8092），或者 IP:端口。127.0.0.1:8091 表示只允许本机访问。"))
			if answer := ask("  新地址（直接回车取消）: "); answer != "" {
				report(changePanelListen(answer))
				pause()
			}
		case "10":
			report(runUpdate(nil))
			pause()
		case "11":
			fmt.Println("  卸载会删除服务和程序。订阅、设置和面板密码默认保留，重新安装后还能用。")
			switch ask("  确定卸载吗？[y/N/purge（连数据一起删）] ") {
			case "y", "Y":
				report(uninstall(false))
				return nil
			case "purge":
				report(uninstall(true))
				return nil
			}
		case "0", "q", "":
			return nil
		}
	}
}
