package deploy

import (
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

func TestOrderBackupFirstAndHostPlanVIPOrdering(t *testing.T) {
	vips := parseGroupVIPs("10.0.0.100")
	// 故意让 MASTER 拥有最小的 ID=1，验证编排仍能把 BACKUP 排在前面、MASTER 排在最后
	nodes := []*store.Node{
		{ID: 1, GroupID: 1, Name: "edge-master", Hostname: "h1", HostKey: "host-master-key", LocalIPs: "10.0.0.11,10.0.0.100"},
		{ID: 2, GroupID: 1, Name: "edge-backup-1", Hostname: "h2", HostKey: "host-backup1-key", LocalIPs: "10.0.0.12"},
		{ID: 3, GroupID: 1, Name: "edge-backup-2", Hostname: "h3", HostKey: "host-backup2-key", LocalIPs: "10.0.0.13"},
	}

	ordered := orderBackupFirst(nodes, vips)
	if ordered[0].Name != "edge-backup-1" || ordered[1].Name != "edge-backup-2" || ordered[2].Name != "edge-master" {
		t.Fatalf("unexpected backup-first order: %s, %s, %s", ordered[0].Name, ordered[1].Name, ordered[2].Name)
	}

	plan := hostPlan(nodes, 1, vips)
	if len(plan) != 3 {
		t.Fatalf("expected 3 plan entries, got %+v", plan)
	}
	// Batch 1 必须是备机，最后一批才是承载 VIP 的主节点
	if plan[0].Name != "edge-backup-1" || plan[0].Batch != 1 {
		t.Fatalf("expected backup node in batch 1, got %+v", plan[0])
	}
	last := plan[len(plan)-1]
	if last.Name != "edge-master" || last.Batch <= plan[0].Batch {
		t.Fatalf("expected VIP master node in final batch, got plan=%+v", plan)
	}
}
