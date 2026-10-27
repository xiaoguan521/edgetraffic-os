// syapp-license-server：厂商授权后台（规格书 8.8），部署在厂商内网，不交付给客户。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("license-server"))
		return
	}
	// P4：客户与订单、License 签发（KMS 签名）、离线激活申请解析、激活码、吊销列表。
	fmt.Fprintln(os.Stderr, "syapp-license-server: not implemented yet (planned for P4)")
	os.Exit(1)
}
