// Package apply：本地发布状态机：加锁 → 下载 → 解包 → syapp -t → 切换 current → reload → 确认加载 → 探活 → 上报；失败回滚，禁止 restart。
//
// 状态：占位，计划在 P2 实现。参见规格书 5.4。
package apply
