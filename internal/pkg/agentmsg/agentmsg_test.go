package agentmsg

import "testing"

// 这条文案是 agent 与控制台之间的约定：控制台据此把「节点本地没有该版本」的回滚退回为重新下发。
// 旧版 agent 已经在发送这个文本，不能改。
func TestRevisionNotPresentWireText(t *testing.T) {
	if RevisionNotPresent != "revision not present on node" {
		t.Fatalf("wire text changed: %q", RevisionNotPresent)
	}
	if !IsRevisionNotPresent("revision not present on node") {
		t.Fatal("exact text must match")
	}
	if IsRevisionNotPresent("rollback rejected") || IsRevisionNotPresent("") {
		t.Fatal("other messages must not match")
	}
}
