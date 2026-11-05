# 前端权限矩阵（P5）

按当前四种角色（viewer / releaser / operator / admin）断言页面元素与可操作按钮；未授权功能置灰；发布进度实时刷新，并直接请求 API 检查服务端越权拒绝。

现有脚本为 [`web/e2e/ui-acceptance.cjs`](../../web/e2e/ui-acceptance.cjs)，运行前提和 CI 调用见 [demo-e2e 工作流](../../.github/workflows/demo-e2e.yml)。现有矩阵只覆盖部分页面与操作，不代表全功能浏览器验收。

新增页面与完整回归范围见 [完整功能测试清单](../../docs/full-test-checklist.md)。

## 配置导入全链路

新增 [`import-acceptance.cjs`](../../web/e2e/import-acceptance.cjs)，支持真实文件上传、报告下载、应用草稿、权限/授权边界、片段审批、双节点发布后的业务请求，并生成 JUnit。一键创建临时环境及详细参数见 [自动化验证说明](../../docs/test-automation.md)。

已有测试环境可运行 `make test-import-ui`；设置 `IMPORT_E2E_LIVE=1` 强制验证真实双节点。缺少节点会失败，而不是将零节点发布视为业务成功。
