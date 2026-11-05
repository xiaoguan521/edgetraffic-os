// syapp-wrapper 由 root 拥有、经 sudoers 授权给 syapp-agent 调用（规格书 5.5）。
// 构建：CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" ./cmd/syapp-wrapper
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/xiaoguan521/edgetraffic-os/internal/wrapper"
)

func main() {
	cmd, err := wrapper.Build(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(wrapper.ExitUsage)
	}
	if cmd.Upgrade != "" {
		var err error
		paths := wrapper.DefaultPathsForInstance(cmd.Instance)
		switch cmd.Upgrade {
		case "start":
			err = wrapper.UpgradeStart(paths, cmd.Version)
		case "commit":
			err = wrapper.UpgradeCommit(paths)
		case "abort":
			err = wrapper.UpgradeAbort(paths)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	c := exec.Command(cmd.Path, cmd.Args...) // 切片执行，不经过 shell
	c.Env = cmd.Env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
