package brand

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// literalAllow 是允许出现默认品牌字面量的位置：本包（默认值所在处），以及不对外可见的内部标识。
// 新增例外必须说明理由（设计 0001 第 4.4 节）。
var literalAllow = map[string]string{
	"internal/brand/brand.go":   "默认品牌值的唯一来源",
	"internal/brand/default.go": "默认品牌（未带 brand 构建标签时编译）",
}

// literalAllowValue 是允许的字面量取值：数据库列名等内部标识，不出现在产品界面、进程、路径或日志中。
var literalAllowValue = map[string]string{
	"syapp_version": "数据库列名（内部标识，迁移已冻结）",
	"syapp_sha256":  "数据库列名（内部标识，迁移已冻结）",
}

// TestNoBrandLiteral：除 internal/brand 外，非测试 Go 代码的字符串字面量中不得出现默认品牌名。
// 品牌在构建时注入（-ldflags -X），任何写死的 syedge 都会让定制品牌的产物残留旧名字。
func TestNoBrandLiteral(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if strings.Contains(rel, "/testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || literalAllow[rel] != "" {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if fld, ok := n.(*ast.Field); ok && fld.Tag != nil {
					// 结构体标签（JSON / 数据库字段名）是内部接口标识，不是对外显示的名字；字段类型中不会有字符串字面量
					return false
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					v = lit.Value
				}
				if strings.Contains(strings.ToLower(v), Default) && literalAllowValue[v] == "" {
					hits = append(hits, fset.Position(lit.Pos()).String()+": "+lit.Value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("%d hard-coded brand literals (use internal/brand or internal/layout):\n%s", len(hits), strings.Join(hits, "\n"))
	}
}
