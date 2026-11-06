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
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/conn"
	"github.com/xiaoguan521/edgetraffic-os/internal/agent/selfupdate"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
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
			fmt.Fprintln(os.Stderr, "usage: "+brand.Agent()+" package-defaults -out <安装根目录的暂存目录>")
			os.Exit(2)
		}
		if err := render.WritePackageDefaults(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	instance := fs.String("instance", envOr(brand.Env("INSTANCE"), ""), "托管的实例名（同机多实例；默认实例留空），路径与端口由实例档案决定")
	root := fs.String("root", envOr(brand.Env("ROOT"), ""), "user 模式的安装根目录（普通用户安装，不需要 root；留空为 system 模式）")
	server := fs.String("server", envOr(brand.Env("CONSOLE"), ""), "控制台 gRPC 地址 host:port")
	stateDir := fs.String("state-dir", "", "身份与状态目录（默认按实例计算）")
	confRoot := fs.String("conf-root", "", "数据面配置根目录（默认按实例计算）")
	wrapper := fs.String("wrapper", envOr(brand.Env("WRAPPER"), ""), "提权入口（按空格切分，不经过 shell；默认按实例计算）")
	token := fs.String("token", "", "一次性注册令牌（enroll）")
	caFP := fs.String("ca-fingerprint", "", "控制台 CA 证书 sha256 指纹（enroll）")
	replaceLocal := fs.Bool("replace-local-config", false, "本机有独立模式下的自定义配置时，备份后由控制台接管（enroll）")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prof, lay, err := loadInstanceAt(*root, *instance)
	if err != nil {
		fatal(log, cmd, err)
	}
	userMode := *root != ""
	if userMode && os.Geteuid() == 0 {
		fatal(log, cmd, fmt.Errorf("user-mode agent (-root) must not run as root"))
	}
	if *stateDir == "" {
		*stateDir = lay.AgentState()
	}
	if *confRoot == "" {
		*confRoot = lay.ConfRoot()
	}
	if *wrapper == "" {
		// wrapper 以数据面用户运行（不是 root，设计 0001 D5），sudoers 只允许这 7 条命令
		*wrapper = "sudo -n -u " + brand.ID() + " " + lay.WrapperBin()
		if userMode {
			*wrapper = lay.WrapperBin() // 与数据面同一用户：直接调用，校验与验签规则不变
		}
		if lay.Instance() != "" {
			*wrapper += " --instance " + lay.Instance()
		}
	}
	cfg := conn.Config{
		Server: *server, StateDir: *stateDir, DataplaneBin: lay.Bin(), Modules: lay.Modules(),
		StatusURL: lay.StatusURL(prof.Status()), VersionURL: lay.VersionURL(prof.Status()), AccessLog: lay.AccessLog(), Profile: prof,
		Apply: apply.Config{
			ConfRoot: *confRoot, PidFile: lay.PidFile(), ErrorLog: lay.ErrorLog(),
			LockFile: lay.AgentLock(), Wrapper: strings.Fields(*wrapper),
			HTTPPort: prof.HTTP(), HTTPSPort: prof.HTTPS(),
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
		// agent 自升级：已安装且签名、哈希校验通过的新版本优先（只 exec 一次）
		selfupdate.MaybeExec(*stateDir, version.Version, runtime.GOARCH, pkgsig.Keys())
		a := conn.New(cfg, log)
		if cfg.Server == "" {
			cfg.Server = serverFromState(*stateDir)
			a = conn.New(cfg, log)
		}
		if userMode {
			go rotateLogs(ctx, log, lay, strings.Fields(*wrapper))
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
	b, err := render.BaselineFor(cfg.Profile)
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
	fmt.Fprintln(os.Stderr, "usage: "+brand.Agent()+" bootstrap | enroll -server H:P -token T -ca-fingerprint F | run | version | firewall render|covers ...")
	os.Exit(2)
}
