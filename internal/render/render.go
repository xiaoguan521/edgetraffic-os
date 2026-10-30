package render

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"join":     strings.Join,
	"routeDir": routeDir,
	"indent":   indentSnippet,
}).ParseFS(tmplFS, "templates/*.tmpl"))

// routeDir 为静态路由生成稳定的目录名，与路由在列表中的位置无关。
func routeDir(r Route) string {
	sum := sha256.Sum256([]byte(r.Match + " " + r.Path))
	return "p" + hex.EncodeToString(sum[:4])
}

// Bundle 是一个版本目录的全部文件（相对路径 → 内容）。
type Bundle struct {
	Revision string
	Files    map[string][]byte
	Manifest Manifest
}

// Manifest 记录版本内每个文件的 sha256；Digest 是整个版本的指纹。
type Manifest struct {
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
	Digest   string            `json:"digest"`
}

// ManifestName 是版本目录中清单文件的名字，agent 用它做漂移检测（规格书 5.6）。
const ManifestName = "MANIFEST.json"

// Render 校验输入并生成版本目录。相同输入总是得到相同的 Digest。
func Render(in Input) (*Bundle, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	// 片段在副本上校验并规范化，不修改调用方的数据
	in.Sites = append([]Site(nil), in.Sites...)
	for i := range in.Sites {
		in.Sites[i].Routes = append([]Route(nil), in.Sites[i].Routes...)
	}
	in.HTTPSnippets = append([]NamedSnippet(nil), in.HTTPSnippets...)
	if err := in.validateInputSnippets(); err != nil {
		return nil, err
	}
	if len(in.MimeTypes) == 0 {
		return nil, fmt.Errorf("render: mime.types content is required")
	}
	files := map[string][]byte{}
	exec := func(name string, data any) ([]byte, error) {
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		return b.Bytes(), nil
	}

	var err error
	if files["syapp.conf"], err = exec("syapp.conf.tmpl", in); err != nil {
		return nil, err
	}
	if files["snippets/security.conf"], err = exec("security.conf.tmpl", in); err != nil {
		return nil, err
	}
	files["mime.types"] = in.MimeTypes
	// 挑战片段总是生成（无挑战时只有注释行），这样 ACME 期间只有这一个文件变化
	if files[AcmeSnippet], err = exec("acme.conf.tmpl", in); err != nil {
		return nil, err
	}

	ups := append([]UpstreamGroup(nil), in.Upstreams...)
	sort.Slice(ups, func(i, j int) bool { return ups[i].Name < ups[j].Name })
	if len(ups) > 0 {
		if files["conf.d/00-upstreams.conf"], err = exec("upstreams.conf.tmpl", struct {
			Revision  string
			Upstreams []UpstreamGroup
		}{in.Revision, ups}); err != nil {
			return nil, err
		}
	}

	for _, sn := range in.HTTPSnippets {
		files["conf.d/snippet-"+sn.Name+".conf"] = []byte("# 自定义片段 " + sn.Name + "（经审批，渲染器规范化输出，禁止手改）\n" + sn.Text)
	}

	for _, s := range in.Sites {
		routes := append([]Route(nil), s.Routes...)
		// 确定性定序：精确匹配在前，其余按路径长度降序、再按字典序
		sort.SliceStable(routes, func(i, j int) bool {
			a, b := routes[i], routes[j]
			if a.Match != b.Match {
				return a.Match == "exact"
			}
			if len(a.Path) != len(b.Path) {
				return len(a.Path) > len(b.Path)
			}
			return a.Path < b.Path
		})
		if files["conf.d/site-"+s.Name+".conf"], err = exec("site.conf.tmpl", struct {
			Revision string
			Site     Site
			Routes   []Route
		}{in.Revision, s, routes}); err != nil {
			return nil, err
		}
		if s.HTTPS != nil {
			files["certs/site-"+s.Name+".crt"] = []byte(s.HTTPS.CertPEM)
			files["certs/site-"+s.Name+".key"] = []byte(s.HTTPS.KeyPEM)
		}
		for _, r := range routes {
			if r.Mode == "static" {
				files[path.Join("sites", s.Name, routeDir(r), "index.html")] = []byte(r.Static.IndexHTML)
			}
		}
	}

	return Repack(in.Revision, files), nil
}

