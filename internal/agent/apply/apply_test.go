package apply

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 集成测试：需要 root、已安装的 /opt/syapp、正在运行的 syapp 与 /usr/libexec/syapp/syapp-wrapper。
// 运行：sudo SYAPP_IT=1 go test ./internal/agent/apply/
func itApplier(t *testing.T) *Applier {
	t.Helper()
	if os.Getenv("SYAPP_IT") != "1" {
		t.Skip("set SYAPP_IT=1 to run against a live syapp")
	}
	return &Applier{C: Config{
		ConfRoot: "/opt/syapp/conf", PidFile: "/run/syapp/syapp.pid", ErrorLog: "/var/log/syapp/error.log",
		LockFile: "/run/syapp-agent/apply.lock", Wrapper: []string{"/usr/libexec/syapp/syapp-wrapper"},
		ConfirmWait: 5 * time.Second,
	}}
}

func page(rev, body string) render.Input {
	mt, _ := os.ReadFile("/opt/syapp/share/defaults/mime.types")
	return render.Input{
		Revision: rev, MimeTypes: mt,
		Sites: []render.Site{{Name: "it", Domains: []string{"it.example.com"},
			Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "static", Static: &render.StaticSpec{IndexHTML: body}}}}},
	}
}

func bundleOf(t *testing.T, in render.Input) ([]byte, string) {
	t.Helper()
	b, err := render.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	tgz, err := b.TarGz()
	if err != nil {
		t.Fatal(err)
	}
	return tgz, b.Manifest.Digest
}

// tamper 在合法 bundle 上增加或替换文件并重算清单（模拟“通过了控制台但内容有问题”的版本）。
func tamper(t *testing.T, tgz []byte, rev string, extra map[string]string) ([]byte, string) {
	t.Helper()
	files := map[string][]byte{}
	gz, _ := gzip.NewReader(bytes.NewReader(tgz))
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
	for k, v := range extra {
		files[k] = []byte(v)
	}
	delete(files, manifestName)
	m := manifest{Revision: rev, Files: map[string]string{}}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	all := sha256.New()
	for _, n := range names {
		s := sha256.Sum256(files[n])
		m.Files[n] = hex.EncodeToString(s[:])
		_, _ = fmt.Fprintf(all, "%s %s\n", m.Files[n], n)
	}
	m.Digest = "sha256:" + hex.EncodeToString(all.Sum(nil))
	mj, _ := json.Marshal(m)
	files[manifestName] = mj
	names = append(names, manifestName)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes(), m.Digest
}

