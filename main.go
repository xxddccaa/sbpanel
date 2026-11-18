package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const projectURL = "https://github.com/xxddccaa/sbpanel"

const usage = `sbpanel —— 在没有桌面的 Linux 上用网页管理 sing-box 订阅

不带参数运行会打开管理菜单。也可以直接用子命令：

  安装和维护
    sbpanel install      安装或修复服务（systemd），首次安装会打印面板密码
    sbpanel update       更新 sbpanel 到最新版本
    sbpanel uninstall    卸载（加 --purge 同时删除订阅和设置）

  日常管理
    sbpanel status       查看运行状态、面板地址、代理是否可用
    sbpanel start | stop | restart
    sbpanel enable | disable     开机自启
    sbpanel logs [-f]    查看日志（-f 持续输出）
    sbpanel passwd       重设网页面板密码
    sbpanel listen ADDR  修改面板监听地址，例如 0.0.0.0:8091 或 127.0.0.1:8091

  其他
    sbpanel serve        前台运行面板（systemd 服务用的就是它）
    sbpanel version

项目主页: ` + projectURL + `
`

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	if len(args) == 0 {
		if stdinIsTerminal() {
			exitOn(runMenu())
			return
		}
		fmt.Print(usage)
		return
	}
	commands := map[string]func([]string) error{
		"serve":     runServe,
		"passwd":    runPasswd,
		"install":   runInstall,
		"update":    runUpdate,
		"uninstall": runUninstall,
		"status":    runStatus,
		"start":     serviceAction("start"),
		"stop":      serviceAction("stop"),
		"restart":   serviceAction("restart"),
		"enable":    serviceAction("enable"),
		"disable":   serviceAction("disable"),
		"logs":      runLogs,
		"listen":    runListen,
		"menu":      func([]string) error { return runMenu() },
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Println("sbpanel", version)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "不认识的命令: %s\n\n%s", args[0], usage)
		os.Exit(2)
	}
	exitOn(command(args[1:]))
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, colorRed("错误: ")+err.Error())
		os.Exit(1)
	}
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func defaultDataDir() string {
	if dir := os.Getenv("STATE_DIRECTORY"); dir != "" { // set by systemd StateDirectory=
		return dir
	}
	return "/var/lib/sbpanel"
}

