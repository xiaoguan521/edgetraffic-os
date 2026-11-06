package api

import (
	"context"
	"strings"
	"testing"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BR-03（设计 0001 第 10.3 节）：另一个品牌构建的 agent 不能注册到本控制台，令牌也不会被消耗。
func TestEnrollRejectsOtherBrand(t *testing.T) {
	f := newImportHTTP(t)
	tok, _, err := f.s.GW.NewEnrollToken(f.group, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.GW.Enroll(context.Background(), &agentv1.EnrollRequest{EnrollToken: tok, Hostname: "edge-x", Brand: "sytest"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "BRAND_MISMATCH") {
		t.Fatalf("other brand: %v", err)
	}
	var used int64
	f.s.Store.DB.Table("enroll_tokens").Where("used_at IS NOT NULL").Count(&used)
	if used != 0 {
		t.Fatal("rejected enrollment consumed the token")
	}
	var n int64
	f.s.Store.DB.Table("audit_logs").Where("action = ? AND result = ?", "node.enroll", "failure").Count(&n)
	if n != 1 {
		t.Fatalf("audit entries = %d", n)
	}
}
