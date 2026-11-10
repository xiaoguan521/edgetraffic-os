package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/configfile"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

type instanceDefaults struct {
	http, https, status, nofile int
	resourceMode, console       string
}

type invocation struct {
	root, cmd string
	rest      []string
	defaults  instanceDefaults
}

// parseInvocation 接受命令前或子命令参数中的配置选项。
func parseInvocation(args []string) (invocation, error) {
	var inv invocation
	var config string
	var rootSet, configSet bool
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--root" || arg == "--config" || arg == "-config" || arg == "-c" || arg == "--c":
			if len(args) < 2 {
				return inv, fmt.Errorf("%w：用法：%s 需要路径", errUsage, arg)
			}
			if arg == "--root" {
				inv.root, rootSet = args[1], true
			} else {
				config, configSet = args[1], true
			}
			args = args[2:]
		case strings.HasPrefix(arg, "--root="):
			inv.root, rootSet = strings.TrimPrefix(arg, "--root="), true
			args = args[1:]
		case strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--c="):
			_, config, _ = strings.Cut(arg, "=")
			configSet = true
			args = args[1:]
		default:
			goto command
		}
	}
command:
	if len(args) == 0 {
		return inv, fmt.Errorf("%w：用法：缺少命令", errUsage)
	}
	inv.cmd, inv.rest = args[0], args[1:]
	clean, subConfig, subSet, err := extractCommandConfig(inv.rest)
	if err != nil {
		return inv, err
	}
	if subSet && configSet {
		return inv, fmt.Errorf("%w：只能指定一个配置文件", errUsage)
	}
	if subSet {
		config, configSet = subConfig, true
	}
	inv.rest = clean
	if configSet && config == "" {
		return inv, fmt.Errorf("%w：配置文件路径不能为空", errUsage)
	}
	if configSet {
		keys := []string{brand.Env("ROOT"), brand.Env("HTTP"), brand.Env("HTTPS"), brand.Env("STATUS"), brand.Env("NOFILE"), brand.Env("RESOURCE_MODE"), brand.Env("CONSOLE")}
		c, err := configfile.Load(config, keys)
		if err != nil {
			return inv, err
		}
		if v, ok := c.Path(brand.Env("ROOT")); ok && !rootSet {
			inv.root = v
		}
		for _, f := range []struct {
			key string
			dst *int
		}{
			{brand.Env("HTTP"), &inv.defaults.http}, {brand.Env("HTTPS"), &inv.defaults.https},
			{brand.Env("STATUS"), &inv.defaults.status}, {brand.Env("NOFILE"), &inv.defaults.nofile},
		} {
			if v, ok := c.Value(f.key); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					return inv, fmt.Errorf("%s 必须是整数", f.key)
				}
				*f.dst = n
			}
		}
		if v, ok := c.Value(brand.Env("RESOURCE_MODE")); ok {
			inv.defaults.resourceMode = v
		}
		if v, ok := c.Value(brand.Env("CONSOLE")); ok {
			inv.defaults.console = v
		}
	}
	if rootSet && inv.root == "" {
		return inv, fmt.Errorf("%w：--root 不能为空", errUsage)
	}
	if inv.root != "" {
		if _, err := layout.User(inv.root, ""); err != nil {
			return inv, fmt.Errorf("--root: %w", err)
		}
	}
	return inv, nil
}

// 已知的带值参数必须整体跳过，避免把它们的值误当作 -c。
func extractCommandConfig(args []string) ([]string, string, bool, error) {
	var clean []string
	var config string
	found := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		switch {
		case arg == "--config" || arg == "-config" || arg == "-c" || arg == "--c":
			if found || i+1 >= len(args) {
				return nil, "", false, fmt.Errorf("%w：配置文件参数无效", errUsage)
			}
			i++
			config, found = args[i], true
		case strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--c="):
			if found {
				return nil, "", false, fmt.Errorf("%w：只能指定一个配置文件", errUsage)
			}
			_, config, _ = strings.Cut(arg, "=")
			found = true
		default:
			clean = append(clean, arg)
			switch arg {
			case "--http", "-http", "--https", "-https", "--status", "-status", "--nofile", "-nofile", "--resource-mode", "-resource-mode", "--console", "-console", "--token", "-token", "--ca-fingerprint", "-ca-fingerprint":
				if i+1 < len(args) {
					i++
					clean = append(clean, args[i])
				}
			}
		}
	}
	return clean, config, found, nil
}
