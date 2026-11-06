package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func addGroup(t *testing.T, d *Service, name string) *store.NodeGroup {
	t.Helper()
	g := &store.NodeGroup{Name: name}
	if err := query.Use(d.Store.DB).NodeGroup.WithContext(context.Background()).Create(g); err != nil {
		t.Fatal(err)
	}
	return g
}

func addNode(t *testing.T, d *Service, g *store.NodeGroup, name, hostKey string) *store.Node {
	t.Helper()
	n := &store.Node{GroupID: g.ID, Name: name, HostKey: hostKey, Status: "online"}
	if err := query.Use(d.Store.DB).Node.WithContext(context.Background()).Create(n); err != nil {
		t.Fatal(err)
	}
	return n
}

func addStream(t *testing.T, d *Service, g *store.NodeGroup, name string, port int, proto string) {
	t.Helper()
	spec, _ := json.Marshal(render.StreamProxy{Name: name, Listen: port, Protocol: proto, Servers: []render.UpstreamServer{{Addr: "10.0.0.1:1"}}})
	if err := query.Use(d.Store.DB).StreamService.WithContext(context.Background()).Create(&store.StreamService{GroupID: g.ID, Name: name, Spec: string(spec), Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

var blue = render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}

// 节点组档案进入渲染输入；默认组仍为零值档案（渲染结果与引入档案前一致）。
func TestGroupProfileFeedsRender(t *testing.T) {
	d, g := newTestService(t)
	addSite(t, d, g, "web")
	in, err := d.Input(g.ID, "r000000")
	if err != nil {
		t.Fatal(err)
	}
	if in.Profile != (render.Profile{}) {
		t.Fatalf("default group profile = %+v", in.Profile)
	}
	if err := d.SetProfile(g.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if in, err = d.Input(g.ID, "r000000"); err != nil || in.Profile != blue {
		t.Fatalf("input profile = %+v, %v", in.Profile, err)
	}
	b, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	main := string(b.Files["syedge.conf"])
	if !strings.Contains(main, "pid   /run/syedge-blue/syedge.pid;") || !strings.Contains(main, "listen       18080  default_server;") {
		t.Fatalf("preview not rendered with the group profile:\n%s", main)
	}
}

func TestSetProfileValidation(t *testing.T) {
	d, g := newTestService(t)
	if err := d.SetProfile(g.ID, render.Profile{Instance: "Bad"}, "alice", ""); !errors.Is(err, render.ErrProfile) {
		t.Fatalf("invalid profile: %v", err)
	}
	// 组内已有节点后，实例名、模式、根目录不可修改（节点上的目录已经按它们创建）；端口可以改
	if err := d.SetProfile(g.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	addNode(t, d, g, "edge-1", "host-a")
	moved := blue
	moved.Instance = "green"
	if err := d.SetProfile(g.ID, moved, "alice", ""); !errors.Is(err, ErrProfileLocked) {
		t.Fatalf("instance change with nodes: %v", err)
	}
	ports := blue
	ports.HTTPPort = 28080
	if err := d.SetProfile(g.ID, ports, "alice", ""); err != nil {
		t.Fatalf("port change rejected: %v", err)
	}
	if p, _ := d.Profile(g.ID); p.HTTPPort != 28080 {
		t.Fatalf("profile not saved: %+v", p)
	}
	// 写审计
	var n int64
	d.Store.DB.Table("audit_logs").Where("action = ?", "group.profile").Count(&n)
	if n != 2 {
		t.Fatalf("audit entries = %d, want 2", n)
	}
}

// 同一主机上的实例：实例名不能相同，端口（含四层代理）不能重叠（设计 0001 第 5.2 节）。
func TestHostConflicts(t *testing.T) {
	d, def := newTestService(t) // 默认实例：80 / 443 / 8081
	other := addGroup(t, d, "blue")
	// 档案在节点加入之前设定（之后实例名不能改，只能改端口）
	if err := d.SetProfile(other.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	addNode(t, d, def, "edge-1", "host-a")
	addNode(t, d, other, "edge-1b", "host-a")
	addStream(t, d, def, "mysql", 13306, "tcp")

	// 端口改到默认实例的 HTTPS 端口
	clash := blue
	clash.HTTPSPort = 443
	if err := d.SetProfile(other.ID, clash, "alice", ""); !errors.Is(err, ErrHostConflict) || !strings.Contains(err.Error(), "tcp/443") {
		t.Fatalf("https port clash: %v", err)
	}
	// 端口改到默认实例的四层代理端口
	clash = blue
	clash.HTTPPort = 13306
	if err := d.SetProfile(other.ID, clash, "alice", ""); !errors.Is(err, ErrHostConflict) || !strings.Contains(err.Error(), "mysql") {
		t.Fatalf("stream port clash: %v", err)
	}
	// 不冲突的端口修改
	moved := blue
	moved.HTTPPort = 28080
	if err := d.SetProfile(other.ID, moved, "alice", ""); err != nil {
		t.Fatalf("valid port change rejected: %v", err)
	}
	// 不同主机上的同名实例互不影响
	far := addGroup(t, d, "blue-dr")
	if err := d.SetProfile(far.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	addNode(t, d, far, "edge-9", "host-z")
	if err := d.SetProfile(far.ID, moved, "alice", ""); err != nil {
		t.Fatalf("same profile on another host rejected: %v", err)
	}
	// 四层代理：新端口与同机其他实例的端口冲突
	if err := d.CheckStreamPorts(def.ID, render.StreamProxy{Name: "redis", Listen: 18443, Protocol: "tcp"}); !errors.Is(err, ErrHostConflict) {
		t.Fatalf("stream on another instance's port: %v", err)
	}
	if err := d.CheckStreamPorts(def.ID, render.StreamProxy{Name: "dns", Listen: 18443, Protocol: "udp"}); err != nil {
		t.Fatalf("udp stream on a tcp-only port rejected: %v", err)
	}
	if err := d.CheckStreamPorts(def.ID, render.StreamProxy{Name: "mysql", Listen: 13307, Protocol: "tcp"}); err != nil {
		t.Fatalf("moving own stream rejected: %v", err)
	}
}

// 注册时：新节点所在主机若已有冲突实例，拒绝注册。
func TestEnrollHostCheck(t *testing.T) {
	d, def := newTestService(t)
	blueG := addGroup(t, d, "blue")
	if err := d.SetProfile(blueG.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	addNode(t, d, def, "edge-1", "host-a")
	if err := d.CheckEnrollHost(blueG.ID, "host-a", 0); err != nil {
		t.Fatalf("blue next to default rejected: %v", err)
	}
	addNode(t, d, blueG, "edge-1b", "host-a")
	// 同一台机器再注册一个默认实例
	def2 := addGroup(t, d, "default-2")
	if err := d.CheckEnrollHost(def2.ID, "host-a", 0); !errors.Is(err, ErrHostConflict) {
		t.Fatalf("second default instance on host-a: %v", err)
	}
	// 同一个组在同一台机器上重复注册（例如重装）：以节点自身为准，不算冲突
	n := addNode(t, d, def2, "edge-2", "host-b")
	if err := d.CheckEnrollHost(def2.ID, "host-b", n.ID); err != nil {
		t.Fatalf("re-enrolling the same node: %v", err)
	}
	// 旧版 agent 不上报主机标识：无法检查，放行（与现有行为一致）
	if err := d.CheckEnrollHost(def2.ID, "", 0); err != nil {
		t.Fatalf("empty host key: %v", err)
	}
}

// agent 上报的本机实例必须与节点组档案一致；旧版 agent（不上报）只能加入默认档案的组。
func TestCheckInstance(t *testing.T) {
	d, def := newTestService(t)
	blueG := addGroup(t, d, "blue")
	if err := d.SetProfile(blueG.ID, blue, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckInstance(def.ID, nil); err != nil {
		t.Fatalf("old agent into default group: %v", err)
	}
	if err := d.CheckInstance(def.ID, &render.Profile{Mode: "system", HTTPPort: 80, HTTPSPort: 443, StatusPort: 8081}); err != nil {
		t.Fatalf("explicit default profile: %v", err)
	}
	if err := d.CheckInstance(blueG.ID, nil); !errors.Is(err, ErrInstanceMismatch) || !strings.Contains(err.Error(), "升级 agent") {
		t.Fatalf("old agent into instance group: %v", err)
	}
	same := blue
	same.Mode, same.NoFile = "system", 4096 // nofile 不参与比较
	if err := d.CheckInstance(blueG.ID, &same); err != nil {
		t.Fatalf("matching instance rejected: %v", err)
	}
	wrong := blue
	wrong.HTTPSPort = 28443
	if err := d.CheckInstance(blueG.ID, &wrong); !errors.Is(err, ErrInstanceMismatch) || !strings.Contains(err.Error(), "syedge-instance add") {
		t.Fatalf("port mismatch: %v", err)
	}
	if err := d.CheckInstance(def.ID, &blue); !errors.Is(err, ErrInstanceMismatch) {
		t.Fatalf("instance agent into default group: %v", err)
	}
}
