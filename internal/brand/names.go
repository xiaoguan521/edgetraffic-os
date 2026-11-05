package brand

import "strings"

// 由品牌派生的程序名与标识。所有组件名都由 ID 加固定后缀构成，便于运维按前缀识别同一产品。

// Agent 返回 agent 程序名，例如 syapp-agent。
func Agent() string { return id + "-agent" }

// Wrapper 返回提权入口程序名，例如 syapp-wrapper。
func Wrapper() string { return id + "-wrapper" }

// Console 返回控制台程序名，例如 syapp-console。
func Console() string { return id + "-console" }

// LicenseServer 返回授权后台程序名。
func LicenseServer() string { return id + "-license-server" }

// LicenseSigner 返回离线签名程序名。
func LicenseSigner() string { return id + "-license-signer" }

// Firewall 返回节点防火墙管理命令名。
func Firewall() string { return id + "-firewall" }

// Lua 返回 Lua 附加包名，例如 syapp-lua。
func Lua() string { return id + "-lua" }

// ConfName 返回数据面主配置文件名，例如 syapp.conf。
func ConfName() string { return id + ".conf" }

// PidName 返回数据面 pid 文件名，例如 syapp.pid。
func PidName() string { return id + ".pid" }

// Env 返回带品牌前缀的环境变量名，例如 Env("CONSOLE") = SYAPP_CONSOLE。
func Env(name string) string { return strings.ToUpper(id) + "_" + name }

// MasterTitle 与 WorkerTitle 是数据面进程标题（由改名 patch 编译进二进制）。
func MasterTitle() string { return Proc() + ": master process" }

// WorkerTitle 见 MasterTitle。
func WorkerTitle() string { return Proc() + ": worker process" }

// VersionPrefix 是数据面 -v / -V 输出中版本号前的名字（改名 patch 中版本宏的值），例如 syapp/1.30.6。
func VersionPrefix() string { return Proc() + "/" }
