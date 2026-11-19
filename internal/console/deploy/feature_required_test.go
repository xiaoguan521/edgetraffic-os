package deploy

import (
	"errors"
	"testing"
)

// 没有接授权检查的 Service 必须拒绝需授权的功能，而不是放行：晚绑定漏接一行不能变成授权失效。
func TestServiceWithoutFeatureCheckDeniesLicensedFeatures(t *testing.T) {
	d := &Service{}
	if err := d.CheckGM(1); !errors.Is(err, ErrGMNotLicensed) {
		t.Fatalf("CheckGM without Feature = %v, want ErrGMNotLicensed", err)
	}
	if err := d.CheckLua(1); !errors.Is(err, ErrLuaNotLicensed) {
		t.Fatalf("CheckLua without Feature = %v, want ErrLuaNotLicensed", err)
	}
	if d.featureOn("stream") || d.featureOn("lua") || d.featureOn("gm_tls") {
		t.Fatal("featureOn must be false when Feature is nil")
	}
}
