// syapp-agent：部署在每台边缘节点的探针（规格书第 5 节）。
// 只主动连接控制台，不监听任何端口。
//
//	syapp-agent bootstrap                       # 安装时以 root 运行：写入初始版本 r000000
//	syapp-agent enroll -server H:P -token T -ca-fingerprint F
//	syapp-agent run    [-server H:P]
//	syapp-agent version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/conn"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "firewall" {
		os.Exit(firewallMain(args, os.Stdout, os.Stderr))
	}
	if cmd == "package-defaults" { // 打包时使用：写入初始版本与独立模式文件
		if len(args) != 2 || args[0] != "-out" {
			fmt.Fprintln(os.Stderr, "usage: syapp-agent package-defaults -out <opt/syapp 暂存目录>")
			os.Exit(2)
		}
		if err := render.WritePackageDefaults(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	server := fs.String("server", envOr("SYAPP_CONSOLE", ""), "控制台 gRPC 地址 host:port")
	stateDir := fs.String("state-dir", "/var/lib/syapp-agent", "身份与状态目录")
	confRoot := fs.String("conf-root", "/opt/syapp/conf", "syapp 配置根目录")
	wrapper := fs.String("wrapper", envOr("SYAPP_WRAPPER", "sudo -n /usr/libexec/syapp/syapp-wrapper"), "提权入口（按空格切分，不经过 shell）")
	token := fs.String("token", "", "一次性注册令牌（enroll）")
	caFP := fs.String("ca-fingerprint", "", "控制台 CA 证书 sha256 指纹（enroll）")
	replaceLocal := fs.Bool("replace-local-config", false, "本机有独立模式下的自定义配置时，备份后由控制台接管（enroll）")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := conn.Config{
		Server: *server, StateDir: *stateDir, SyappBin: "/opt/syapp/sbin/syapp",
		StatusURL: "http://127.0.0.1:8081/__syapp_status", AccessLog: "/var/log/syapp/access.json",
		Apply: apply.Config{
			ConfRoot: *confRoot, PidFile: "/run/syapp/syapp.pid", ErrorLog: "/var/log/syapp/error.log",
			LockFile: "/run/syapp-agent/apply.lock", Wrapper: strings.Fields(*wrapper),
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version", "-version", "--version":
		fmt.Println(version.String("agent"))
	case "bootstrap":
		if err := bootstrap(cfg); err != nil {
			fatal(log, "bootstrap", err)
		}
		log.Info("bootstrap done", "current", (&apply.Applier{C: cfg.Apply}).Current())
	case "enroll":
		if cfg.Server == "" || *token == "" || *caFP == "" {
			fatal(log, "enroll", fmt.Errorf("-server, -token and -ca-fingerprint are required"))
		}
		// 每个令牌只能用一次，能拿到新令牌说明管理员要求（重新）注册本机：
		// 例如节点被吊销后重新添加。新身份只在控制台接受后才替换旧身份，失败时旧身份保持不变。
		a := conn.New(cfg, log)
		had := a.Enrolled()
		if !had {
			bk, err := localConfigCheck(&apply.Applier{C: cfg.Apply}, *replaceLocal, filepath.Join(*stateDir, "backups"), time.Now())
			if err != nil {
				fatal(log, "enroll", err)
			}
			if bk != "" {
				log.Warn("local standalone configuration backed up; it keeps running until the first publish from the console", "backup", bk)
			}
		}
		if err := a.Enroll(ctx, *token, *caFP); err != nil {
			if had {
				log.Error("enroll failed; existing identity kept unchanged", "err", err)
				os.Exit(1)
			}
			fatal(log, "enroll", err)
		}
		if had {
			log.Info("re-enrolled: previous identity replaced; a running agent picks it up on its next reconnect")
		}
	case "run":
		a := conn.New(cfg, log)
		if cfg.Server == "" {
			cfg.Server = serverFromState(*stateDir)
			a = conn.New(cfg, log)
		}
		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			fatal(log, "run", err)
		}
	default:
		usage()
	}
}

// bootstrap 写入空站点的初始版本 r000000，使 syapp 能以基线配置启动。
func bootstrap(cfg conn.Config) error {
	b, err := render.Baseline()
	if err != nil {
		return err
	}
	tgz, err := b.TarGz()
	if err != nil {
		return err
	}
	return (&apply.Applier{C: cfg.Apply}).Bootstrap(render.BaselineRevision, tgz, b.Manifest.Digest)
}

func serverFromState(dir string) string {
	b, err := os.ReadFile(dir + "/node.json")
	if err != nil {
		return ""
	}
	const key = `"server": "`
	s := string(b)
	if i := strings.Index(s, key); i >= 0 {
		s = s[i+len(key):]
		if j := strings.IndexByte(s, '"'); j >= 0 {
			return s[:j]
		}
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what+" failed", "err", err)
	time.Sleep(100 * time.Millisecond)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: syapp-agent bootstrap | enroll -server H:P -token T -ca-fingerprint F | run | version | firewall render|covers ...")
	os.Exit(2)
}
