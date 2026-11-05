package firewall

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xiaoguan521/edgetraffic-os/internal/layout"
)

// 同机多实例（设计 0001 第 5.8 节）：防火墙放行的端口来自本机全部实例当前生效的配置，
// 不再写死 80/443。实例的端口档案、四层代理端口都体现在配置的 listen 指令中，按配置计算可以与实际监听保持一致。

// Ports 是对所有来源开放的端口。
type Ports struct {
	TCP []int
	UDP []int
}

// InstanceConfs 返回本机（system 模式）全部实例当前生效的主配置文件：默认实例，加上 instances/ 下已创建的具名实例。
func InstanceConfs() []string {
	return instanceConfs(layout.System(""), layout.System)
}

func instanceConfs(def layout.Layout, inst func(string) layout.Layout) []string {
	out := []string{def.CurrentConf()}
	ents, err := os.ReadDir(def.InstancesDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		if !e.IsDir() || !layout.ValidInstance(e.Name()) {
			continue
		}
		l := inst(e.Name())
		if _, err := os.Stat(l.InstanceFile()); err == nil {
			out = append(out, l.CurrentConf())
		}
	}
	return out
}

// ListenPorts 扫描主配置文件及同一版本目录下 conf.d/、stream.d/ 中的 listen 指令，返回对外监听的端口。
// 只监听本机回环地址的端口（状态页）不放行；带 udp（四层代理）或 quic（HTTP/3）的为 UDP 端口。
// 一个实例都读不到时返回 ok=false，调用方回到默认的 80/443。
func ListenPorts(mainConfs []string) (p Ports, ok bool) {
	tcp, udp := map[int]bool{}, map[int]bool{}
	for _, main := range mainConfs {
		files := []string{main}
		dir := filepath.Dir(main)
		for _, sub := range []string{"conf.d", "stream.d"} {
			m, _ := filepath.Glob(filepath.Join(dir, sub, "*.conf"))
			files = append(files, m...)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if f == main {
				ok = true
			}
			for _, l := range parseListens(string(b)) {
				if l.udp {
					udp[l.port] = true
				} else {
					tcp[l.port] = true
				}
			}
		}
	}
	return Ports{TCP: keys(tcp), UDP: keys(udp)}, ok
}

type listen struct {
	port int
	udp  bool
}

func parseListens(conf string) []listen {
	var out []listen
	var b strings.Builder
	for _, line := range strings.Split(conf, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, stmt := range strings.FieldsFunc(b.String(), func(r rune) bool { return r == ';' || r == '{' || r == '}' }) {
		f := strings.Fields(stmt)
		if len(f) < 2 || f[0] != "listen" {
			continue
		}
		port, public := listenPort(f[1])
		if port == 0 || !public {
			continue
		}
		l := listen{port: port}
		for _, o := range f[2:] {
			if o == "udp" || o == "quic" {
				l.udp = true
			}
		}
		out = append(out, l)
	}
	return out
}

// listenPort 解析 listen 的地址部分：8080、0.0.0.0:8080、[::]:8080、127.0.0.1:8081（回环，不对外）、unix:...（不是端口）。
func listenPort(a string) (int, bool) {
	if strings.HasPrefix(a, "unix:") {
		return 0, false
	}
	host, port := "", a
	if i := strings.LastIndexByte(a, ':'); i >= 0 && !strings.HasSuffix(a, "]") {
		host, port = a[:i], a[i+1:]
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	host = strings.Trim(host, "[]")
	loop := host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
	return n, !loop
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
