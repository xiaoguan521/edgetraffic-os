package collect

import (
	"os"
	"path/filepath"
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
