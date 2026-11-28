package collect

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
)

// TailOptions 控制单文件实时日志跟踪的初始回放、分批、截断与限速保护。
type TailOptions struct {
	InitialLines   int           // 启动时回放末尾最多多少行（默认 100）
	InitialBytes   int64         // 启动回放最多从文件末尾往前读多少字节（默认 64 KiB）
	MaxLineBytes   int           // 单行最大字节数，超出截断（默认 4096）
	BatchLines     int           // 每批最多发送行数（默认 100）
	BatchBytes     int           // 每批最多字节数（默认 64 KiB）
	PollInterval   time.Duration // 轮询间隔（默认 300ms）
	MaxLinesPerSec int           // 每秒最多发送行数，超速时快进到末尾保护控制通道（默认 500）
}

func (o TailOptions) withDefaults() TailOptions {
	if o.InitialLines <= 0 {
		o.InitialLines = 100
	}
	if o.InitialBytes <= 0 {
		o.InitialBytes = 64 << 10
	}
	if o.MaxLineBytes <= 0 {
		o.MaxLineBytes = 4096
	}
	if o.BatchLines <= 0 {
		o.BatchLines = 100
	}
	if o.BatchBytes <= 0 {
		o.BatchBytes = 64 << 10
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 300 * time.Millisecond
	}
	if o.MaxLinesPerSec <= 0 {
		o.MaxLinesPerSec = 500
	}
	return o
}

func formatLogLine(raw []byte, maxBytes int) string {
	raw = bytes.TrimRight(raw, "\r\n")
	if len(raw) > maxBytes {
		return string(raw[:maxBytes]) + "...[truncated]"
	}
	return string(raw)
}

func emitBatched(lines []string, maxLines, maxBytes int, emit func([]string)) {
	if len(lines) == 0 {
		return
	}
	var batch []string
	size := 0
	flush := func() {
		if len(batch) > 0 {
			emit(batch)
			batch = nil
			size = 0
		}
	}
	for _, l := range lines {
		if len(batch) >= maxLines || (len(batch) > 0 && size+len(l) > maxBytes) {
			flush()
		}
		batch = append(batch, l)
		size += len(l)
	}
	flush()
}

// readInitialTail 读取文件末尾最近 initialLines 行，返回这些行、文件末尾偏移量与文件元信息。
func readInitialTail(path string, opt TailOptions) ([]string, int64, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, nil, err
	}
	size := fi.Size()
	start := int64(0)
	if size > opt.InitialBytes {
		start = size - opt.InitialBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, size, fi, err
	}
	data, err := io.ReadAll(io.LimitReader(f, opt.InitialBytes))
	if err != nil {
		return nil, size, fi, err
	}
	readEnd := start + int64(len(data))
	discarded := int64(0)
	// 从中间开始读时，丢弃第一段可能残缺的行
	if start > 0 {
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			discarded = int64(idx + 1)
			data = data[idx+1:]
		} else {
			return nil, readEnd, fi, nil
		}
	}
	// 只保留以 \n 结尾的完整行，避免与后续增量重复
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return nil, start + discarded, fi, nil
	}
	endOff := readEnd - int64(len(data)-(lastNL+1))
	data = data[:lastNL]
	rawLines := bytes.Split(data, []byte{'\n'})
	if len(rawLines) > opt.InitialLines {
		rawLines = rawLines[len(rawLines)-opt.InitialLines:]
	}
	out := make([]string, 0, len(rawLines))
	for _, rl := range rawLines {
		out = append(out, formatLogLine(rl, opt.MaxLineBytes))
	}
	return out, endOff, fi, nil
}

// TailFile 先回放文件末尾若干行，再持续增量跟踪新写入的行，支持日志轮转/截断检测与超速保护，直到 ctx 取消。
func TailFile(ctx context.Context, path string, opts TailOptions, emit func(lines []string)) error {
	opt := opts.withDefaults()
	var (
		off    int64
		prevFI os.FileInfo
	)
	if lines, nextOff, fi, err := readInitialTail(path, opt); err == nil {
		off = nextOff
		prevFI = fi
		emitBatched(lines, opt.BatchLines, opt.BatchBytes, emit)
	}

	ticker := time.NewTicker(opt.PollInterval)
	defer ticker.Stop()

	windowStart := time.Now()
	windowSent := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			if now.Sub(windowStart) >= time.Second {
				windowStart = now
				windowSent = 0
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			fi, err := f.Stat()
			if err != nil {
				_ = f.Close()
				continue
			}
			// 检测文件轮转（inode 变更）或截断（文件变小）
			if prevFI == nil || !os.SameFile(prevFI, fi) || fi.Size() < off {
				off = 0
			}
			prevFI = fi
			if fi.Size() == off {
				_ = f.Close()
				continue
			}
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				_ = f.Close()
				continue
			}
			r := bufio.NewReaderSize(f, 64<<10)
			var lines []string
			rateLimited := false
			for {
				line, rerr := r.ReadBytes('\n')
				if len(line) > 0 && line[len(line)-1] == '\n' {
					off += int64(len(line))
					if windowSent >= opt.MaxLinesPerSec {
						rateLimited = true
						// 快进到当前文件尾，避免高并发场景拖垮控制通道
						off = fi.Size()
						break
					}
					lines = append(lines, formatLogLine(line, opt.MaxLineBytes))
					windowSent++
				}
				if rerr != nil {
					break
				}
			}
			_ = f.Close()
			if rateLimited {
				lines = append(lines, "["+brand.Agent()+"] 日志输出速率过高，已跳过部分中间行以保护控制通道")
			}
			emitBatched(lines, opt.BatchLines, opt.BatchBytes, emit)
		}
	}
}
