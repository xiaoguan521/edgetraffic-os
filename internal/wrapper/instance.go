package wrapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// InstanceFile 是实例前缀下的实例档案（render.Profile 的 JSON），由 <品牌>-instance add 以 root 写入。
const InstanceFile = layout.InstanceFile

// CheckInstance 确认具名实例确实由 root 创建（设计 0001 第 5.5 节）：wrapper 不为不存在或被篡改的实例执行任何操作。
// 默认实例（空串）不需要检查。
func CheckInstance(instance string) error {
	if instance == "" {
		return nil
	}
	return checkInstanceAt(layout.System(instance).Prefix(), instance, 0)
}

// CheckInstanceUser 是 user 模式下的检查（设计 0001 第 6.2 节）：实例档案属于当前用户、他人不可写，
// 为 user 模式且根目录与 wrapper 所在的根目录一致。默认实例也必须有档案（端口来自它）。
func CheckInstanceUser(root, instance string, uid int) error {
	l, err := layout.User(root, instance)
	if err != nil {
		return err
	}
	return checkAt(l.Prefix(), instance, uid, string(layout.ModeUser), root)
}

// checkInstanceAt 是 CheckInstance 的实现；owner 为要求的属主 uid（生产固定为 0，测试使用当前用户）。
func checkInstanceAt(prefix, instance string, owner int) error {
	return checkAt(prefix, instance, owner, string(layout.ModeSystem), "")
}

func checkAt(prefix, instance string, owner int, mode, root string) error {
	if err := ownedNotShared(prefix, owner, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("instance %s is not installed", instance)
		}
		return err
	}
	path := filepath.Join(prefix, InstanceFile)
	if err := ownedNotShared(path, owner, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("instance %s is not installed (no %s)", instance, InstanceFile)
		}
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// 只读取 wrapper 需要的两个字段（与 render.Profile 的 JSON 一致）；提权程序不引入渲染器
	var p struct {
		Instance string `json:"instance"`
		Mode     string `json:"mode"`
		Root     string `json:"root"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("%s: %w", InstanceFile, err)
	}
	if p.Instance != instance {
		return fmt.Errorf("%s names instance %q, which does not match %q", InstanceFile, p.Instance, instance)
	}
	got := p.Mode
	if got == "" {
		got = string(layout.ModeSystem)
	}
	if got != mode {
		return fmt.Errorf("instance %s is a %s-mode install; this wrapper manages %s-mode instances", instance, got, mode)
	}
	if mode == string(layout.ModeUser) && p.Root != root {
		return fmt.Errorf("%s names install root %q, but this wrapper belongs to %q", InstanceFile, p.Root, root)
	}
	return nil
}

// ownedNotShared 要求 path（不跟随软链接）属于 owner，且不可被组或其他用户写入。
func ownedNotShared(path string, owner int, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case dir && !fi.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s is writable by group or others", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != owner {
		return fmt.Errorf("%s must be owned by uid %d (owned by another user)", path, owner)
	}
	return nil
}
