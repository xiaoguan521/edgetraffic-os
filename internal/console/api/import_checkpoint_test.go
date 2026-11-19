package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

// failResultWrite 让 config_imports 中包含 marker 的结果写入失败 times 次（<=0 表示一直失败）。
func failResultWrite(t *testing.T, f *importHTTP, marker string, times int32) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	db := f.s.Store.DB
	err := db.Callback().Update().Before("gorm:update").Register("test:fail_result_write", func(tx *gorm.DB) {
		if tx.Statement.Table != "config_imports" || !strings.Contains(fmt.Sprint(tx.Statement.Dest), marker) {
			return
		}
		if n := hits.Add(1); times <= 0 || n <= times {
			_ = tx.AddError(errors.New("injected result write failure"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove("test:fail_result_write") })
	return &hits
}

func importState(t *testing.T, f *importHTTP, id uint) (applied bool, result string) {
	t.Helper()
	ci := f.s.q().ConfigImport
	row, err := ci.WithContext(context.Background()).Where(ci.ID.Eq(id)).First()
	if err != nil {
		t.Fatal(err)
	}
	return row.AppliedAt != nil, row.Result
}

// 开始应用时第一次记录进度失败：还什么都没建，必须释放领取，可以直接重试，且不返回 200。
func TestImportApplyReleasesTheClaimWhenNothingCouldBeRecorded(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	hits := failResultWrite(t, f, `"state":"applying"`, 1)
	w := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("apply reported %d although progress could not be recorded: %s", w.Code, w.Body)
	}
	if hits.Load() == 0 {
		t.Fatal("injection never fired: the apply does not record progress")
	}
	if n, _ := f.s.q().Site.WithContext(context.Background()).Count(); n != 0 {
		t.Fatalf("%d drafts created before progress could be recorded", n)
	}
	if applied, _ := importState(t, f, row.ID); applied {
		t.Fatal("claim was not released")
	}
	requireStatus(t, f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), http.StatusOK, "")
}

// 草稿已建好、最后一次写结果失败：返回错误（不是 200），复核仍能看到已建的草稿并标明未完成。
func TestImportApplyFinalResultFailureKeepsTheDraftsReviewable(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	failResultWrite(t, f, `"state":"done"`, 0)
	w := f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{})
	requireStatus(t, w, http.StatusInternalServerError, "RESULT_NOT_SAVED")
	if n, _ := f.s.q().Site.WithContext(context.Background()).Count(); n != 1 {
		t.Fatalf("expected the draft to exist, got %d", n)
	}
	rv := f.json(t, "operator", http.MethodGet, fmt.Sprintf("/imports/%d/review", row.ID), nil)
	requireStatus(t, rv, http.StatusOK, "")
	if !strings.Contains(rv.Body.String(), `"state":"applying"`) || !strings.Contains(rv.Body.String(), `"source_name"`) {
		t.Fatalf("review does not show the recorded draft as incomplete: %s", rv.Body)
	}
	// 已建了草稿，重复应用仍被拒绝，避免重复创建
	requireStatus(t, f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), http.StatusConflict, "ALREADY_APPLIED")
}

// 正常完成：结果标记 done，复核标记 done。
func TestImportApplyRecordsDone(t *testing.T) {
	f := newImportHTTP(t)
	row := decodeImport(t, f.upload(t, "operator", "dump.txt", []byte(importDump)))
	requireStatus(t, f.json(t, "operator", http.MethodPost, fmt.Sprintf("/imports/%d/apply", row.ID), map[string]any{}), http.StatusOK, "")
	if _, res := importState(t, f, row.ID); !strings.Contains(res, `"state":"done"`) {
		t.Fatalf("stored result is not marked done: %s", res)
	}
	rv := f.json(t, "operator", http.MethodGet, fmt.Sprintf("/imports/%d/review", row.ID), nil)
	requireStatus(t, rv, http.StatusOK, "")
	if !strings.Contains(rv.Body.String(), `"state":"done"`) {
		t.Fatalf("review is not marked done: %s", rv.Body)
	}
}
