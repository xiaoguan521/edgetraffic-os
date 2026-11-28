package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseStub(t *testing.T) {
	s, err := ParseStub("Active connections: 3 \nserver accepts handled requests\n 10 10 25 \nReading: 0 Writing: 1 Waiting: 2 \n")
	if err != nil || s.Active != 3 || s.Requests != 25 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestAccessLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "access.json")
	lines := `{"ts":"2026-10-27T10:00:05+08:00","site":"shop.example.com","status":200,"bytes":100,"rt":0.010}
{"ts":"2026-10-27T10:00:30+08:00","site":"shop.example.com","status":502,"bytes":50,"rt":0.200}
{"ts":"2026-10-27T10:00:40+08:00","site":"_","status":404,"bytes":10,"rt":0.001}
{"ts":"2026-10-27T10:01:01+08:00","site":"shop.example.com","status":301,"bytes":1,"rt":0.002}
{"ts":"2026-10-27T10:01:02+08:00","site":"shop.exam`
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &AccessLog{Path: p}
	now, _ := time.Parse(time.RFC3339, "2026-10-27T10:01:30+08:00")
	got := a.Collect(now)
	if len(got) != 2 {
		t.Fatalf("got %d minutes: %+v", len(got), got)
	}
	var shop Minute
	for _, m := range got {
		if m.Site == "shop.example.com" {
			shop = m
		}
	}
	if shop.Requests != 2 || shop.S2xx != 1 || shop.S5xx != 1 || shop.Bytes != 150 {
		t.Fatalf("shop minute: %+v", shop)
	}
	// 补全最后一行后，下一分钟结束时上报 10:01
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`ple.com","status":200,"bytes":5,"rt":0.003}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	later, _ := time.Parse(time.RFC3339, "2026-10-27T10:02:10+08:00")
	got = a.Collect(later)
	if len(got) != 1 || got[0].Requests != 2 || got[0].S3xx != 1 {
		t.Fatalf("second collect: %+v", got)
	}
}

func TestReadVersion(t *testing.T) {
	for _, c := range []struct {
		code int
		body string
		want string
	}{
		{200, "1.30.6\n", "1.30.6"},
		{404, "<html>404</html>", ""},   // 旧配置没有版本路径
		{200, "1.30.6; rm -rf /\n", ""}, // 只接受版本号
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		got, err := ReadVersion(context.Background(), srv.URL)
		srv.Close()
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("%d %q: got %q, %v", c.code, c.body, got, err)
		}
	}
}

func TestDiskUsage(t *testing.T) {
	used, inode, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if used < 0 || used > 100 || inode < 0 || inode > 100 {
		t.Fatalf("unexpected disk usage: used=%v inode=%v", used, inode)
	}
}

func TestTailFileInitialAppendRotationAndTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "access.json")
	if err := os.WriteFile(p, []byte("line-1\nline-2\nline-3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []string
	notify := make(chan struct{}, 32)
	done := make(chan error, 1)
	go func() {
		done <- TailFile(ctx, p, TailOptions{
			InitialLines: 2,
			MaxLineBytes: 24,
			PollInterval: 30 * time.Millisecond,
		}, func(lines []string) {
			mu.Lock()
			got = append(got, lines...)
			mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
		})
	}()

	waitFor := func(wantSub string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			mu.Lock()
			joined := strings.Join(got, "\n")
			mu.Unlock()
			if strings.Contains(joined, wantSub) {
				return
			}
			select {
			case <-notify:
			case <-deadline:
				t.Fatalf("timed out waiting for %q; got:\n%s", wantSub, joined)
			}
		}
	}

	// 1. 初始回放末尾 2 行（不包含 line-1）
	waitFor("line-3")
	mu.Lock()
	initJoined := strings.Join(got, "\n")
	mu.Unlock()
	if strings.Contains(initJoined, "line-1") || !strings.Contains(initJoined, "line-2") {
		t.Fatalf("unexpected initial lines: %q", initJoined)
	}

	// 2. 增量写入 + 超长行截断
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("live-4\n" + strings.Repeat("x", 40) + "\n")
	_ = f.Close()
	waitFor("live-4")
	waitFor("...[truncated]")

	// 3. 模拟日志轮转（重命名旧文件并创建新文件）
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rotated-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("rotated-1")

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
