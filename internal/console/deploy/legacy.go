package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

var legacyReleaseToken = "@" + strings.ToUpper(brand.ID()) + "_RELEASE@"

const maxLegacyArchive = 20 << 20
const maxLegacyContents = 48 << 20
const maxLegacyFiles = 4000

var (
	ErrLegacyAdminRequired = errors.New("legacy configuration requires a separate administrator approval")
	ErrLegacyModeRequired  = errors.New("legacy configuration requires the legacy management mode")
)

// legacyMetadata is frozen into the bundle as the marker file. The agent
// enforces the embedded configbundle.LegacyPolicy fields.
type legacyMetadata struct {
	Checks []*agentv1.PostCheck `json:"checks"`
	configbundle.LegacyPolicy
}

// LegacyOptions are submitter declarations that become part of the frozen,
// separately approved bundle.
type LegacyOptions struct {
	// AllowRootMaster permits loading the bundle on nodes whose master runs as root.
	AllowRootMaster bool
}

const legacyRootMasterNote = "【已声明：允许在 master 以 root 运行的节点上加载】"

func isLegacyRevision(rev *store.Revision) bool {
	return rev != nil && configbundle.IsLegacy(rev.Bundle)
}

// Native configuration and scripts can contain credentials in any file.
// Comparison views therefore expose only filenames and hashes.
func diffFilesByHash(old, next map[string][]byte) []FileDiff {
	names := map[string]bool{}
	for name := range old {
		names[name] = true
	}
	for name := range next {
		names[name] = true
	}
	var sorted []string
	for name := range names {
		if name != render.ManifestName && name != configbundle.LegacyMarker {
			sorted = append(sorted, name)
		}
	}
	sort.Strings(sorted)
	out := []FileDiff{}
	for _, name := range sorted {
		a, inOld := old[name]
		b, inNew := next[name]
		change := "modified"
		switch {
		case !inOld:
			change = "added"
		case !inNew:
			change = "removed"
		case bytes.Equal(a, b):
			continue
		}
		oldHash, newHash := "-", "-"
		if inOld {
			oldHash = fmt.Sprintf("%x", sha256.Sum256(a))
		}
		if inNew {
			newHash = fmt.Sprintf("%x", sha256.Sum256(b))
		}
		out = append(out, FileDiff{Path: name, Change: change, Diff: "仅显示 SHA-256：旧 " + oldHash + "；新 " + newHash})
	}
	return out
}

func (d *Service) revisionFiles(rev *store.Revision) (map[string][]byte, error) {
	b, err := configbundle.Open(d.Box, rev.Bundle)
	if err != nil {
		return nil, err
	}
	return render.ReadTarGz(b)
}

