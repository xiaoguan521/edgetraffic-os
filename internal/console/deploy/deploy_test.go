package deploy

import "testing"

// 版本号列为 varchar(16)，PostgreSQL 严格校验长度：临时版本号不能超过 16 个字符，且不能与正式格式 r%06d 冲突。
func TestTempRevisionIDFitsColumn(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := tempRevisionID()
		if len(id) > 16 || id[0] != 't' {
			t.Fatalf("temp revision id %q does not fit varchar(16)", id)
		}
		seen[id] = true
	}
	if len(seen) < 2 {
		t.Fatal("temp revision ids do not vary")
	}
}
