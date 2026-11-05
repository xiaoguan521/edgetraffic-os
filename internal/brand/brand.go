// Package brand 是产品品牌名的唯一来源（设计 0001 第 4 节）。
//
// 品牌在构建时注入，例如：
//
//	-ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/brand.id=syedge -X ...brand.proc=SyEdge"
//
// 默认值与当前产品一致。其他代码不得写死品牌名，一律通过本包取得（TestNoBrandLiteral）。
package brand

// Default 是默认品牌标识。
const Default = "syapp"

// 由 -ldflags -X 覆盖；必须是 var 且为字符串。
var (
	id      = Default
	proc    = ""
	product = "EdgeTraffic OS"
)

// ID 返回品牌标识：命令名、二进制名、路径、用户名、服务单元名、安装包名。
func ID() string { return id }

// Proc 返回进程标题前缀与对外显示名（ps、错误日志前缀、Server 头），未设置时等于 ID。
func Proc() string {
	if proc == "" {
		return id
	}
	return proc
}

// Product 返回界面中显示的产品名。
func Product() string { return product }
