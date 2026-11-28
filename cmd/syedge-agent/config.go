package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/configfile"
)

type agentOptions struct {
	instance, root, server, stateDir, confRoot, wrapper string
	token, caFP                                         string
	replaceLocal, importConfig, importCerts             bool
	configSpecified                                     bool
}

// 支持 run -c 文件以及 -c 文件 run，配置不适用于防火墙和构建专用子命令。
func parseAgentInvocation(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("缺少命令")
	}
	var config string
	leading := false
	switch {
	case args[0] == "--config" || args[0] == "-config" || args[0] == "-c" || args[0] == "--c":
		if len(args) < 3 {
			return "", nil, fmt.Errorf("配置文件参数需要路径和命令")
		}
		config, args, leading = args[1], args[2:], true
	case strings.HasPrefix(args[0], "--config=") || strings.HasPrefix(args[0], "-config=") || strings.HasPrefix(args[0], "-c=") || strings.HasPrefix(args[0], "--c="):
		_, config, _ = strings.Cut(args[0], "=")
		args, leading = args[1:], true
	}
	if len(args) == 0 || (leading && config == "") {
		return "", nil, fmt.Errorf("配置文件参数需要路径和命令")
	}
	cmd, rest := args[0], args[1:]
	if leading {
		if cmd == "firewall" || cmd == "package-defaults" {
			return "", nil, fmt.Errorf("%s 不使用 agent 配置文件", cmd)
		}
		rest = append([]string{"--config", config}, rest...)
	}
	return cmd, rest, nil
}

func parseAgentOptions(cmd string, args []string) (agentOptions, error) {
	return parseAgentOptionsTo(cmd, args, io.Discard)
}

func parseAgentOptionsTo(cmd string, args []string, out io.Writer) (agentOptions, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(out)
	var o agentOptions
	var config string
	fs.StringVar(&config, "config", "", "配置文件（KEY=VALUE，也可用 -c）")
	fs.StringVar(&config, "c", "", "配置文件")
	fs.StringVar(&o.instance, "instance", envOr(brand.Env("INSTANCE"), ""), "托管的实例名")
	fs.StringVar(&o.root, "root", envOr(brand.Env("ROOT"), ""), "user 模式的安装根目录")
	fs.StringVar(&o.server, "server", envOr(brand.Env("CONSOLE"), ""), "控制台 gRPC 地址 host:port")
	fs.StringVar(&o.stateDir, "state-dir", envOr(brand.Env("STATE_DIR"), ""), "身份与状态目录")
	fs.StringVar(&o.confRoot, "conf-root", envOr(brand.Env("CONF_ROOT"), ""), "数据面配置根目录")
	fs.StringVar(&o.wrapper, "wrapper", envOr(brand.Env("WRAPPER"), ""), "提权入口")
	fs.StringVar(&o.token, "token", "", "一次性注册令牌（enroll）")
	fs.StringVar(&o.caFP, "ca-fingerprint", "", "控制台 CA 指纹（enroll）")
	fs.BoolVar(&o.replaceLocal, "replace-local-config", false, "备份独立配置并由控制台接管（enroll）")
	fs.BoolVar(&o.importConfig, "import-config", false, "注册后自动将本机当前配置上报控制台生成草稿（enroll，默认不含私钥）")
	fs.BoolVar(&o.importCerts, "import-certs", false, "随 mTLS 加密通道一并上报配置目录内引用的证书与私钥（enroll / push-config）")
	if err := fs.Parse(args); err != nil {
		return agentOptions{}, err
	}
	if config == "" {
		if configfile.Specified(fs, "config") || configfile.Specified(fs, "c") {
			return agentOptions{}, fmt.Errorf("配置文件路径不能为空")
		}
		return o, nil
	}
	o.configSpecified = true
	fields := []struct {
		key, name string
		dst       *string
		path      bool
	}{
		{brand.Env("INSTANCE"), "instance", &o.instance, false}, {brand.Env("ROOT"), "root", &o.root, true},
		{brand.Env("CONSOLE"), "server", &o.server, false}, {brand.Env("STATE_DIR"), "state-dir", &o.stateDir, true},
		{brand.Env("CONF_ROOT"), "conf-root", &o.confRoot, true}, {brand.Env("WRAPPER"), "wrapper", &o.wrapper, false},
	}
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.key)
	}
	c, err := configfile.Load(config, keys)
	if err != nil {
		return agentOptions{}, err
	}
	for _, f := range fields {
		if configfile.Specified(fs, f.name) {
			continue
		}
		var v string
		var ok bool
		if f.path {
			v, ok = c.Path(f.key)
		} else {
			v, ok = c.Value(f.key)
		}
		if ok {
			*f.dst = v
		}
	}
	return o, nil
}
