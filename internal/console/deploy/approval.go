package deploy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/textdiff"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"gorm.io/gen/field"
)

// 发布审批（规格书 12.3 P7，双人复核）。
//
// 开启后，普通发布先生成一个冻结的版本并进入待审批；另一位发布员（提交人 ≠ 批准人）看过差异后批准，
// 下发的就是提交时冻结的那个版本。以下操作不经审批：回滚（目标是已发布过的版本，且必须在授权过期时可用）、
// 漂移恢复、证书续期与 ACME 刷新（规格书 8.6：任何授权状态下可用）、节点上线自动对齐。

// 审批状态。
const (
	ApprovalPending   = "pending"
	ApprovalApproved  = "approved"
	ApprovalRejected  = "rejected"
	ApprovalCancelled = "cancelled"
	ApprovalStale     = "stale" // 提交后线上版本已变化，需要重新提交
)

const approvalKey = "release_approval"

var (
	ErrApprovalRequired = errors.New("release approval is enabled: submit a release request instead")
	ErrPendingRequest   = errors.New("this node group already has a pending release request")
	ErrSelfApproval     = errors.New("the requester cannot approve their own release")
	ErrRequestClosed    = errors.New("release request is no longer pending")
	ErrStaleRequest     = errors.New("the live revision changed after this request was submitted; submit a new request")
	ErrForbidden        = errors.New("only the requester or an administrator can cancel this request")
)

// ApprovalEnabled 返回是否开启了发布审批。
func (d *Service) ApprovalEnabled() bool {
	kv := d.q().SystemKV
	row, err := kv.WithContext(bg).Where(kv.Key.Eq(approvalKey)).First()
	return err == nil && row.Value == "on"
}

// SetApproval 开启或关闭发布审批（授权检查由 API 层负责：开启需要 release_approval 功能）。
func (d *Service) SetApproval(on bool, user, ip string) error {
	v := "off"
	if on {
		v = "on"
	}
	if err := d.q().SystemKV.WithContext(bg).Save(&store.SystemKV{Key: approvalKey, Value: v}); err != nil {
		return err
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "settings.release_approval", ObjectType: "settings", ObjectID: approvalKey,
		Detail: map[string]string{"value": v}})
}

// RequestRelease 生成冻结版本并提交审批，不改变线上版本。
func (d *Service) RequestRelease(groupID uint, note, user, ip string) (*store.ReleaseRequest, error) {
	lk := d.groupLock(groupID)
	if !lk.TryLock() {
		return nil, ErrBusy
	}
	defer lk.Unlock()
	g, err := store.First[store.NodeGroup](d.Store, groupID)
	if err != nil {
		return nil, err
	}
	if err := d.ensureConfigManaged(groupID); err != nil {
		return nil, err
	}
	rr := d.q().ReleaseRequest
	n, err := rr.WithContext(bg).Where(rr.GroupID.Eq(groupID), rr.Status.Eq(ApprovalPending)).Count()
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrPendingRequest
	}
	_, digest, err := d.Preview(groupID)
	if err != nil {
		return nil, err
	}
	pending, err := d.handoffPending(groupID)
	if err != nil {
		return nil, err
	}
	if g.CurrentRevisionID != "" && !pending {
		if cur, err := d.revision(g.CurrentRevisionID, 0); err == nil && cur.Digest == digest {
			return nil, ErrNoChanges
		}
	}
	rev, err := d.newRevision(groupID, g.CurrentRevisionID, note, user)
	if err != nil {
		return nil, err
	}
	req := &store.ReleaseRequest{GroupID: groupID, RevisionID: rev.ID, BaseRevisionID: g.CurrentRevisionID, Digest: rev.Digest,
		Status: ApprovalPending, Note: note, RequestedBy: user, RequestedAt: time.Now()}
	if err := rr.WithContext(bg).Create(req); err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.request", ObjectType: "release_request", ObjectID: fmt.Sprint(req.ID),
		Detail: map[string]any{"group": groupID, "revision": rev.ID, "base": g.CurrentRevisionID, "digest": rev.Digest, "note": note}})
	return req, nil
}

