set -e
# 旧版 RPM 的 /opt 文件在新包 %post 之后才被 RPM 删除；事务完成后补齐缺失文件。
r=$(syedge_root) || exit 1
restore_package_payload "$r"
