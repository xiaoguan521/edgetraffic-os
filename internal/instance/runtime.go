package instance

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// RuntimeTarget 描述功能包安装时可选择的实例和该实例的运行文件位置。
type RuntimeTarget struct {
	Name string
	Mode string
	Root string
}

// prepareInstancesDir 确保实例父目录不可被其他用户替换；只为新建目录设置权限，不更改已有目录。
func (e Env) prepareInstancesDir(l layout.Layout) error {
	if l.Instance() == "" {
		return nil
	}
	dir := e.path(l.InstancesDir())
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	owner := 0
	if e.user() || e.Root != "/" {
		owner = os.Getuid()
	}
	for _, p := range []string{dir, e.path(l.Prefix())} {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) && p != dir {
			continue
		}
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || int(sys.Uid) != owner || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("实例目录不安全：%s", p)
		}
	}
	return nil
}

// RuntimeTargets 返回所有实例；档案损坏或权限不安全时拒绝操作，避免漏掉被影响的实例。
func (e Env) RuntimeTargets() ([]RuntimeTarget, error) {
	parent := e.path(e.lay("").InstancesDir())
	if st, err := os.Lstat(parent); err == nil {
		owner := 0
		if e.user() || e.Root != "/" {
			owner = os.Getuid()
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || int(sys.Uid) != owner || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("实例目录不安全：%s", parent)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	list, err := e.installed()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, in := range list {
		seen[in.Name] = true
	}
	dir := e.path(e.lay("").InstancesDir())
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !layout.ValidInstance(name) || name == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, name, layout.InstanceFile)); err == nil && !seen[name] {
			return nil, fmt.Errorf("实例 %s 的档案无法读取或解析", name)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	// 安装中的基础节点可能只有程序文件、尚无 conf/current。
	if !seen[""] {
		if st, err := os.Stat(e.path(e.lay("").Bin())); err == nil && st.Mode().IsRegular() {
			list = append([]Info{{Name: ""}}, list...)
		}
	}
	var out []RuntimeTarget
	for _, in := range list {
		l := e.infoLayout(in)
		if in.Name != "" {
			if in.Profile.Instance != in.Name {
				return nil, fmt.Errorf("实例 %s 的档案名称不一致", in.Name)
			}
			if err := in.Profile.Validate(); err != nil {
				return nil, err
			}
			if e.user() {
				if in.Profile.Mode != string(layout.ModeUser) || in.Profile.Root != e.UserRoot {
					return nil, fmt.Errorf("实例 %s 不属于当前 user 安装", in.Name)
				}
			} else if (in.Profile.Mode != "" && in.Profile.Mode != string(layout.ModeSystem)) || (in.Profile.Root != "" && in.Profile.Root != l.Root()) {
				return nil, fmt.Errorf("实例 %s 不属于当前 system 安装", in.Name)
			}
			owner := 0
			if e.user() || e.Root != "/" {
				owner = os.Getuid()
			}
			for _, p := range []string{e.path(l.Prefix()), e.path(l.InstanceFile())} {
				st, err := os.Lstat(p)
				if err != nil {
					return nil, err
				}
				sys, ok := st.Sys().(*syscall.Stat_t)
				if !ok || int(sys.Uid) != owner || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 {
					return nil, fmt.Errorf("实例档案路径不安全：%s", p)
				}
			}
		}
		name, mode := in.Name, "shared"
		if name == "" {
			name = "default"
		}
		if l.IndependentRuntime() {
			mode = "independent"
		}
		out = append(out, RuntimeTarget{Name: name, Mode: mode, Root: filepath.Dir(filepath.Dir(l.Bin()))})
	}
	return out, nil
}

// prepareIndependentRuntime 为新具名实例复制当前数据面并创建独立版本库与模块目录。
func (e Env) prepareIndependentRuntime(l layout.Layout) error {
	if !l.IndependentRuntime() {
		return nil
	}
	for _, d := range []string{filepath.Dir(l.Bin()), l.Versions(), l.Modules()} {
		p := e.path(d)
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("independent runtime directory already exists: %s", d)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return err
		}
	}
	src, err := os.Open(e.path(filepath.Join(l.Root(), "sbin", brand.ID())))
	if err != nil {
		return fmt.Errorf("read shared data plane: %w", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(e.path(l.Bin()), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("create independent data plane: %w", err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(e.path(l.Bin()), 0o755); err != nil {
		return err
	}
	if !e.user() {
		for _, d := range []string{filepath.Dir(l.Bin()), l.Versions(), l.Modules()} {
			if err := e.Run("chown", layout.WorkerUser()+":"+layout.Group(), d); err != nil {
				return err
			}
		}
	}
	return nil
}

// preserveInstanceData 在删除自定义根目录的实例前，将内嵌日志与缓存移到同一根目录的归档目录。
func (e Env) preserveInstanceData(l layout.Layout) (string, error) {
	items := []struct{ name, path string }{{"logs", l.LogDir()}, {"cache", l.CacheDir()}}
	var present []struct{ name, path string }
	for _, item := range items {
		st, err := os.Lstat(e.path(item.path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("不能保留非目录：%s", item.path)
		}
		present = append(present, item)
	}
	if len(present) == 0 {
		return "", nil
	}
	parent := e.path(filepath.Join(l.Root(), "removed-instances"))
	if _, err := os.Lstat(parent); os.IsNotExist(err) {
		if err := os.Mkdir(parent, 0o755); err != nil {
			return "", err
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	st, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	owner := 0
	if e.Root != "/" {
		owner = os.Getuid()
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != owner || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("归档目录不安全：%s", parent)
	}
	dest, err := os.MkdirTemp(parent, l.Instance()+"-")
	if err != nil {
		return "", err
	}
	for _, item := range present {
		if err := os.Rename(e.path(item.path), filepath.Join(dest, item.name)); err != nil {
			return dest, fmt.Errorf("保留 %s 失败（已保留的文件位于 %s）：%w", item.path, dest, err)
		}
	}
	return dest, nil
}

// writeIndependentUnits 只覆盖该实例的程序路径，并允许 agent 调用的 wrapper 写入其独立运行目录。
func (e Env) writeIndependentUnits(l layout.Layout) error {
	unit := fmt.Sprintf(`[Service]
ExecStartPre=
ExecStartPre=%[1]s -t -q -e %[2]s -c %[3]s
ExecStart=
ExecStart=%[1]s -e %[2]s -c %[3]s
ExecReload=
ExecReload=%[1]s -e %[2]s -c %[3]s -s reload
ExecStop=
ExecStop=%[1]s -e %[2]s -c %[3]s -s quit
`, l.Bin(), l.ErrorLog(), l.CurrentConf())
	agent := fmt.Sprintf("[Service]\nReadWritePaths=%s %s %s\n", filepath.Dir(l.Bin()), l.Versions(), l.Modules())
	for _, item := range []struct{ unit, body string }{{l.DataplaneUnit(), unit}, {l.AgentUnit(), agent}} {
		dir := e.path("/etc/systemd/system/" + item.unit + ".d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "runtime.conf"), []byte(item.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
