package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/apply"
	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

type preflightReport struct {
	Mode            string   `json:"mode"`
	Root            string   `json:"root"`
	Instance        string   `json:"instance"`
	HTTPPort        int      `json:"http_port"`
	HTTPSPort       int      `json:"https_port"`
	StatusPort      int      `json:"status_port"`
	ConfRoot        string   `json:"conf_root"`
	CurrentRevision string   `json:"current_revision"`
	MasterRunning   bool     `json:"master_running"`
	MasterPID       int      `json:"master_pid,omitempty"`
	HasCustomConfig bool     `json:"has_custom_config"`
	DigestDetail    string   `json:"digest_detail,omitempty"`
	RootMaster      bool     `json:"root_master"`
	HasLuaAddon     bool     `json:"has_lua_addon"`
	HasNativeLua    bool     `json:"has_native_lua"`
	RecommendedMode string   `json:"recommended_mode"`
	Hints           []string `json:"hints"`
}

var luaDirectiveRe = regexp.MustCompile(`(?m)^\s*(?:init|init_worker|ssl_certificate|set|rewrite|access|content|balancer|header_filter|body_filter|log)_by_lua(?:_block|_file)?\b`)

// preflightMain 在本机执行独立节点接入前自检，输出控制台实例档案对照表与接管建议。
func preflightMain(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(out)
	root := fs.String("root", envOr(brand.Env("ROOT"), ""), "user 模式的安装根目录")
	instance := fs.String("instance", envOr(brand.Env("INSTANCE"), ""), "实例名（留空为默认实例）")
	confRoot := fs.String("conf-root", "", "覆盖配置根目录")
	asJSON := fs.Bool("json", false, "以 JSON 格式输出")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prof, lay, err := loadInstanceAt(*root, *instance)
	if err != nil {
		return err
	}
	userMode := *root != ""
	cr := *confRoot
	if cr == "" {
		cr = lay.ConfRoot()
	}
	ap := &apply.Applier{C: apply.Config{ConfRoot: cr}}
	rep := preflightReport{
		Mode:            "system",
		Root:            lay.Root(),
		Instance:        *instance,
		HTTPPort:        prof.HTTP(),
		HTTPSPort:       prof.HTTPS(),
		StatusPort:      prof.Status(),
		ConfRoot:        cr,
		CurrentRevision: ap.Current(),
		RootMaster:      !userMode && layout.RootMasterAt("/"),
		RecommendedMode: "observe",
	}
	if userMode {
		rep.Mode = "user"
	}
	if pidBytes, perr := os.ReadFile(lay.PidFile()); perr == nil {
		if pid, aerr := strconv.Atoi(strings.TrimSpace(string(pidBytes))); aerr == nil && pid > 0 {
			if syscall.Kill(pid, 0) == nil {
				rep.MasterRunning = true
				rep.MasterPID = pid
			}
		}
	}

	if rep.CurrentRevision != "" {
		d, derr := ap.TreeDigest()
		if derr != nil {
			rep.HasCustomConfig = true
			rep.DigestDetail = derr.Error()
		} else if strings.HasPrefix(d, "drifted") {
			rep.HasCustomConfig = true
			rep.DigestDetail = strings.TrimSpace(strings.TrimPrefix(d, "drifted:"))
		}
	}

	// 检查 Lua 附加包是否存在
	if entries, err := os.ReadDir(lay.Modules()); err == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), "lua") && strings.HasSuffix(e.Name(), ".so") {
				rep.HasLuaAddon = true
				break
			}
		}
	}

	// 静态扫描 current 主配置与 conf.d 是否包含原生 Lua 指令
	currentConfDir, err := filepath.EvalSymlinks(filepath.Join(cr, "current"))
	if err == nil {
		_ = filepath.WalkDir(currentConfDir, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".conf") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr == nil && luaDirectiveRe.Match(b) {
				rep.HasNativeLua = true
			}
			return nil
		})
	}

	if rep.HasNativeLua {
		rep.RecommendedMode = "legacy"
		rep.Hints = append(rep.Hints, "检测到原生 *_by_lua* 指令：建议在控制台先将节点组设为“兼容配置（legacy）”或“观察接入（observe）”，确认后再决定是否迁移为受限脚本。")
	} else if rep.HasCustomConfig {
		rep.RecommendedMode = "observe"
		rep.Hints = append(rep.Hints, "检测到独立模式自定义配置：请先在控制台将节点组设为“观察接入（observe）”，并在节点使用带 -replace-local-config 的已有实例注册命令。")
		rep.Hints = append(rep.Hints, fmt.Sprintf("注册前建议执行：%s backup-config 或 %s export-config 备份并导出完整配置。", brand.Agent(), brand.Agent()))
	} else {
		rep.RecommendedMode = "managed"
		rep.Hints = append(rep.Hints, "当前配置为软件包基线，无手工改动，可直接接入“控制台托管（managed）”节点组。")
	}
	if rep.RootMaster {
		rep.Hints = append(rep.Hints, fmt.Sprintf("当前主进程处于 root-master 兼容模式：如使用兼容配置包，需提交人显式勾选允许 root master；建议先执行 %s-instance nonroot 无中断切换。", brand.ID()))
	}

	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	instName := rep.Instance
	if instName == "" {
		instName = "（留空：默认实例）"
	}
	_, _ = fmt.Fprintf(out, "=== 控制台实例档案对照表 ===\n")
	_, _ = fmt.Fprintf(out, "运行方式       : %s\n", rep.Mode)
	_, _ = fmt.Fprintf(out, "安装根目录     : %s\n", rep.Root)
	_, _ = fmt.Fprintf(out, "实例名         : %s\n", instName)
	_, _ = fmt.Fprintf(out, "端口           : HTTP %d / HTTPS %d / 状态页 %d\n", rep.HTTPPort, rep.HTTPSPort, rep.StatusPort)
	_, _ = fmt.Fprintf(out, "配置根目录     : %s（当前版本：%s）\n", rep.ConfRoot, rep.CurrentRevision)
	_, _ = fmt.Fprintf(out, "数据面进程     : %v（PID %d）\n", rep.MasterRunning, rep.MasterPID)
	_, _ = fmt.Fprintf(out, "本地自定义配置 : %v\n", rep.HasCustomConfig)
	_, _ = fmt.Fprintf(out, "Lua 附加包     : %v（配置含原生 Lua：%v）\n", rep.HasLuaAddon, rep.HasNativeLua)
	_, _ = fmt.Fprintf(out, "推荐接管方式   : %s\n", rep.RecommendedMode)
	_, _ = fmt.Fprintf(out, "\n=== 接管前建议 ===\n")
	for i, h := range rep.Hints {
		_, _ = fmt.Fprintf(out, "%d. %s\n", i+1, h)
	}
	return nil
}