func get(t *testing.T) string {
	t.Helper()
	tr := &http.Transport{Proxy: nil}
	req, _ := http.NewRequest("GET", "http://127.0.0.1/", nil)
	req.Host = "it.example.com"
	resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestLiveLifecycle(t *testing.T) {
	a := itApplier(t)
	ctx := context.Background()
	base := 900000 + int(time.Now().Unix()%9000)*10
	rev := func(i int) string { return fmt.Sprintf("r%06d", base+i) }
	check := []PostCheck{{Host: "it.example.com", Path: "/", ExpectStatus: []int{200}}}

	// 1. 正常发布
	start := a.Current()
	tgz, dg := bundleOf(t, page(rev(1), "v1"))
	if r := a.Apply(ctx, rev(1), start, tgz, dg, check); r.Status != Applied {
		t.Fatalf("apply v1: %+v", r)
	}
	if got := get(t); got != "v1" {
		t.Fatalf("served %q", got)
	}

	// 2. 父版本不符
	tgz2, dg2 := bundleOf(t, page(rev(2), "v2"))
	if r := a.Apply(ctx, rev(2), "r000001", tgz2, dg2, check); r.Status != Conflict {
		t.Fatalf("expected conflict: %+v", r)
	}

	// 3. 语法错误：syapp -t 失败，current 不变
	bad, dgb := tamper(t, tgz2, rev(2), map[string]string{"conf.d/zz-bad.conf": "server { listen 80 }\n"})
	if r := a.Apply(ctx, rev(2), rev(1), bad, dgb, check); r.Status != Invalid || !strings.Contains(r.Stderr, "syapp:") {
		t.Fatalf("expected invalid: %+v", r)
	}
	if a.Current() != rev(1) || get(t) != "v1" {
		t.Fatal("current changed after invalid config")
	}

	// 4. 通过 -t 但 reload 失败（端口被占用）：自动回滚
	ln, err := net.Listen("tcp", "127.0.0.1:18099")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	busy, _ := tamper(t, tgz2, rev(3), map[string]string{"conf.d/zz-busy.conf": "server { listen 127.0.0.1:18099; return 200; }\n"})
	busy, dgx := retagRevision(t, busy, rev(3))
	r := a.Apply(ctx, rev(3), rev(1), busy, dgx, check)
	if r.Status != RolledBack {
		t.Fatalf("expected rollback on bind failure: %+v", r)
	}
	if a.Current() != rev(1) || get(t) != "v1" {
		t.Fatalf("not rolled back: current=%s body=%q", a.Current(), get(t))
	}
	_ = ln.Close()

	// 5. 后置探活失败：自动回滚
	tgz4, dg4 := bundleOf(t, page(rev(4), "v4"))
	r = a.Apply(ctx, rev(4), rev(1), tgz4, dg4, []PostCheck{{Host: "it.example.com", Path: "/", ExpectStatus: []int{418}}})
	if r.Status != RolledBack || get(t) != "v1" {
		t.Fatalf("expected rollback on post check: %+v body=%q", r, get(t))
	}

	// 6. 再发布一个正常版本，然后回滚到上一个
	tgz5, dg5 := bundleOf(t, page(rev(5), "v5"))
	if r := a.Apply(ctx, rev(5), rev(1), tgz5, dg5, check); r.Status != Applied || get(t) != "v5" {
		t.Fatalf("apply v5: %+v", r)
	}
	if r := a.Rollback(ctx, rev(1), check); r.Status != Applied || get(t) != "v1" {
		t.Fatalf("rollback: %+v body=%q", r, get(t))
	}
	if d, err := a.TreeDigest(); err != nil || strings.HasPrefix(d, "drifted") {
		t.Fatalf("tree digest: %s %v", d, err)
	}
	// 7. 漂移检测
	_ = os.WriteFile("/opt/syapp/conf/current/conf.d/hand-edit.conf", []byte("# manual\n"), 0o644)
	if d, _ := a.TreeDigest(); !strings.HasPrefix(d, "drifted") {
		t.Fatalf("drift not detected: %s", d)
	}
	_ = os.Remove("/opt/syapp/conf/current/conf.d/hand-edit.conf")

	// 恢复测试前的版本
	if start != "" {
		_ = a.Rollback(ctx, start, nil)
	}
}

// retagRevision 修正 tamper 生成包中的清单版本号（tamper 用的是传入 rev）。
func retagRevision(t *testing.T, tgz []byte, rev string) ([]byte, string) {
	return tamper(t, tgz, rev, nil)
}

func TestUnpackRejectsUnsafe(t *testing.T) {
	dir := t.TempDir()
	a := &Applier{C: Config{ConfRoot: dir, LockFile: dir + "/lock"}}
	_ = os.MkdirAll(dir+"/releases", 0o755)
	mk := func(name string) []byte {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		_ = gw.Close()
		return buf.Bytes()
	}
	for _, n := range []string{"../evil", "/etc/passwd", "a/../../evil", "./x"} {
		if err := a.unpack("r000123", mk(n), "sha256:x"); err == nil {
			t.Errorf("%s accepted", n)
		}
	}
	// 符号链接
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	_ = gw.Close()
	if err := a.unpack("r000124", buf.Bytes(), "sha256:x"); err == nil {
		t.Error("symlink accepted")
	}
	// 清单不符
	tgz, _ := bundleOf(t, render.Input{Revision: "r000125", MimeTypes: []byte("types {}\n"),
		Sites: []render.Site{{Name: "a1", Domains: []string{"a.example.com"}, Routes: []render.Route{{Path: "/", Match: "prefix", Mode: "deny"}}}}})
	if err := a.unpack("r000125", tgz, "sha256:wrong"); err == nil {
		t.Error("digest mismatch accepted")
	}
	if a.Apply(context.Background(), "r1/../x", "", nil, "", nil).Status != Rejected {
		t.Error("bad revision accepted")
	}
	// 磁盘空间不足预检
	aLowDisk := &Applier{C: Config{
		ConfRoot:     dir,
		LockFile:     dir + "/lock",
		MinFreeBytes: 64 << 20,
		FreeBytes:    func(string) (uint64, error) { return 1 << 20, nil },
	}}
	res := aLowDisk.Apply(context.Background(), "r000125", "", tgz, "sha256:x", nil)
	if res.Status != Rejected || !strings.Contains(res.Message, "insufficient disk space") {
		t.Fatalf("expected insufficient disk space rejection, got %+v", res)
	}
}
