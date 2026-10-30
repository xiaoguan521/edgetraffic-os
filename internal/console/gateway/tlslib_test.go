package gateway

import "testing"

func TestTLSLib(t *testing.T) {
	gm := "syapp version: syapp/1.30.6 (gmssl)\nbuilt with OpenSSL 3.5.4 30 Sep 2026\nconfigure arguments: --prefix=/opt/syapp --with-openssl=/tmp/w/deps/Tongsuo --with-openssl-opt='enable-ntls no-shared'\n"
	std := "syapp version: syapp/1.30.6\nbuilt with OpenSSL 3.5.9 1 Sep 2026\nconfigure arguments: --prefix=/opt/syapp --with-openssl-opt='no-shared no-tests'\n"
	if TLSLib(gm) != "tongsuo" || TLSLib(std) != "openssl" || TLSLib("") != "openssl" {
		t.Fatal("variant detection")
	}
}
