package conn

import (
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 默认实例上报补齐后的默认值，控制台据此识别为默认实例。
func TestInstanceInfo(t *testing.T) {
	d := instanceInfo(render.Profile{})
	if d.Name != "" || d.Mode != "system" || d.HttpPort != 80 || d.HttpsPort != 443 || d.StatusPort != 8081 {
		t.Fatalf("default = %+v", d)
	}
	b := instanceInfo(render.Profile{Instance: "blue", HTTPPort: 18080, HTTPSPort: 18443, StatusPort: 8082})
	if b.Name != "blue" || b.HttpPort != 18080 || b.StatusPort != 8082 {
		t.Fatalf("blue = %+v", b)
	}
}
