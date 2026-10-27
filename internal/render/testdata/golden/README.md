# 渲染器黄金用例

`r000001/` 是规格书 6.3 的基线配置，已在 2026-10-27 用上游 1.31.9 实测 `-t` 通过。

验证方法（P1 接入 CI）：

1. 把 `mime.types` 复制进版本目录（由数据面安装包提供）。
2. 准备自签证书 `certs/site-example.{crt,key}`（测试时生成，不入库）。
3. `syapp -t -c <dir>/r000001/syapp.conf`，以及经 `current` 软链：`syapp -T -c <conf>/current/syapp.conf`，确认所有 include 都经 `current/` 解析。

渲染器输出必须与此目录逐字节一致（除版本号注释）。
