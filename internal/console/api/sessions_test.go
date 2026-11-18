package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

const sessionTestPassword = "Import@2026!test"

// withCookie 用指定的 cookie 发请求（绕过 fixture 按角色保存的 cookie，便于复用“旧”会话）。
func (f *importHTTP) withCookie(t *testing.T, ck *http.Cookie, method, path string, v any) int {
	t.Helper()
	const role = "__probe__"
	f.cookies[role] = ck
	defer delete(f.cookies, role)
	if v == nil {
		return f.request(role, method, path, nil, "", true).Code
	}
	return f.json(t, role, method, path, v).Code
}

func (f *importHTTP) login(t *testing.T, user, password string) *http.Cookie {
	t.Helper()
	w := f.json(t, "", http.MethodPost, "/auth/login", map[string]string{"username": user, "password": password})
	requireStatus(t, w, http.StatusOK, "")
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("login did not set session cookie")
	return nil
}

// 退出后，被复制出去的旧 cookie 必须立即失效，而不是等到空闲超时。
func TestLogoutRevokesTheSession(t *testing.T) {
	f := newImportHTTP(t)
	old := f.login(t, "viewer", sessionTestPassword)
	if code := f.withCookie(t, old, http.MethodGet, "/auth/me", nil); code != http.StatusOK {
		t.Fatalf("fresh session rejected: %d", code)
	}
	if code := f.withCookie(t, old, http.MethodPost, "/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code := f.withCookie(t, old, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("session still valid after logout: %d", code)
	}
	// 重新登录不受影响
	fresh := f.login(t, "viewer", sessionTestPassword)
	if code := f.withCookie(t, fresh, http.MethodGet, "/auth/me", nil); code != http.StatusOK {
		t.Fatalf("login after logout rejected: %d", code)
	}
}

// 没有有效会话的退出请求只清 cookie，不能影响任何账号。
func TestLogoutWithoutSessionChangesNothing(t *testing.T) {
	f := newImportHTTP(t)
	ck := f.login(t, "viewer", sessionTestPassword)
	if code := f.withCookie(t, &http.Cookie{Name: cookieName, Value: "garbage"}, http.MethodPost, "/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code := f.withCookie(t, ck, http.MethodGet, "/auth/me", nil); code != http.StatusOK {
		t.Fatalf("unrelated session was revoked: %d", code)
	}
}

// 管理员重置口令后，重置前签发的会话全部失效；用户必须用新口令登录并先改口令。
func TestPasswordResetRevokesExistingSessions(t *testing.T) {
	f := newImportHTTP(t)
	old := f.login(t, "viewer", sessionTestPassword)
	var id uint
	if u, err := f.s.q().User.WithContext(context.Background()).Where(f.s.q().User.Username.Eq("viewer")).First(); err != nil {
		t.Fatal(err)
	} else {
		id = u.ID
	}
	w := f.json(t, "admin", http.MethodPut, "/users/"+fmt.Sprint(id), map[string]string{"reset_password": "Reset@2026!newpw"})
	requireStatus(t, w, http.StatusOK, "")
	if code := f.withCookie(t, old, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("pre-reset session still valid: %d", code)
	}
	if code := f.json(t, "", http.MethodPost, "/auth/login", map[string]string{"username": "viewer", "password": sessionTestPassword}).Code; code == http.StatusOK {
		t.Fatal("old password still accepted")
	}
	fresh := f.login(t, "viewer", "Reset@2026!newpw")
	if code := f.withCookie(t, fresh, http.MethodGet, "/revisions", nil); code != http.StatusForbidden {
		t.Fatalf("forced password change not enforced for the new session: %d", code)
	}
}

// 修改自己的口令：其他设备上的旧会话失效，当前会话拿到新 cookie 继续可用。
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	f := newImportHTTP(t)
	other := f.login(t, "viewer", sessionTestPassword)
	cur := f.login(t, "viewer", sessionTestPassword)
	f.cookies["viewer"] = cur
	w := f.json(t, "viewer", http.MethodPut, "/auth/password", map[string]string{"old_password": sessionTestPassword, "new_password": "Changed@2026!pw1"})
	requireStatus(t, w, http.StatusOK, "")
	var renewed *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			renewed = c
		}
	}
	if renewed == nil {
		t.Fatal("password change did not reissue the current session")
	}
	if code := f.withCookie(t, other, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("other session still valid after password change: %d", code)
	}
	if code := f.withCookie(t, cur, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("pre-change cookie of the current session still valid: %d", code)
	}
	if code := f.withCookie(t, renewed, http.MethodGet, "/auth/me", nil); code != http.StatusOK {
		t.Fatalf("reissued session rejected: %d", code)
	}
}

type blockSaveKey struct{}

// 管理员更新用户时读取了旧行；期间该用户退出。更新写回不能把退出递增的会话版本覆盖掉。
func TestConcurrentAdminUpdateDoesNotRevalidateLoggedOutSession(t *testing.T) {
	f := newImportHTTP(t)
	old := f.login(t, "viewer", sessionTestPassword)
	uq := f.s.q().User
	viewer, err := uq.WithContext(context.Background()).Where(uq.Username.Eq("viewer")).First()
	if err != nil {
		t.Fatal(err)
	}
	reached, resume := make(chan struct{}), make(chan struct{})
	// 管理员请求读到用户行之后暂停（此时不占用数据库连接），模拟“读到的是旧行”。
	// 该请求对 users 表的查询依次是：鉴权中间件、updateUser 读取目标用户；在第二次查询后暂停。
	var reads atomic.Int32
	block := func(tx *gorm.DB) {
		if tx.Statement.Context.Value(blockSaveKey{}) == true && tx.Statement.Table == "users" && reads.Add(1) == 2 {
			close(reached)
			<-resume
		}
	}
	db := f.s.Store.DB
	if err := db.Callback().Query().After("gorm:query").Register("test:pause_after_read", block); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Callback().Query().Remove("test:pause_after_read") }()

	r := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+fmt.Sprint(viewer.ID), bytes.NewBufferString(`{"unlock":true}`))
	r = r.WithContext(context.WithValue(r.Context(), blockSaveKey{}, true))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "etos")
	r.AddCookie(f.cookies["admin"])
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); f.h.ServeHTTP(w, r); done <- w }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("update never reached the write")
	}
	if code := f.withCookie(t, old, http.MethodPost, "/auth/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	close(resume)
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("update: %d %s", w.Code, w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update did not finish")
	}
	if code := f.withCookie(t, old, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("logged-out session accepted again after a concurrent admin update: %d", code)
	}
}

// 管理员重置口令与退出并发：两次递增都必须生效，旧会话不得恢复。
func TestAdminUpdateStillRevokesOnReset(t *testing.T) {
	f := newImportHTTP(t)
	old := f.login(t, "viewer", sessionTestPassword)
	uq := f.s.q().User
	v, _ := uq.WithContext(context.Background()).Where(uq.Username.Eq("viewer")).First()
	w := f.json(t, "admin", http.MethodPut, "/users/"+fmt.Sprint(v.ID), map[string]string{"reset_password": "Reset@2026!newpw"})
	requireStatus(t, w, http.StatusOK, "")
	after, _ := uq.WithContext(context.Background()).Where(uq.ID.Eq(v.ID)).First()
	if after.TokenVersion != v.TokenVersion+1 {
		t.Fatalf("token version %d -> %d, want +1", v.TokenVersion, after.TokenVersion)
	}
	if code := f.withCookie(t, old, http.MethodGet, "/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("session survived reset: %d", code)
	}
}
