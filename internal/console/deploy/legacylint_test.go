package deploy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
)

func lintFiles(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

func codes(ws []legacyWarning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func hasCode(ws []legacyWarning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestLintLegacyVersionedModulePathIsReported(t *testing.T) {
	ws := lintLegacyLua(lintFiles(map[string]string{
		"syedge.conf": "http {\n    lua_package_path \"/opt/syedge/modules/lua-1.30.6-1/lualib/?.lua;;\";\n    lua_package_cpath \"/opt/syedge/modules/lua-1.30.6-1/lualib/?.so;;\";\n}\n",
	}))
	var got []legacyWarning
	for _, w := range ws {
		if w.Code == "lua-versioned-path" {
			got = append(got, w)
		}
	}
	if len(got) != 2 || got[0].File != "syedge.conf" || got[0].Line != 2 || got[1].Line != 3 {
		t.Fatalf("versioned path warnings = %+v", ws)
	}
	if !strings.Contains(got[0].Message, "modules/lua") {
		t.Fatalf("message must point at the stable entry: %q", got[0].Message)
	}
}

func TestLintLegacyStablePathAndCommentsAreQuiet(t *testing.T) {
	ws := lintLegacyLua(lintFiles(map[string]string{
		"syedge.conf": "http {\n    # lua_package_path \"/opt/syedge/modules/lua-1.30.6-1/lualib/?.lua;;\";\n    lua_package_path \"/opt/syedge/lualib/?.lua;/opt/syedge/modules/lua/lualib/?.lua;;\";\n}\n",
	}))
	if hasCode(ws, "lua-versioned-path") {
		t.Fatalf("stable path or commented line was reported: %+v", ws)
	}
}

func TestLintLegacyMissingPackagePath(t *testing.T) {
	body := "http {\n  server {\n    location / {\n      rewrite_by_lua_block {\n        ngx.log(ngx.INFO, 'x')\n      }\n    }\n  }\n}\n"
	ws := lintLegacyLua(lintFiles(map[string]string{"syedge.conf": body}))
	if !hasCode(ws, "lua-package-path-missing") {
		t.Fatalf("a Lua directive without lua_package_path was not reported: %v", codes(ws))
	}
	// 路径可以写在任意一个 conf 文件里
	ws = lintLegacyLua(lintFiles(map[string]string{
		"syedge.conf":             body,
		"conf.d/00-lua-path.conf": "lua_package_path \"/opt/syedge/lualib/?.lua;;\";\n",
	}))
	if hasCode(ws, "lua-package-path-missing") {
		t.Fatalf("lua_package_path in an included file was ignored: %v", codes(ws))
	}
	// 没有 Lua 指令就不提示
	ws = lintLegacyLua(lintFiles(map[string]string{"syedge.conf": "http { server { listen 80; } }\n"}))
	if len(ws) != 0 {
		t.Fatalf("plain configuration produced warnings: %+v", ws)
	}
	// 被注释掉的 Lua 指令不算
	ws = lintLegacyLua(lintFiles(map[string]string{"syedge.conf": "http {\n#  rewrite_by_lua_block { }\n}\n"}))
	if hasCode(ws, "lua-package-path-missing") {
		t.Fatalf("commented Lua directive counted: %v", codes(ws))
	}
}

func TestLintLegacyUninitializedGlobal(t *testing.T) {
	use := "http {\n server {\n  location / {\n   rewrite_by_lua_block {\n    skywalking_tracer:start(\"upstream service\")\n   }\n  }\n }\n}\n"
	ws := lintLegacyLua(lintFiles(map[string]string{"syedge.conf": use, "conf.d/p.conf": "lua_package_path \"/opt/syedge/lualib/?.lua;;\";\n"}))
	var got *legacyWarning
	for i := range ws {
		if ws[i].Code == "lua-global-uninitialized" {
			got = &ws[i]
		}
	}
	if got == nil || got.Target != "skywalking_tracer" || got.File != "syedge.conf" || got.Line != 5 {
		t.Fatalf("uninitialized skywalking_tracer not reported at its first use: %+v", ws)
	}
	for name, init := range map[string]string{
		"assigned in init_worker":  "init_worker_by_lua_block {\n    skywalking_tracer = require(\"skywalking.tracer\")\n}\n",
		"assigned via _G":          "init_by_lua_block {\n    _G.skywalking_tracer = {}\n}\n",
		"assigned in a Lua module": "",
	} {
		files := map[string]string{"syedge.conf": use, "conf.d/p.conf": "lua_package_path \"/opt/syedge/lualib/?.lua;;\";\n"}
		if init != "" {
			files["conf.d/10-init.conf"] = init
		} else {
			files["lua/init.lua"] = "skywalking_tracer = require('skywalking.tracer')\n"
		}
		if ws := lintLegacyLua(lintFiles(files)); hasCode(ws, "lua-global-uninitialized") {
			t.Fatalf("%s: initialized global still reported: %+v", name, ws)
		}
	}
	// local 变量不会创建全局，仍要提示
	files := map[string]string{
		"syedge.conf":         use,
		"conf.d/p.conf":       "lua_package_path \"/opt/syedge/lualib/?.lua;;\";\n",
		"conf.d/10-init.conf": "init_worker_by_lua_block {\n    local skywalking_tracer = require(\"skywalking.tracer\")\n}\n",
	}
	if ws := lintLegacyLua(lintFiles(files)); !hasCode(ws, "lua-global-uninitialized") {
		t.Fatalf("a local variable was taken as a global initialization: %+v", ws)
	}
	// 被注释掉的初始化不算
	files["conf.d/10-init.conf"] = "# init_worker_by_lua_block {\n#    skywalking_tracer = require(\"skywalking.tracer\")\n# }\n"
	if ws := lintLegacyLua(lintFiles(files)); !hasCode(ws, "lua-global-uninitialized") {
		t.Fatalf("a commented-out initialization was taken as real: %+v", ws)
	}
}

func TestLintLegacyOtherKnownGlobals(t *testing.T) {
	ws := lintLegacyLua(lintFiles(map[string]string{
		"syedge.conf": "http {\n lua_package_path \"/opt/syedge/lualib/?.lua;;\";\n server {\n  listen 9145;\n  location /metrics {\n   content_by_lua_block {\n    metric_connections:set(ngx.var.connections_reading, {\"reading\"})\n    prometheus:collect()\n   }\n  }\n }\n}\n",
	}))
	var targets []string
	for _, w := range ws {
		if w.Code == "lua-global-uninitialized" {
			targets = append(targets, w.Target)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("metric globals not reported once each: %+v", ws)
	}
}

func TestLintLegacyIgnoresNonConfigFilesAndIsDeterministic(t *testing.T) {
	files := lintFiles(map[string]string{
		"README.txt":  "lua_package_path \"/opt/syedge/modules/lua-1.0.0/lualib/?.lua;;\";\n",
		"b.conf":      "lua_package_path \"/opt/syedge/modules/lua-2.0.0/lualib/?.lua;;\";\n",
		"a.conf":      "lua_package_path \"/opt/syedge/modules/lua-1.0.0/lualib/?.lua;;\";\n",
		"lua/x.lua":   "-- lua_package_path \"/opt/syedge/modules/lua-3.0.0/lualib/?.lua;;\";\n",
		"private.key": "-----BEGIN PRIVATE KEY-----\n",
	})
	first, second := lintLegacyLua(files), lintLegacyLua(files)
	if len(first) != 2 || first[0].File != "a.conf" || first[1].File != "b.conf" {
		t.Fatalf("expected a.conf then b.conf only, got %+v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("warnings are not deterministic")
		}
	}
}

func TestLegacyWarningNoteIsShortAndLinksEvidence(t *testing.T) {
	var ws []legacyWarning
	for i := 1; i <= 6; i++ {
		ws = append(ws, legacyWarning{Code: "lua-versioned-path", File: "syedge.conf", Line: i})
	}
	note := legacyWarningNote(ws)
	if !strings.Contains(note, "静态检查") || !strings.Contains(note, "syedge.conf:1") || !strings.Contains(note, "6") {
		t.Fatalf("note = %q", note)
	}
	if strings.Contains(note, "syedge.conf:4") {
		t.Fatalf("note must list only the first few items: %q", note)
	}
	if legacyWarningNote(nil) != "" {
		t.Fatal("no warnings must add nothing to the note")
	}
}

func TestRequestLegacyShowsLuaWarningsToTheApproverAndAuditsThem(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "admin-a", ""); err != nil {
		t.Fatal(err)
	}
	data := legacyArchive(t, map[string]string{
		"syedge.conf": "events {} http {\n lua_package_path \"/opt/syedge/modules/lua-1.30.6-1/lualib/?.lua;;\";\n server { location / { rewrite_by_lua_block { skywalking_tracer:start('x') } } }\n}\n",
	})
	req, err := d.RequestLegacy(g.ID, "migration", data, nil, LegacyOptions{}, "admin-a", "")
	if err != nil {
		t.Fatalf("warnings must not block the submission: %v", err)
	}
	if !strings.HasPrefix(req.Note, "migration") || !strings.Contains(req.Note, "静态检查") || !strings.Contains(req.Note, "syedge.conf:2") {
		t.Fatalf("approver cannot see the static-check hints: %q", req.Note)
	}
}

func TestRequestLegacyCleanPackageKeepsTheNote(t *testing.T) {
	d, g := newTestService(t)
	box, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	d.Box = box
	if err := d.SetManagementMode(g.ID, ManagementModeLegacy, "admin-a", ""); err != nil {
		t.Fatal(err)
	}
	clean := legacyArchive(t, map[string]string{"syedge.conf": "events {} http { server { listen 80; } }\n"})
	req, err := d.RequestLegacy(g.ID, "migration", clean, nil, LegacyOptions{}, "admin-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Note != "migration" {
		t.Fatalf("a clean package changed the note: %q", req.Note)
	}
}
