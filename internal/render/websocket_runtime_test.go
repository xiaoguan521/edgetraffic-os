//go:build linux

package render

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWebSocketRealSyedge checks the rendered route using an actual protocol
// upgrade and frames in both directions, not just matching configuration text.
func TestWebSocketRealSyedge(t *testing.T) {
	bin := os.Getenv("SYEDGE_BIN")
	if bin == "" {
		bin = "/opt/syedge/sbin/syedge"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("syedge binary not installed")
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var ip netip.Addr
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() && !p.Addr().IsLinkLocalUnicast() {
			ip = p.Addr()
			break
		}
	}
	if !ip.IsValid() {
		t.Fatal("real WebSocket test requires a non-loopback IPv4 address")
	}
	backend, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	backendResult := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendResult <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		req, err := http.ReadRequest(r)
		if err != nil {
			backendResult <- err
			return
		}
		if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || !strings.EqualFold(req.Header.Get("Connection"), "upgrade") || req.ProtoMajor != 1 || req.ProtoMinor != 1 {
			backendResult <- fmt.Errorf("upgrade headers or HTTP version were not forwarded")
			return
		}
		_, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		if err == nil {
			_, err = conn.Write([]byte{0x81, 4, 'p', 'i', 'n', 'g'})
		}
		if err == nil {
			frame := make([]byte, 10)
			_, err = io.ReadFull(r, frame)
			if err == nil && !bytes.Equal(frame, []byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'o' ^ 2, 'n' ^ 3, 'g' ^ 4}) {
				err = fmt.Errorf("client frame changed while proxying")
			}
		}
		backendResult <- err
	}()
	var reservations []net.Listener
	var ports []int
	for range 3 {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		reservations = append(reservations, l)
		defer func() { _ = l.Close() }()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	root := t.TempDir()
	for _, dir := range []string{"run", "logs", "cache"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	in := Input{
		Revision: "r000001", MimeTypes: DefaultMimeTypes(), AllowedNet: []netip.Prefix{netip.PrefixFrom(ip, 32)},
		Profile:   Profile{Mode: "user", Root: root, HTTPPort: ports[0], HTTPSPort: ports[1], StatusPort: ports[2], NoFile: 4096},
		Upstreams: []UpstreamGroup{{Name: "backend", Servers: []UpstreamServer{{Addr: backend.Addr().String()}}}},
		Sites:     []Site{{Name: "socket", Domains: []string{"socket.example.com"}, Routes: []Route{{Path: "/socket", Match: "prefix", Mode: "proxy", Proxy: &ProxySpec{Upstream: "backend", WebSocket: true}}}}},
	}
	bundle, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "conf", "releases", in.Revision)
	if err := bundle.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, l := range reservations {
		_ = l.Close()
	}
	logPath := filepath.Join(root, "process.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(bin, "-c", filepath.Join(dir, "syedge.conf"), "-g", "daemon off;")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	var conn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", ports[0]), 250*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("syedge did not start: %v\n%s", err, output)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: socket.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade failed: response=%v error=%v", resp, err)
	}
	frame := make([]byte, 6)
	if _, err := io.ReadFull(r, frame); err != nil || !bytes.Equal(frame, []byte{0x81, 4, 'p', 'i', 'n', 'g'}) {
		t.Fatalf("server frame missing: %v %v", frame, err)
	}
	if _, err := conn.Write([]byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'o' ^ 2, 'n' ^ 3, 'g' ^ 4}); err != nil {
		t.Fatal(err)
	}
	if err := <-backendResult; err != nil {
		t.Fatal(err)
	}
}
