// Package conf 解析与输出 syedge 配置语法（与上游配置格式相同）。
//
// 用于配置导入（规格书 12.4）与自定义片段（12.7）：用户提交的文本先解析成语法树，
// 经白名单检查后由 Print 重新序列化输出，从不把用户文本原样拼进配置。
//
// 语法：指令 = 名称 参数* ( ";" | "{" 指令* "}" )；参数是裸词或单 / 双引号字符串（支持 \ 转义）；
// "#" 到行尾是注释；"${var}" 中的花括号属于参数本身。
package conf

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Directive 是一条指令；Block 非 nil 表示带块（可以是空块）。
type Directive struct {
	Name  string
	Args  []string
	Block []*Directive
	File  string
	Line  int
}

// IsBlock 返回指令是否带块。
func (d *Directive) IsBlock() bool { return d.Block != nil }

// Error 是带文件与行号的解析错误。
type Error struct {
	File string
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg) }

type token struct {
	text   string
	line   int
	quoted bool
}

// lex 把源文本切成词：裸词、引号字符串，以及独立的 ; { }。
func lex(file string, src []byte) ([]token, error) {
	var toks []token
	line := 1
	i := 0
	for i < len(src) {
		c := src[i]
		switch c {
		case '\n':
			line++
			i++
		case ' ', '\t', '\r':
			i++
		case '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ';', '{', '}':
			toks = append(toks, token{text: string(c), line: line})
			i++
		case '"', '\'':
			start := line
			q := c
			i++
			var b strings.Builder
			closed := false
			for i < len(src) {
				ch := src[i]
				if ch == '\\' && i+1 < len(src) {
					nx := src[i+1]
					switch nx {
					case q, '\\':
						b.WriteByte(nx)
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte('\\')
						b.WriteByte(nx)
					}
					if nx == '\n' {
						line++
					}
					i += 2
					continue
				}
				if ch == q {
					closed = true
					i++
					break
				}
				if ch == '\n' {
					line++
				}
				b.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, &Error{file, start, "unterminated quoted string"}
			}
			toks = append(toks, token{text: b.String(), line: start, quoted: true})
		default:
			var b strings.Builder
			for i < len(src) {
				ch := src[i]
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ';' || ch == '"' || ch == '\'' {
					break
				}
				if ch == '{' || ch == '}' {
					// "${name}" 中的花括号属于参数
					if ch == '{' && b.Len() > 0 && strings.HasSuffix(b.String(), "$") {
						j := bytes.IndexByte(src[i:], '}')
						if j < 0 {
							return nil, &Error{file, line, "unterminated ${ variable"}
						}
						b.Write(src[i : i+j+1])
						i += j + 1
						continue
					}
					break
				}
				if ch == '\\' && i+1 < len(src) {
					b.WriteByte(ch)
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if ch == '#' && b.Len() == 0 {
					break
				}
				b.WriteByte(ch)
				i++
			}
			toks = append(toks, token{text: b.String(), line: line})
		}
	}
	return toks, nil
}

// Parse 解析一段配置文本。
func Parse(file string, src []byte) ([]*Directive, error) {
	toks, err := lex(file, src)
	if err != nil {
		return nil, err
	}
	p := &parser{file: file, toks: toks}
	ds, err := p.list(false)
	if err != nil {
		return nil, err
	}
	return ds, nil
}

type parser struct {
	file string
	toks []token
	pos  int
}

func (p *parser) list(inBlock bool) ([]*Directive, error) {
	out := []*Directive{}
	for p.pos < len(p.toks) {
		t := p.toks[p.pos]
		if !t.quoted && t.text == "}" {
			if !inBlock {
				return nil, &Error{p.file, t.line, `unexpected "}"`}
			}
			p.pos++
			return out, nil
		}
		if !t.quoted && (t.text == ";" || t.text == "{") {
			return nil, &Error{p.file, t.line, fmt.Sprintf("unexpected %q (missing directive name)", t.text)}
		}
		d := &Directive{Name: t.text, File: p.file, Line: t.line, Args: []string{}}
		p.pos++
		for {
			if p.pos >= len(p.toks) {
				return nil, &Error{p.file, d.Line, fmt.Sprintf("directive %q is not terminated by \";\"", d.Name)}
			}
			a := p.toks[p.pos]
			p.pos++
			if !a.quoted && a.text == ";" {
				break
			}
			if !a.quoted && a.text == "{" {
				blk, err := p.list(true)
				if err != nil {
					return nil, err
				}
				d.Block = blk
				break
			}
			if !a.quoted && a.text == "}" {
				return nil, &Error{p.file, a.line, fmt.Sprintf("directive %q is not terminated by \";\"", d.Name)}
			}
			d.Args = append(d.Args, a.text)
		}
		out = append(out, d)
	}
	if inBlock {
		line := 1
		if len(p.toks) > 0 {
			line = p.toks[len(p.toks)-1].line
		}
		return nil, &Error{p.file, line, `unexpected end of file, expecting "}"`}
	}
	return out, nil
}

// File 是 -T 输出中的一个文件。
type File struct {
	Name string
	Body []byte
}

var dumpHeader = regexp.MustCompile(`(?m)^# configuration file (.+):\s*$`)

// SplitDump 把 `-T` 的输出（所有 include 展开后逐个打印的文件）拆成文件列表。
func SplitDump(b []byte) ([]File, error) {
	idx := dumpHeader.FindAllSubmatchIndex(b, -1)
	if len(idx) == 0 {
		return nil, fmt.Errorf("not a -T dump: no \"# configuration file <path>:\" header found")
	}
	var out []File
	for i, m := range idx {
		end := len(b)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		body := b[m[1]:end]
		body = bytes.TrimPrefix(body, []byte("\n"))
		out = append(out, File{Name: string(b[m[2]:m[3]]), Body: body})
	}
	return out, nil
}

var bare = regexp.MustCompile(`^[A-Za-z0-9_\-./:$^*?+()\[\]|=!~@,%&<>]+$`)

// Quote 返回参数的安全写法：只含安全字符时原样输出，否则用双引号并转义。
// 输出的参数不可能闭合指令或块（; { } 引号与换行都在引号内或被转义）。
func Quote(a string) string {
	if a != "" && bare.MatchString(a) && !strings.HasPrefix(a, "#") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range a {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			// 丢弃
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Print 以规范格式输出指令（每级缩进 4 个空格，参数之间两个空格，与模板风格一致）。
func Print(ds []*Directive, indent int) string {
	var b strings.Builder
	printTo(&b, ds, indent)
	return b.String()
}

func printTo(b *strings.Builder, ds []*Directive, indent int) {
	pad := strings.Repeat("    ", indent)
	for _, d := range ds {
		b.WriteString(pad)
		b.WriteString(Quote(d.Name))
		for _, a := range d.Args {
			b.WriteString("  ")
			b.WriteString(Quote(a))
		}
		if d.Block == nil {
			b.WriteString(";\n")
			continue
		}
		b.WriteString("  {\n")
		printTo(b, d.Block, indent+1)
		b.WriteString(pad)
		b.WriteString("}\n")
	}
}
