package gateway

import (
	"testing"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

func TestReportedProfile(t *testing.T) {
	if ReportedProfile(nil) != nil {
		t.Fatal("missing report must stay nil (old agent)")
	}
	p := ReportedProfile(&agentv1.InstanceInfo{Name: "blue", Mode: "system", HttpPort: 18080, HttpsPort: 18443, StatusPort: 8082})
	want := render.Profile{Instance: "blue", Mode: "system", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082}
	if p == nil || *p != want {
		t.Fatalf("got %+v", p)
	}
}
