package api

import (
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
)

// 流量曲线：按分钟汇总（请求数求和、p95 取最大），可按节点组过滤（子查询）。SQLite 与 PostgreSQL 都要一致。
func TestSeriesAggregation(t *testing.T) {
	st := storetest.Open(t)
	s := &Server{Store: st}
	st.DB.Create(&store.NodeGroup{Name: "a"})
	st.DB.Create(&store.NodeGroup{Name: "b"})
	st.DB.Create(&store.Node{GroupID: 1, Name: "a1"})
	st.DB.Create(&store.Node{GroupID: 1, Name: "a2"})
	st.DB.Create(&store.Node{GroupID: 2, Name: "b1"})
	m := time.Now().Add(-5 * time.Minute).Truncate(time.Minute).Unix()
	for _, r := range []store.MetricMinute{
		{NodeID: 1, Site: "shop", Minute: m, Requests: 10, S5xx: 1, S4xx: 2, P95ms: 30},
		{NodeID: 2, Site: "shop", Minute: m, Requests: 5, S5xx: 0, S4xx: 1, P95ms: 80},
		{NodeID: 3, Site: "blog", Minute: m, Requests: 100, S5xx: 7, S4xx: 0, P95ms: 10},
		{NodeID: 1, Site: "shop", Minute: m - 60*60, Requests: 999}, // 超出 30 分钟窗口
	} {
		r := r
		st.DB.Create(&r)
	}
	find := func(ps []point) point {
		for _, p := range ps {
			if p.Minute == m {
				return p
			}
		}
		t.Fatalf("minute %d missing", m)
		return point{}
	}
	all := s.series(0, 30)
	if len(all) != 30 {
		t.Fatalf("points: %d", len(all))
	}
	if p := find(all); p.Requests != 115 || p.S5xx != 8 || p.S4xx != 3 || p.P95ms != 80 {
		t.Fatalf("all groups: %+v", p)
	}
	if p := find(s.series(1, 30)); p.Requests != 15 || p.S5xx != 1 || p.S4xx != 3 || p.P95ms != 80 {
		t.Fatalf("group 1: %+v", p)
	}
	if p := find(s.series(2, 30)); p.Requests != 100 || p.S5xx != 7 || p.P95ms != 10 {
		t.Fatalf("group 2: %+v", p)
	}
	var total uint64
	for _, p := range all {
		total += p.Requests
	}
	if total != 115 {
		t.Fatalf("window leaked old rows: %d", total)
	}
}
