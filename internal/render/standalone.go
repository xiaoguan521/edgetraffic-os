package render

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// 独立模式（未接入控制台）随 syapp 软件包分发的文件。内容是固定文本，不含任何用户输入。

//go:embed standalone/example.conf
var standaloneExample []byte

//go:embed standalone/STANDALONE.txt
var standaloneGuide []byte

// BaselineRevision 是安装时写入的初始版本号（agent bootstrap 与 syapp 软件包使用同一版本）。
const BaselineRevision = "r000000"

// Baseline 渲染没有任何站点的初始版本。
func Baseline() (*Bundle, error) {
	return BaselineFor(Profile{})
}

// BaselineFor 返回指定实例档案的初始版本（具名实例与 user 模式的路径、端口按档案生成）。
func BaselineFor(p Profile) (*Bundle, error) {
	return Render(Input{Revision: BaselineRevision, MimeTypes: DefaultMimeTypes(), Profile: p})
}

// StandaloneFiles 返回独立模式的附带文件，键为相对 /opt/syapp 的路径。
func StandaloneFiles() map[string][]byte {
	return map[string][]byte{
		"share/defaults/standalone-example.conf": branded(standaloneExample),
		"share/doc/STANDALONE.txt":               branded(standaloneGuide),
	}
}

// WritePackageDefaults 在 root（打包时的 /opt/syapp 暂存目录）下写入：
// share/defaults/baseline/（初始版本，含 MANIFEST.json）与独立模式的示例和说明。
func WritePackageDefaults(root string) error {
	b, err := Baseline()
	if err != nil {
		return err
	}
	if err := b.WriteDir(filepath.Join(root, "share", "defaults", "baseline")); err != nil {
		return err
	}
	for name, data := range StandaloneFiles() {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// branded 把随包文档中的 @ID@ 占位符替换为构建时的品牌标识（命令、路径与服务名）。
// 文档以占位符嵌入，定制品牌的二进制中不残留默认品牌名。
func branded(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "@ID@", brand.ID()))
}
