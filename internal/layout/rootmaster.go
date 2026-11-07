package layout

import (
	"os"
	"path/filepath"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// 兼容模式（root master，与传统 Web 服务器相同）：内核或 systemd 不支持 ambient 能力的系统（CentOS 7：内核 3.10、systemd 219）
// 无法让非 root 的 master 绑定 80/443。这类系统上 master 以 root 运行、绑定端口后按编译进二进制的默认用户
// （--user=<品牌>）启动 worker；wrapper 经 sudo 以 root 运行。其余系统仍按设计 0001 D5 运行时不使用 root。
//
// 模式由 <品牌>-instance nonroot 决定并写入标记文件，agent、wrapper、实例管理都只读这一个标记。

// RootMasterMarker 返回兼容模式标记文件的路径（system 模式）。
func RootMasterMarker() string { return "/etc/" + brand.ID() + "/root-master" }

// RootMasterAt 报告文件系统根 fsroot 下是否处于兼容模式：标记必须是 root 所有、组与他人不可写的普通文件
// （否则任何能写该目录的用户都能让 wrapper 以 root 运行）。
func RootMasterAt(fsroot string) bool {
	fi, err := os.Lstat(filepath.Join(fsroot, filepath.FromSlash(RootMasterMarker())))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	return ownedByRoot(fi)
}
