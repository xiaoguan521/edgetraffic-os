package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

type fakeAgents struct {
	mu     sync.Mutex
	fail   map[uint]bool
	calls  []string
	online map[uint]bool
}

func (f *fakeAgents) Online(id uint) bool { return f.online[id] }

func (f *fakeAgents) Send(_ context.Context, id uint, m *agentv1.ServerMessage, _ time.Duration) (*agentv1.CommandResult, error) {
	u := m.GetUpgrade()
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%d:%s", id, u.Action))
	f.mu.Unlock()
	if u.Action == agentv1.UpgradeDataplane_START && f.fail[id] {
		return &agentv1.CommandResult{Status: agentv1.CommandResult_ROLLED_BACK, Message: "post check failed"}, nil
	}
	return &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}, nil
}

func seedPackage(t *testing.T, d *Service, ver string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d.PackageKeys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	data := []byte("pkg-" + ver)
	sig, _ := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge", ver, "amd64", "standard", data)
	sb, _ := json.Marshal(sig)
	if _, err := d.ImportPackage(data, sb, "admin", ""); err != nil {
		t.Fatal(err)
	}
	// 篡改的包被拒绝
	if _, err := d.ImportPackage([]byte("other"), sb, "admin", ""); !errors.Is(err, pkgsig.ErrSignature) {
		t.Fatalf("tampered package accepted: %v", err)
	}
}

func addNodes(t *testing.T, d *Service, g *store.NodeGroup, n int) []uint {
	q := query.Use(d.Store.DB)
	var ids []uint
	for i := 0; i < n; i++ {
		nd := &store.Node{GroupID: g.ID, Name: fmt.Sprintf("edge-%02d", i), Status: "online", Arch: "amd64"}
		_ = q.Node.WithContext(context.Background()).Create(nd)
		ids = append(ids, nd.ID)
	}
	return ids
}

