package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/brand"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/api"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/service"
	"github.com/xiaoguan521/edgetraffic-os/internal/portal/store"
	"github.com/xiaoguan521/edgetraffic-os/web/portal"
)

// cmdServe 启动授权后台 Web（规格书 8.8）。部署在厂商内网，只持有公钥；签名在离线签名机上完成。
//
//	syedge-license-server serve -listen 0.0.0.0:8600 -data /var/lib/syedge-license-server -tls-auto [-db postgres://...]
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", envOr("ETOS_PORTAL_LISTEN", "127.0.0.1:8600"), "监听地址")
	dataDir := fs.String("data", envOr("ETOS_PORTAL_DATA", "/var/lib/"+brand.LicenseServer()), "数据目录")
	dsn := fs.String("db", os.Getenv("ETOS_PORTAL_DB"), "数据库：SQLite 文件路径或 postgres:// DSN（默认 <data>/portal.db）")
	tlsCert := fs.String("tls-cert", "", "HTTPS 证书")
	tlsKey := fs.String("tls-key", "", "HTTPS 私钥")
	tlsAuto := fs.Bool("tls-auto", false, "使用内置 Web CA 自动签发 HTTPS 证书（首次访问时下载 /web-ca.crt 导入浏览器）")
	hosts := fs.String("hosts", "", "自动 HTTPS 证书包含的主机名 / IP，逗号分隔（默认本机名与 localhost）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	if *dsn == "" {
		*dsn = filepath.Join(*dataDir, "portal.db")
	}
	st, err := store.Open(*dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	srv := &api.Server{Svc: service.New(st), Store: st, Log: log, Web: portal.Dist(), SecureCookie: *tlsCert != "" || *tlsAuto}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	var webCA *pki.WebCA
	if *tlsAuto && *tlsCert == "" {
		if webCA, err = pki.LoadOrCreateWebCA(filepath.Join(*dataDir, "pki")); err != nil {
			return err
		}
		pem := webCA.CertPEM()
		mux.HandleFunc("/web-ca.crt", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Disposition", `attachment; filename="edgetraffic-license-portal-ca.crt"`)
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			_, _ = w.Write(pem)
		})
	}
	mux.Handle("/", h)
	hs := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute}
	if webCA != nil {
		names := splitCSV(*hosts)
		if len(names) == 0 {
			hn, _ := os.Hostname()
			names = []string{hn}
		}
		names = append(names, "localhost", "127.0.0.1")
		hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: webCA.GetCertificate(names)}
	}
	if host, _, _ := net.SplitHostPort(*listen); *tlsCert == "" && webCA == nil && !loopbackHost(host) {
		log.Warn("授权后台未启用 HTTPS 却监听在非本机地址：登录凭据会以明文传输。请加 -tls-auto 或 -tls-cert / -tls-key")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		switch {
		case *tlsCert != "":
			errc <- hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		case webCA != nil:
			errc <- hs.ListenAndServeTLS("", "")
		default:
			errc <- hs.ListenAndServe()
		}
	}()
	log.Info("license portal started", "listen", *listen, "https", *tlsCert != "" || webCA != nil, "db", dbKind(*dsn))
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func dbKind(dsn string) string {
	if strings.HasPrefix(dsn, "postgres") {
		return "postgres"
	}
	return "sqlite"
}
