package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
)

// 节点组的完整生命周期：新建 → 改名与设置 → 查看占用 → 删除（含前置检查）。
func TestGroupLifecycleHTTP(t *testing.T) {
	f := newImportHTTP(t)
	q := query.Use(f.s.Store.DB)
	ctx := context.Background()

	// 新建：仅管理员；名称校验；重名
	requireStatus(t, f.json(t, "operator", http.MethodPost, "/node-groups", map[string]any{"name": "edge-x"}), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/node-groups", map[string]any{"name": "Bad Name"}), http.StatusBadRequest, "")
	w := f.json(t, "admin", http.MethodPost, "/node-groups", map[string]any{"name": "edge-x", "description": "x"})
	requireStatus(t, w, http.StatusOK, "")
	var g store.NodeGroup
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil || g.ID == 0 {
		t.Fatalf("create: %v %s", err, w.Body)
	}
	requireStatus(t, f.json(t, "admin", http.MethodPost, "/node-groups", map[string]any{"name": "edge-x"}), http.StatusConflict, "NAME_TAKEN")

	// 改名：校验、重名；不带 name 时保持原名
	path := fmt.Sprintf("/node-groups/%d", g.ID)
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"name": "Bad Name"}), http.StatusBadRequest, "")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"name": "imports"}), http.StatusConflict, "NAME_TAKEN")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"name": "edge-y", "description": "y"}), http.StatusOK, "")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]any{"description": "z"}), http.StatusOK, "")
	got, err := store.First[store.NodeGroup](f.s.Store, g.ID)
	if err != nil || got.Name != "edge-y" || got.Description != "z" {
		t.Fatalf("after update: %+v %v", got, err)
	}

	// 组内内容：站点、上游、注册令牌、一个已吊销的节点、一个在线节点；另一个组的站点不受影响
	mk := func(v any) {
		t.Helper()
		if err := f.s.Store.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk(&store.Site{GroupID: g.ID, Name: "a", Spec: "{}"})
	mk(&store.Upstream{GroupID: g.ID, Name: "app", Spec: "{}"})
	mk(&store.EnrollToken{GroupID: g.ID, TokenHash: "t1", ExpiresAt: time.Now().Add(time.Hour)})
	mk(&store.Site{GroupID: f.group, Name: "keep", Spec: "{}"})
	revoked := &store.Node{GroupID: g.ID, Name: "old", Status: "revoked"}
	mk(revoked)
	live := &store.Node{GroupID: g.ID, Name: "edge-1", Status: "online"}
	mk(live)

	w = f.request("viewer", http.MethodGet, path+"/usage", nil, "", false)
	requireStatus(t, w, http.StatusOK, "")
	var u struct {
		Nodes        int64 `json:"nodes"`
		RevokedNodes int64 `json:"revoked_nodes"`
		Sites        int64 `json:"sites"`
		Upstreams    int64 `json:"upstreams"`
		Busy         string
		Last         *bool `json:"last"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil || u.Nodes != 1 || u.RevokedNodes != 1 || u.Sites != 1 || u.Upstreams != 1 || u.Last == nil || *u.Last {
		t.Fatalf("usage = %s", w.Body)
	}

	// 删除：仅管理员；必须输入组名确认；组内有未吊销的节点时拒绝
	requireStatus(t, f.json(t, "operator", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-x"}), http.StatusBadRequest, "CONFIRM_MISMATCH")
	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusConflict, "GROUP_HAS_NODES")
	if _, err := q.Node.WithContext(ctx).Where(q.Node.ID.Eq(live.ID)).Update(q.Node.Status, "revoked"); err != nil {
		t.Fatal(err)
	}

	// 有待审批的发布申请、进行中的灰度或升级时拒绝
	rr := &store.ReleaseRequest{GroupID: g.ID, Status: "pending", RevisionID: "r000001"}
	mk(rr)
	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusConflict, "GROUP_BUSY")
	if _, err := q.ReleaseRequest.WithContext(ctx).Where(q.ReleaseRequest.ID.Eq(rr.ID)).Update(q.ReleaseRequest.Status, "cancelled"); err != nil {
		t.Fatal(err)
	}
	up := &store.DataplaneUpgrade{GroupID: g.ID, Status: "running"}
	mk(up)
	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusConflict, "GROUP_BUSY")
	if _, err := q.DataplaneUpgrade.WithContext(ctx).Where(q.DataplaneUpgrade.ID.Eq(up.ID)).Update(q.DataplaneUpgrade.Status, "done"); err != nil {
		t.Fatal(err)
	}

	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusOK, "")
	if _, err := store.First[store.NodeGroup](f.s.Store, g.ID); err == nil {
		t.Fatal("group still exists")
	}
	for name, n := range map[string]func() (int64, error){
		"sites":     func() (int64, error) { return q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(g.ID)).Count() },
		"upstreams": func() (int64, error) { return q.Upstream.WithContext(ctx).Where(q.Upstream.GroupID.Eq(g.ID)).Count() },
		"tokens": func() (int64, error) {
			return q.EnrollToken.WithContext(ctx).Where(q.EnrollToken.GroupID.Eq(g.ID)).Count()
		},
		"nodes": func() (int64, error) { return q.Node.WithContext(ctx).Where(q.Node.GroupID.Eq(g.ID)).Count() },
		"requests": func() (int64, error) {
			return q.ReleaseRequest.WithContext(ctx).Where(q.ReleaseRequest.GroupID.Eq(g.ID)).Count()
		},
	} {
		if c, err := n(); err != nil || c != 0 {
			t.Fatalf("%s left after delete: %d %v", name, c, err)
		}
	}
	if c, _ := q.Site.WithContext(ctx).Where(q.Site.GroupID.Eq(f.group)).Count(); c != 1 {
		t.Fatalf("other group's site removed: %d", c)
	}
	var n int64
	f.s.Store.DB.Table("audit_logs").Where("action = ? AND object_id = ?", "group.delete", fmt.Sprint(g.ID)).Count(&n)
	if n != 1 {
		t.Fatalf("group.delete audit entries = %d", n)
	}
	requireStatus(t, f.json(t, "admin", http.MethodDelete, path, map[string]any{"confirm": "edge-y"}), http.StatusNotFound, "")

	// 最后一个节点组不能删除
	requireStatus(t, f.json(t, "admin", http.MethodDelete, fmt.Sprintf("/node-groups/%d", f.group), map[string]any{"confirm": "imports"}), http.StatusConflict, "LAST_GROUP")
}

func TestGroupManagementModeHTTP(t *testing.T) {
	f := newImportHTTP(t)
	path := fmt.Sprintf("/node-groups/%d/management-mode", f.group)

	w := f.request("viewer", http.MethodGet, path, nil, "", false)
	requireStatus(t, w, http.StatusOK, "")
	var mode struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &mode); err != nil || mode.Mode != "managed" {
		t.Fatalf("default management mode response = %s", w.Body)
	}
	requireStatus(t, f.json(t, "viewer", http.MethodPut, path, map[string]string{"mode": "observe"}), http.StatusForbidden, "")
	requireStatus(t, f.json(t, "operator", http.MethodPut, path, map[string]string{"mode": "unsafe"}), http.StatusBadRequest, "INVALID_MANAGEMENT_MODE")
	w = f.json(t, "operator", http.MethodPut, path, map[string]string{"mode": "observe"})
	requireStatus(t, w, http.StatusOK, "")
	if err := json.Unmarshal(w.Body.Bytes(), &mode); err != nil || mode.Mode != "observe" {
		t.Fatalf("observe management mode response = %s", w.Body)
	}
	if got := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/node-groups/%d/publish", f.group), map[string]string{"note": "must not overwrite"}); got.Code != http.StatusConflict {
		t.Fatalf("publish in observe mode = %d: %s", got.Code, got.Body)
	} else {
		requireStatus(t, got, http.StatusConflict, "CONFIG_MANAGEMENT_PAUSED")
	}
	w = f.request("viewer", http.MethodGet, "/node-groups", nil, "", false)
	requireStatus(t, w, http.StatusOK, "")
	var groups []struct {
		ID             uint   `json:"id"`
		ManagementMode string `json:"management_mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil || len(groups) == 0 || groups[0].ManagementMode != "observe" {
		t.Fatalf("list group management mode = %s", w.Body)
	}
}
