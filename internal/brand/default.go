//go:build !brand

package brand

// 默认品牌（未带 brand 构建标签）：与当前产品一致。
func init() {
	if id == "" {
		id = Default
	}
	if proc == "" {
		proc = DefaultProc
	}
	if product == "" {
		product = "SyEdge"
	}
	if fullName == "" {
		fullName = "神玥企业级动态网关与负载均衡引擎"
	}
}
