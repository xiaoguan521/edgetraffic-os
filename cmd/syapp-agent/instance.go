package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// loadInstance 返回 agent 托管的实例的档案与布局（设计 0001 第 5.4 节）。
// 默认实例不需要档案文件；具名实例必须已由 <品牌>-instance add 创建（<实例前缀>/instance.json）。
func loadInstance(name string) (render.Profile, layout.Layout, error) {
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
	if p.Mode != "" && p.Mode != string(layout.ModeSystem) {
		return p, fmt.Errorf("instance %s is a %s-mode install", name, p.Mode)
	}
	return p, nil
}
