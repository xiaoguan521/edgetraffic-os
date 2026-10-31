// demo-backend：演示用的上游业务服务。返回实例名与请求信息，便于观察负载均衡与回源。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	name := os.Getenv("INSTANCE")
	if name == "" {
		name, _ = os.Hostname()
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":9001"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":    "order-api",
			"instance":   name,
			"path":       r.URL.Path,
			"client_ip":  r.Header.Get("X-Real-IP"),
			"request_id": r.Header.Get("X-Request-ID"),
			"time":       time.Now().Format(time.RFC3339),
		})
	})
	// /stream?seconds=N：每秒输出一行、持续 N 秒（最多 600），用于验证升级 / reload 期间长连接不断
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
		if n < 1 || n > 600 {
			n = 10
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fl, _ := w.(http.Flusher)
		for i := 1; i <= n; i++ {
			if _, err := fmt.Fprintf(w, "%s line %d/%d\n", name, i, n); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			if i < n {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	})
	log.Printf("demo-backend %s listening on %s", name, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
