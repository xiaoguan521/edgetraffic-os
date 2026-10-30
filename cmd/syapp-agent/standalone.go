package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
)

// localConfigCheck 在首次注册前检查本机是否有独立模式下的手工配置（规格书 12.7：受管节点上没有合法的手工改动）。
//
// 未改动过的初始版本：直接注册。有改动时默认拒绝，避免被控制台的第一次发布静默替换；
// replace 为 true 时先把当前配置备份为 0600 的 tar.gz 再继续，返回备份路径。
func localConfigCheck(ap *apply.Applier, replace bool, backupDir string, now time.Time) (string, error) {
	if ap.Current() == "" {
		return "", nil
	}
	d, err := ap.TreeDigest()
	if err != nil {
		// 没有清单（例如手工建的目录）同样视为本机自定义配置
		d = "drifted: " + err.Error()
	}
	if !strings.HasPrefix(d, "drifted") {
		return "", nil
	}
	if !replace {
		return "", fmt.Errorf("本机有独立模式下的自定义配置（%s）。注册后控制台的发布会替换它；"+
			"请先在控制台录入这些站点，或确认由控制台接管：加 -replace-local-config（当前配置会先备份）", strings.TrimPrefix(d, "drifted:"))
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(ap.C.ConfRoot, "current"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(backupDir, "standalone-"+now.UTC().Format("20060102T150405Z")+".tar.gz")
	if err := tarDir(dir, out); err != nil {
		os.Remove(out)
		return "", fmt.Errorf("备份本机配置失败：%w", err)
	}
	return out, nil
}

func tarDir(dir, out string) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil // 不跟随软链等特殊文件
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Sync()
}
