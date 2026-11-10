package importer

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/render/conf"
)

// Include expansion only reads the uploaded files. Limits prevent cycles and
// repeated includes from turning a small upload into unbounded work.
const (
	maxIncludeDepth       = 32
	maxExpandedDirectives = 100000
)

type includeFile struct {
	name   string
	body   []byte
	parsed []*conf.Directive
	ready  bool
}

type includeExpander struct {
	files   map[string]*includeFile
	visited map[string]bool
	archive bool
	count   int
}

func includeKey(name string) string { return path.Clean("/" + strings.TrimPrefix(name, "/")) }

func (e *includeExpander) parse(key string) ([]*conf.Directive, error) {
	f := e.files[key]
	if f.ready {
		return f.parsed, nil
	}
	ds, err := conf.Parse(f.name, f.body)
	if err != nil {
		return nil, err
	}
	f.parsed, f.ready = ds, true
	return ds, nil
}

// match first uses the actual path. For an archive or separately selected
// files, an absolute path may have lost leading directories; a unique longest
// suffix is accepted. Wildcards must retain at least one directory component.
func (e *includeExpander) match(ref, base string) ([]string, error) {
	if ref == "" || strings.Contains(ref, "$") {
		return nil, nil
	}
	wild := strings.ContainsAny(ref, "*?[")
	pattern := ref
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(base, pattern)
	}
	pattern = includeKey(pattern)
	if _, err := path.Match(pattern, pattern); err != nil {
		return nil, err
	}
	var exact []string
	for name := range e.files {
		if ok, _ := path.Match(pattern, name); ok {
			exact = append(exact, name)
		}
	}
	if len(exact) > 0 {
		sort.Strings(exact)
		return exact, nil
	}
	if !e.archive {
		return nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	for n := len(parts) - 1; n >= 1; n-- {
		if wild && n < 2 {
			break
		}
		suffix := strings.Join(parts[len(parts)-n:], "/")
		var matched []string
		prefixes := map[string]bool{}
		for name := range e.files {
			segs := strings.Split(strings.TrimPrefix(name, "/"), "/")
			if len(segs) < n {
				continue
			}
			if ok, _ := path.Match(suffix, strings.Join(segs[len(segs)-n:], "/")); ok {
				matched = append(matched, name)
				prefixes[strings.Join(segs[:len(segs)-n], "/")] = true
			}
		}
		if len(matched) > 0 {
			if len(prefixes) != 1 || (!wild && len(matched) != 1) {
				return nil, fmt.Errorf("include %q 匹配多个位置，请保留目录结构", ref)
			}
			sort.Strings(matched)
			return matched, nil
		}
	}
	return nil, nil
}

func (e *includeExpander) expandFile(key, base string, stack map[string]bool, depth int) ([]*conf.Directive, error) {
	if depth > maxIncludeDepth {
		return nil, fmt.Errorf("include 嵌套超过 %d 层", maxIncludeDepth)
	}
	if stack[key] {
		return nil, fmt.Errorf("include 循环引用 %s", e.files[key].name)
	}
	ds, err := e.parse(key)
	if err != nil {
		return nil, err
	}
	e.visited[key] = true
	stack[key] = true
	defer delete(stack, key)
	return e.expandList(ds, base, stack, depth)
}

func (e *includeExpander) expandList(ds []*conf.Directive, base string, stack map[string]bool, depth int) ([]*conf.Directive, error) {
	out := make([]*conf.Directive, 0, len(ds))
	for _, d := range ds {
		e.count++
		if e.count > maxExpandedDirectives {
			return nil, fmt.Errorf("include 展开后指令超过 %d 条", maxExpandedDirectives)
		}
		if d.Name == "include" && !d.IsBlock() && len(d.Args) == 1 {
			matches, err := e.match(d.Args[0], base)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", d.File, d.Line, err)
			}
			if len(matches) > 0 {
				for _, key := range matches {
					included, err := e.expandFile(key, base, stack, depth+1)
					if err != nil {
						return nil, err
					}
					out = append(out, included...)
				}
				continue
			}
		}
		copy := *d
		if d.IsBlock() {
			var err error
			copy.Block, err = e.expandList(d.Block, base, stack, depth)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, &copy)
	}
	return out, nil
}

func containsBlock(ds []*conf.Directive, names ...string) bool {
	for _, d := range ds {
		if d.IsBlock() {
			for _, name := range names {
				if d.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// expandIncludes returns only reachable files when a main configuration is
// present. Standalone archives may instead contain several independent files.
func expandIncludes(source string, files []conf.File, content map[string][]byte) ([][]*conf.Directive, []Issue, error) {
	e := &includeExpander{files: map[string]*includeFile{}, visited: map[string]bool{}, archive: source != "dump"}
	for name, body := range content {
		e.files[includeKey(name)] = &includeFile{name: name, body: body}
	}
	var keys []string
	for _, f := range files {
		key := includeKey(f.Name)
		if old, ok := e.files[key]; ok && !bytes.Equal(old.body, f.Body) {
			return nil, nil, fmt.Errorf("配置文件路径重复：%s", f.Name)
		}
		if _, ok := e.files[key]; !ok {
			e.files[key] = &includeFile{name: f.Name, body: f.Body}
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("没有可解析的配置文件")
	}
	var roots []string
	if source == "dump" {
		roots = keys[:1] // -T output begins with the actual main file.
	} else {
		var fallback []string
		for _, key := range keys {
			ds, err := e.parse(key)
			if err != nil {
				return nil, nil, err
			}
			if containsBlock(ds, "http") {
				roots = append(roots, key)
			} else if containsBlock(ds, "events", "stream") {
				fallback = append(fallback, key)
			}
		}
		if len(roots) > 1 {
			return nil, nil, fmt.Errorf("找到多个主配置，请只上传一份主配置和它引用的文件")
		}
		if len(roots) == 0 && len(fallback) == 1 {
			roots = fallback
		}
		if len(roots) == 0 {
			// Infer standalone roots by removing files referenced by another
			// uploaded file; the expansion itself still detects cycles.
			incoming := map[string]bool{}
			var scan func([]*conf.Directive, string)
			scan = func(ds []*conf.Directive, base string) {
				for _, d := range ds {
					if d.Name == "include" && len(d.Args) == 1 {
						matches, _ := e.match(d.Args[0], base)
						for _, m := range matches {
							incoming[m] = true
						}
					}
					scan(d.Block, base)
				}
			}
			for _, key := range keys {
				ds, _ := e.parse(key)
				scan(ds, path.Dir(key))
			}
			for _, key := range keys {
				if !incoming[key] {
					roots = append(roots, key)
				}
			}
			if len(roots) == 0 {
				return nil, nil, fmt.Errorf("无法确定主配置：include 可能存在循环引用")
			}
		}
	}
	var parsed [][]*conf.Directive
	for _, key := range roots {
		if e.visited[key] {
			continue
		}
		ds, err := e.expandFile(key, path.Dir(key), map[string]bool{}, 0)
		if err != nil {
			return nil, nil, err
		}
		parsed = append(parsed, ds)
	}
	var issues []Issue
	for _, key := range keys {
		if !e.visited[key] && strings.HasSuffix(key, ".conf") {
			issues = append(issues, Issue{File: e.files[key].name, Line: 1, Directive: "include", Reason: "文件未被主配置引用，未导入；请检查路径和上传内容"})
		}
	}
	return parsed, issues, nil
}
