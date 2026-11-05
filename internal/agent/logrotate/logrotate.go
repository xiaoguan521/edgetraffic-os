// Package logrotate 是 user 模式下 agent 内置的日志轮转（设计 0001 第 6.4 节）：普通用户没有系统 logrotate。
//
// 每天第一次检查时（或单个文件超过大小上限时）把非空日志重命名为 <文件>.<时间>，通知数据面重新打开日志（reopen），
// 再压缩重命名后的文件并只保留最近 Keep 份。数据面重新打开之前不压缩：它可能仍在写那个文件。
package logrotate

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rotator 轮转一组日志文件。
type Rotator struct {
	Files    []string
	Keep     int          // 每个文件保留的历史份数
	MaxBytes int64        // 超过即当天也轮转；0 表示只按天
	Reopen   func() error // 让数据面重新打开日志（经 wrapper reopen）
	Now      func() time.Time

	day string // 上次检查所在的日期
}

func (r *Rotator) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Check 执行一次检查，返回轮转的文件数。
func (r *Rotator) Check() (int, error) {
	now := r.now()
	today := now.Format("20060102")
	daily := r.day != "" && r.day != today // 第一次检查不知道文件属于哪一天，不按天轮转
	r.day = today
	var rotated []string
	for _, f := range r.Files {
		st, err := os.Stat(f)
		if err != nil || st.Size() == 0 {
			continue
		}
		if !daily && (r.MaxBytes <= 0 || st.Size() < r.MaxBytes) {
			continue
		}
		dst := f + "." + now.Format("20060102-150405")
		if err := os.Rename(f, dst); err != nil {
			return len(rotated), err
		}
		rotated = append(rotated, dst)
	}
	if len(rotated) == 0 {
		return 0, nil
	}
	if r.Reopen != nil {
		if err := r.Reopen(); err != nil {
			return len(rotated), fmt.Errorf("reopen after rotating logs: %w", err)
		}
	}
	var errs []error
	for _, p := range rotated {
		if err := compress(p); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range r.Files {
		errs = append(errs, r.prune(f))
	}
	return len(rotated), errors.Join(errs...)
}

func compress(p string) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := p + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p+".gz"); err != nil {
		return err
	}
	return os.Remove(p)
}

// prune 只保留 f 最近的 Keep 份历史（按文件名中的时间排序）。
func (r *Rotator) prune(f string) error {
	if r.Keep <= 0 {
		return nil
	}
	ents, err := os.ReadDir(filepath.Dir(f))
	if err != nil {
		return err
	}
	prefix := filepath.Base(f) + "."
	var old []string
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".gz") {
			old = append(old, n)
		}
	}
	sort.Strings(old)
	for len(old) > r.Keep {
		if err := os.Remove(filepath.Join(filepath.Dir(f), old[0])); err != nil {
			return err
		}
		old = old[1:]
	}
	return nil
}
