package instance

import (
	"os"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

func prefixUnitHeader() string { return "# " + brand.ID() + "-instance prefix 生成" }

// prefixUnit 把软件包提供的 systemd 单元改写为自定义安装根目录。它只改写
// 路径与数据面启动参数，并且对已生成的内容再次执行仍得到相同结果。
func prefixUnit(src, root string) string {
	if root == "" || root == layout.DefaultSystemRoot() {
		return src
	}
	var b strings.Builder
	b.WriteString(prefixUnitHeader() + "（安装根目录：" + root + "）。请勿手改：\n")
	b.WriteString("# 每次执行 " + brand.ID() + "-instance nonroot 都会按软件包单元重新生成；删除安装根目录标记后执行 nonroot 恢复\n")
	for _, raw := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, prefixUnitHeader()) || strings.HasPrefix(t, "# 每次执行 "+brand.ID()+"-instance nonroot 都会按软件包单元重新生成；删除安装根目录标记") {
			continue
		}
		if strings.HasPrefix(t, "StateDirectory=") {
			continue
		}
		line := raw
		for _, r := range []struct{ from, to string }{
			{"/var/lib/" + brand.Agent() + "-%i", root + "/instances/%i/agent"},
			{"/var/lib/" + brand.Agent(), root + "/agent"},
			{"/var/log/" + brand.ID() + "-%i", root + "/instances/%i/logs"},
			{"/var/log/" + brand.ID(), root + "/logs"},
			{"/var/cache/" + brand.ID() + "-%i", root + "/instances/%i/cache"},
			{"/var/cache/" + brand.ID(), root + "/cache"},
			{"/opt/" + brand.ID(), root},
		} {
			line = strings.ReplaceAll(line, r.from, r.to)
		}
		if strings.Contains(line, "/sbin/"+brand.ID()) && (strings.HasPrefix(t, "ExecStart") || strings.HasPrefix(t, "ExecReload") || strings.HasPrefix(t, "ExecStop")) {
			conf := root + "/conf/current/" + brand.ConfName()
			errlog := root + "/logs/error.log"
			if strings.Contains(line, "/instances/%i/") {
				conf = root + "/instances/%i/conf/current/" + brand.ConfName()
				errlog = root + "/instances/%i/logs/error.log"
			}
			if !strings.Contains(line, " -c ") {
				line += " -c " + conf
			}
			if !strings.Contains(line, " -e ") {
				line += " -e " + errlog
			}
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// customSystemRoot 报告本机 system 模式是否使用了非默认根目录。
func (e Env) customSystemRoot() (string, bool) {
	root := e.lay("").Root()
	return root, root != layout.DefaultSystemRoot()
}

var prefixUnits = []string{
	brand.ID() + ".service",
	brand.ID() + "@.service",
	brand.Agent() + ".service",
	brand.Agent() + "@.service",
	brand.Firewall() + "-refresh.path",
}

// writePrefixUnits 为自定义根目录生成 /etc/systemd/system 下的完整覆盖单元。
func (e Env) writePrefixUnits() error {
	root, custom := e.customSystemRoot()
	if !custom {
		return nil
	}
	units := append([]string(nil), prefixUnits...)
	sort.Strings(units)
	for _, unit := range units {
		src, data, ok := e.unitSource(unit)
		if !ok {
			continue
		}
		dst := e.path("/etc/systemd/system/" + unit)
		if err := writeFile(dst, []byte(prefixUnit(string(data), root)), 0o644); err != nil {
			return err
		}
		e.say("   %s：已按 %s 生成自定义根目录单元 /etc/systemd/system/%s", unit, src, unit)
	}
	return e.writeLogrotate(root)
}

func (e Env) writeLogrotate(root string) error {
	body := root + "/logs/*.log " + root + "/logs/*.json " + root + "/instances/*/logs/*.log " + root + "/instances/*/logs/*.json {\n" +
		"    daily\n    su " + layout.WorkerUser() + " " + layout.Group() + "\n    rotate 14\n    missingok\n    notifempty\n    compress\n    delaycompress\n    sharedscripts\n    postrotate\n" +
		"        /usr/sbin/" + brand.ID() + "-instance reopen-all >/dev/null 2>&1 || true\n" +
		"    endscript\n}\n"
	return writeFile(e.path("/etc/logrotate.d/"+brand.ID()), []byte(body), 0o644)
}

// removePrefixUnits 恢复默认根目录时删除由 prefixUnit 生成的 systemd 覆盖。
// 自定义根目录一旦写入后按安装约定不做迁移；该函数仅用于兼容测试和手工切回。
func (e Env) removePrefixUnits() {
	for _, unit := range prefixUnits {
		p := e.path("/etc/systemd/system/" + unit)
		b, err := os.ReadFile(p)
		if err != nil || !strings.HasPrefix(string(b), prefixUnitHeader()) {
			continue
		}
		vendor := false
		for _, d := range vendorUnitDirs {
			if _, err := os.Stat(e.path(d + "/" + unit)); err == nil {
				vendor = true
			}
		}
		if vendor {
			_ = os.Remove(p)
		}
	}
	_ = os.Remove(e.path("/etc/logrotate.d/" + brand.ID()))
}