var refDirectiveRe = regexp.MustCompile(`(?m)^\s*(include|ssl_certificate|ssl_certificate_key|[a-z_]+_by_lua_file)\s+([^;\s#]+)`)

const (
	exportMaxFiles = 4000
	exportMaxBytes = 48 << 20
)

// exportConfig 静态扫描主配置及其 include/证书/Lua 文件引用，打包为可直接上传控制台的 tar.gz。
func exportConfig(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export-config", flag.ContinueOnError)
	fs.SetOutput(out)
	root := fs.String("root", envOr(brand.Env("ROOT"), ""), "user 模式的安装根目录（省略 --config 时自动定位）")
	instance := fs.String("instance", envOr(brand.Env("INSTANCE"), ""), "实例名（省略 --config 时自动定位）")
	config := fs.String("config", "", "主配置文件绝对路径（留空则自动读取本实例 conf/current/"+brand.ConfName()+"）")
	output := fs.String("output", "", "输出 tar.gz 绝对路径（--dry-run 时可省略）")
	dryRun := fs.Bool("dry-run", false, "仅扫描并打印发现的文件与外部依赖，不写入归档")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfgPath := *config
	if cfgPath == "" {
		_, lay, err := loadInstanceAt(*root, *instance)
		if err != nil {
			return fmt.Errorf("未指定 --config 且无法定位实例配置：%w", err)
		}
		cfgPath = filepath.Join(lay.ConfRoot(), "current", brand.ConfName())
	}
	if !filepath.IsAbs(cfgPath) {
		return errors.New("请提供 --config 主配置绝对路径")
	}
	if !*dryRun && !filepath.IsAbs(*output) {
		return errors.New("请提供 --output 输出 tar.gz 绝对路径（或加 --dry-run）")
	}
	mainPath, err := filepath.EvalSymlinks(filepath.Clean(cfgPath))
	if err != nil {
		return fmt.Errorf("无法读取主配置 %s：%w", *config, err)
	}
	baseDir := filepath.Dir(mainPath)

	files := map[string]string{} // archive rel path -> abs disk path
	external := map[string]bool{}
	missing := map[string]bool{}
	scanned := map[string]bool{}

	var scanConf func(absPath string)
	scanConf = func(absPath string) {
		if scanned[absPath] {
			return
		}
		scanned[absPath] = true
		b, rerr := os.ReadFile(absPath)
		if rerr != nil {
			missing[absPath] = true
			return
		}
		rel, rerr := filepath.Rel(baseDir, absPath)
		if rerr == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			files[filepath.ToSlash(rel)] = absPath
		} else {
			external[absPath] = true
		}
		for _, m := range refDirectiveRe.FindAllStringSubmatch(string(b), -1) {
			dir, arg := m[1], strings.Trim(m[2], `"'`)
			if arg == "" || strings.Contains(arg, "$") {
				continue
			}
			target := arg
			if !filepath.IsAbs(target) {
				target = filepath.Join(baseDir, target)
			}
			matches, gerr := filepath.Glob(target)
			if gerr != nil || len(matches) == 0 {
				missing[target] = true
				continue
			}
			for _, mp := range matches {
				fi, serr := os.Stat(mp)
				if serr != nil || !fi.Mode().IsRegular() {
					continue
				}
				if dir == "include" {
					scanConf(mp)
				} else {
					r, rerr := filepath.Rel(baseDir, mp)
					if rerr == nil && !strings.HasPrefix(r, "..") && !filepath.IsAbs(r) {
						files[filepath.ToSlash(r)] = mp
					} else {
						external[mp] = true
					}
				}
			}
		}
	}
	scanConf(mainPath)

	// 根目录放主配置名（若原文件名不是 syedge.conf 也保留原名并提示）
	relList := make([]string, 0, len(files))
	var totalBytes int64
	for rel, abs := range files {
		fi, serr := os.Stat(abs)
		if serr != nil {
			return serr
		}
		totalBytes += fi.Size()
		relList = append(relList, rel)
	}
	sort.Strings(relList)

	if len(relList) > exportMaxFiles {
		return fmt.Errorf("文件数量 %d 超过上限 %d", len(relList), exportMaxFiles)
	}
	if totalBytes > exportMaxBytes {
		return fmt.Errorf("解压总大小 %d 字节超过上限 %d 字节", totalBytes, exportMaxBytes)
	}

	_, _ = fmt.Fprintf(out, "扫描主配置：%s\n", mainPath)
	_, _ = fmt.Fprintf(out, "可打包文件：%d 个（共 %d 字节）\n", len(relList), totalBytes)
	if len(external) > 0 {
		ext := make([]string, 0, len(external))
		for p := range external {
			ext = append(ext, p)
		}
		sort.Strings(ext)
		_, _ = fmt.Fprintf(out, "注意：以下外部路径不在主配置目录内，未自动打包（请按需备份或手动放入）：\n")
		for _, p := range ext {
			_, _ = fmt.Fprintf(out, "  - %s\n", p)
		}
	}
	if len(missing) > 0 {
		ms := make([]string, 0, len(missing))
		for p := range missing {
			ms = append(ms, p)
		}
		sort.Strings(ms)
		_, _ = fmt.Fprintf(out, "注意：以下引用路径未找到：\n")
		for _, p := range ms {
			_, _ = fmt.Fprintf(out, "  - %s\n", p)
		}
	}
	if *dryRun {
		return nil
	}

	f, err := os.OpenFile(filepath.Clean(*output), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(*output)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, rel := range relList {
		abs := files[rel]
		fi, err := os.Stat(abs)
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		h.Name = rel
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		src, err := os.Open(abs)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	ok = true
	_, _ = fmt.Fprintf(out, "已导出配置包到 %s，可直接上传至控制台“配置导入”或“兼容配置”。\n", *output)
	return nil
}
