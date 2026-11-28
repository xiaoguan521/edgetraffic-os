package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// RevisionFile 描述某个历史版本中的一个文件（私钥与兼容包不暴露内容）。
type RevisionFile struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	SHA256  string `json:"sha256"`
	Content string `json:"content,omitempty"`
}

const maxInspectFileBytes = 128 << 10

// RevisionFiles 返回指定版本的脱敏文件列表。兼容包与私钥仅返回路径与哈希。
func (d *Service) RevisionFiles(groupID uint, revID string) ([]RevisionFile, bool, error) {
	rev, err := d.revision(revID, groupID)
	if err != nil {
		return nil, false, err
	}
	files, err := d.revisionFiles(rev)
	if err != nil {
		return nil, false, err
	}
	legacy := isLegacyRevision(rev)
	names := make([]string, 0, len(files))
	for name := range files {
		if name != render.ManifestName {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]RevisionFile, 0, len(names))
	for _, name := range names {
		b := files[name]
		sum := sha256.Sum256(b)
		item := RevisionFile{
			Path:   name,
			Size:   len(b),
			SHA256: hex.EncodeToString(sum[:]),
		}
		if legacy {
			item.Content = "（兼容配置包只显示文件路径与哈希，不展示内容）"
		} else if strings.HasSuffix(name, ".key") {
			item.Content = "（私钥内容不显示）"
		} else if name == "mime.types" || len(b) > maxInspectFileBytes || !utf8.Valid(b) {
			item.Content = "（文件较大或为二进制内容，未展开）"
		} else {
			item.Content = string(b)
		}
		out = append(out, item)
	}
	return out, legacy, nil
}

// CompareRevisions 比较同一节点组任意两个历史版本的文件差异。
func (d *Service) CompareRevisions(groupID uint, fromID, toID string) ([]FileDiff, error) {
	fromRev, err := d.revision(fromID, groupID)
	if err != nil {
		return nil, err
	}
	toRev, err := d.revision(toID, groupID)
	if err != nil {
		return nil, err
	}
	oldFiles, err := d.revisionFiles(fromRev)
	if err != nil {
		return nil, err
	}
	newFiles, err := d.revisionFiles(toRev)
	if err != nil {
		return nil, err
	}
	if isLegacyRevision(fromRev) || isLegacyRevision(toRev) {
		return diffFilesByHash(oldFiles, newFiles), nil
	}
	return diffFiles(oldFiles, newFiles), nil
}
