// Package configfile 读取由管理员显式指定的 KEY=VALUE 配置；不执行 shell 或变量替换。
package configfile

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxBytes = 1 << 20

var keyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// File 是已经校验过的配置；相对文件路径以配置文件所在目录为基准。
type File struct {
	dir    string
	values map[string]string
}

// Load 只接受调用方列出的键，避免拼写错误被静默忽略。配置必须是普通文件。
func Load(name string, allowed []string) (File, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return File{}, err
	}
	f, err := os.Open(absolute)
	if err != nil {
		return File{}, fmt.Errorf("open config %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return File{}, fmt.Errorf("stat config %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return File{}, errors.New("config must be a regular file no larger than 1 MiB")
	}
	keys := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		keys[k] = true
	}
	out := File{dir: filepath.Dir(absolute), values: make(map[string]string)}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), maxBytes+1)
	line, size := 0, 0
	for s.Scan() {
		line++
		size += len(s.Bytes()) + 1
		if size > maxBytes {
			return File{}, errors.New("config exceeds 1 MiB")
		}
		t := strings.TrimSpace(s.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "export ") {
			t = strings.TrimSpace(strings.TrimPrefix(t, "export "))
		}
		k, raw, ok := strings.Cut(t, "=")
		k = strings.TrimSpace(k)
		if !ok || !keyRE.MatchString(k) {
			return File{}, fmt.Errorf("config line %d: expected KEY=VALUE", line)
		}
		if !keys[k] {
			return File{}, fmt.Errorf("config line %d: unsupported key %s", line, k)
		}
		if _, dup := out.values[k]; dup {
			return File{}, fmt.Errorf("config line %d: duplicate key %s", line, k)
		}
		v, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return File{}, fmt.Errorf("config line %d (%s): %w", line, k, err)
		}
		out.values[k] = v
	}
	if err := s.Err(); err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	return out, nil
}

func parseValue(v string) (string, error) {
	if len(v) > 0 {
		switch v[0] {
		case '"':
			var err error
			v, err = strconv.Unquote(v)
			if err != nil {
				return "", errors.New("invalid double-quoted value")
			}
		case '\'':
			if len(v) < 2 || v[len(v)-1] != '\'' || strings.Contains(v[1:len(v)-1], "'") {
				return "", errors.New("invalid single-quoted value")
			}
			v = v[1 : len(v)-1]
		}
	}
	if strings.ContainsAny(v, "\x00\r\n") {
		return "", errors.New("value must not contain control characters")
	}
	return v, nil
}

// Value 查找键，区分未指定与显式空值。
func (f File) Value(k string) (string, bool) { v, ok := f.values[k]; return v, ok }

// Path 返回配置中的路径；非空相对路径以配置文件目录解析。
func (f File) Path(k string) (string, bool) {
	v, ok := f.Value(k)
	if ok && v != "" && !filepath.IsAbs(v) {
		v = filepath.Join(f.dir, v)
	}
	return v, ok
}

// Specified 判断命令行是否显式设置过某个参数。
func Specified(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