// Repack 为一组文件重新生成清单（忽略其中旧的 MANIFEST.json）。
// 证书续期用它在线上版本的基础上只替换证书文件，得到新版本。
func Repack(rev string, src map[string][]byte) *Bundle {
	files := make(map[string][]byte, len(src))
	for n, b := range src {
		if n != ManifestName {
			files[n] = b
		}
	}
	m := Manifest{Revision: rev, Files: map[string]string{}}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	all := sha256.New()
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		m.Files[n] = hex.EncodeToString(sum[:])
		fmt.Fprintf(all, "%s %s\n", m.Files[n], n)
	}
	m.Digest = "sha256:" + hex.EncodeToString(all.Sum(nil))
	mj, _ := json.MarshalIndent(m, "", "  ")
	files[ManifestName] = append(mj, '\n')
	return &Bundle{Revision: rev, Files: files, Manifest: m}
}

// AcmeSnippet 是 ACME HTTP-01 挑战片段，被每个监听 80 端口的站点 server 块 include。
const AcmeSnippet = "snippets/acme-challenge.conf"

const acmeHeader = "# ACME HTTP-01 challenges (generated)"

var acmeLineRe = regexp.MustCompile(`^location = /\.well-known/acme-challenge/([A-Za-z0-9_-]{16,128}) \{ default_type text/plain; return 200 "([A-Za-z0-9_-]{16,128})\.[A-Za-z0-9_-]{32,64}"; \}$`)

// RenderAcmeSnippet 只渲染挑战片段（用于在线上版本基础上更新挑战，不重新渲染其他文件）。
func RenderAcmeSnippet(ch []AcmeChallenge) ([]byte, error) {
	in := Input{Revision: "r000000", AcmeChallenges: ch}
	for _, c := range ch {
		if !acmeTokenRe.MatchString(c.Token) || !strings.HasPrefix(c.KeyAuth, c.Token+".") || !acmeThumbRe.MatchString(c.KeyAuth[len(c.Token)+1:]) {
			return nil, fmt.Errorf("render: invalid ACME challenge")
		}
	}
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, "acme.conf.tmpl", in); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ValidAcmeSnippet 严格校验挑战片段：只允许注释头、空行和固定格式的挑战 location。
// agent 在授权只读时用它确认 ACME 版本没有夹带其他配置。
func ValidAcmeSnippet(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || line == acmeHeader {
			continue
		}
		m := acmeLineRe.FindStringSubmatch(line)
		if m == nil || m[1] != m[2] {
			return false
		}
	}
	return true
}

// isCertFile 判断是否为渲染器生成的站点证书 / 私钥文件。
func isCertFile(n string) bool {
	if !strings.HasPrefix(n, "certs/site-") || strings.Contains(n[len("certs/"):], "/") {
		return false
	}
	return strings.HasSuffix(n, ".crt") || strings.HasSuffix(n, ".key")
}

// CertOnlyChange 报告 next 相对 cur 是否只替换了已有的证书 / 私钥文件（文件集合不变，
// 其他文件逐字节相同，且至少一个证书文件有变化）。agent 在授权只读时据此放行证书续期，
// 不依赖控制台的任何标记。
func CertOnlyChange(cur, next map[string][]byte) bool {
	changed := false
	count := 0
	for n, nb := range next {
		if n == ManifestName {
			continue
		}
		count++
		cb, ok := cur[n]
		if !ok {
			return false
		}
		if bytes.Equal(cb, nb) {
			continue
		}
		if n == AcmeSnippet {
			if !ValidAcmeSnippet(nb) {
				return false
			}
		} else if !isCertFile(n) {
			return false
		}
		changed = true
	}
	curCount := 0
	for n := range cur {
		if n != ManifestName {
			curCount++
		}
	}
	return changed && count == curCount
}

// WriteDir 把版本写入目录 dir（不存在则创建）。私钥文件权限 0640，其余 0644。
func (b *Bundle) WriteDir(dir string) error {
	for name, data := range b.Files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".key") {
			mode = 0o640
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// TarGz 把版本打包为 tar.gz（文件按名称排序、时间戳固定），用于下发给 agent。
func (b *Bundle) TarGz() ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(b.Files))
	for n := range b.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mode := int64(0o644)
		if strings.HasSuffix(n, ".key") {
			mode = 0o640
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(b.Files[n])), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, bytes.NewReader(b.Files[n])); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadTarGz 解开 TarGz 生成的包，返回文件内容（用于预览 diff 与探活生成）。
func ReadTarGz(b []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, err
		}
		out[h.Name] = data
	}
}

//go:embed defaults/mime.types
var defaultMimeTypes []byte

// DefaultMimeTypes 返回随产品分发的 mime.types（来自数据面安装包的 share/defaults）。
func DefaultMimeTypes() []byte { return defaultMimeTypes }
