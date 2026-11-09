// syedge-instance 管理同机多实例（设计 0001 第 5.7 节），以 root 运行。
// 安装后的命令名为 <品牌>-instance；构建：CGO_ENABLED=0 go build -trimpath ./cmd/syedge-instance
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/sys/unix"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/instance"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/wrapper"
)

func main() {
	// launcher 模式：nonroot 切换时，以 root 运行的旧 master 经共用软链执行本程序（argv0 为数据面二进制的路径），
	// 降权为数据面用户后再执行真正的二进制（设计 0001 D5，wrapper.Handover）
	if filepath.Base(os.Args[0]) == brand.ID() {
		wrapper.Launch(os.Args[0])
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Geteuid()))
}

// handover 是生产环境的 Env.Handover：把以 root 运行的实例 master 切换为数据面用户。
func handover(name string) error {
	u, err := user.Lookup(layout.WorkerUser())
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	paths, err := wrapper.DefaultPathsAt("", name)
	if err != nil {
		return err
	}
	return wrapper.Handover(paths, self, uid, gid)
}

func usage(w io.Writer) {
	n := brand.ID() + "-instance"
	_, _ = fmt.Fprintf(w, `用法：
  %[1]s add <实例名> --http <端口> --https <端口> --status <端口> [--nofile N] [--resource-mode shared|independent]
        [--console <控制台 host:port> --token <注册令牌> --ca-fingerprint <指纹>]
  %[1]s remove <实例名> [--purge] [--yes]     不加 --yes 只显示将执行的操作
  %[1]s list
  %[1]s runtimes                              列出实例、共享方式和功能包安装目录
  %[1]s check
  %[1]s reopen-all                            所有实例重新打开日志（logrotate 使用，可由数据面用户执行）
  %[1]s prefix [<安装根目录>]                  首次设置 system 模式安装根目录（默认 /opt/%[1]s）
  %[1]s nonroot [--root-master]               运行时不使用 root：交出运行时目录、仍以 root 运行的 master
                                              无中断切换为数据面用户（安装与升级软件包时自动执行）。
                                              内核或 systemd 不支持 ambient 能力（如 CentOS 7）时，或加 --root-master，
                                              进入兼容模式：与传统 Web 服务器相同由 root 运行 master，worker 以数据面用户运行

user 模式（普通用户安装，不需要 root；端口须 ≥ 1024）：在以上命令前加 --root <安装根目录>，另有
  %[1]s --root <目录> init --http <端口> --https <端口> --status <端口> [注册参数]   创建该根目录的默认实例
  %[1]s --root <目录> ensure                     启动未运行的数据面与 agent（cron 每分钟调用）
`, n)
}

func env(out io.Writer, userRoot string) instance.Env {
	home, _ := os.UserHomeDir()
	return instance.Env{
		UserRoot: userRoot,
		Home:     home,
		UserSystemd: func() bool {
			return exec.Command("systemctl", "--user", "show-environment").Run() == nil
		},
		Spawn:    spawn,
		Handover: handover,
		SetCaps: func(path string, value []byte) error {
			return unix.Setxattr(path, "security.capability", value, 0)
		},
		Root: "/",
		Out:  out,
		Run: func(name string, args ...string) error {
			c := exec.Command(name, args...) // 参数切片执行，不经过 shell
			c.Stdout, c.Stderr = out, os.Stderr
			return c.Run()
		},
		Listening:   func() (map[string]bool, error) { return instance.ListeningFromProc("/") },
		Ambient:     ambientSupported,
		HomeSandbox: homeSandboxSupported,
	}
}

func homeSandboxSupported() bool {
	out, err := exec.Command("systemctl", "--version").Output()
	return err == nil && instance.SystemdAtLeast(string(out), 238)
}

// ambientSupported：内核支持 ambient 能力（4.3 起，/proc/self/status 有 CapAmb 行）且 systemd 支持
// AmbientCapabilities（229 起）。任一不满足（CentOS 7：内核 3.10、systemd 219）即用兼容模式。
func ambientSupported() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(b), "\nCapAmb:") {
		return false
	}
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		return true // 没有 systemd（容器等）：不经 systemd 授予能力，按内核判断
	}
	return instance.SystemdAtLeast(string(out), 229)
}

