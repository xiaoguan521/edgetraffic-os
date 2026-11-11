package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// repairPackageBaseline 更新自定义安装目录下软件包初始配置的清单，保留全部
// 本机文件。清单只按软件包默认文件计算，因此独立模式的手工改动仍会被检测。
func repairPackageBaseline(confRoot, defaults string) error {
	fi, err := os.Lstat(confRoot)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid configuration directory %s", confRoot)
	}
	conf, err := os.OpenRoot(confRoot)
	if err != nil {
		return err
	}
	defer func() { _ = conf.Close() }()
	current, err := conf.Readlink("current")
	if err != nil || current != filepath.Join("releases", render.BaselineRevision) {
		return nil // current 已由管理员或控制台切换，不修改任何版本
	}
	rel := filepath.Join("releases", render.BaselineRevision)
	for _, dir := range []string{"releases", rel} {
		fi, err := conf.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid package baseline directory %s", dir)
		}
	}
	revision, err := conf.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer func() { _ = revision.Close() }()
	fi, err = revision.Lstat(render.ManifestName)
	if err != nil || !fi.Mode().IsRegular() {
		return nil // 不替换已被删除或手工改为非普通文件的清单
	}
	old, err := revision.ReadFile(render.ManifestName)
	if err != nil {
		return err
	}
	packaged, err := os.ReadFile(filepath.Join(defaults, render.ManifestName))
	if err != nil {
		return err
	}
	if !bytes.Equal(old, packaged) {
		return nil // 不是原始软件包清单，保留用户或其他版本的清单
	}
	var packageManifest render.Manifest
	if err := json.Unmarshal(packaged, &packageManifest); err != nil || packageManifest.Revision != render.BaselineRevision || len(packageManifest.Files) == 0 {
		return fmt.Errorf("invalid package baseline manifest")
	}
	source, err := os.OpenRoot(defaults)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	files := map[string][]byte{}
	for name := range packageManifest.Files {
		if !fs.ValidPath(name) || name == "." || name == render.ManifestName {
			return fmt.Errorf("invalid package baseline file %s", name)
		}
		fi, err := source.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("invalid package baseline file %s", name)
		}
		b, err := source.ReadFile(name)
		if err != nil {
			return err
		}
		files[name] = b
	}
	data := render.Repack(render.BaselineRevision, files).Files[render.ManifestName]
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmpName := ".manifest-" + hex.EncodeToString(nonce[:])
	tmp, err := revision.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = revision.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return revision.Rename(tmpName, render.ManifestName)
}

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
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("生成备份文件名失败：%w", err)
	}
	out := filepath.Join(backupDir, "standalone-"+now.UTC().Format("20060102T150405Z")+"-"+hex.EncodeToString(nonce[:])+".tar.gz")
	if err := tarDir(dir, out); err != nil {
		return "", fmt.Errorf("备份本机配置失败：%w", err)
	}
	return out, nil
}

func tarDir(dir, out string) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(out)
		}
	}()
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
		_, err = io.Copy(tw, src)
		if closeErr := src.Close(); err == nil {
			err = closeErr
		}
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
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
