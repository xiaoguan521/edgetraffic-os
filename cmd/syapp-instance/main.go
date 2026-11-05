// syapp-instance 管理同机多实例（设计 0001 第 5.7 节），以 root 运行。
// 安装后的命令名为 <品牌>-instance；构建：CGO_ENABLED=0 go build -trimpath ./cmd/syapp-instance
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/instance"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Geteuid()))
}

func usage(w io.Writer) {
	n := brand.ID() + "-instance"
	_, _ = fmt.Fprintf(w, `用法：
  %[1]s add <实例名> --http <端口> --https <端口> --status <端口> [--nofile N]
        [--console <控制台 host:port> --token <注册令牌> --ca-fingerprint <指纹>]
  %[1]s remove <实例名> [--purge] [--yes]     不加 --yes 只显示将执行的操作
  %[1]s list
  %[1]s check
  %[1]s reopen-all                            所有实例重新打开日志（logrotate 使用）
`, n)
}

func env(out io.Writer) instance.Env {
	return instance.Env{
		Root: "/",
		Out:  out,
		Run: func(name string, args ...string) error {
			c := exec.Command(name, args...) // 参数切片执行，不经过 shell
			c.Stdout, c.Stderr = out, os.Stderr
			return c.Run()
		},
		Listening: func() (map[string]bool, error) { return instance.ListeningFromProc("/") },
	}
}

func run(args []string, stdout, stderr io.Writer, euid int) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage(stdout)
		return 0
	}
	if euid != 0 && cmd != "list" && cmd != "check" {
		_, _ = fmt.Fprintln(stderr, brand.ID()+"-instance: 请以 root 运行")
		return 1
	}
	e := env(stdout)
	var err error
	switch cmd {
	case "add":
		err = add(e, rest)
	case "remove":
		err = remove(e, rest)
	case "list":
		err = list(e, stdout)
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
		Profile: render.Profile{Instance: name, HTTPPort: *httpP, HTTPSPort: *httpsP, StatusPort: *status, NoFile: *nofile},
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
	_, _ = fmt.Fprintln(tw, "实例\tHTTP\tHTTPS\t状态页\t数据面")
	for _, in := range l {
		name, state := in.Name, "未运行"
		if name == "" {
			name = "(默认)"
		}
		if in.Running {
			state = fmt.Sprintf("运行中 pid %d", in.PID)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", name, in.Profile.HTTP(), in.Profile.HTTPS(), in.Profile.Status(), state)
	}
	return tw.Flush()
}
