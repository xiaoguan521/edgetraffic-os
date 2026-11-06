SyEdge 边缘节点（user 模式，不需要 root）
================================================

本包包含 syedge 数据面、syedge-agent、syedge-wrapper、syedge-instance，全部安装在一个由普通用户拥有的目录下，
不写 /etc、/usr、/opt，不需要 sudo。适用于没有 root 权限的主机（详见 docs/install-maintenance.md 的 user 模式一节）。

限制：
  - 只能监听 1024 及以上的端口（80/443 请在前面用负载均衡或防火墙端口转发）
  - 不做 syedge 进程降权（worker 与 master 同一用户）；不配置本机防火墙
  - 打开文件数受该用户 ulimit -n 限制

安装：

  tar -xzf syedge-node-user-<版本>-linux-<架构>.tar.gz
  cd syedge-node-user-<版本>-linux-<架构>
  ./install-user.sh $HOME/syedge --http 8080 --https 8443 --status 8081 \
      [--console <控制台>:9443 --token <令牌> --ca-fingerprint <指纹>]

进程守护：有 systemd 用户实例（loginctl enable-linger 已开启）时安装 ~/.config/systemd/user 下的单元；
否则安装程序会打印一行 crontab，每分钟执行 syedge-instance --root <目录> ensure 拉起未运行的进程。

常用命令（均需带 --root <目录>）：

  <目录>/bin/syedge-instance --root <目录> list
  <目录>/bin/syedge-instance --root <目录> add blue --http 18080 --https 18443 --status 18082
  <目录>/bin/syedge-instance --root <目录> remove blue --yes

Based on NGINX Open Source (BSD-2-Clause)，见 NOTICE。
