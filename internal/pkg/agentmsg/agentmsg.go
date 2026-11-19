// Package agentmsg 放控制台与 agent 之间靠文本约定的结果信息，两侧引用同一个常量，
// 避免改一侧文案而另一侧的判断悄悄失效。这些文本已随旧版 agent 发出，值不能改。
package agentmsg

// RevisionNotPresent：回滚 / 恢复时节点本地没有目标版本。控制台据此把回滚退回为重新下发。
const RevisionNotPresent = "revision not present on node"

// IsRevisionNotPresent 判断 agent 回报的信息是否表示节点本地没有目标版本。
func IsRevisionNotPresent(msg string) bool { return msg == RevisionNotPresent }