// RequestDetail 是审批详情：请求本身加上冻结版本相对提交时线上版本的差异。
type RequestDetail struct {
	*store.ReleaseRequest
	GroupName      string     `json:"group_name"`
	Files          []FileDiff `json:"files"`
	Stale          bool       `json:"stale"` // 待审批但线上版本已变化
	Legacy         bool       `json:"legacy"`
	LegacyRollback bool       `json:"legacy_rollback"`
}

// GetRequest 返回审批详情。
func (d *Service) GetRequest(id uint) (*RequestDetail, error) {
	rr := d.q().ReleaseRequest
	req, err := rr.WithContext(bg).Where(rr.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	out := &RequestDetail{ReleaseRequest: req, Files: []FileDiff{}}
	if g, err := store.First[store.NodeGroup](d.Store, req.GroupID); err == nil {
		out.GroupName = g.Name
		out.Stale = req.Status == ApprovalPending && g.CurrentRevisionID != req.BaseRevisionID
	}
	rev, err := d.revision(req.RevisionID, req.GroupID)
	if err != nil {
		return nil, err
	}
	nb, err := d.revisionFiles(rev)
	if err != nil {
		return nil, err
	}
	old := map[string][]byte{}
	redact := isLegacyRevision(rev)
	out.Legacy = redact
	if redact && req.Status == ApprovalPending {
		out.LegacyRollback, err = d.LegacyRequestIsRollback(id)
		if err != nil {
			return nil, err
		}
	}
	if req.BaseRevisionID != "" {
		if base, err := d.revision(req.BaseRevisionID, req.GroupID); err == nil {
			old, err = d.revisionFiles(base)
			if err != nil {
				return nil, err
			}
			redact = redact || isLegacyRevision(base)
		}
	}
	if redact {
		out.Files = diffFilesByHash(old, nb)
	} else {
		out.Files = diffFiles(old, nb)
	}
	return out, nil
}

// ListRequests 列出审批请求（groupID 为 0 表示全部；status 为空表示全部状态），最多 100 条。
func (d *Service) ListRequests(groupID uint, status string) ([]*store.ReleaseRequest, error) {
	rr := d.q().ReleaseRequest
	sel := rr.WithContext(bg).Order(rr.ID.Desc()).Limit(100)
	if groupID != 0 {
		sel = sel.Where(rr.GroupID.Eq(groupID))
	}
	if status != "" {
		sel = sel.Where(rr.Status.Eq(status))
	}
	return sel.Find()
}

// close 把待审批的请求改为终态；并发处理时只有一个能成功。
func (d *Service) close(tx *query.Query, id uint, status, user, comment string, depID uint) error {
	rr := tx.ReleaseRequest
	set := []field.AssignExpr{rr.Status.Value(status), rr.ReviewedBy.Value(user), rr.ReviewedAt.Value(time.Now()), rr.ReviewComment.Value(comment)}
	if depID != 0 {
		set = append(set, rr.DeploymentID.Value(depID))
	}
	res, err := rr.WithContext(bg).Where(rr.ID.Eq(id), rr.Status.Eq(ApprovalPending)).UpdateSimple(set...)
	if err != nil {
		return err
	}
	if res.RowsAffected != 1 {
		return ErrRequestClosed
	}
	return nil
}

func (d *Service) pending(id uint) (*store.ReleaseRequest, error) {
	rr := d.q().ReleaseRequest
	req, err := rr.WithContext(bg).Where(rr.ID.Eq(id)).First()
	if err != nil {
		return nil, store.ErrNotFound
	}
	if req.Status != ApprovalPending {
		return nil, ErrRequestClosed
	}
	return req, nil
}

// Approve 批准并下发冻结的版本。批准人不能是提交人；线上版本在提交后变化过则请求过期。
func (d *Service) Approve(id uint, comment, user, ip string) (*store.Deployment, error) {
	req, err := d.pending(id)
	if err != nil {
		return nil, err
	}
	if req.RequestedBy == user {
		return nil, ErrSelfApproval
	}
	target, err := d.revision(req.RevisionID, req.GroupID)
	if err != nil {
		return nil, err
	}
	if isLegacyRevision(target) {
		return nil, ErrLegacyAdminRequired
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
	if err := d.ensureConfigManaged(req.GroupID); err != nil {
		lk.Unlock()
		return nil, err
	}
	if g.CurrentRevisionID != req.BaseRevisionID {
		err := d.close(d.q(), id, ApprovalStale, "system", "线上版本已从 "+req.BaseRevisionID+" 变为 "+g.CurrentRevisionID, 0)
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
	note := strings.TrimSpace(fmt.Sprintf("%s（审批 #%d：%s 提交，%s 批准）", req.Note, req.ID, req.RequestedBy, user))
	// 先把请求原子地改为已批准，再向节点下发：读取 pending 到这里之间另一位管理员可能已驳回或撤回，
	// 此时 close 返回 ErrRequestClosed，发布记录标记失败，不会向任何节点发送已关闭的请求的版本。
	// start 负责释放锁。
	dep, err := d.startWithMode(lk, g, rev, "publish", note, user, nil, false, func(depID uint) error {
		return d.close(d.q(), id, ApprovalApproved, user, comment, depID)
	})
	if err != nil {
		return nil, err
	}
	_ = d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.approve", ObjectType: "release_request", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"group": req.GroupID, "revision": rev.ID, "requested_by": req.RequestedBy, "deployment": dep.ID, "comment": comment}})
	return dep, nil
}

// Reject 驳回请求（批准人不能是提交人，提交人自己请用 Cancel）。
func (d *Service) Reject(id uint, comment, user, ip string) error {
	req, err := d.pending(id)
	if err != nil {
		return err
	}
	if req.RequestedBy == user {
		return ErrSelfApproval
	}
	if err := d.close(d.q(), id, ApprovalRejected, user, comment, 0); err != nil {
		return err
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.reject", ObjectType: "release_request", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"group": req.GroupID, "revision": req.RevisionID, "requested_by": req.RequestedBy, "comment": comment}})
}

// Cancel 撤回请求：提交人本人或管理员。
func (d *Service) Cancel(id uint, user string, admin bool, ip string) error {
	req, err := d.pending(id)
	if err != nil {
		return err
	}
	if req.RequestedBy != user && !admin {
		return ErrForbidden
	}
	if err := d.close(d.q(), id, ApprovalCancelled, user, "", 0); err != nil {
		return err
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "release.cancel", ObjectType: "release_request", ObjectID: fmt.Sprint(id),
		Detail: map[string]any{"group": req.GroupID, "revision": req.RevisionID}})
}

// diffFiles 比较两个版本包的文件（不含清单），私钥只标明变化不显示内容。
func diffFiles(old, nw map[string][]byte) []FileDiff {
	out := []FileDiff{}
	names := map[string]bool{}
	for n := range nw {
		names[n] = true
	}
	for n := range old {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		if n != render.ManifestName {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		nb, inNew := nw[n]
		ob, inOld := old[n]
		change := ""
		switch {
		case inNew && !inOld:
			change = "added"
		case !inNew && inOld:
			change = "removed"
		case string(nb) != string(ob):
			change = "modified"
		default:
			continue
		}
		diff := ""
		if strings.HasSuffix(n, ".key") {
			diff = "（私钥内容不显示）"
		} else if n != "mime.types" {
			diff = textdiff.Unified(n, string(ob), string(nb))
		}
		out = append(out, FileDiff{Path: n, Change: change, Diff: diff})
	}
	return out
}
