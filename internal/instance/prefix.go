package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// Prefix 写入 system 模式安装根目录标记。标记只在首次安装时可改变；已有
// 安装或已有标记都不允许通过该命令迁移到另一个目录。
func (e Env) Prefix(root string) error {
	if err := layout.ValidateSystemRoot(root); err != nil {
		return err
	}
	marker := e.path(layout.PrefixMarker())
	current, err := layout.LoadSystemRootAt(e.Root)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(marker); err == nil {
		if current == root {
			e.say("安装根目录已是 %s", root)
			return nil
		}
		return fmt.Errorf("已有安装根目录 %s，不允许改为 %s（不执行迁移）", current, root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// 标记缺失时，默认安装一旦已经有 current，就不能把已有数据解释成新根目录。
	def := layout.SystemAt(current, "")
	if root != current {
		if _, err := os.Lstat(e.path(filepath.Join(def.ConfRoot(), "current"))); err == nil {
			return fmt.Errorf("检测到已有默认安装 %s，不允许设置自定义根目录（不执行迁移）", def.Root())
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := layout.CheckSystemRootAt(e.Root, root); err != nil {
		return err
	}
	markerDir := e.path(filepath.Dir(layout.PrefixMarker()))
	_, statErr := os.Stat(markerDir)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		// 新建目录不受调用者的 umask 影响；agent 与 wrapper 需要读取标记。
		if err := os.Chmod(markerDir, 0o755); err != nil {
			return err
		}
	}
	if err := writeFile(marker, []byte(root+"\n"), 0o644); err != nil {
		return err
	}
	if e.Root == "" || e.Root == "/" {
		_ = os.Chown(marker, 0, 0)
	}
	if got, err := layout.LoadSystemRootAt(e.Root); err != nil || got != root {
		if err != nil {
			return err
		}
		return fmt.Errorf("写入安装根目录标记后校验不一致：得到 %s，期望 %s", got, root)
	}
	e.say("已设置 system 安装根目录：%s", root)
	return nil
}