// readLegacyArchive accepts only bounded regular files in a self-contained
// archive. It does not execute or load any of the uploaded code.
func readLegacyArchive(data []byte) (map[string][]byte, error) {
	if len(data) == 0 || len(data) > maxLegacyArchive {
		return nil, errors.New("兼容配置包为空或超过 20 MiB")
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("兼容配置包不是 tar.gz：%w", err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(io.LimitReader(zr, maxLegacyContents+(4<<20)))
	files := map[string][]byte{}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// Standard `tar -C directory -czf package.tar.gz .` uses ./ prefixes.
		for strings.HasPrefix(h.Name, "./") {
			h.Name = strings.TrimPrefix(h.Name, "./")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("兼容配置包不支持链接或特殊文件：%s", h.Name)
		}
		if !fs.ValidPath(h.Name) || h.Name == "." || path.Clean(h.Name) != h.Name || strings.Contains(h.Name, `\`) ||
			h.Name == render.ManifestName || h.Name == configbundle.LegacyMarker {
			return nil, fmt.Errorf("兼容配置包路径无效或使用保留名称：%s", h.Name)
		}
		if _, exists := files[h.Name]; exists {
			return nil, fmt.Errorf("兼容配置包路径重复：%s", h.Name)
		}
		if len(files) >= maxLegacyFiles || h.Size < 0 || h.Size > maxLegacyContents || total+h.Size > maxLegacyContents {
			return nil, errors.New("兼容配置包的文件数量或解压大小超限")
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) != h.Size {
			return nil, fmt.Errorf("文件长度不匹配：%s", h.Name)
		}
		files[h.Name] = body
		total += h.Size
	}
	if _, ok := files[brand.ConfName()]; !ok {
		return nil, fmt.Errorf("兼容配置包根目录必须包含 %s", brand.ConfName())
	}
	for name := range files {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, exists := files[dir]; exists {
				return nil, fmt.Errorf("文件和目录路径冲突：%s", dir)
			}
		}
	}
	return files, nil
}

// LegacyRequestBundle is exposed only through an administrator-only endpoint.
// It returns the frozen files for substantive review before approval. Private
// keys are replaced by their SHA-256: reviewers need the scripts and
// configuration, and key material must not leave the console in a response.
func (d *Service) LegacyRequestBundle(id uint, user, ip string) ([]byte, error) {
	rr := d.q().ReleaseRequest
	req, err := rr.WithContext(bg).Where(rr.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	rev, err := d.revision(req.RevisionID, req.GroupID)
	if err != nil {
		return nil, err
	}
	if !isLegacyRevision(rev) {
		return nil, ErrLegacyModeRequired
	}
	files, err := d.revisionFiles(rev)
	if err != nil {
		return nil, err
	}
	b, redacted, err := legacyReviewArchive(files)
	if err != nil {
		return nil, err
	}
	if err := d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "legacy.review_download", ObjectType: "release_request", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"group": req.GroupID, "revision": rev.ID, "digest": rev.Digest, "redacted_keys": redacted}}); err != nil {
		return nil, err
	}
	return b, nil
}

// isPrivateKeyFile matches the drift rule (*.key reports only a hash) and also
// catches PEM private keys stored under any other name.
func isPrivateKeyFile(name string, body []byte) bool {
	return strings.HasSuffix(strings.ToLower(name), ".key") || bytes.Contains(body, []byte("PRIVATE KEY-----"))
}

// legacyReviewArchive packs the frozen files for a reviewer, replacing private
// keys with a one-line hash placeholder. It returns the number of files replaced.
func legacyReviewArchive(files map[string][]byte) ([]byte, int, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	redacted := 0
	for _, name := range names {
		body := files[name]
		if isPrivateKeyFile(name, body) {
			body = []byte(fmt.Sprintf("私钥内容不随复核包下载。SHA-256: %x\n", sha256.Sum256(body)))
			redacted++
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), redacted, nil
}

// LegacyRequestIsRollback distinguishes a previously approved frozen version
// from a new package, so emergency rollback can remain available on expiry.
func (d *Service) LegacyRequestIsRollback(id uint) (bool, error) {
	req, err := d.pending(id)
	if err != nil {
		return false, err
	}
	rev, err := d.revision(req.RevisionID, req.GroupID)
	if err != nil {
		return false, err
	}
	if !isLegacyRevision(rev) {
		return false, ErrLegacyAdminRequired
	}
	rr := d.q().ReleaseRequest
	count, err := rr.WithContext(bg).Where(rr.GroupID.Eq(req.GroupID), rr.RevisionID.Eq(rev.ID), rr.Status.Eq(ApprovalApproved)).Count()
	return count > 0, err
}

func validateLegacyChecks(checks []*agentv1.PostCheck) error {
	if len(checks) > 100 {
		return errors.New("探活检查不能超过 100 项")
	}
	for _, check := range checks {
		if check == nil || len(check.Host) == 0 || len(check.Host) > 253 || strings.ContainsAny(check.Host, "\r\n\t /") ||
			len(check.Path) == 0 || len(check.Path) > 2048 || check.Path[0] != '/' || strings.ContainsAny(check.Path, "\r\n") {
			return errors.New("探活检查的域名或路径无效")
		}
		r, err := http.NewRequest(http.MethodGet, "http://"+check.Host+check.Path, nil)
		if err != nil || r.URL.Host != check.Host || r.URL.User != nil || r.URL.Fragment != "" {
			return errors.New("探活检查的域名或路径无效")
		}
		for _, status := range check.ExpectStatus {
			if status < 100 || status > 599 {
				return errors.New("探活状态码无效")
			}
		}
	}
	return nil
}

// RequestLegacy freezes an encrypted version and a pending two-person release
// request. Caller authentication and the administrator role are enforced by API.
func (d *Service) RequestLegacy(groupID uint, note string, data []byte, checks []*agentv1.PostCheck, opts LegacyOptions, user, ip string) (*store.ReleaseRequest, error) {
	if opts.AllowRootMaster {
		// The approver must see the declaration wherever the request is listed.
		note = strings.TrimSpace(note + " " + legacyRootMasterNote)
	}
	files, err := readLegacyArchive(data)
	if err != nil {
		return nil, err
	}
	// 静态提示只读包内文本，不改写、不阻止提交；写进发布说明，审批人在列表里就能看到。
	luaWarnings := lintLegacyLua(files)
	if n := legacyWarningNote(luaWarnings); n != "" {
		note = strings.TrimSpace(note + " " + n)
	}
	if err := validateLegacyChecks(checks); err != nil {
		return nil, err
	}
	if d.Box == nil {
		return nil, errors.New("兼容配置加密密钥不可用")
	}
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	defer lk.Unlock()
	if d.canaryRunning(groupID) {
		return nil, ErrCanaryRunning
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return nil, err
	}
	mode, err := d.ManagementMode(groupID)
	if err != nil {
		return nil, err
	}
	if mode != ManagementModeLegacy {
		return nil, ErrLegacyModeRequired
	}
	p, err := groupProfile(g)
	if err != nil {
		return nil, err
	}
	var req *store.ReleaseRequest
	err = d.q().Transaction(func(tx *query.Query) error {
		rr := tx.ReleaseRequest
		n, err := rr.WithContext(bg).Where(rr.GroupID.Eq(groupID), rr.Status.Eq(ApprovalPending)).Count()
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrPendingRequest
		}
		r := &store.Revision{ID: tempRevisionID(), GroupID: groupID, ParentID: g.CurrentRevisionID, Note: note, CreatedBy: user}
		if err := tx.Revision.WithContext(bg).Create(r); err != nil {
			return err
		}
		r.ID = fmt.Sprintf("r%06d", r.Seq)
		releaseDir := strings.TrimSuffix(p.ReleasesDir(), "/") + "/" + r.ID
		final := make(map[string][]byte, len(files)+1)
		for name, body := range files {
			if strings.HasSuffix(strings.ToLower(name), ".conf") || strings.HasSuffix(strings.ToLower(name), ".lua") {
				body = bytes.ReplaceAll(body, []byte(legacyReleaseToken), []byte(releaseDir))
			}
			final[name] = body
		}
		meta, err := json.Marshal(legacyMetadata{Checks: checks, LegacyPolicy: configbundle.LegacyPolicy{AllowRootMaster: opts.AllowRootMaster}})
		if err != nil {
			return err
		}
		final[configbundle.LegacyMarker] = meta
		bundle := render.Repack(r.ID, final)
		tgz, err := bundle.TarGz()
		if err != nil {
			return err
		}
		sealed, err := configbundle.SealLegacy(d.Box, tgz)
		if err != nil {
			return err
		}
		manifest, err := json.Marshal(bundle.Manifest)
		if err != nil {
			return err
		}
		r.Digest, r.Bundle, r.Manifest = bundle.Manifest.Digest, sealed, string(manifest)
		if err := tx.Revision.WithContext(bg).Save(r); err != nil {
			return err
		}
		req = &store.ReleaseRequest{GroupID: groupID, RevisionID: r.ID, BaseRevisionID: g.CurrentRevisionID,
			Digest: r.Digest, Status: ApprovalPending, Note: note, RequestedBy: user, RequestedAt: time.Now()}
		return rr.WithContext(bg).Create(req)
	})
	if err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "legacy.request", ObjectType: "release_request", ObjectID: fmt.Sprint(req.ID),
		Detail: map[string]any{"group": groupID, "revision": req.RevisionID, "digest": req.Digest, "files": len(files), "allow_root_master": opts.AllowRootMaster, "lua_warnings": legacyWarningAudit(luaWarnings)}})
	return req, nil
}

// ApproveLegacy may only be called by the administrator-only API endpoint.
// Mark the request approved before starting delivery, so a node can never fetch
// a package that is still awaiting its second administrator.
func (d *Service) ApproveLegacy(id uint, comment, user, ip string) (*store.Deployment, error) {
	req, err := d.pending(id)
	if err != nil {
		return nil, err
	}
	if req.RequestedBy == user {
		return nil, ErrSelfApproval
	}
	if d.canaryRunning(req.GroupID) {
		return nil, ErrCanaryRunning
	}
	lk := d.groupLock(req.GroupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	g, err := store.First[store.NodeGroup](d.Store, req.GroupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if g.CurrentRevisionID != req.BaseRevisionID {
		err := d.close(d.q(), id, ApprovalStale, "system", "线上版本已发生变化", 0)
		lk.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, ErrStaleRequest
	}
	rev, err := d.revision(req.RevisionID, req.GroupID)
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if !isLegacyRevision(rev) {
		lk.Unlock()
		return nil, ErrLegacyAdminRequired
	}
	kind := "publish"
	rr := d.q().ReleaseRequest
	prior, err := rr.WithContext(bg).Where(rr.GroupID.Eq(req.GroupID), rr.RevisionID.Eq(rev.ID), rr.Status.Eq(ApprovalApproved)).Count()
	if err != nil {
		lk.Unlock()
		return nil, err
	}
	if prior > 0 {
		kind = "rollback"
	}
	note := fmt.Sprintf("%s（兼容配置审批 #%d：%s 提交，%s 批准）", req.Note, id, req.RequestedBy, user)
	dep, err := d.startLegacy(lk, g, rev, kind, note, user, nil, func(depID uint) error {
		return d.close(d.q(), id, ApprovalApproved, user, comment, depID)
	})
	if err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "legacy.approve", ObjectType: "release_request", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"group": req.GroupID, "revision": rev.ID, "requested_by": req.RequestedBy, "deployment": dep.ID, "comment": comment}})
	return dep, nil
}

// RequestLegacyRollback requires a fresh independent approval even for a
// previously applied native version.
func (d *Service) RequestLegacyRollback(groupID uint, revID, note, user, ip string) (*store.ReleaseRequest, error) {
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	defer lk.Unlock()
	if d.canaryRunning(groupID) {
		return nil, ErrCanaryRunning
	}
	mode, err := d.ManagementMode(groupID)
	if err != nil {
		return nil, err
	}
	if mode != ManagementModeLegacy {
		return nil, ErrLegacyModeRequired
	}
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return nil, err
	}
	rev, err := d.revision(revID, groupID)
	if err != nil {
		return nil, err
	}
	if !isLegacyRevision(rev) {
		return nil, ErrLegacyModeRequired
	}
	if revID == g.CurrentRevisionID {
		return nil, ErrNoChanges
	}
	dp := d.q().Deployment
	n, err := dp.WithContext(bg).Where(dp.GroupID.Eq(groupID), dp.RevisionID.Eq(revID), dp.Status.In("APPLIED", "PENDING_NODES")).Count()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("只能回退到已成功发布的兼容版本")
	}
	rr := d.q().ReleaseRequest
	n, err = rr.WithContext(bg).Where(rr.GroupID.Eq(groupID), rr.Status.Eq(ApprovalPending)).Count()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrPendingRequest
	}
	req := &store.ReleaseRequest{GroupID: groupID, RevisionID: rev.ID, BaseRevisionID: g.CurrentRevisionID,
		Digest: rev.Digest, Status: ApprovalPending, Note: note, RequestedBy: user, RequestedAt: time.Now()}
	if err := rr.WithContext(bg).Create(req); err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "legacy.rollback_request", ObjectType: "release_request", ObjectID: fmt.Sprint(req.ID),
		Detail: map[string]any{"group": groupID, "revision": rev.ID}})
	return req, nil
}
