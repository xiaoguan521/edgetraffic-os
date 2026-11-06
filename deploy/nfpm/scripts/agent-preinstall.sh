ensure_group syapp
ensure_user syapp-agent syapp
# 可选节点防火墙的专用用户（开机加载与自动刷新只带 CAP_NET_ADMIN，运行时不使用 root）
ensure_group syapp-firewall
ensure_user syapp-firewall syapp-firewall