func run(args []string, stdout, stderr io.Writer, euid int) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	userRoot := ""
	if args[0] == "--root" {
		if len(args) < 3 {
			usage(stderr)
			return 2
		}
		userRoot, args = args[1], args[2:]
		if _, err := layout.User(userRoot, ""); err != nil {
			_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: --root: "+err.Error())
			return 2
		}
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage(stdout)
		return 0
	}
	switch {
	case userRoot != "" && euid == 0:
		_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: user 模式（--root）请以普通用户运行")
		return 1
	case userRoot == "" && euid != 0 && cmd != "list" && cmd != "check" && cmd != "reopen-all":
		_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: 请以 root 运行（普通用户请使用 --root <安装根目录>）")
		return 1
	case userRoot == "" && (cmd == "init" || cmd == "ensure"):
		_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: "+cmd+" 只用于 user 模式（--root）")
		return 2
	}
	if userRoot == "" && cmd != "prefix" {
		if _, err := layout.LoadSystemRoot(); err != nil {
			_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: "+err.Error())
			return 1
		}
	}
	e := env(stdout, userRoot)
	var err error
	switch cmd {
	case "prefix":
		if len(rest) > 1 {
			usage(stderr)
			return 2
		}
		if len(rest) == 0 {
			root, err := layout.LoadSystemRootAt(e.Root)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: "+err.Error())
				return 1
			}
			_, _ = fmt.Fprintln(stdout, root)
			return 0
		}
		err = e.Prefix(rest[0])
	case "add":
		err = add(e, rest)
	case "init":
		err = add(e, append([]string{""}, rest...))
	case "ensure":
		err = e.Ensure()
	case "remove":
		err = remove(e, rest)
	case "list":
		err = list(e, stdout)
	case "runtimes":
		if len(rest) != 0 {
			return 2
		}
		var targets []instance.RuntimeTarget
		targets, err = e.RuntimeTargets()
		if err == nil {
			for _, t := range targets {
				_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\n", t.Name, t.Mode, t.Root)
			}
		}
	case "check":
		if probs := e.Check(); len(probs) > 0 {
			for _, p := range probs {
				_, _ = fmt.Fprintln(stdout, "问题："+p)
			}
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "未发现问题")
	case "reopen-all":
		err = e.ReopenAll()
	case "nonroot":
		// --root-master：兼容模式（与传统 Web 服务器相同，master 以 root 运行）。内核或 systemd 不支持 ambient 能力时自动进入
		switch {
		case len(rest) == 1 && rest[0] == "--root-master":
			e.RootMaster = true
		case len(rest) > 0:
			usage(stderr)
			return 2
		}
		err = e.Nonroot()
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: "+err.Error())
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return 0
}

var errUsage = errors.New("参数错误")

// splitName 取出第一个位置参数（实例名），其余交给 flag 解析。
func splitName(args []string) (string, []string, error) {
	if len(args) > 0 && args[0] == "" { // init：user 模式的默认实例
		return "", args[1:], nil
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("%w：缺少实例名", errUsage)
	}
	return args[0], args[1:], nil
}

func add(e instance.Env, args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	httpP := fs.Int("http", 0, "")
	httpsP := fs.Int("https", 0, "")
	status := fs.Int("status", 0, "")
	nofile := fs.Int("nofile", 0, "")
	resourceMode := fs.String("resource-mode", "", "")
	console := fs.String("console", "", "")
	token := fs.String("token", "", "")
	caFP := fs.String("ca-fingerprint", "", "")
	if err := fs.Parse(rest); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("%w：%v", errUsage, err)
	}
	// 具名实例的端口必须显式给出：默认值是默认实例的 80 / 443 / 8081，具名实例用默认值必然冲突
	if *httpP == 0 || *httpsP == 0 || *status == 0 {
		return fmt.Errorf("%w：--http、--https、--status 都必须指定", errUsage)
	}
	return e.Add(instance.AddOptions{
		Profile: render.Profile{Instance: name, HTTPPort: *httpP, HTTPSPort: *httpsP, StatusPort: *status, NoFile: *nofile, ResourceMode: *resourceMode},
		Console: *console, Token: *token, CAFingerprint: *caFP,
	})
}

func remove(e instance.Env, args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	purge := fs.Bool("purge", false, "")
	yes := fs.Bool("yes", false, "")
	if err := fs.Parse(rest); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("%w：%v", errUsage, err)
	}
	return e.Remove(name, *purge, *yes)
}

func list(e instance.Env, out io.Writer) error {
	l, err := e.List()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "实例\t运行文件\tHTTP\tHTTPS\t状态页\t数据面")
	for _, in := range l {
		name, state := in.Name, "未运行"
		if name == "" {
			name = "(默认)"
		}
		if in.Running {
			state = fmt.Sprintf("运行中 pid %d", in.PID)
		}
		resources := "共享"
		if in.Profile.ResourceMode == "independent" {
			resources = "独立"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", name, resources, in.Profile.HTTP(), in.Profile.HTTPS(), in.Profile.Status(), state)
	}
	return tw.Flush()
}

// spawn 在后台启动进程（新会话，脱离终端），输出追加到 logFile，并写 pid 文件。
func spawn(pidFile, logFile, name string, args ...string) error {
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	c := exec.Command(name, args...) // 参数切片执行，不经过 shell
	c.Stdout, c.Stderr = lf, lf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)+"\n"), 0o644); err != nil {
		return err
	}
	return c.Process.Release()
}
