EdgeTraffic OS 边缘节点
=======================

本包包含 syapp 数据面、syapp-agent、syapp-wrapper 与 systemd 单元。

系统要求：
  - Linux（systemd），glibc 2.17 及以上，需要 sudo
    每次构建都在以下发行版上做安装测试：CentOS 7、Rocky 8（glibc 2.28，与麒麟 V10、统信 UOS 20 相同）、
    Rocky 9、openEuler 22.03、Debian 11 / 12、Ubuntu 20.04 / 22.04 / 24.04
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

常用命令：
  syapp -v                          查看版本
  systemctl status syapp            数据面状态
  journalctl -u syapp-agent -f      agent 日志

Based on NGINX Open Source (BSD-2-Clause)，详见 NOTICE。
