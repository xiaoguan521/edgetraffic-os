package render

import (
	"errors"
	"fmt"
	"sort"
)

// 四层代理（规格书 7.3 / P9，授权功能 stream）：TCP / UDP 端口转发，可选按 TLS SNI 分流（ssl_preread，不解密）。

// StreamProxy 是一个四层代理服务。
type StreamProxy struct {
	Name      string           `json:"name"`
	Listen    int              `json:"listen"`   // 监听端口
	Protocol  string           `json:"protocol"` // tcp | udp
	Servers   []UpstreamServer `json:"servers"`  // 默认后端
	Algorithm string           `json:"algorithm"`
	// SNIRoutes 按 TLS SNI 分流到不同后端（仅 tcp）；未匹配的走 Servers
	SNIRoutes      []SNIRoute `json:"sni_routes,omitempty"`
	ConnectTimeout int        `json:"connect_timeout"` // 秒，默认 5
	IdleTimeout    int        `json:"idle_timeout"`    // 秒，默认 600
	ProxyProtocol  bool       `json:"proxy_protocol"`  // 向后端发送 PROXY protocol 头
}

// SNIRoute 是一条 SNI 分流规则。
type SNIRoute struct {
	Domain  string           `json:"domain"`
	Servers []UpstreamServer `json:"servers"`
}

// 这些端口由平台使用，四层代理不能占用。
var reservedPorts = map[int]bool{80: true, 443: true, 8081: true, 22: true}

var ErrStream = errors.New("invalid stream proxy")

func (in *Input) validateStreams() error {
	names := map[string]bool{}
	ports := map[string]bool{}
	for i := range in.Streams {
		s := &in.Streams[i]
		if err := ValidateName(s.Name); err != nil {
			return fmt.Errorf("stream: %w", err)
		}
		if names[s.Name] {
			return fmt.Errorf("%w: %q defined twice", ErrStream, s.Name)
		}
		names[s.Name] = true
		if s.Protocol == "" {
			s.Protocol = "tcp"
		}
		if s.Protocol != "tcp" && s.Protocol != "udp" {
			return fmt.Errorf("%w: %s: protocol must be tcp or udp", ErrStream, s.Name)
		}
		if s.Listen < 1 || s.Listen > 65535 || reservedPorts[s.Listen] {
			return fmt.Errorf("%w: %s: port %d not allowed (1–65535, not 22/80/443/8081)", ErrStream, s.Name, s.Listen)
		}
		key := fmt.Sprintf("%s/%d", s.Protocol, s.Listen)
		if ports[key] {
			return fmt.Errorf("%w: port %s used twice", ErrStream, key)
		}
		ports[key] = true
		switch s.Algorithm {
		case "", "round_robin", "least_conn", "hash":
		default:
			return fmt.Errorf("%w: %s: unknown algorithm %q", ErrStream, s.Name, s.Algorithm)
		}
		if s.ConnectTimeout < 0 || s.ConnectTimeout > 60 || s.IdleTimeout < 0 || s.IdleTimeout > 86400 {
			return fmt.Errorf("%w: %s: timeout out of range", ErrStream, s.Name)
		}
		if s.ConnectTimeout == 0 {
			s.ConnectTimeout = 5
		}
		if s.IdleTimeout == 0 {
			s.IdleTimeout = 600
		}
		check := func(list []UpstreamServer) error {
			if len(list) == 0 || len(list) > 64 {
				return fmt.Errorf("%w: %s: 1–64 servers required", ErrStream, s.Name)
			}
			for _, sv := range list {
				if err := ValidateUpstream(sv.Addr, in.AllowedNet); err != nil {
					return fmt.Errorf("stream %s: %w", s.Name, err)
				}
				if sv.Weight < 0 || sv.Weight > 100 {
					return fmt.Errorf("%w: %s: weight out of range", ErrStream, s.Name)
				}
			}
			return nil
		}
		if err := check(s.Servers); err != nil {
			return err
		}
		if len(s.SNIRoutes) > 0 && s.Protocol != "tcp" {
			return fmt.Errorf("%w: %s: SNI routing requires tcp", ErrStream, s.Name)
		}
		seen := map[string]bool{}
		for _, r := range s.SNIRoutes {
			if err := ValidateDomain(r.Domain); err != nil {
				return fmt.Errorf("stream %s: %w", s.Name, err)
			}
			if seen[r.Domain] {
				return fmt.Errorf("%w: %s: SNI %s twice", ErrStream, s.Name, r.Domain)
			}
			seen[r.Domain] = true
			if err := check(r.Servers); err != nil {
				return err
			}
		}
	}
	sort.Slice(in.Streams, func(i, j int) bool { return in.Streams[i].Name < in.Streams[j].Name })
	return nil
}
