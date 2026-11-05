package apply

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 漂移 diff 采集（规格书 12.7 路径 B）：只读取受管版本目录（releases/<当前版本>）内的文件。
// 私钥文件（*.key）只报哈希；单文件超过 MaxDriftFile 只报哈希；最多 MaxDriftFiles 个文件、合计 MaxDriftBytes。

const (
	MaxDriftFile  = 256 << 10
	MaxDriftBytes = 2 << 20
	MaxDriftFiles = 200
)

// DriftFile 是一个与清单不一致的文件。
type DriftFile struct {
	Path    string
	Change  string // modified | added | removed
	SHA256  string
	Size    int64
	Content []byte // 私钥或超限时为空
	Omitted bool
}

// DriftReport 是当前版本目录相对清单的全部差异。
type DriftReport struct {
	Revision string
	TreeHash string // 实际文件状态的指纹（用于去重：同一状态只上报一次）
	Files    []DriftFile
}

// Drift 比较当前版本目录与清单；没有漂移时返回 nil。
func (a *Applier) Drift() (*DriftReport, error) {
	dir, err := filepath.EvalSymlinks(a.link("current"))
	if err != nil {
		return nil, err
	}
	// Compare canonical paths on both sides: ConfRoot may itself contain a
	// symlink (for example /var -> /private/var on macOS).
	releases, err := filepath.EvalSymlinks(a.releases())
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(releases, dir)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, string(filepath.Separator)) || !revRe.MatchString(rel) {
		return nil, nil // current 不在受管版本目录内：不读取
	}
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	rep := &DriftReport{Revision: rel}
	state := sha256.New()
	seen := map[string]bool{}
	total := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		r, _ := filepath.Rel(dir, p)
		r = filepath.ToSlash(r)
		if r == manifestName {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			// 手工放入的链接或特殊文件：只报存在，不跟随
			rep.Files = append(rep.Files, DriftFile{Path: r, Change: "added", Omitted: true})
			state.Write([]byte(r + ":link\n"))
			return nil
		}
		seen[r] = true
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		state.Write([]byte(r + ":" + h + "\n"))
		want, ok := m.Files[r]
		if ok && want == h {
			return nil
		}
		f := DriftFile{Path: r, Change: "modified", SHA256: h, Size: int64(len(b))}
		if !ok {
			f.Change = "added"
		}
		if strings.HasSuffix(r, ".key") || len(b) > MaxDriftFile || total+len(b) > MaxDriftBytes || len(rep.Files) >= MaxDriftFiles {
			f.Omitted = true
		} else {
			f.Content = b
			total += len(b)
		}
		rep.Files = append(rep.Files, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for r := range m.Files {
		if !seen[r] {
			rep.Files = append(rep.Files, DriftFile{Path: r, Change: "removed"})
			state.Write([]byte(r + ":removed\n"))
		}
	}
	if len(rep.Files) == 0 {
		return nil, nil
	}
	if len(rep.Files) > MaxDriftFiles {
		rep.Files = rep.Files[:MaxDriftFiles]
	}
	sort.Slice(rep.Files, func(i, j int) bool { return rep.Files[i].Path < rep.Files[j].Path })
	rep.TreeHash = "sha256:" + hex.EncodeToString(state.Sum(nil))
	return rep, nil
}
