// Package collect 采集节点指标：stub_status、主机 CPU/内存、按站点按分钟聚合的访问日志（规格书 5.3）。
package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stub 是 stub_status 的解析结果。
type Stub struct {
	Active   uint64
	Requests uint64
}

// ReadStub 读取本机状态页，例如 http://127.0.0.1:8081/__syapp_status。
func ReadStub(ctx context.Context, url string) (Stub, error) {
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return Stub{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return ParseStub(string(b))
}

var runningVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ReadVersion 读取本实例 worker 实际运行的数据面版本（状态服务的版本路径，见 layout.VersionPath）。
// 旧配置没有该路径时返回错误，调用方上报空串。
func ReadVersion(ctx context.Context, url string) (string, error) {
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	v := strings.TrimSpace(string(b))
	if resp.StatusCode != http.StatusOK || !runningVersionRe.MatchString(v) {
		return "", fmt.Errorf("collect: no running version (%d)", resp.StatusCode)
	}
	return v, nil
}

// ParseStub 解析 stub_status 文本：
//
//	Active connections: 1
//	server accepts handled requests
//	 10 10 25
func ParseStub(s string) (Stub, error) {
	var st Stub
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 3 {
		return st, fmt.Errorf("collect: unexpected stub_status")
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "Active connections: %d", &st.Active); err != nil {
		return st, err
	}
	f := strings.Fields(lines[2])
	if len(f) >= 3 {
		st.Requests, _ = strconv.ParseUint(f[2], 10, 64)
	}
	return st, nil
}

// Host 返回 CPU 使用率（两次调用之间）与已用内存。
type Host struct {
	mu         sync.Mutex
	idle, tot  uint64
	haveSample bool
}

func (h *Host) Sample() (cpuPercent float64, memUsed uint64) {
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		var idle, tot uint64
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			tot += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
		h.mu.Lock()
		if h.haveSample && tot > h.tot {
			cpuPercent = 100 * (1 - float64(idle-h.idle)/float64(tot-h.tot))
		}
		h.idle, h.tot, h.haveSample = idle, tot, true
		h.mu.Unlock()
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail uint64
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			n, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = n * 1024
			case "MemAvailable:":
				avail = n * 1024
			}
		}
		if total > avail {
			memUsed = total - avail
		}
	}
	return
}

// Minute 是某站点某分钟的聚合。
type Minute struct {
	Site                   string
	Minute                 int64 // unix 秒，按分钟取整
	Requests               uint64
	S2xx, S3xx, S4xx, S5xx uint64
	P50ms, P95ms           float64
	Bytes                  uint64
}

type bucket struct {
	m   Minute
	rts []float64
}

// AccessLog 增量读取 JSON 访问日志并按分钟聚合。
type AccessLog struct {
	Path    string
	off     int64
	buckets map[string]*bucket
}

type logLine struct {
	TS     string  `json:"ts"`
	Site   string  `json:"site"`
	Status int     `json:"status"`
	Bytes  uint64  `json:"bytes"`
	RT     float64 `json:"rt"`
}

const maxSamples = 10000

// Collect 读入新增日志行，返回已经结束（早于当前分钟）的分钟聚合。
func (a *AccessLog) Collect(now time.Time) []Minute {
	if a.buckets == nil {
		a.buckets = map[string]*bucket{}
	}
	f, err := os.Open(a.Path)
	if err == nil {
		if fi, err := f.Stat(); err == nil && fi.Size() < a.off {
			a.off = 0 // 日志已轮转
		}
		if _, err := f.Seek(a.off, io.SeekStart); err == nil {
			r := bufio.NewReaderSize(f, 64<<10)
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 && line[len(line)-1] == '\n' {
					a.off += int64(len(line))
					a.add(line)
				}
				if err != nil {
					break // 不完整的最后一行留到下次
				}
			}
		}
		_ = f.Close()
	}
	cur := now.Truncate(time.Minute).Unix()
	var out []Minute
	for k, b := range a.buckets {
		if b.m.Minute < cur {
			sort.Float64s(b.rts)
			if n := len(b.rts); n > 0 {
				b.m.P50ms = b.rts[n/2] * 1000
				b.m.P95ms = b.rts[(n*95)/100] * 1000
			}
			out = append(out, b.m)
			delete(a.buckets, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minute != out[j].Minute {
			return out[i].Minute < out[j].Minute
		}
		return out[i].Site < out[j].Site
	})
	return out
}

func (a *AccessLog) add(raw []byte) {
	var l logLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	ts, err := time.Parse(time.RFC3339, l.TS)
	if err != nil {
		return
	}
	site := l.Site
	if site == "" || site == "_" {
		site = "(default)"
	}
	min := ts.Truncate(time.Minute).Unix()
	key := site + "|" + strconv.FormatInt(min, 10)
	b := a.buckets[key]
	if b == nil {
		b = &bucket{m: Minute{Site: site, Minute: min}}
		a.buckets[key] = b
	}
	b.m.Requests++
	b.m.Bytes += l.Bytes
	switch {
	case l.Status >= 500:
		b.m.S5xx++
	case l.Status >= 400:
		b.m.S4xx++
	case l.Status >= 300:
		b.m.S3xx++
	default:
		b.m.S2xx++
	}
	if len(b.rts) < maxSamples {
		b.rts = append(b.rts, l.RT)
	}
}

// SkipToEnd 让首次采集从日志末尾开始，避免把历史日志一次性上报。
func (a *AccessLog) SkipToEnd() {
	if fi, err := os.Stat(a.Path); err == nil {
		a.off = fi.Size()
	}
}
