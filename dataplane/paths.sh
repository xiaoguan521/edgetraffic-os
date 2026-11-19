#!/usr/bin/env bash
# 数据面编译期的日志与缓存默认路径（被 build-syedge.sh 引用）。
#
# 必须与安装布局一致（internal/layout 的 LogDir / CacheDir、deploy/node/install.sh）：
#   前缀是历史默认的 /opt/<品牌>：日志与缓存在 /var/log、/var/cache 下（软件包遵循 FHS）；
#   其他前缀（例如发布构建的 /home/atwasoft/syedge）：日志在 <前缀>/logs，缓存在 <前缀>/cache。
# 这样手动用 tar 包启动时，不带 -c / -e 也会在安装目录里写日志，而不是去写 /var/log。
# internal/layout/compiled_paths_test.go 校验这里与 layout 的输出一致。

# dataplane_path_flags <前缀> <品牌 ID>：每行输出一个 configure 选项（--名=值）
dataplane_path_flags() {
  local prefix=$1 b=$2 logs cache
  if [[ "$prefix" == "/opt/$b" ]]; then
    logs=/var/log/$b; cache=/var/cache/$b
  else
    logs=$prefix/logs; cache=$prefix/cache
  fi
  printf '%s\n' \
    "--error-log-path=$logs/error.log" \
    "--http-log-path=$logs/access.log" \
    "--http-client-body-temp-path=$cache/client_temp" \
    "--http-proxy-temp-path=$cache/proxy_temp" \
    "--http-fastcgi-temp-path=$cache/fastcgi_temp" \
    "--http-uwsgi-temp-path=$cache/uwsgi_temp" \
    "--http-scgi-temp-path=$cache/scgi_temp"
}
