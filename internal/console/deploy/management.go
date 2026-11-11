package deploy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/query"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
)

// Node-group management modes control whether the console is allowed to send a
// configuration bundle to a node.  The default is managed so installations
// created before these modes existed keep their original behavior.
const (
	ManagementModeManaged = "managed"
	ManagementModeObserve = "observe"
	ManagementModeLegacy  = "legacy"
)

var (
	ErrInvalidManagementMode = errors.New("invalid node group management mode")
	ErrHandoffPending        = errors.New("node group handoff requires an explicit publish")
	// ErrConfigManagementPaused is returned before a configuration deployment is
	// created.  This is the safety boundary for nodes that still serve a local
	// configuration, including legacy Lua configurations.
	ErrConfigManagementPaused = errors.New("configuration management is paused for this node group")
)

const managementModeKeyPrefix = "node_group_management_mode:"
const handoffPendingKeyPrefix = "node_group_handoff_pending:"

// ManagementModeView is the stable API representation used by the console UI.
type ManagementModeView struct {
	Mode           string `json:"mode"`
	Label          string `json:"label"`
	Description    string `json:"description"`
	HandoffPending bool   `json:"handoff_pending"`
}

func managementModeKey(groupID uint) string {
	return managementModeKeyPrefix + strconv.FormatUint(uint64(groupID), 10)
}

func handoffPendingKey(groupID uint) string {
	return handoffPendingKeyPrefix + strconv.FormatUint(uint64(groupID), 10)
}

// A persistent marker prevents reconnects from adopting an old console
// revision after the operator has changed the group back to managed.
func (d *Service) handoffPending(groupID uint) (bool, error) {
	_, err := d.Store.GetKV(handoffPendingKey(groupID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func validManagementMode(mode string) bool {
	switch mode {
	case ManagementModeManaged, ManagementModeObserve, ManagementModeLegacy:
		return true
	default:
		return false
	}
}

func describeManagementMode(mode string) ManagementModeView {
	switch mode {
	case ManagementModeObserve:
		return ManagementModeView{Mode: mode, Label: "观察接入", Description: "保留节点本地配置；控制台只分析、审计和查看漂移，不会自动覆盖配置。"}
	case ManagementModeLegacy:
		return ManagementModeView{Mode: mode, Label: "兼容配置", Description: "原生配置包由管理员提交，经另一位管理员审批后下发；控制台不会自动发布渲染配置。"}
	default:
		return ManagementModeView{Mode: ManagementModeManaged, Label: "控制台托管", Description: "节点上线自动对齐，配置通过控制台预览、审批和发布。"}
	}
}

// ManagementMode returns a node group's mode. Missing mode records intentionally
// resolve to managed for backwards compatibility.
func (d *Service) ManagementMode(groupID uint) (string, error) {
	if _, err := store.First[store.NodeGroup](d.Store, groupID); err != nil {
		return "", err
	}
	v, err := d.Store.GetKV(managementModeKey(groupID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ManagementModeManaged, nil
		}
		return "", err
	}
	if strings.TrimSpace(v) == "" {
		return ManagementModeManaged, nil
	}
	if !validManagementMode(v) {
		// A corrupt value must fail closed instead of silently re-enabling
		// automatic deployment.
		return "", fmt.Errorf("%w: stored value %q", ErrInvalidManagementMode, v)
	}
	return v, nil
}

// ManagementModeView returns the mode plus a human-readable explanation.
func (d *Service) ManagementModeView(groupID uint) (ManagementModeView, error) {
	mode, err := d.ManagementMode(groupID)
	if err != nil {
		return ManagementModeView{}, err
	}
	pending, err := d.handoffPending(groupID)
	if err != nil {
		return ManagementModeView{}, err
	}
	view := describeManagementMode(mode)
	view.HandoffPending = pending
	return view, nil
}

// SetManagementMode changes the safety boundary for a node group. Switching to
// managed does not publish or align anything; the operator must explicitly
// preview and publish after checking the imported configuration.
func (d *Service) SetManagementMode(groupID uint, mode, user, ip string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !validManagementMode(mode) {
		return fmt.Errorf("%w: %q", ErrInvalidManagementMode, mode)
	}
	lk := d.groupLock(groupID)
	lk.Lock()
	defer lk.Unlock()
	if _, err := store.First[store.NodeGroup](d.Store, groupID); err != nil {
		return err
	}
	old, err := d.ManagementMode(groupID)
	if err != nil {
		return err
	}
	if old != mode && d.canaryRunning(groupID) {
		return ErrCanaryRunning
	}
	if old != mode {
		cr := d.q().CanaryRelease
		count, err := cr.WithContext(bg).Where(cr.GroupID.Eq(groupID), cr.Status.Eq("running")).Count()
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrCanaryRunning
		}
	}
	if err := d.q().Transaction(func(tx *query.Query) error {
		kv := tx.SystemKV.WithContext(bg)
		if old != mode && (old != ManagementModeManaged || mode != ManagementModeManaged) {
			if err := kv.Save(&store.SystemKV{Key: handoffPendingKey(groupID), Value: "pending"}); err != nil {
				return err
			}
		}
		return kv.Save(&store.SystemKV{Key: managementModeKey(groupID), Value: mode})
	}); err != nil {
		return err
	}
	return d.Audit.Write(audit.Entry{Username: user, IP: ip, Action: "group.management_mode", ObjectType: "node_group", ObjectID: fmt.Sprint(groupID),
		Detail: map[string]string{"old": old, "new": mode}})
}

func configManagementError(mode string) error {
	return fmt.Errorf("%w: 节点组当前为 %s 模式；如需下发配置，请先切换为控制台托管并人工确认导入结果", ErrConfigManagementPaused, describeManagementMode(mode).Label)
}

func (d *Service) ensureConfigManaged(groupID uint) error {
	mode, err := d.ManagementMode(groupID)
	if err != nil {
		return err
	}
	if mode != ManagementModeManaged {
		return configManagementError(mode)
	}
	return nil
}

func (d *Service) ensureHandoffComplete(groupID uint) error {
	pending, err := d.handoffPending(groupID)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: 请先确认迁移结果并显式发布", ErrHandoffPending)
	}
	return nil
}
