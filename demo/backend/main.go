// demo-backend：演示用的上游业务服务。返回实例名与请求信息，便于观察负载均衡与回源。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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
	log.Printf("demo-backend %s listening on %s", name, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
