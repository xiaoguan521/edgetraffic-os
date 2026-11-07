package instance

import "testing"

func TestSystemdAtLeast(t *testing.T) {
	for out, want := range map[string]bool{
		"systemd 219\n+PAM +AUDIT":         false, // CentOS 7
		"systemd 239 (239-74.el8_8)\n+PAM": true,  // Rocky 8
		"systemd 252 (252.16-1.el9)":       true,
		"systemd 229":                      true,
		"systemd 228":                      false,
		"":                                 false,
		"something else 300":               false,
	} {
		if got := SystemdAtLeast(out, 229); got != want {
			t.Errorf("%q: got %v want %v", out, got, want)
		}
	}
}
