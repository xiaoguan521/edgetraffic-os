EdgeTraffic OS 边缘节点
=======================

本包包含 syapp 数据面、syapp-agent、syapp-wrapper 与 systemd 单元。

系统要求：
  - Linux（systemd），glibc 2.17 及以上，需要 sudo
    每次构建都在以下发行版上做安装测试：CentOS 7、Rocky 8（glibc 2.28，与麒麟 V10、统信 UOS 20 相同）、
    Rocky 9、openEuler 22.03、Debian 12 / 13、Ubuntu 20.04 / 22.04 / 24.04
  - syapp 已静态链接 OpenSSL 3.5、PCRE2、zlib，不依赖系统的这些库

更推荐用软件包安装（同一 Release 中下载）：
  RPM：dnf install ./syapp-*.rpm ./syapp-agent-*.rpm
  DEB：apt install ./syapp_*.deb ./syapp-agent_*.deb

安装（root）：

  tar -xzf edgetraffic-os-node-<版本>-linux-<架构>.tar.gz
  cd edgetraffic-os-node-<版本>-linux-<架构>
  sudo ./install.sh

安装完成后 syapp 以基线配置运行（未知域名直接断开）。
在控制台“节点”页点“添加节点”，把生成的命令粘贴到本机执行即可接入，例如：

  sudo syapp-agent enroll -server <控制台>:9443 -token <令牌> -ca-fingerprint <指纹> \
    && sudo systemctl enable --now syapp-agent

也可以在安装时一步完成：

  sudo ./install.sh --server <控制台>:9443 --token <令牌> --ca-fingerprint <指纹>

独立模式（不接入控制台）：

  只安装 syapp 软件包（rpm -i / dnf install / apt install syapp）即可独立使用，安装时写入初始配置：
    systemctl enable --now syapp
    cp /opt/syapp/share/defaults/standalone-example.conf /opt/syapp/conf/current/conf.d/www.conf
    syapp -t && systemctl reload syapp
  详见 /opt/syapp/share/doc/STANDALONE.txt。之后要接入控制台时，若本机已有自定义配置，注册会被拒绝以免被
  静默替换；确认由控制台接管时加 --replace-local-config（install.sh）或 -replace-local-config（syapp-agent enroll），
  当前配置会先备份到 /var/lib/syapp-agent/backups/。

节点防火墙（可选，默认不启用，需要 nftables）：

  sudo syapp-firewall enable --ssh-from <管理网段或 IP，逗号分隔> [--allow-tcp 9100] [--allow-udp 443]
  sudo syapp-firewall status
  sudo syapp-firewall disable

  启用后 80/443 对所有来源开放，SSH 只允许 --ssh-from，其余入站丢弃；出站不限制（agent 主动连接控制台）。
  规则在独立的 nftables 表 inet syapp_fw 中，不修改已有规则；当前 SSH 会话来源不在列表中时拒绝启用。
  安装时可一并启用：sudo ./install.sh --firewall-ssh-from 10.0.0.0/8
  已在使用 firewalld / ufw 时，它们的规则同样生效，需在其中放行 80/443。

常用命令：
  syapp -v                          查看版本
  systemctl status syapp            数据面状态
  journalctl -u syapp-agent -f      agent 日志

Based on NGINX Open Source (BSD-2-Clause)，详见 NOTICE。
