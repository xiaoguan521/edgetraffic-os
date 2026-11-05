//go:build brand

package brand

// 定制品牌构建（-tags brand）必须用 -ldflags -X 注入 id；否则程序在启动时就失败，避免出现没有名字的产物。
func init() {
	if id == "" {
		panic("brand build (-tags brand) requires -ldflags -X github.com/xiaoguan521/edgetraffic-os/internal/brand.id=<name>")
	}
}
