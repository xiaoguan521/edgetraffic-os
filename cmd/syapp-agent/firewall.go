package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/agent/firewall"
)

// firewallMain 实现离线子命令（不连接控制台，由 syapp-firewall 脚本调用）：
//
//	syapp-agent firewall render -ssh-from CIDRS [-ssh-port N] [-allow-tcp L] [-allow-udp L] [-allow-any-ssh]
//	syapp-agent firewall covers -ssh-from CIDRS -ip IP     退出码 0 表示 IP 在列表中，1 表示不在
func firewallMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法：syapp-agent firewall render|covers ...")
		return 2
	}
	fs := flag.NewFlagSet("firewall "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	sshFrom := fs.String("ssh-from", "", "允许 SSH 的来源，逗号分隔的 CIDR 或 IP")
	sshPort := fs.Int("ssh-port", 22, "SSH 端口")
	allowTCP := fs.String("allow-tcp", "", "额外对所有来源开放的 TCP 端口，逗号分隔")
	allowUDP := fs.String("allow-udp", "", "对所有来源开放的 UDP 端口，逗号分隔（如 HTTP/3 用 443）")
	anySSH := fs.Bool("allow-any-ssh", false, "允许 SSH 来源为 0.0.0.0/0（不建议）")
	ip := fs.String("ip", "", "要检查的地址（covers）")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var from []string
	for _, s := range strings.Split(*sshFrom, ",") {
		if s = strings.TrimSpace(s); s != "" {
			from = append(from, s)
		}
	}
	switch args[0] {
	case "render":
		tcp, err := firewall.ParsePorts(*allowTCP)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		udp, err := firewall.ParsePorts(*allowUDP)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		out, err := firewall.Render(firewall.Options{SSHFrom: from, SSHPort: *sshPort, AllowTCP: tcp, AllowUDP: udp, AllowAnySSH: *anySSH})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprint(stdout, out)
		return 0
	case "covers":
		ok, err := firewall.Covers(from, *ip)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if ok {
			return 0
		}
		return 1
	}
	fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
	return 2
}