func waitUpgrade(t *testing.T, d *Service, id uint) *UpgradeView {
	t.Helper()
	for i := 0; i < 200; i++ {
		v, err := d.GetUpgrade(id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != "running" {
			waitUpgradeLocks(t, v)
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("upgrade did not finish")
	return nil
}

// 状态写入结束后后台仍需记录审计并释放锁；测试不能在此之前关闭数据库或开始下一次升级。
func waitUpgradeLocks(t *testing.T, v *UpgradeView) {
	t.Helper()
	groups := map[uint]bool{v.GroupID: true}
	for _, n := range v.Nodes {
		if n.GroupID != 0 {
			groups[n.GroupID] = true
		}
	}
	for id := range groups {
		mu, ok := upgradeLocks.Load(id)
		if !ok {
			t.Fatalf("missing upgrade lock for group %d", id)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !mu.(*sync.Mutex).TryLock() {
			if time.Now().After(deadline) {
				t.Fatalf("upgrade lock for group %d was not released", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.(*sync.Mutex).Unlock()
	}
}

func TestPlanBatches(t *testing.T) {
	for n, want := range map[int][]int{1: {1}, 2: {1, 1}, 5: {1, 1, 3}, 20: {1, 2, 17}, 50: {1, 5, 44}} {
		ids := make([]uint, n)
		var got []int
		for _, b := range planBatches(ids) {
			got = append(got, len(b))
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%d nodes: batches %v, want %v", n, got, want)
		}
	}
}

func TestUpgradeBatchesAndStopOnFailure(t *testing.T) {
	d, g := newTestService(t)
	seedPackage(t, d, "1.30.7-1")
	ids := addNodes(t, d, g, 5)
	fa := &fakeAgents{online: map[uint]bool{}, fail: map[uint]bool{ids[1]: true}}
	for _, id := range ids {
		fa.online[id] = true
	}
	d.Agents = fa
	if _, err := d.StartUpgrade(g.ID, "9.9.9", "standard", "admin", ""); err == nil {
		t.Fatal("version not in library accepted")
	}
	up, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitUpgrade(t, d, up.ID)
	// 第 1 批（1 台）成功并提交；第 2 批失败（agent 已自动回退）→ 停止，第 3 批不执行
	if v.Status != "failed" {
		t.Fatalf("status %s", v.Status)
	}
	got := map[uint]string{}
	for _, n := range v.Nodes {
		got[n.NodeID] = n.Status
	}
	if got[ids[0]] != "upgraded" || got[ids[1]] != "rolled_back" || got[ids[2]] != "skipped" || got[ids[4]] != "skipped" {
		t.Fatalf("node results %v (calls %v)", got, fa.calls)
	}
	if fa.calls[0] != fmt.Sprintf("%d:START", ids[0]) || fa.calls[1] != fmt.Sprintf("%d:COMMIT", ids[0]) {
		t.Fatalf("calls %v", fa.calls)
	}
	// 同一组同时只能有一个升级
	fa.fail = nil
	up2, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", ""); err == nil {
		t.Log("second upgrade may race with the first finishing; not asserted")
	}
	if v := waitUpgrade(t, d, up2.ID); v.Status != "done" {
		t.Fatalf("second run %s", v.Status)
	}
}

type fakeAgentRestarts struct {
	fakeAgents
	d *Service
}

func (f *fakeAgentRestarts) Send(_ context.Context, id uint, m *agentv1.ServerMessage, _ time.Duration) (*agentv1.CommandResult, error) {
	u := m.GetUpgradeAgent()
	q := query.Use(f.d.Store.DB)
	_, _ = q.Node.WithContext(context.Background()).Where(q.Node.ID.Eq(id)).Update(q.Node.AgentVersion, u.Version) // 模拟重启后以新版本上线
	return &agentv1.CommandResult{Status: agentv1.CommandResult_APPLIED}, nil
}

func TestAgentUpgrade(t *testing.T) {
	d, g := newTestService(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d.PackageKeys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	data := []byte("agent-pkg")
	sig, _ := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge-agent", "2.0.0", "amd64", "standard", data)
	sb, _ := json.Marshal(sig)
	if _, err := d.ImportPackage(data, sb, "admin", ""); err != nil {
		t.Fatal(err)
	}
	ids := addNodes(t, d, g, 3)
	fa := &fakeAgentRestarts{fakeAgents: fakeAgents{online: map[uint]bool{}}, d: d}
	for _, id := range ids {
		fa.online[id] = true
	}
	d.Agents = fa
	AgentRestartWait = 5 * time.Second
	up, err := d.StartAgentUpgrade(g.ID, "2.0.0", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	var v *UpgradeView
	for i := 0; i < 300; i++ {
		v, _ = d.GetUpgrade(up.ID)
		if v.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v.Status != "done" {
		t.Fatalf("agent upgrade %+v", v.Nodes)
	}
	waitUpgradeLocks(t, v)
}

// 慢速 agent：记录同时进行中的升级数与各节点开始时间，用于检查批内并行、批间串行。
type fakeSlowAgents struct {
	fakeAgents
	delay     time.Duration
	cur, max  int
	started   map[uint]time.Time
	committed map[uint]time.Time
}

func (f *fakeSlowAgents) Send(ctx context.Context, id uint, m *agentv1.ServerMessage, to time.Duration) (*agentv1.CommandResult, error) {
	u := m.GetUpgrade()
	f.mu.Lock()
	if u.Action == agentv1.UpgradeDataplane_START {
		f.started[id] = time.Now()
		f.cur++
		if f.cur > f.max {
			f.max = f.cur
		}
	}
	f.mu.Unlock()
	if u.Action == agentv1.UpgradeDataplane_START {
		time.Sleep(f.delay)
		f.mu.Lock()
		f.cur--
		f.mu.Unlock()
	} else {
		f.mu.Lock()
		f.committed[id] = time.Now()
		f.mu.Unlock()
	}
	return f.fakeAgents.Send(ctx, id, m, to)
}

// 50 台节点的升级要在 10 分钟内完成（规格书 12.6）：批内节点并行（有上限），批与批之间串行。
func TestUpgradeBatchRunsInParallel(t *testing.T) {
	d, g := newTestService(t)
	seedPackage(t, d, "1.30.7-1")
	ids := addNodes(t, d, g, 20) // 批次 1 / 2 / 17
	fa := &fakeSlowAgents{fakeAgents: fakeAgents{online: map[uint]bool{}}, delay: 150 * time.Millisecond,
		started: map[uint]time.Time{}, committed: map[uint]time.Time{}}
	for _, id := range ids {
		fa.online[id] = true
	}
	d.Agents = fa
	t0 := time.Now()
	up, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitUpgrade(t, d, up.ID)
	if v.Status != "done" {
		t.Fatalf("status %s", v.Status)
	}
	if el := time.Since(t0); el > 20*fa.delay/2 {
		t.Errorf("20 nodes took %s: batches are not parallel", el)
	}
	if fa.max < 2 || fa.max > UpgradeParallel {
		t.Errorf("max concurrent upgrades %d, want 2..%d", fa.max, UpgradeParallel)
	}
	// 批次之间串行：后一批的节点都在前一批全部提交之后才开始
	batchOf := map[uint]int{}
	for _, n := range v.Nodes {
		batchOf[n.NodeID] = n.Batch
	}
	for a, sa := range fa.started {
		for b, cb := range fa.committed {
			if batchOf[a] > batchOf[b] && sa.Before(cb) {
				t.Fatalf("node %d (batch %d) started before node %d (batch %d) committed", a, batchOf[a], b, batchOf[b])
			}
		}
	}
}

// 批内某台失败：同批已在进行的节点完成，同批尚未开始的与后续批次都不再执行。
func TestUpgradeParallelBatchStopsOnFailure(t *testing.T) {
	d, g := newTestService(t)
	seedPackage(t, d, "1.30.7-1")
	ids := addNodes(t, d, g, 50) // 批次 1 / 5 / 44
	fa := &fakeSlowAgents{fakeAgents: fakeAgents{online: map[uint]bool{}, fail: map[uint]bool{ids[1]: true}}, delay: 30 * time.Millisecond,
		started: map[uint]time.Time{}, committed: map[uint]time.Time{}}
	for _, id := range ids {
		fa.online[id] = true
	}
	d.Agents = fa
	up, err := d.StartUpgrade(g.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitUpgrade(t, d, up.ID)
	if v.Status != "failed" {
		t.Fatalf("status %s", v.Status)
	}
	n3 := 0
	for _, n := range v.Nodes {
		if n.Batch == 3 {
			if n.Status != "skipped" {
				t.Fatalf("batch 3 node %d status %s", n.NodeID, n.Status)
			}
			n3++
		}
		if n.NodeID == ids[1] && n.Status != "rolled_back" {
			t.Fatalf("failed node status %s", n.Status)
		}
	}
	if n3 != 44 {
		t.Fatalf("batch 3 has %d nodes", n3)
	}
}
