// syedge-wrapper 由 root 拥有、经 sudoers 授权给 syedge-agent 以数据面用户身份调用（规格书 5.5；运行时不使用 root，设计 0001 D5）。
// 构建：CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" ./cmd/syedge-wrapper
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
	"github.com/xiaoguan521/edgetraffic-os/internal/wrapper"
)

func main() {
	// 模式由 wrapper 自身的位置决定（system：固定路径，经 sudo 调用；user：<根目录>/bin 下，直接调用），从不来自参数
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(wrapper.ExitUsage)
	}
	root, err := wrapper.RootFromExecutable(exe)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(wrapper.ExitUsage)
	}
	if root != "" && os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "user-mode wrapper must not run as root")
		os.Exit(wrapper.ExitUsage)
	}
	// system 模式：运行时不使用 root（设计 0001 D5）。sudoers 以数据面用户运行 wrapper，与 master 同一身份。
	// 例外是兼容模式（内核或 systemd 不支持 ambient 能力，如 CentOS 7）：master 与传统 Web 服务器一样以 root 运行，
	// wrapper 须以 root 才能向它发信号；是否兼容模式只由 root 所有的标记文件决定（layout.RootMasterAt）
	if root == "" && os.Geteuid() == 0 && !layout.RootMasterAt("/") {
		fmt.Fprintln(os.Stderr, "wrapper must not run as root: sudoers runs it as the data-plane user (sudo -u "+layout.WorkerUser()+")")
		os.Exit(wrapper.ExitUsage)
	}
	if root == "" && os.Geteuid() != 0 {
		// `-t` 绑定配置中的端口（80/443）：把 wrapper 文件带的 CAP_NET_BIND_SERVICE 交给子进程（只此一项）
		if err := wrapper.RaiseBindCap(); err != nil {
			fmt.Fprintln(os.Stderr, "raise CAP_NET_BIND_SERVICE:", err)
			os.Exit(1)
		}
	}
	cmd, err := wrapper.BuildAt(root, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(wrapper.ExitUsage)
	}
	// system 模式：具名实例必须已由 root 创建；user 模式：实例档案属于当前用户、为本根目录。否则不执行任何操作
	if root == "" {
		err = wrapper.CheckInstance(cmd.Instance)
	} else {
		err = wrapper.CheckInstanceUser(root, cmd.Instance, os.Getuid())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(wrapper.ExitUsage)
	}
	if cmd.Upgrade != "" {
		paths, err := wrapper.DefaultPathsAt(root, cmd.Instance)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(wrapper.ExitUsage)
		}
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
