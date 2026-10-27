# ADR 0004：技术栈对齐 nginx-ui，但不复用其代码

- 状态：已采纳
- 日期：2026-10-27

## 背景
团队需要成熟、可单二进制交付的技术栈；nginx-ui 为 AGPL-3.0。

## 决策
Go + Gin + GORM/gen + gRPC，Vue 3 + Vite + antdv-next，前端内嵌二进制；只借鉴选型与交互，不复制代码；不引入其 cosy 框架（许可证未确认）。

## 后果
交付形态简单（一个程序加一个 PostgreSQL）；需要自行实现 nginx-ui 已有的部分功能。
