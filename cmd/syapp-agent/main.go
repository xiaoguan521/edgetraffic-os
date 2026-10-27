// syapp-agent：部署在每台边缘节点的探针（规格书第 5 节）。
// 只主动连接控制台，不监听任何端口。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	server := flag.String("server", "", "控制台 gRPC 地址，例如 console.internal:9443")
	token := flag.String("enroll-token", "", "一次性注册令牌（仅首次注册时需要）")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("agent"))
		return
	}
	_ = token
	if *server == "" {
		fmt.Fprintln(os.Stderr, "missing -server")
		os.Exit(2)
	}
	// P2：注册（Enroll）→ 建立 Connect 双向流 → 执行 internal/agent/apply 状态机。
	fmt.Fprintln(os.Stderr, "syapp-agent: not implemented yet (planned for P2)")
	os.Exit(1)
}
