package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/configfile"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
)

var consoleConfigKeys = []string{"ETOS_LISTEN", "ETOS_TLS_CERT", "ETOS_TLS_KEY", "ETOS_TLS_AUTO", "ETOS_WEB_HOSTS", "ETOS_GRPC_LISTEN", "ETOS_GRPC_HOSTS", "ETOS_GRPC_ADVERTISE", "ETOS_DATA", "ETOS_DB", "ETOS_ACME_CA_FILE"}

func parseOptions(args []string) (options, bool, error) { return parseOptionsTo(args, io.Discard) }

func parseOptionsTo(args []string, out io.Writer) (options, bool, error) {
	fs := flag.NewFlagSet(brand.Console(), flag.ContinueOnError)
	fs.SetOutput(out)
	var o options
	var config string
	var showVersion bool
	fs.StringVar(&config, "config", "", "配置文件（KEY=VALUE，也可用 -c）")
	fs.StringVar(&config, "c", "", "配置文件")
	fs.StringVar(&o.listen, "listen", envOr("ETOS_LISTEN", "127.0.0.1:8443"), "Web 控制台监听地址")
	fs.StringVar(&o.tlsCert, "tls-cert", os.Getenv("ETOS_TLS_CERT"), "Web 控制台 TLS 证书")
	fs.StringVar(&o.tlsKey, "tls-key", os.Getenv("ETOS_TLS_KEY"), "Web 控制台 TLS 私钥")
	fs.StringVar(&o.grpcListen, "grpc-listen", envOr("ETOS_GRPC_LISTEN", "0.0.0.0:9443"), "节点连接的 gRPC 监听地址")
	fs.StringVar(&o.grpcHosts, "grpc-hosts", envOr("ETOS_GRPC_HOSTS", ""), "gRPC 服务端证书包含的主机名/IP")
	fs.StringVar(&o.grpcAdvertise, "grpc-advertise", envOr("ETOS_GRPC_ADVERTISE", ""), "节点连接控制台的地址")
	fs.StringVar(&o.dataDir, "data", envOr("ETOS_DATA", "/var/lib/"+brand.Console()), "数据目录")
	fs.StringVar(&o.dsn, "db", envOr("ETOS_DB", ""), "数据库路径或 PostgreSQL DSN")
	fs.StringVar(&o.acmeCA, "acme-ca-file", os.Getenv("ETOS_ACME_CA_FILE"), "额外信任的 ACME CA 证书")
	fs.BoolVar(&o.tlsAuto, "tls-auto", envBool("ETOS_TLS_AUTO"), "使用内置 Web CA 自动签发 HTTPS 证书")
	fs.StringVar(&o.webHosts, "web-hosts", envOr("ETOS_WEB_HOSTS", ""), "Web 证书包含的主机名/IP")
	fs.BoolVar(&showVersion, "version", false, "打印版本并退出")
	if err := fs.Parse(args); err != nil {
		return options{}, false, err
	}
	if fs.NArg() != 0 {
		return options{}, false, fmt.Errorf("未知参数或命令 %q", fs.Arg(0))
	}
	if config == "" {
		if configfile.Specified(fs, "config") || configfile.Specified(fs, "c") {
			return options{}, false, fmt.Errorf("配置文件路径不能为空")
		}
		return o, showVersion, nil
	}
	c, err := configfile.Load(config, consoleConfigKeys)
	if err != nil {
		return options{}, false, err
	}
	for _, f := range []struct {
		key, name string
		dst       *string
		path      bool
	}{
		{"ETOS_LISTEN", "listen", &o.listen, false}, {"ETOS_TLS_CERT", "tls-cert", &o.tlsCert, true},
		{"ETOS_TLS_KEY", "tls-key", &o.tlsKey, true}, {"ETOS_GRPC_LISTEN", "grpc-listen", &o.grpcListen, false},
		{"ETOS_GRPC_HOSTS", "grpc-hosts", &o.grpcHosts, false}, {"ETOS_GRPC_ADVERTISE", "grpc-advertise", &o.grpcAdvertise, false},
		{"ETOS_DATA", "data", &o.dataDir, true}, {"ETOS_ACME_CA_FILE", "acme-ca-file", &o.acmeCA, true},
		{"ETOS_WEB_HOSTS", "web-hosts", &o.webHosts, false},
	} {
		if configfile.Specified(fs, f.name) {
			continue
		}
		var v string
		var ok bool
		if f.path {
			v, ok = c.Path(f.key)
		} else {
			v, ok = c.Value(f.key)
		}
		if ok {
			*f.dst = v
		}
	}
	if v, ok := c.Value("ETOS_DB"); ok && !configfile.Specified(fs, "db") {
		o.dsn = consoleConfigDB(c, v)
	}
	if v, ok := c.Value("ETOS_TLS_AUTO"); ok {
		var b bool
		if v = strings.TrimSpace(v); v != "" {
			b, err = strconv.ParseBool(v)
			if err != nil {
				return options{}, false, fmt.Errorf("ETOS_TLS_AUTO 必须是 true/false 或 1/0")
			}
		}
		if !configfile.Specified(fs, "tls-auto") {
			o.tlsAuto = b
		}
	}
	return o, showVersion, nil
}

func consoleConfigDB(c configfile.File, v string) string {
	if v == "" || v == ":memory:" || store.IsPostgres(v) || strings.HasPrefix(v, "file:") {
		return v
	}
	p, _ := c.Path("ETOS_DB")
	return p
}
