// syapp-console：EdgeTraffic OS Web 控制台（规格书第 7 节）。
//
// 骨架阶段只提供 /healthz 与 /api/v1/version，用于打通构建与部署。
// P3 起切换到 Gin，并内嵌 web/dist 前端。
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/xiaoguan521/edgetraffic-os/internal/version"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8443", "HTTP 监听地址（生产环境前置 TLS）")
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()
	if *showVersion {
		println(version.String("console"))
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"product": version.Product, "version": version.Version,
			"commit": version.Commit, "build_date": version.BuildDate,
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("syapp-console starting", "listen", *addr, "version", version.Version)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
