# 数据面 syapp

syapp = 官方 stable 源码（`UPSTREAM`）+ 唯一的改名 patch（`patches/`）。

## 构建

```bash
dataplane/build-syapp.sh -o dist          # 从官方 GitHub 镜像拉取 release-1.30.6 并校验提交哈希
dataplane/build-syapp.sh -s ./nginx-src   # 或使用本地源码目录
```

依赖：gcc、make、libpcre2-dev、libssl-dev、zlib1g-dev。产物 `dist/syapp-<版本>-<架构>.tar.gz` 解压到 `/` 得到：

```
/opt/syapp/sbin/syapp            # 已剥离符号
/opt/syapp/conf/releases/        # 版本目录，由 syapp-agent 写入；conf/current 是指向某个版本的软链
/opt/syapp/html/                 # EdgeTraffic OS 默认页
/opt/syapp/share/defaults/       # mime.types 等默认文件，渲染器复制进每个版本
/opt/syapp/share/doc/NOTICE      # BSD 归因
```

构建脚本会自检：版本输出、二进制与文件名中无上游名、`rename-scan` 通过。

## 验收

```bash
scripts/golden-rename.sh /opt/syapp/sbin/syapp www.example.com
```

2026-10-27 在 1.30.6 上的结果：14 项检查全部通过（无 systemd 环境跳过 systemd 一项）；
reload 后 master PID 不变、worker 全部更换；reopen 生成新日志文件。

## 升级上游

1. 修改 `UPSTREAM` 的版本、tag、commit
2. 运行构建；patch 冲突或 `rename-scan` 失败则人工处理（只允许改字符串）
3. 跑 `golden-rename.sh` 与渲染器黄金用例
