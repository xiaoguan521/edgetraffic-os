package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/logrotate"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// loadInstance 返回 agent 托管的实例的档案与布局（设计 0001 第 5.4 节）。
// 默认实例不需要档案文件；具名实例必须已由 <品牌>-instance add 创建（<实例前缀>/instance.json）。
func loadInstance(name string) (render.Profile, layout.Layout, error) {
	return loadInstanceAt("", name)
}

// loadInstanceAt 同 loadInstance；root 非空时为 user 模式（设计 0001 第 6 节）：默认实例也必须有档案（端口来自它）。
func loadInstanceAt(root, name string) (render.Profile, layout.Layout, error) {
	if root != "" {
		lay, err := layout.User(root, name)
		if err != nil {
			return render.Profile{}, layout.Layout{}, err
		}
		b, err := os.ReadFile(lay.InstanceFile())
		if err != nil {
			return render.Profile{}, lay, fmt.Errorf("user-mode instance is not installed: %w", err)
		}
		p, err := parseInstanceMode(b, name, layout.ModeUser)
		if err != nil {
			return render.Profile{}, lay, fmt.Errorf("%s: %w", lay.InstanceFile(), err)
		}
		if p.Root != root {
			return render.Profile{}, lay, fmt.Errorf("%s names install root %q, not %q", lay.InstanceFile(), p.Root, root)
		}
		return p, lay, nil
	}
	if name == "" {
		return render.Profile{}, layout.System(""), nil
	}
	if !layout.ValidInstance(name) {
		return render.Profile{}, layout.Layout{}, fmt.Errorf("invalid instance name %q", name)
	}
	lay := layout.System(name)
	b, err := os.ReadFile(lay.InstanceFile())
	if err != nil {
		return render.Profile{}, lay, fmt.Errorf("instance %s is not installed: %w", name, err)
	}
	p, err := parseInstance(b, name)
	if err != nil {
		return render.Profile{}, lay, fmt.Errorf("%s: %w", lay.InstanceFile(), err)
	}
	return p, lay, nil
}

// parseInstance 解析并校验实例档案：名称一致、档案合法、为 system 模式。
func parseInstance(b []byte, name string) (render.Profile, error) {
	return parseInstanceMode(b, name, layout.ModeSystem)
}

func parseInstanceMode(b []byte, name string, mode layout.Mode) (render.Profile, error) {
	var p render.Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.Instance != name {
		return p, fmt.Errorf("names instance %q, not %q", p.Instance, name)
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	got := layout.Mode(p.Mode)
	if got == "" {
		got = layout.ModeSystem
	}
	if got != mode {
		return p, fmt.Errorf("instance %q is a %s-mode install, expected %s mode", name, got, mode)
	}
	return p, nil
}

// rotateLogs 在 user 模式下替代系统 logrotate：每 5 分钟检查一次，按天或超过 256 MiB 轮转，保留 14 份。
func rotateLogs(ctx context.Context, log *slog.Logger, lay layout.Layout, wrapper []string) {
	r := &logrotate.Rotator{
		Files:    []string{lay.AccessLog(), lay.ErrorLog(), lay.LogDir() + "/stream.log"},
		Keep:     14,
		MaxBytes: 256 << 20,
		Reopen: func() error {
			args := append(append([]string{}, wrapper[1:]...), "reopen")
			return exec.CommandContext(ctx, wrapper[0], args...).Run() // 参数切片执行，不经过 shell
		},
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		if n, err := r.Check(); err != nil {
			log.Warn("log rotation", "err", err)
		} else if n > 0 {
			log.Info("logs rotated", "files", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
