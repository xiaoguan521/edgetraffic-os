package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkgsig"
)

func addHostNode(t *testing.T, d *Service, g *store.NodeGroup, name, host string) uint {
	t.Helper()
	n := &store.Node{GroupID: g.ID, Name: name, Hostname: name, HostKey: host, Status: "online", Arch: "amd64"}
	if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(n); err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// 三台主机：h1、h2 上各有默认组与 blue 组的实例，h3 只有默认组的实例。
func hostFixture(t *testing.T) (*Service, *store.NodeGroup, *store.NodeGroup, map[string]uint, *fakeAgents) {
	d, def := newTestService(t)
	seedPackage(t, d, "1.30.7-1")
	blueG := addGroup(t, d, "blue")
	if err := d.SetProfile(blueG.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	id := map[string]uint{
		"d1": addHostNode(t, d, def, "d1", "h1aaaaaaaaaaaaaaaa"),
		"d2": addHostNode(t, d, def, "d2", "h2bbbbbbbbbbbbbbbb"),
		"d3": addHostNode(t, d, def, "d3", "h3cccccccccccccccc"),
		"b1": addHostNode(t, d, blueG, "b1", "h1aaaaaaaaaaaaaaaa"),
		"b2": addHostNode(t, d, blueG, "b2", "h2bbbbbbbbbbbbbbbb"),
	}
	fa := &fakeAgents{online: map[uint]bool{}, fail: map[uint]bool{}}
	for _, v := range id {
		fa.online[v] = true
	}
	d.Agents = fa
	return d, def, blueG, id, fa
}

func statusOf(v *UpgradeView) map[uint]string {
	m := map[uint]string{}
	for _, n := range v.Nodes {
		m[n.NodeID] = n.Status
	}
	return m
}

// 升级默认组：同机的 blue 实例一起升级；主机内本组实例先、同机实例后，依次 START→COMMIT。
func TestHostUpgradeIncludesSiblings(t *testing.T) {
	d, def, _, id, fa := hostFixture(t)
	up, err := d.StartUpgrade(def.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitUpgrade(t, d, up.ID)
	if v.Status != "done" || len(v.Nodes) != 5 || len(v.Hosts) != 3 {
		t.Fatalf("status %s nodes %d hosts %+v", v.Status, len(v.Nodes), v.Hosts)
	}
	for _, n := range v.Nodes {
		if n.Sibling != (n.Name[0] == 'b') {
			t.Fatalf("sibling flag wrong for %s", n.Name)
		}
	}
	pos := map[string]int{}
	for i, c := range fa.calls {
		pos[c] = i
	}
	for _, h := range [][2]string{{"d1", "b1"}, {"d2", "b2"}} {
		a, b := id[h[0]], id[h[1]]
		startA, commitA, startB := pos[fmt.Sprintf("%d:START", a)], pos[fmt.Sprintf("%d:COMMIT", a)], pos[fmt.Sprintf("%d:START", b)]
		if startA >= commitA || commitA >= startB {
			t.Fatalf("host %s: instances not upgraded one after another: %v", h, fa.calls)
		}
	}
	// 第一批只有一台主机（含其全部实例）
	for _, h := range v.Hosts {
		if h.Batch == 1 && len(h.Nodes) != 2 {
			t.Fatalf("first batch host %+v", h)
		}
	}
}

// 同机第二个实例失败：主机部分升级，尚未开始的主机跳过；重试只执行未完成的实例；整体回退只针对部分升级主机上已升级的实例。
func TestHostPartialRetryAndRollback(t *testing.T) {
	d, def, _, id, fa := hostFixture(t)
	fa.fail[id["b1"]] = true // 第一批 h1：d1 成功、b1 失败
	up, err := d.StartUpgrade(def.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitUpgrade(t, d, up.ID)
	st := statusOf(v)
	if v.Status != "partial" || st[id["d1"]] != "upgraded" || st[id["b1"]] != "rolled_back" ||
		st[id["d2"]] != "skipped" || st[id["b2"]] != "skipped" || st[id["d3"]] != "skipped" {
		t.Fatalf("status %s nodes %v", v.Status, st)
	}
	hs := map[string]string{}
	for _, h := range v.Hosts {
		hs[h.Host] = h.Status
	}
	if hs["h1aaaaaaaaaa"] != "partial" || hs["h2bbbbbbbbbb"] != "skipped" {
		t.Fatalf("hosts %v", hs)
	}

	// 整体回退：只有部分升级主机上已升级的 d1
	seedPackage(t, d, "1.30.6-1")
	fa.mu.Lock()
	fa.calls = nil
	fa.mu.Unlock()
	rb, err := d.RollbackHosts(up.ID, "1.30.6-1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	rv := waitUpgrade(t, d, rb.ID)
	if rv.Status != "done" || len(rv.Nodes) != 1 || rv.Nodes[0].NodeID != id["d1"] || rv.Version != "1.30.6-1" {
		t.Fatalf("rollback %s %+v", rv.Status, rv.Nodes)
	}

	// 重试剩余实例：b1、d2、b2、d3，不含已升级的 d1
	fa.fail = map[uint]bool{}
	fa.mu.Lock()
	fa.calls = nil
	fa.mu.Unlock()
	re, err := d.RetryUpgrade(up.ID, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	r := waitUpgrade(t, d, re.ID)
	if r.Status != "done" || len(r.Nodes) != 4 {
		t.Fatalf("retry %s %+v", r.Status, r.Nodes)
	}
	for _, c := range fa.calls {
		if strings.HasPrefix(c, fmt.Sprintf("%d:", id["d1"])) {
			t.Fatalf("retry touched the already upgraded instance: %v", fa.calls)
		}
	}
	if _, err := d.RetryUpgrade(re.ID, "admin", ""); err == nil {
		t.Fatal("retry of a finished upgrade with nothing left accepted")
	}
	if _, err := d.RollbackHosts(re.ID, "1.30.6-1", "admin", ""); err == nil {
		t.Fatal("rollback without partial hosts accepted")
	}
}

type blockingAgents struct {
	fakeAgents
	release chan struct{}
	once    sync.Once
	started chan struct{}
}

func (b *blockingAgents) Send(ctx context.Context, id uint, m *agentv1.ServerMessage, to time.Duration) (*agentv1.CommandResult, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.fakeAgents.Send(ctx, id, m, to)
}

// 升级默认组时，同机的 blue 组也被锁住，不能同时开始另一个升级。
func TestHostUpgradeLocksSiblingGroups(t *testing.T) {
	d, def, blueG, id, _ := hostFixture(t)
	ba := &blockingAgents{fakeAgents: fakeAgents{online: map[uint]bool{}}, release: make(chan struct{}), started: make(chan struct{})}
	for _, v := range id {
		ba.online[v] = true
	}
	d.Agents = ba
	up, err := d.StartUpgrade(def.ID, "1.30.7-1", "standard", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	<-ba.started
	if _, err := d.StartUpgrade(blueG.ID, "1.30.7-1", "standard", "admin", ""); !errors.Is(err, ErrUpgradeRunning) {
		t.Fatalf("sibling group upgrade during host upgrade: %v", err)
	}
	close(ba.release)
	if v := waitUpgrade(t, d, up.ID); v.Status != "done" {
		t.Fatalf("status %s", v.Status)
	}
	if _, err := d.StartUpgrade(blueG.ID, "1.30.7-1", "standard", "admin", ""); err != nil {
		t.Fatalf("sibling group after host upgrade: %v", err)
	}
}

func TestHostsView(t *testing.T) {
	d, _, _, id, _ := hostFixture(t)
	q := query.Use(d.Store.DB)
	set := func(name, v string) {
		_, _ = q.Node.WithContext(context.Background()).Where(q.Node.ID.Eq(id[name])).Update(q.Node.RunningVersion, v)
	}
	set("d1", "1.30.7")
	set("b1", "1.30.6") // h1：两个实例运行不同版本
	set("d2", "1.30.7")
	set("b2", "1.30.7")
	hs, err := d.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]HostView{}
	for _, h := range hs {
		by[h.Host] = h
	}
	h1 := by["h1aaaaaaaaaa"]
	if len(hs) != 3 || len(h1.Nodes) != 2 || !h1.Mixed || by["h2bbbbbbbbbb"].Mixed {
		t.Fatalf("hosts %+v", hs)
	}
	for _, n := range h1.Nodes {
		if n.Name == "b1" && (n.Instance != "blue" || n.HTTPPort != 18080 || n.GroupName != "blue") {
			t.Fatalf("blue instance %+v", n)
		}
		if n.Name == "d1" && (n.Instance != "" || n.HTTPPort != 80) {
			t.Fatalf("default instance %+v", n)
		}
	}
}

// BR-03：其他品牌的升级包即使签名有效也不能导入版本库。
func TestImportRejectsOtherBrand(t *testing.T) {
	d, _ := newTestService(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d.PackageKeys = map[string]ed25519.PublicKey{"etos-ci-pkg-test": pub}
	data := []byte("pkg")
	sig, err := pkgsig.Sign(priv, "etos-ci-pkg-test", "syedge", "1.30.7-1", "amd64", "standard", data)
	if err != nil {
		t.Fatal(err)
	}
	sig.Name = "sytest" // 另一个品牌的包（签名格式只接受本品牌的包名，见 pkgsig.Names）
	sb, _ := json.Marshal(sig)
	if _, err := d.ImportPackage(data, sb, "admin", ""); !errors.Is(err, pkgsig.ErrSignature) {
		t.Fatalf("other brand package: %v", err)
	}
}
