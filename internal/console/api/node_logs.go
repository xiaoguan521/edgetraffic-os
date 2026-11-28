package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	agentv1 "github.com/xiaoguan521/edgetraffic-os/api/gen/agent/v1"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

// streamNodeLogs 通过 SSE 向前端实时推送边缘节点的 access.json 或 error.log（支持初始末尾回放与增量跟踪）。
// 只读排障能力：授权过期/只读状态下依然可用；仅限 operator 及以上角色并记录审计日志。
func (s *Server) streamNodeLogs(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	fileParam := strings.ToLower(strings.TrimSpace(c.DefaultQuery("file", "access")))
	var protoFile agentv1.TailLog_File
	switch fileParam {
	case "access", "access.json":
		fileParam = "access"
		protoFile = agentv1.TailLog_ACCESS
	case "error", "error.log":
		fileParam = "error"
		protoFile = agentv1.TailLog_ERROR
	default:
		fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "file 参数仅支持 access 或 error")
		return
	}
	seconds := 120
	if raw := strings.TrimSpace(c.Query("seconds")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 5 || n > 300 {
			fail(c, http.StatusBadRequest, "INVALID_ARGUMENT", "seconds 须在 5–300 之间")
			return
		}
		seconds = n
	}

	n, err := store.First[store.Node](s.Store, id)
	if err != nil {
		failErr(c, err)
		return
	}
	if n.Status == "revoked" {
		fail(c, http.StatusBadRequest, "NODE_REVOKED", "已吊销的节点无法查看日志")
		return
	}
	if s.GW == nil || !s.GW.Online(n.ID) {
		fail(c, http.StatusConflict, "NODE_OFFLINE", "节点当前离线，无法拉取实时日志")
		return
	}

	ch, stop, err := s.GW.StartLogTail(c.Request.Context(), n.ID, protoFile, seconds)
	if err != nil {
		if errors.Is(err, gateway.ErrOffline) {
			fail(c, http.StatusConflict, "NODE_OFFLINE", "节点当前离线，无法拉取实时日志")
			return
		}
		fail(c, http.StatusBadGateway, "LOG_TAIL_FAILED", err.Error())
		return
	}
	defer stop()

	s.audit(c, "node.tail_log", "node", fmt.Sprint(n.ID), map[string]any{
		"node":    n.Name,
		"file":    fileParam,
		"seconds": seconds,
	})

	// SSE 长连接需清除单次请求的写超时，避免被服务器 WriteTimeout 提前切断
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetWriteDeadline(time.Time{})

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	writeEvent := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}

	if !writeEvent("ready", map[string]any{
		"node_id": n.ID,
		"node":    n.Name,
		"file":    fileParam,
		"seconds": seconds,
	}) {
		return
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(c.Writer, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case chunk, open := <-ch:
			if !open {
				writeEvent("end", map[string]string{"reason": "stream_ended"})
				return
			}
			if chunk == nil || len(chunk.Lines) == 0 {
				continue
			}
			if !writeEvent("chunk", map[string]any{
				"file":  chunk.File,
				"lines": chunk.Lines,
			}) {
				return
			}
		}
	}
}
