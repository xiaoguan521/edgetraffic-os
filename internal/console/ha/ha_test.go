package ha

import (
	"context"
	"database/sql"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func pg(t *testing.T) *sql.DB {
	dsn := os.Getenv("ETOS_TEST_DB")
	if dsn == "" {
		t.Skip("ETOS_TEST_DB not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestSingleIsLeader(t *testing.T) {
	e := &Elector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var led atomic.Bool
	go e.Run(ctx, func(context.Context) { led.Store(true) }, nil)
	waitFor(t, e.IsLeader)
	if !led.Load() || e.Enabled() {
		t.Fatal("single console must lead immediately")
	}
}

func TestFailover(t *testing.T) {
	a := &Elector{DB: pg(t), Interval: 50 * time.Millisecond}
	b := &Elector{DB: pg(t), Interval: 50 * time.Millisecond}
	actx, acancel := context.WithCancel(context.Background())
	bctx, bcancel := context.WithCancel(context.Background())
	defer bcancel()
	var aLost, bLed atomic.Bool
	aDone := make(chan struct{})
	go func() {
		a.Run(actx, func(l context.Context) { go func() { <-l.Done(); aLost.Store(true) }() }, nil)
		close(aDone)
	}()
	waitFor(t, a.IsLeader)
	go b.Run(bctx, func(context.Context) { bLed.Store(true) }, nil)
	time.Sleep(300 * time.Millisecond)
	if b.IsLeader() || bLed.Load() {
		t.Fatal("two leaders")
	}
	acancel()
	<-aDone
	waitFor(t, b.IsLeader)
	if a.IsLeader() || !aLost.Load() {
		t.Fatal("old leader must step down and cancel its tasks")
	}
}

func TestUnlicensedStandbyDoesNotTakeOver(t *testing.T) {
	a := &Elector{DB: pg(t), Interval: 50 * time.Millisecond}
	b := &Elector{DB: pg(t), Interval: 50 * time.Millisecond, Licensed: func() bool { return false }}
	actx, acancel := context.WithCancel(context.Background())
	bctx, bcancel := context.WithCancel(context.Background())
	defer bcancel()
	aDone := make(chan struct{})
	go func() { a.Run(actx, func(context.Context) {}, nil); close(aDone) }()
	waitFor(t, a.IsLeader)
	go b.Run(bctx, func(context.Context) {}, nil)
	time.Sleep(150 * time.Millisecond)
	acancel()
	<-aDone
	time.Sleep(300 * time.Millisecond)
	if b.IsLeader() {
		t.Fatal("unlicensed standby must not take over automatically")
	}
}

// 主控制台的数据库会话被断开（网络中断、数据库重启）：立即降级并调用 resign；
// 未授权但当过主的进程在数据库恢复后重新取锁。
func TestSessionLossStepsDownAndRecovers(t *testing.T) {
	db := pg(t)
	a := &Elector{DB: db, Interval: 50 * time.Millisecond, Licensed: func() bool { return false }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var leads, resigns atomic.Int32
	go a.Run(ctx, func(context.Context) { leads.Add(1) }, func() { resigns.Add(1) })
	waitFor(t, a.IsLeader)

	if _, err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid = $1 AND objid = $2`,
		int64(uint64(LockKey)>>32), int64(uint64(LockKey)&0xffffffff)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return resigns.Load() == 1 })
	waitFor(t, func() bool { return a.IsLeader() && leads.Load() == 2 })
}
