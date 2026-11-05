//go:build !brand

package brand

// 默认品牌（未带 brand 构建标签）：与当前产品一致。
func init() {
	if id == "" {
		id = Default
	}
	if product == "" {
		product = "EdgeTraffic OS"
	}
}
