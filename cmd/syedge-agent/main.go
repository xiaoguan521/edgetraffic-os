// syedge-agent：部署在每台边缘节点的探针（规格书第 5 节）。
// 只主动连接控制台，不监听任何端口。
//
//	syedge-agent bootstrap                       # 安装时以 root 运行：写入初始版本 r000000
//	syedge-agent enroll -server H:P -token T -ca-fingerprint F
//	syedge-agent run    [-server H:P]
//	syedge-agent version
package main

import (
	"context"
	"errors"
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
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args, err := parseAgentInvocation(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, brand.Agent()+":", err)
		os.Exit(2)
	}
	if cmd == "firewall" {
		os.Exit(firewallMain(args, os.Stdout, os.Stderr))
	}
	if cmd == "backup-config" {
		if err := backupConfig(args, os.Stdout); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, brand.Agent()+":", err)
			os.Exit(1)
		}
		return
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
	o, err := parseAgentOptionsTo(cmd, args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, brand.Agent()+":", err)
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prof, lay, err := loadInstanceAt(o.root, o.instance)
	if err != nil {
		fatal(log, cmd, err)
	}
	userMode := o.root != ""
	if userMode && os.Geteuid() == 0 {
		fatal(log, cmd, fmt.Errorf("user-mode agent (-root) must not run as root"))
	}
	if o.stateDir == "" {
		o.stateDir = lay.AgentState()
	}
	if o.confRoot == "" {
		o.confRoot = lay.ConfRoot()
	}
	if o.wrapper == "" {
		o.wrapper = defaultWrapper(lay, userMode, !userMode && layout.RootMasterAt("/"))
	}
	cfg := conn.Config{
		Server: o.server, StateDir: o.stateDir, DataplaneBin: lay.Bin(), Modules: lay.Modules(),
		StatusURL: lay.StatusURL(prof.Status()), VersionURL: lay.VersionURL(prof.Status()), AccessLog: lay.AccessLog(), Profile: prof,
		Apply: apply.Config{
			ConfRoot: o.confRoot, Prefix: lay.Prefix(), PidFile: lay.PidFile(), ErrorLog: lay.ErrorLog(),
			LockFile: lay.AgentLock(), Wrapper: strings.Fields(o.wrapper),
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
	case "repair-package-baseline":
		if err := repairPackageBaseline(cfg.Apply.ConfRoot, filepath.Join(lay.Root(), "share", "defaults", "baseline")); err != nil {
			fatal(log, cmd, err)
		}
	case "enroll":
		if cfg.Server == "" || o.token == "" || o.caFP == "" {
			fatal(log, "enroll", fmt.Errorf("-server, -token and -ca-fingerprint are required"))
		}
		// 每个令牌只能用一次，能拿到新令牌说明管理员要求（重新）注册本机：
		// 例如节点被吊销后重新添加。新身份只在控制台接受后才替换旧身份，失败时旧身份保持不变。
		a := conn.New(cfg, log)
		had := a.Enrolled()
		if !had || o.replaceLocal {
			bk, err := localConfigCheck(&apply.Applier{C: cfg.Apply}, o.replaceLocal, filepath.Join(o.stateDir, "backups"), time.Now())
			if err != nil {
				fatal(log, "enroll", err)
			}
			if bk != "" {
				log.Warn("current configuration directory backed up; external includes and dependencies require a separate backup-config snapshot", "backup", bk)
			}
		}
		if err := a.Enroll(ctx, o.token, o.caFP); err != nil {
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
		if err := selfupdate.MaybeExec(o.stateDir, version.Version, runtime.GOARCH, pkgsig.Keys(), o.configSpecified); err != nil {
			log.Warn("agent self-update skipped", "err", err)
		}
		a := conn.New(cfg, log)
		if cfg.Server == "" {
			cfg.Server = serverFromState(o.stateDir)
			a = conn.New(cfg, log)
		}
		if userMode {
			go rotateLogs(ctx, log, lay, strings.Fields(o.wrapper))
		}
		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			fatal(log, "run", err)
		}
	default:
		usage()
	}
}

// bootstrap 写入空站点的初始版本 r000000，使 syedge 能以基线配置启动。
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
	fmt.Fprintln(os.Stderr, "usage: "+brand.Agent()+" bootstrap | enroll -server H:P -token T -ca-fingerprint F | run [-c <配置文件>] | backup-config --config <主配置> --source <文件或目录> --output <备份> | version | firewall render|covers ...")
	os.Exit(2)
}

// defaultWrapper 返回调用 wrapper 的命令：wrapper 以数据面用户运行（不是 root，设计 0001 D5），sudoers 只允许这 7 条命令；
// 兼容模式（master 以 root 运行，与传统 Web 服务器相同）以 root 运行；user 模式与数据面同一用户，直接调用。
func defaultWrapper(lay layout.Layout, userMode, rootMaster bool) string {
	w := "sudo -n -u " + brand.ID() + " " + lay.WrapperBin()
	switch {
	case userMode:
		w = lay.WrapperBin()
	case rootMaster:
		w = "sudo -n " + lay.WrapperBin()
	}
	if lay.Instance() != "" {
		w += " --instance " + lay.Instance()
	}
	return w
}
