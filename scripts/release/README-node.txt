EdgeTraffic OS 边缘节点
=======================

本包包含 syapp 数据面、syapp-agent、syapp-wrapper 与 systemd 单元。

系统要求：
  - Linux（systemd），glibc 2.35 及以上，OpenSSL 3
    例如 Ubuntu 22.04 / 24.04、Debian 12 / 13
  - 运行库：libpcre2-8、libssl3、zlib、sudo
      Ubuntu / Debian：apt install libpcre2-8-0 libssl3 zlib1g sudo

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