func runServe(args []string) error {
	// Defaults come from the environment so the systemd unit can stay fixed and the
	// settings live in /etc/sbpanel/sbpanel.env.
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := flags.String("data", envOr("SBPANEL_DATA", defaultDataDir()), "数据目录（环境变量 SBPANEL_DATA）")
	listen := flags.String("listen", envOr("SBPANEL_LISTEN", defaultPanelListen), "网页面板监听地址（环境变量 SBPANEL_LISTEN）")
	singBox := flags.String("sing-box", envOr("SBPANEL_SING_BOX", "sing-box"), "sing-box 可执行文件（环境变量 SBPANEL_SING_BOX）")
	secureCookie := flags.Bool("secure-cookie", envOr("SBPANEL_SECURE_COOKIE", "") == "1", "面板通过 HTTPS 反向代理访问时打开（环境变量 SBPANEL_SECURE_COOKIE=1）")
	flags.Parse(args)

	// The system sing-box is optional once a kernel has been installed from the panel.
	systemBinary, _ := exec.LookPath(*singBox)
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockDataDir(*dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	kernel := NewKernel(systemBinary, filepath.Join(*dataDir, "kernel"))
	if kernel.Active() == "" {
		log.Printf("还没有安装 sing-box：可以在网页面板“设置 → sing-box 内核”里一键安装")
	}
	runDir := filepath.Join(*dataDir, "run")
	tempDir := filepath.Join(runDir, "tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return err
	}

	auth := NewAuth(filepath.Join(*dataDir, "auth.json"))
	if !auth.Exists() {
		password := randomPassword(12)
		if err := auth.SetPassword(password); err != nil {
			return err
		}
		log.Printf("======== 首次启动，已生成网页面板密码: %s ========", password)
		log.Printf("可以登录后在“设置”里修改，或运行 sbpanel passwd 重设")
	}

	store, err := OpenStore(filepath.Join(*dataDir, "state.json"))
	if err != nil {
		return err
	}
	if err := store.Update(func(state *State) error {
		if state.ClashSecret == "" {
			state.ClashSecret = randomPassword(24)
		}
		if state.ClashPort == 0 || !portFree(state.ClashPort) {
			port, err := freeLoopbackPort(state.Settings.ListenPort)
			if err != nil {
				return err
			}
			state.ClashPort = port
		}
		return nil
	}); err != nil {
		return err
	}
	state := store.Snapshot()

	if active := kernel.Active(); active != "" {
		log.Printf("sbpanel %s，使用 %s (sing-box %s)", version, active, kernel.Version(active))
	} else {
		log.Printf("sbpanel %s", version)
	}

	configPath := filepath.Join(*dataDir, "config.json")
	core := NewCore(kernel.Active, configPath, runDir, clashAddress(state.ClashPort), state.ClashSecret)
	app := NewApp(store, core, &singBoxChecker{binary: kernel.Active, tempDir: tempDir}, kernel, AppPaths{
		Config:  configPath,
		RuleSet: filepath.Join(*dataDir, "rules"),
		Temp:    tempDir,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("网页面板无法监听 %s: %w", *listen, err)
	}
	server := &http.Server{
		Handler:           (&Server{app: app, auth: auth, limiter: newLoginLimiter(), secure: *secureCookie}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("网页面板: http://%s", *listen)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("网页面板退出: %v", err)
			stop()
		}
	}()

	app.Startup(ctx)
	go app.RunScheduler(ctx)

	<-ctx.Done()
	log.Printf("正在退出")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	// Stop the process but keep CoreEnabled, so sing-box comes back on the next start.
	return core.terminate()
}

func portFree(port int) bool {
	listener, err := net.Listen("tcp", clashAddress(port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

func freeLoopbackPort(avoid int) (int, error) {
	for range 20 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		if port != avoid {
			return port, nil
		}
	}
	return 0, errors.New("找不到空闲端口")
}

func runPasswd(args []string) error {
	flags := flag.NewFlagSet("passwd", flag.ExitOnError)
	dataDir := flags.String("data", envOr("SBPANEL_DATA", defaultDataDir()), "数据目录")
	password := flags.String("password", "", "新密码（不填则提示输入，直接回车随机生成）")
	random := flags.Bool("random", false, "随机生成一个新密码")
	flags.Parse(args)

	if !*random && *password == "" {
		if !stdinIsTerminal() {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return err
			}
			*password = strings.TrimRight(line, "\r\n")
		} else {
			*password = readSecret("新密码（至少 8 位，直接回车随机生成）: ")
		}
	}
	generated := *random || *password == ""
	if generated {
		*password = randomPassword(12)
	}
	if err := setPanelPassword(*dataDir, *password); err != nil {
		return err
	}
	if generated {
		fmt.Printf("新密码: %s\n", colorBold(*password))
	}
	fmt.Println("密码已更新，所有已登录的浏览器需要重新登录")
	return nil
}

func setPanelPassword(dataDir, password string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	authPath := filepath.Join(dataDir, "auth.json")
	if err := NewAuth(authPath).SetPassword(password); err != nil {
		return err
	}
	// When run as root against the service's directory, hand the file back to the service user.
	chownLike(authPath, dataDir)
	return nil
}

// chownLike gives path the owner of reference (used for files root writes into the
// service's DynamicUser-owned state directory).
func chownLike(path, reference string) {
	if info, err := os.Stat(reference); err == nil {
		if owner, ok := info.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(path, int(owner.Uid), int(owner.Gid))
		}
	}
}

// readSecret prompts for input without echoing it.
func readSecret(prompt string) string {
	fmt.Print(prompt)
	if err := sttyEcho(false); err == nil {
		defer func() { _ = sttyEcho(true); fmt.Println() }()
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func sttyEcho(on bool) error {
	mode := "-echo"
	if on {
		mode = "echo"
	}
	command := exec.Command("stty", mode)
	command.Stdin = os.Stdin
	return command.Run()
}

// lockDataDir stops a second sbpanel (say, a manual `sbpanel serve` while the service
// runs) from managing the same data directory and fighting over sing-box.
func lockDataDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("数据目录 %s 已经被另一个 sbpanel 使用（是不是 systemd 服务在运行？）", dir)
	}
	return func() { file.Close() }, nil
}
