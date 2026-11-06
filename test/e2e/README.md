# 端到端测试（P2 起）

用 Testcontainers 启动：真实 syedge 容器 + syedge-agent + syedge-console + PostgreSQL。
必须覆盖（规格书 12.1）：并发 20 次发布无串扰、坏配置 100% 回滚、端口冲突类 reload 失败被检出并回滚、
控制台断开时业务请求正常、wrapper 注入用例全部拒绝、任何授权状态下 syedge 不被停止。
