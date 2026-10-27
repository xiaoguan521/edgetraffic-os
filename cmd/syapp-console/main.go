// syapp-console：EdgeTraffic OS Web 控制台（规格书第 7 节）。
//
// 一个进程同时提供：Web 控制台与 REST API（-listen），以及供节点主动连接的 gRPC 网关（-grpc-listen）。
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/xiaoguan521/edgetraffic-os/internal/console/api"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/deploy"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/gateway"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/licsvc"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/pki"
	"github.com/xiaoguan521/edgetraffic-os/internal/console/store"
	"github.com/xiaoguan521/edgetraffic-os/internal/license"
	"github.com/xiaoguan521/edgetraffic-os/internal/pkg/audit"
	"github.com/xiaoguan521/edgetraffic-os/internal/render"
	"github.com/xiaoguan521/edgetraffic-os/internal/version"
	"github.com/xiaoguan521/edgetraffic-os/web"
)

func main() {
	listen := flag.String("listen", envOr("ETOS_LISTEN", "127.0.0.1:8443"), "Web 控制台监听地址（生产环境只在内网或前置 TLS）")
	tlsCert := flag.String("tls-cert", os.Getenv("ETOS_TLS_CERT"), "Web 控制台 TLS 证书（可选）")
	tlsKey := flag.String("tls-key", os.Getenv("ETOS_TLS_KEY"), "Web 控制台 TLS 私钥（可选）")
	grpcListen := flag.String("grpc-listen", envOr("ETOS_GRPC_LISTEN", "0.0.0.0:9443"), "节点连接的 gRPC 监听地址")
	grpcHosts := flag.String("grpc-hosts", envOr("ETOS_GRPC_HOSTS", ""), "gRPC 服务端证书包含的主机名/IP，逗号分隔（节点用它们连接）")
	grpcAdvertise := flag.String("grpc-advertise", envOr("ETOS_GRPC_ADVERTISE", ""), "写入节点安装命令的控制台地址 host:port")
	dataDir := flag.String("data", envOr("ETOS_DATA", "/var/lib/syapp-console"), "数据目录（数据库、CA、安装 ID）")
	dsn := flag.String("db", envOr("ETOS_DB", ""), "数据库：SQLite 文件路径或 postgres:// DSN（默认 <data>/console.db）")
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("console"))
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log, *listen, *tlsCert, *tlsKey, *grpcListen, *grpcHosts, *grpcAdvertise, *dataDir, *dsn); err != nil {
		log.Error("syapp-console stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, tlsCert, tlsKey, grpcListen, grpcHosts, grpcAdvertise, dataDir, dsn string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if dsn == "" {
		dsn = filepath.Join(dataDir, "console.db")
	}
	st, err := store.Open(dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	ca, err := pki.LoadOrCreate(filepath.Join(dataDir, "pki"))
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	cmdKey, err := commandKey(st)
	if err != nil {
		return err
	}
	lic := &licsvc.Service{Store: st, DataDir: dataDir, Keys: license.BuiltinKeyRing()}
	if err := lic.Init(); err != nil {
		return fmt.Errorf("license: %w", err)
	}
	lic.Tick(time.Now())
	aud := &audit.Logger{DB: st.DB}

	gw := &gateway.Server{Store: st, CA: ca, CmdKey: cmdKey, Lic: lic, Audit: aud, Log: log}
	dep := &deploy.Service{Store: st, GW: gw, Audit: aud, Log: log, MimeTypes: render.DefaultMimeTypes()}
	gw.OnOnline = dep.Align
	// 启动时所有节点先视为离线，等待重新连接
	st.DB.Model(&store.Node{}).Where("status = ?", "online").Update("status", "offline")

	hosts := splitList(grpcHosts)
	if len(hosts) == 0 {
		h, _ := os.Hostname()
		hosts = []string{h, "localhost", "127.0.0.1"}
	}
	if grpcAdvertise == "" {
		_, port, _ := net.SplitHostPort(grpcListen)
		grpcAdvertise = net.JoinHostPort(hosts[0], port)
	}
	tlsCfg, err := gw.TLSConfig(hosts)
	if err != nil {
		return err
	}
	gs := gw.GRPCServer(tlsCfg)
	gl, err := net.Listen("tcp", grpcListen)
	if err != nil {
		return err
	}

	apiSrv := &api.Server{Store: st, GW: gw, Deploy: dep, Lic: lic, Audit: aud, Log: log, Web: web.Dist(),
		GRPCAddr: grpcAdvertise, CAFingerprint: pki.Fingerprint(ca.Cert.Raw), SecureCookie: tlsCert != ""}
	h, err := apiSrv.Handler()
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go housekeeping(ctx, st, lic)

	errc := make(chan error, 2)
	go func() { errc <- gs.Serve(gl) }()
	go func() {
		if tlsCert != "" {
			errc <- hs.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	log.Info("syapp-console started", "version", version.Version, "web", listen, "grpc", grpcListen,
		"grpc_advertise", grpcAdvertise, "grpc_hosts", strings.Join(hosts, ","), "installation_id", lic.InstallationID())

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	gs.GracefulStop()
	return nil
}

// commandKey 读取或生成指令签名密钥（Ed25519），私钥只保存在控制台数据库。
func commandKey(st *store.Store) (ed25519.PrivateKey, error) {
	if v, err := st.GetKV("command_key"); err == nil {
		b, err := base64.StdEncoding.DecodeString(v)
		if err == nil && len(b) == ed25519.PrivateKeySize {
			return ed25519.PrivateKey(b), nil
		}
		return nil, errors.New("corrupt command_key")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return priv, st.SetKV("command_key", base64.StdEncoding.EncodeToString(priv))
}

// housekeeping：每小时记录已见时间（防时钟回拨）、清理 7 天前的分钟指标。
func housekeeping(ctx context.Context, st *store.Store, lic *licsvc.Service) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			lic.Tick(now)
			st.DB.Where("minute < ?", now.Add(-7*24*time.Hour).Unix()).Delete(&store.MetricMinute{})
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
