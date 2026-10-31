// etos-loadgen 是验收测试用的 HTTP 压测客户端（不随产品分发）：keep-alive 长连接、每个请求一个随机客户端地址
// （X-Forwarded-For），统计“客户端看到的”结果。它使用 Go 标准库的 HTTP 客户端，行为与浏览器等常见客户端一致：
// 复用的空闲连接恰好被服务端关闭时，对幂等请求自动重试一次（RFC 9112 9.3.1）。
//
//	etos-loadgen -url http://172.30.0.101/api/load -host shop.scale.local -c 8 -d 120s
//
// 输出一行：requests=<总数> ok=<2xx> non2xx=<非 2xx> errors=<失败> retried_conns=<重连次数>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "", "目标 URL")
	host := flag.String("host", "", "Host 头")
	conc := flag.Int("c", 8, "并发连接数")
	dur := flag.Duration("d", 30*time.Second, "持续时间")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "usage: etos-loadgen -url URL [-host H] [-c N] [-d D]")
		os.Exit(2)
	}
	var dials atomic.Int64
	d := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: *conc,
		MaxConnsPerHost:     *conc,
		IdleConnTimeout:     90 * time.Second,
		DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
			dials.Add(1)
			return d.DialContext(ctx, n, a)
		},
	}
	cl := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	var total, ok, non2xx, errs atomic.Int64
	var mu sync.Mutex
	var firstErrs []string
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				req, _ := http.NewRequest(http.MethodGet, *url, nil)
				if *host != "" {
					req.Host = *host
				}
				req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", 100+rand.IntN(100), rand.IntN(256), 1+rand.IntN(254)))
				total.Add(1)
				resp, err := cl.Do(req)
				if err != nil {
					errs.Add(1)
					mu.Lock()
					if len(firstErrs) < 3 {
						firstErrs = append(firstErrs, err.Error())
					}
					mu.Unlock()
					continue
				}
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				switch {
				case err != nil:
					errs.Add(1)
				case resp.StatusCode/100 == 2:
					ok.Add(1)
				default:
					non2xx.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	fmt.Printf("requests=%d ok=%d non2xx=%d errors=%d conns=%d\n", total.Load(), ok.Load(), non2xx.Load(), errs.Load(), dials.Load())
	for _, e := range firstErrs {
		fmt.Println("error:", e)
	}
}
