SyEdge 整合包（数据面 + 国密版 + Lua）
=======================================

一个包里有：标准版 syedge、国密版 syedge（铜锁，国密 TLCP）、Lua 附加包、syedge-agent、syedge-wrapper 与 systemd 单元。
安装时选择变体，三者版本、架构天然配套，不用再分别下载、核对。

系统要求与节点包相同：Linux（systemd），glibc 2.17 及以上，需要 sudo。

安装（root）：

  tar -xzf syedge-all-<版本>-linux-<架构>.tar.gz
  cd syedge-all-<版本>-linux-<架构>
  sudo ./install.sh                      标准版 + Lua 附加包（默认）
  sudo ./install.sh --gmssl              国密版 + Lua 附加包
  sudo ./install.sh --no-lua             不装 Lua 附加包
  sudo ./install.sh --lua-standalone     不接入控制台时，一步启用独立模式 Lua（写入配置并 reload）

其余参数（--prefix、--server / --token / --ca-fingerprint、--firewall-ssh-from、--root-master）与节点包相同。
默认安装根目录为 /opt/syedge，编译期前缀不同时以包内 install.sh 中的值为准；自定义位置用 --prefix（只允许首次安装）。

说明：
  - 附加包装好后，没有写 lua_package_path 的配置也能 require 本前缀下的库：<前缀>/modules/lua/lualib、
    <前缀>/modules/lua/extlib、<前缀>/lualib（运维自己的库放这里，附加包升级不会覆盖）。
  - 变体只在安装时选择。重复执行 install.sh 只更新文件，运行中的 master 不重启；
    新二进制在下次二进制热升级（USR2）时才生效，不会因为重装而中断连接。
  - 已装标准版的节点想换成国密版：用控制台的数据面热升级（国密升级包），不要靠重装。
  - 经控制台统一升级仍使用厂商签名的单独升级包（syedge-<版本>-<架构>.tar.gz 等），整合包不进入验签流程。
  - 数据面进程、命令、日志名都是 syedge。
