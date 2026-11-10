#!/usr/bin/env bash
# 与 internal/layout.ValidateSystemRoot 保持一致；构建前拒绝可能覆盖系统的目录。
validate_system_prefix() {
  local root=$1 legacy=$2 part dir
  local -a parts
  [[ "$root" =~ ^(/[A-Za-z0-9._@+-]+)+$ ]] || return 1
  IFS=/ read -r -a parts <<< "$root"
  for part in "${parts[@]}"; do [[ "$part" != . && "$part" != .. ]] || return 1; done
  case "$root" in /usr|/usr/local|/opt|/var|/var/lib|/var/cache|/home) return 1 ;; esac
  for dir in /bin /sbin /lib /lib64 /boot /dev /etc /proc /run /sys /root /var/log \
    /usr/bin /usr/sbin /usr/lib /usr/lib64 /usr/libexec /usr/share \
    /usr/local/bin /usr/local/sbin /usr/local/lib /usr/local/lib64 /usr/local/libexec /usr/local/share; do
    [[ "$root" != "$dir" && "$root" != "$dir/"* ]] || return 1
  done
  [[ "$root" == "$legacy" || "$root" != "$legacy/"* ]]
}
