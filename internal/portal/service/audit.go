package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store/query"
)

// auditLog 是授权后台的只增审计日志，每条记录包含前一条的哈希（与控制台相同的哈希链格式）。
type auditLog struct {
	q  *query.Query
	mu sync.Mutex
}

func auditHash(r *store.AuditLog) string {
	s := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s", r.TS.UTC().Format(time.RFC3339Nano), r.Username, r.IP,
		r.Action, r.ObjectType, r.ObjectID, r.Detail, r.Result, r.PrevHash)
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (a *auditLog) write(ctx context.Context, tx *query.Query, act Actor, action, objType, objID string, detail any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := ""
	if detail != nil {
		b, _ := json.Marshal(detail)
		d = string(b)
	}
	t := tx.AuditLog
	prev := ""
	last, err := t.WithContext(ctx).Order(t.ID.Desc()).Limit(1).Find()
	if err != nil {
		return err
	}
	if len(last) == 1 {
		prev = last[0].Hash
	}
	r := &store.AuditLog{TS: time.Now().UTC().Truncate(time.Microsecond), Username: act.User, IP: act.IP, Action: action,
		ObjectType: objType, ObjectID: objID, Detail: d, Result: "success", PrevHash: prev}
	r.Hash = auditHash(r)
	return t.WithContext(ctx).Create(r)
}

// VerifyAudit 校验整条哈希链，返回第一条被篡改记录的 ID（0 表示完好）。
func (s *Service) VerifyAudit(ctx context.Context) (uint, error) {
	t := s.q().AuditLog
	rows, err := t.WithContext(ctx).Order(t.ID).Find()
	if err != nil {
		return 0, err
	}
	prev := ""
	for _, r := range rows {
		if r.PrevHash != prev || auditHash(r) != r.Hash {
			return r.ID, nil
		}
		prev = r.Hash
	}
	return 0, nil
}
