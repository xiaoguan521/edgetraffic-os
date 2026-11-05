package deploy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 静态资源发布（规格书 7.3 / P9）：上传 tar.gz 或 zip → 校验并规范化为不可变的资源包 → 站点静态路由引用 →
// 随配置版本下发，agent 原子切换版本目录（即原子切换 root）；回滚配置版本即回滚静态资源。

const maxStaticUpload = 64 << 20

func validateStaticArchiveEntry(name string) error {
	name = strings.ReplaceAll(name, "\\", "/")
	driveAbsolute := len(name) >= 2 && name[1] == ':' &&
		((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z'))
	if name == "" || strings.HasPrefix(name, "/") || driveAbsolute {
		return fmt.Errorf("资源包包含绝对路径 %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("资源包包含越界路径 %q", name)
		}
	}
	return nil
}

// extractStatic 读取 tar.gz / zip，返回规范化的文件表（去掉单一顶层目录、跳过目录与链接、校验路径与大小）。
func extractStatic(data []byte) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	add := func(name string, r io.Reader, size int64) error {
		if err := validateStaticArchiveEntry(name); err != nil {
			return err
		}
		name = strings.ReplaceAll(name, "\\", "/")
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		if strings.HasPrefix(path.Base(name), ".") || name == "" {
			return nil // 隐藏文件（.git、.DS_Store 等）不发布
		}
		if err := render.ValidateStaticPath(name); err != nil {
			return err
		}
		if len(files) >= render.MaxStaticFiles {
			return fmt.Errorf("资源包超过 %d 个文件", render.MaxStaticFiles)
		}
		if size > render.MaxStaticBytes {
			return errors.New("资源包过大")
		}
		b, err := io.ReadAll(io.LimitReader(r, render.MaxStaticBytes+1))
		if err != nil {
			return err
		}
		total += len(b)
		if total > render.MaxStaticBytes {
			return fmt.Errorf("资源包解压后超过 %d MiB", render.MaxStaticBytes>>20)
		}
		files[name] = b
		return nil
	}
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(zr)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
				return nil, fmt.Errorf("资源包包含符号链接或硬链接 %q，请打包实际文件", h.Name)
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if err := add(h.Name, tr, h.Size); err != nil {
				return nil, err
			}
		}
	} else if len(data) > 4 && string(data[:2]) == "PK" {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Mode()&fs.ModeSymlink != 0 {
				return nil, fmt.Errorf("资源包包含符号链接 %q，请打包实际文件", f.Name)
			}
			if f.FileInfo().IsDir() || !f.Mode().IsRegular() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = add(f.Name, rc, int64(f.UncompressedSize64))
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
		}
	} else {
		return nil, errors.New("只支持 .tar.gz 与 .zip")
	}
	if len(files) == 0 {
		return nil, errors.New("资源包中没有文件")
	}
	// 只有一个顶层目录时去掉它（常见的 dist/ 打包方式）
	tops := map[string]bool{}
	for n := range files {
		tops[strings.SplitN(n, "/", 2)[0]] = true
	}
	if len(tops) == 1 {
		for top := range tops {
			if _, isFile := files[top]; !isFile {
				out := map[string][]byte{}
				for n, b := range files {
					out[strings.TrimPrefix(n, top+"/")] = b
				}
				files = out
			}
		}
	}
	if _, ok := files["index.html"]; !ok {
		return nil, errors.New("资源包必须包含根目录 index.html")
	}
	return files, nil
}

// packStatic 把文件表打包为确定性的 tar.gz（同样内容得到同样哈希）。
func packStatic(files map[string][]byte) ([]byte, error) {
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ImportStatic 上传一个静态资源包。
func (d *Service) ImportStatic(groupID uint, name string, data []byte, user, ip string) (*store.StaticBundle, error) {
	if len(data) > maxStaticUpload {
		return nil, errors.New("上传文件过大")
	}
	if _, err := store.First[store.NodeGroup](d.Store, groupID); err != nil {
		return nil, err
	}
	files, err := extractStatic(data)
	if err != nil {
		return nil, err
	}
	norm, err := packStatic(files)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(norm)
	total := 0
	for _, b := range files {
		total += len(b)
	}
	row := &store.StaticBundle{GroupID: groupID, Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(total), Files: len(files), Data: norm, UploadedBy: user}
	if err := d.q().StaticBundle.WithContext(bg).Create(row); err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "static.upload", ObjectType: "static_bundle", ObjectID: fmt.Sprint(row.ID),
		Detail: map[string]any{"group": groupID, "name": name, "sha256": row.SHA256, "files": row.Files, "size": total}})
	return row, nil
}

// staticFiles 读取资源包内容（渲染时使用），资源包必须属于同一节点组。
func (d *Service) staticFiles(groupID, id uint) (map[string][]byte, error) {
	sb := d.q().StaticBundle
	row, err := sb.WithContext(bg).Where(sb.ID.Eq(id), sb.GroupID.Eq(groupID)).First()
	if err != nil {
		return nil, fmt.Errorf("静态资源包 #%d 不存在或不属于该节点组", id)
	}
	return render.ReadTarGz(row.Data)
}
