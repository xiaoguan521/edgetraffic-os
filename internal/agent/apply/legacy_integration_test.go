package apply

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/configbundle"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// Real native Lua migration lifecycle, run by the Lua standard/GM and CPU
// matrix using disposable processes, ports and directories.
func TestLegacyLuaLiveLifecycle(t *testing.T) {
	bin, modules := os.Getenv("SYEDGE_LEGACY_IT_BIN"), os.Getenv("SYEDGE_LEGACY_IT_MODULES")
	if bin == "" || modules == "" {
		if os.Getenv("SYEDGE_LEGACY_IT_REQUIRED") != "" {
			t.Fatal("SYEDGE_LEGACY_IT_REQUIRED is set but SYEDGE_LEGACY_IT_BIN / SYEDGE_LEGACY_IT_MODULES are missing: the native Lua lifecycle must not be skipped here")
		}
		t.Skip("set SYEDGE_LEGACY_IT_BIN and SYEDGE_LEGACY_IT_MODULES")
	}
	for _, mode := range []string{"default", "named-shared", "named-independent"} {
		t.Run(mode, func(t *testing.T) { runLegacyLuaLifecycle(t, bin, modules, mode) })
	}
}

func runLegacyLuaLifecycle(t *testing.T, bin, modules, mode string) {
	t.Helper()
	base := t.TempDir()
	root := base
	confRoot := filepath.Join(base, "conf")
	if mode != "default" {
		confRoot = filepath.Join(base, "instances", "blue", "conf")
	}
	if mode == "named-independent" {
		root = filepath.Join(base, "instances", "blue")
	}
	pidFile := filepath.Join(root, "syedge.pid")
	logFile := filepath.Join(root, "error.log")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(confRoot, "releases", "r000000"), 0o755); err != nil {
		t.Fatal(err)
	}
	user := ""
	if os.Geteuid() == 0 {
		user = "user root root;\n"
	}
	makeConfig := func(rev string, syntaxError bool) string {
		rdir := filepath.Join(confRoot, "releases", rev)
		bad := ""
		if syntaxError {
			bad = "invalid_directive on;"
		}
		return fmt.Sprintf(`load_module %s/ndk_http_module.so;
load_module %s/ngx_http_lua_module.so;
%sworker_processes 1;
pid %s;
error_log %s notice;
events { worker_connections 128; }
http {
  access_log off;
  client_body_temp_path %s/client_temp;
  proxy_temp_path %s/proxy_temp;
  fastcgi_temp_path %s/fastcgi_temp;
  uwsgi_temp_path %s/uwsgi_temp;
  scgi_temp_path %s/scgi_temp;
  lua_package_path '%s/lua/?.lua;%s/lualib/?.lua;%s/lualib/?/init.lua;%s/extlib/?.lua;;';
  init_by_lua_file %s/lua/init.lua;
  %s
  server { listen 127.0.0.1:%d; location / { content_by_lua_file %s/lua/content.lua; } }
}
`, modules, modules, user, pidFile, logFile, root, root, root, root, root, rdir, modules, modules, modules, rdir, bad, port, rdir)
	}
	writeBaseline := filepath.Join(confRoot, "releases", "r000000")
	if err := os.MkdirAll(filepath.Join(writeBaseline, "lua"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"syedge.conf": makeConfig("r000000", false), "lua/init.lua": "legacy_init = true", "lua/content.lua": "ngx.say('baseline')"} {
		if err := os.WriteFile(filepath.Join(writeBaseline, name), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("releases", "r000000"), filepath.Join(confRoot, "current")); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(confRoot, "current", "syedge.conf")
	if out, err := exec.Command(bin, "-p", root, "-e", logFile, "-c", current).CombinedOutput(); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	defer func() { _ = exec.Command(bin, "-p", root, "-e", logFile, "-c", current, "-s", "quit").Run() }()
	wrapper := filepath.Join(root, "wrapper.sh")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = test ]; then exec %q -p %q -e %q -t -q -c %q/\"$2\"/syedge.conf; fi\nexec %q -p %q -e %q -c %q -s reload\n", bin, root, logFile, filepath.Join(confRoot, "releases"), bin, root, logFile, current)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Applier{C: Config{ConfRoot: confRoot, Prefix: root, PidFile: pidFile, ErrorLog: logFile, LockFile: filepath.Join(root, "apply.lock"), Wrapper: []string{wrapper}, HTTPPort: port, ConfirmWait: 10 * time.Second}}
	checks := []PostCheck{{Host: "legacy.example", Path: "/", ExpectStatus: []int{200}}}
	// The CI matrix runs as root, so the test master is root: bundles need the
	// frozen root-master declaration, and one without it must be refused.
	rootMaster := os.Geteuid() == 0
	markerBody := "{}"
	if rootMaster {
		markerBody = `{"allow_root_master":true}`
	}
	makeBundle := func(rev, result string, bad bool) ([]byte, string) {
		b := render.Repack(rev, map[string][]byte{
			"syedge.conf":             []byte(makeConfig(rev, bad)),
			"lua/init.lua":            []byte("legacy_init = true"),
			"lua/content.lua":         []byte("local m = require('thirdparty'); assert(require('resty.rsa').PADDING); ngx.say(m.value())"),
			"lua/thirdparty.lua":      []byte(fmt.Sprintf("return {value = function() return %q end}", result)),
			configbundle.LegacyMarker: []byte(markerBody),
		})
		data, err := b.TarGz()
		if err != nil {
			t.Fatal(err)
		}
		return data, b.Manifest.Digest
	}
	get := func() string {
		cl := &http.Client{Timeout: 3 * time.Second}
		r, err := cl.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	ctx := context.Background()
	if rootMaster {
		declared := markerBody
		markerBody = "{}"
		b0, d0 := makeBundle("r000009", "undeclared", false)
		markerBody = declared
		if r := a.Apply(ctx, "r000009", "r000000", b0, d0, checks); r.Status != Rejected || !strings.Contains(r.Message, "root") {
			t.Fatalf("undeclared bundle on a root master: %+v", r)
		}
		if got := get(); got != "baseline" || a.Current() != "r000000" {
			t.Fatalf("refused bundle changed the node: %q, current %s", got, a.Current())
		}
	}
	b1, d1 := makeBundle("r000001", "first", false)
	if r := a.Apply(ctx, "r000001", "r000000", b1, d1, checks); r.Status != Applied {
		t.Fatalf("first apply: %+v", r)
	}
	if got := get(); got != "first" {
		t.Fatalf("first response: %q", got)
	}
	bad, badDigest := makeBundle("r000002", "bad", true)
	if r := a.Apply(ctx, "r000002", "r000001", bad, badDigest, checks); r.Status != Invalid {
		t.Fatalf("invalid config: %+v", r)
	}
	if got := get(); got != "first" {
		t.Fatalf("invalid changed active response: %q", got)
	}
	b3, d3 := makeBundle("r000003", "second", false)
	if r := a.Apply(ctx, "r000003", "r000001", b3, d3, []PostCheck{{Host: "legacy.example", Path: "/", ExpectStatus: []int{418}}}); r.Status != RolledBack {
		t.Fatalf("post-check rollback: %+v", r)
	}
	if got := get(); got != "first" {
		t.Fatalf("rollback response: %q", got)
	}
	b4, d4 := makeBundle("r000004", "second", false)
	if r := a.Apply(ctx, "r000004", "r000001", b4, d4, checks); r.Status != Applied {
		t.Fatalf("second apply: %+v", r)
	}
	if got := get(); got != "second" {
		t.Fatalf("second response: %q", got)
	}
	if r := a.Apply(ctx, "r000001", "r000004", b1, d1, checks); r.Status != Applied {
		t.Fatalf("verified rollback: %+v", r)
	}
	if got := get(); got != "first" {
		t.Fatalf("verified rollback response: %q", got)
	}
}
