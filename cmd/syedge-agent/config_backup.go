package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type backupSources []string

func (s *backupSources) String() string { return strings.Join(*s, ", ") }
func (s *backupSources) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type backupEntry struct {
	path string
	info fs.FileInfo
	link string
}

type backupRoot struct {
	path string
	dir  bool
}

func coveredByBackupRoot(target string, roots []backupRoot) bool {
	for _, root := range roots {
		if target == root.path {
			return true
		}
		if root.dir {
			rel, err := filepath.Rel(root.path, target)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				return true
			}
		}
	}
	return false
}

// backupConfig is a local, operator-initiated snapshot. It never runs via the
// agent protocol and does not infer that unknown includes are safe to omit.
func backupConfig(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("backup-config", flag.ContinueOnError)
	flags.SetOutput(out)
	config := flags.String("config", "", "数据面实际使用的主配置绝对路径")
	output := flags.String("output", "", "新建的备份 tar.gz 绝对路径")
	var sources backupSources
	flags.Var(&sources, "source", "需要额外备份的文件或目录（可重复）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*config) || !filepath.IsAbs(*output) {
		return errors.New("请提供 --config 和 --output 绝对路径；外部依赖用 --source 指定")
	}
	configPath := filepath.Clean(*config)
	fi, err := os.Stat(configPath)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("主配置不是可读的普通文件：%s", configPath)
	}
	sources = append([]string{configPath}, sources...)
	roots := make([]backupRoot, 0, len(sources))
	for i, source := range sources {
		if !filepath.IsAbs(source) || filepath.Clean(source) == string(filepath.Separator) {
			return fmt.Errorf("--source 需要非根目录的绝对路径：%s", source)
		}
		sources[i] = filepath.Clean(source)
		info, err := os.Lstat(sources[i])
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved, err := filepath.EvalSymlinks(sources[i])
			if err != nil {
				return err
			}
			roots = append(roots, backupRoot{path: resolved, dir: info.IsDir()})
		}
	}
	outputPath := filepath.Clean(*output)
	parent, err := filepath.EvalSymlinks(filepath.Dir(outputPath))
	if err != nil {
		return fmt.Errorf("备份目录不可访问：%w", err)
	}
	if coveredByBackupRoot(filepath.Join(parent, filepath.Base(outputPath)), roots) {
		return errors.New("备份文件不能放在备份源目录中")
	}
	var entries []backupEntry
	seen := map[string]bool{}
	for _, source := range sources {
		err := filepath.WalkDir(source, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if seen[p] {
				return nil
			}
			seen[p] = true
			info, err := d.Info()
			if err != nil {
				return err
			}
			entry := backupEntry{path: p, info: info}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				entry.link, err = os.Readlink(p)
				if err != nil {
					return err
				}
				target, err := filepath.EvalSymlinks(p)
				if err != nil {
					return fmt.Errorf("无法解析软链接 %s：%w", p, err)
				}
				if !coveredByBackupRoot(target, roots) {
					return fmt.Errorf("软链接 %s 的目标未包含在备份源中，请添加 --source %s", p, target)
				}
			case info.IsDir(), info.Mode().IsRegular():
			default:
				return fmt.Errorf("不支持备份特殊文件 %s", p)
			}
			entries = append(entries, entry)
			return nil
		})
		if err != nil {
			return err
		}
	}
	// If --config traverses a selected link such as conf/current, the same
	// file is also included via releases/. Do not archive a regular file under
	// the link path: extracting it would first create a directory that prevents
	// the current symlink from being restored.
	configThroughLink := false
	for _, entry := range entries {
		if entry.info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		rel, err := filepath.Rel(entry.path, configPath)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			configThroughLink = true
		}
	}
	if configThroughLink {
		foundTarget := false
		for _, entry := range entries {
			if entry.path != configPath && entry.info.Mode().IsRegular() && os.SameFile(fi, entry.info) {
				foundTarget = true
				break
			}
		}
		if !foundTarget {
			return errors.New("主配置经过软链接，但目标文件未包含在备份源中，请补充 --source")
		}
	}
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	metadata, err := json.MarshalIndent(struct {
		Config  string   `json:"config"`
		Sources []string `json:"sources"`
	}{configPath, sources}, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "backup-manifest.json", Mode: 0o600, Size: int64(len(metadata)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(metadata); err != nil {
		return err
	}
	for _, entry := range entries {
		if configThroughLink && entry.path == configPath {
			continue
		}
		h, err := tar.FileInfoHeader(entry.info, entry.link)
		if err != nil {
			return err
		}
		h.Name = "rootfs/" + strings.TrimPrefix(filepath.ToSlash(entry.path), "/")
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if entry.info.Mode().IsRegular() {
			src, err := os.OpenFile(entry.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			current, statErr := src.Stat()
			if statErr != nil || !os.SameFile(entry.info, current) {
				_ = src.Close()
				return fmt.Errorf("备份期间文件发生变化：%s", entry.path)
			}
			_, copyErr := io.Copy(tw, src)
			closeErr := src.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
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
	ok = true
	_, _ = fmt.Fprintf(out, "已备份 %d 个文件或目录到 %s；请核对主配置中引用的路径均已包含。\n", len(entries), outputPath)
	return nil
}
