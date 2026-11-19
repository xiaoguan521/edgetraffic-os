package apply

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/agentmsg"
)

// 节点本地没有目标版本时，回滚必须以约定的文案拒绝：控制台据此退回为重新下发。
func TestRollbackToMissingRevisionUsesAgreedMessage(t *testing.T) {
	root := t.TempDir()
	a := &Applier{C: Config{ConfRoot: root, LockFile: filepath.Join(root, "lock")}}
	r := a.Rollback(context.Background(), "r000042", nil, "")
	if r.Status != Rejected || !agentmsg.IsRevisionNotPresent(r.Message) {
		t.Fatalf("rollback to a missing revision = %+v", r)
	}
}
