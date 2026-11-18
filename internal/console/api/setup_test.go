package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xiaoguan521/edgetraffic-os/internal/console/certs"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store/storetest"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/secretbox"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
)

// 尚未初始化的控制台（没有任何用户）。
func newUninitialized(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st := storetest.Open(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lic := &licsvc.Service{Store: st, DataDir: dir, Keys: license.KeyRing{"etos-ci-setup": pub}}
	if err := lic.Init(); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	a := &audit.Logger{DB: st.DB}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cs := &certs.Service{Store: st, Box: box, Audit: a}
	gw := &gateway.Server{Store: st, Lic: lic, Audit: a, Log: log}
	d := &deploy.Service{Store: st, Certs: cs, GW: gw, Audit: a, Log: log, Box: box, MimeTypes: render.DefaultMimeTypes()}
	s := &Server{Store: st, Lic: lic, Certs: cs, Deploy: d, GW: gw, Audit: a, Log: log}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}

func postSetup(h http.Handler, user string) int {
	b, _ := json.Marshal(map[string]string{"username": user, "password": "Setup@2026!first1"})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/setup", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "etos")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// 多个请求同时初始化：最终只能产生一个管理员，其余必须被拒绝。
func TestConcurrentSetupCreatesOneAdmin(t *testing.T) {
	s, h := newUninitialized(t)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = postSetup(h, fmt.Sprintf("admin-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	users, err := s.q().User.WithContext(context.Background()).Count()
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || users != 1 {
		t.Fatalf("successful setups=%d users=%d (codes=%v), want exactly one", ok, users, codes)
	}
}

func TestSetupRejectedOnceInitialized(t *testing.T) {
	_, h := newUninitialized(t)
	if c := postSetup(h, "owner"); c != http.StatusOK {
		t.Fatalf("first setup: %d", c)
	}
	if c := postSetup(h, "intruder"); c != http.StatusConflict {
		t.Fatalf("second setup: %d, want 409", c)
	}
}
