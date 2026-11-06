package conf

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	src := `# comment
gzip on;
server {
    listen 80;   # trailing
    server_name a.example.com "b.example.com";
    location /api/ {
        proxy_pass http://up_api;
        add_header X-A 'single quoted';
    }
}
`
	ds, err := Parse("x.conf", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Name != "gzip" || ds[0].Args[0] != "on" || ds[0].Line != 2 {
		t.Fatalf("top: %+v", ds)
	}
	srv := ds[1]
	if srv.Name != "server" || srv.Block == nil || len(srv.Block) != 3 || srv.Line != 3 {
		t.Fatalf("server: %+v", srv)
	}
	if got := srv.Block[1].Args; len(got) != 2 || got[1] != "b.example.com" {
		t.Fatalf("server_name args %q", got)
	}
	loc := srv.Block[2]
	if loc.Name != "location" || loc.Args[0] != "/api/" || loc.Block[1].Args[1] != "single quoted" || loc.Block[1].Line != 8 {
		t.Fatalf("location %+v", loc)
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"unterminated":       "gzip on",
		"unclosed block":     "server { listen 80;",
		"extra close":        "}",
		"unterminated quote": `add_header X "abc;`,
		"empty statement":    ";",
		"block without name": "{ }",
	} {
		if _, err := Parse("x", []byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseEscapesAndVariables(t *testing.T) {
	ds, err := Parse("x", []byte(`return 200 "a\"b";
rewrite ^/(.*)$ /x/$1 break;
set $v "${host}x";`))
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].Args[1] != `a"b` || ds[1].Args[1] != "/x/$1" || ds[2].Args[1] != "${host}x" {
		t.Fatalf("%q %q %q", ds[0].Args, ds[1].Args, ds[2].Args)
	}
}

func TestDumpSplitsFiles(t *testing.T) {
	dump := `syedge: the configuration file /etc/x/x.conf syntax is ok
# configuration file /etc/x/x.conf:
http {
    include conf.d/*.conf;
}

# configuration file /etc/x/conf.d/a.conf:
server {
    listen 80;
}
`
	files, err := SplitDump([]byte(dump))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "/etc/x/x.conf" || files[1].Name != "/etc/x/conf.d/a.conf" {
		t.Fatalf("%+v", files)
	}
	ds, err := Parse(files[1].Name, files[1].Body)
	if err != nil || ds[0].Name != "server" || ds[0].Line != 1 {
		t.Fatalf("%v %+v", err, ds)
	}
}

func TestPrintRoundTripAndQuoting(t *testing.T) {
	src := `location /a {
    add_header X-A "has space";
    return 200 'semi;colon';
    rewrite ^/(.*)$ /$1 break;
    if ($http_x = "y") { return 403; }
}`
	ds, err := Parse("x", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := Print(ds, 0)
	want := `location  /a  {
    add_header  X-A  "has space";
    return  200  "semi;colon";
    rewrite  ^/(.*)$  /$1  break;
    if  ($http_x  =  y  )  {
        return  403;
    }
}
`
	if out != want {
		t.Fatalf("print:\n%s\nwant:\n%s", out, want)
	}
	// 输出可以重新解析且结果一致
	again, err := Parse("y", []byte(out))
	if err != nil || Print(again, 0) != out {
		t.Fatalf("not stable: %v\n%s", err, Print(again, 0))
	}
}

func TestQuoteNeutralizesInjection(t *testing.T) {
	// 参数中的 ; { } 引号与换行都被引起来，不能闭合指令或块
	for _, a := range []string{"a;b", "a}b", "a{b", `a"b`, "a'b", "a\nb", "a b", "", "#x"} {
		q := Quote(a)
		ds, err := Parse("x", []byte("d "+q+";"))
		if err != nil || len(ds) != 1 || len(ds[0].Args) != 1 || ds[0].Args[0] != a {
			t.Errorf("quote %q -> %s: %v %+v", a, q, err, ds)
		}
	}
	if strings.Contains(Quote("a\nb"), "\n") {
		t.Error("newline must be escaped")
	}
}
