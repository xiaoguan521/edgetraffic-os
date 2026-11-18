这个目录就是现成的 <安装根目录>/lualib/：整个目录的内容原样复制过去即可，不要改里面的文件。

    sudo cp -r lualib/. <安装根目录>/lualib/        # 例如 /opt/syedge/lualib/

来源与许可（原样取自 demo/skywalking-nginx-lua-0.5.0.zip 的 lib/，去掉测试文件）：
  skywalking/*                       Apache-2.0   skywalking-nginx-lua 0.5.0
  prometheus*.lua                    MIT          nginx-lua-prometheus（LICENSE-prometheus）
  resty/http.lua http_headers.lua    BSD-2-Clause lua-resty-http
  resty/jit-uuid.lua                 MIT          jit-uuid

scripts/ci/skywalking-conf-check.sh 会核对这里的文件与压缩包一致，防止被改动或遗漏。
