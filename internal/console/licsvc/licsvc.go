// Package licsvc 是控制台侧的授权服务：安装 ID、主机指纹、时钟回拨检测、License 导入与状态（规格书 8.4–8.6）。
package licsvc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

// Service 维护当前授权快照。
type Service struct {
	Store   *store.Store
	DataDir string
	Keys    license.KeyRing

	mu             sync.Mutex
	installationID string
	idMismatch     bool
}

// Init 读取或生成 installation_id（数据库与本地文件各存一份，不一致视为复制部署）。
func (s *Service) Init() error {
	path := filepath.Join(s.DataDir, "installation.json")
	dbID, dbErr := s.Store.GetKV("installation_id")
	var file struct {
		ID string `json:"installation_id"`
	}
	b, fileErr := os.ReadFile(path)
	if fileErr == nil {
		_ = json.Unmarshal(b, &file)
	}
	switch {
	case dbErr != nil && fileErr != nil: // 全新安装
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		id := "inst_" + hex.EncodeToString(raw)
		if err := s.Store.SetKV("installation_id", id); err != nil {
			return err
		}
		out, _ := json.Marshal(map[string]string{"installation_id": id})
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return err
		}
		dbID = id
	case dbErr == nil && fileErr != nil:
		out, _ := json.Marshal(map[string]string{"installation_id": dbID})
		_ = os.WriteFile(path, out, 0o600)
	case dbErr != nil && fileErr == nil:
		dbID = file.ID
		_ = s.Store.SetKV("installation_id", dbID)
	default:
		if dbID != file.ID {
			s.idMismatch = true
		}
	}
	s.installationID = dbID
	return nil
}

// InstallationID 返回本安装的 ID。
func (s *Service) InstallationID() string { return s.installationID }

// FingerprintHashes 返回 5 项主机特征各自的加盐哈希（规格书 8.4）。
func FingerprintHashes() []string {
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
	mac := ""
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) > 0 {
				mac = i.HardwareAddr.String()
				break
			}
		}
	}
	cpu := ""
	for _, line := range strings.Split(read("/proc/cpuinfo"), "\n") {
		if strings.HasPrefix(line, "model name") {
			cpu = line
			break
		}
	}
	facts := []string{read("/etc/machine-id"), read("/sys/class/dmi/id/product_uuid"), rootFS(), mac, cpu}
	out := make([]string, len(facts))
	for i, f := range facts {
		h := sha256.Sum256([]byte(fmt.Sprintf("etos-fp-%d:%s", i, f)))
		out[i] = hex.EncodeToString(h[:8])
	}
	return out
}

func rootFS() string {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && f[4] == "/" {
			return f[2] // 设备号 major:minor
		}
	}
	return ""
}

func fingerprintMatch(want []string) bool {
	if len(want) == 0 {
		return false
	}
	local := map[string]bool{}
	for _, h := range FingerprintHashes() {
		local[h] = true
	}
	n := 0
	for _, h := range want {
		if local[h] {
			n++
		}
	}
	return n >= 3
}

func (s *Service) raw() string {
	var l store.LicenseStore
	if err := s.Store.DB.Order("id desc").Limit(1).Find(&l).Error; err != nil || l.ID == 0 {
		return ""
	}
	return l.Raw
}

// Raw 返回当前 License 原文（推送给 agent 用）。
func (s *Service) Raw() string { return s.raw() }

func (s *Service) env(now time.Time) license.Env {
	maxSeen := time.Time{}
	if v, err := s.Store.GetKV("max_seen_time"); err == nil {
		maxSeen, _ = time.Parse(time.RFC3339, v)
	}
	bd, _ := time.Parse("2006-01-02", version.BuildDate)
	if bd.Year() < 2000 {
		bd = time.Time{} // 开发构建不做维保校验
	}
	return license.Env{Now: now, InstallationID: s.installationID, MaxSeenTime: maxSeen, BuildDate: bd}
}

func (s *Service) evaluate(raw string, now time.Time) license.Snapshot {
	env := s.env(now)
	if p, err := license.Verify(raw, s.Keys); err == nil {
		env.FingerprintMatch = fingerprintMatch(p.Binding.FPHashes)
	}
	snap := license.Evaluate(raw, s.Keys, env)
	if s.idMismatch && snap.State != license.StateUnlicensed {
		snap.State, snap.Reason = license.StateInvalid, "installation id mismatch between database and data dir"
	}
	return snap
}

// Snapshot 每次都重新验签计算当前状态（规格书 8.6）。
func (s *Service) Snapshot() license.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evaluate(s.raw(), time.Now())
}

// Tick 记录已见最大时间，供时钟回拨检测；由定时任务每小时调用。
func (s *Service) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, err := s.Store.GetKV("max_seen_time"); err == nil {
		if t, err := time.Parse(time.RFC3339, v); err == nil && !now.After(t) {
			return
		}
	}
	_ = s.Store.SetKV("max_seen_time", now.UTC().Format(time.RFC3339))
}

// Import 校验并保存新的 License；INVALID 的 License 不会被保存。
func (s *Service) Import(raw, user string) (license.Snapshot, error) {
	raw = strings.TrimSpace(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.evaluate(raw, time.Now())
	if snap.State == license.StateInvalid || snap.State == license.StateUnlicensed {
		return snap, errors.New("license rejected: " + snap.Reason)
	}
	if err := s.Store.DB.Create(&store.LicenseStore{Raw: raw, ImportedBy: user, ImportedAt: time.Now()}).Error; err != nil {
		return snap, err
	}
	return snap, nil
}

// ActivationRequest 生成离线激活申请（规格书 8.5）。
type ActivationRequest struct {
	InstallationID string    `json:"installation_id"`
	FPHashes       []string  `json:"fp_hashes"`
	Product        string    `json:"product"`
	Version        string    `json:"version"`
	Nodes          int64     `json:"nodes"`
	CreatedAt      time.Time `json:"created_at"`
}

func (s *Service) ActivationRequest() ActivationRequest {
	var n int64
	s.Store.DB.Model(&store.Node{}).Where("status <> ?", "revoked").Count(&n)
	return ActivationRequest{InstallationID: s.installationID, FPHashes: FingerprintHashes(), Product: version.Product,
		Version: version.Version, Nodes: n, CreatedAt: time.Now().UTC()}
}
