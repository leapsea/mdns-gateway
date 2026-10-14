package mdns

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"mdns-gateway/internal/sites"
)

const multicastAddr = "224.0.0.251:5353"

type Service struct {
	ip        net.IP
	iface     *net.Interface
	timeout   time.Duration
	mu        sync.RWMutex
	domains   map[string]bool
	conflicts map[string]bool
	waiters   map[string][]chan bool
	conn      *net.UDPConn
}

func New(ipString string, timeout time.Duration) (*Service, error) {
	ip := net.ParseIP(ipString).To4()
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var selected *net.Interface
	for i := range interfaces {
		if interfaces[i].Flags&net.FlagUp == 0 || interfaces[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := interfaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			if ip == nil || n.IP.Equal(ip) {
				ip = n.IP.To4()
				selected = &interfaces[i]
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		return nil, errors.New("no matching gateway IPv4 interface")
	}
	slog.Info("mdns interface selected", "interface", selected.Name, "ipv4", ip.String())
	return &Service{ip: ip, iface: selected, timeout: timeout, domains: map[string]bool{}, conflicts: map[string]bool{}, waiters: map[string][]chan bool{}}, nil
}

func (s *Service) Start(ctx context.Context) error {
	addr, _ := net.ResolveUDPAddr("udp4", multicastAddr)
	conn, err := net.ListenMulticastUDP("udp4", s.iface, addr)
	if err != nil {
		return err
	}
	s.conn = conn
	go func() { defer func() { _ = recover() }(); <-ctx.Done(); _ = conn.Close() }()
	go func() { defer func() { _ = recover() }(); s.serve(ctx, conn) }()
	go func() { defer func() { _ = recover() }(); s.monitor(ctx) }()
	return nil
}
func (s *Service) Stop() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
func (s *Service) Status() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.conflicts) > 0 {
		return "CONFLICT"
	}
	if s.conn == nil {
		return "STOPPED"
	}
	return "OK"
}
func (s *Service) Conflicts() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]bool{}
	for k, v := range s.conflicts {
		out[k] = v
	}
	return out
}

func (s *Service) Replace(ctx context.Context, state []sites.Site) error {
	next := map[string]bool{}
	for _, v := range state {
		if v.Enabled {
			next[v.Domain] = true
		}
	}
	s.mu.Lock()
	previous := s.domains
	s.domains = next
	for domain := range s.conflicts {
		if !next[domain] {
			delete(s.conflicts, domain)
		}
	}
	s.mu.Unlock()
	if s.conn != nil {
		dst, _ := net.ResolveUDPAddr("udp4", multicastAddr)
		for domain := range previous {
			if !next[domain] {
				_, _ = s.conn.WriteToUDP(responseTTL(domain, s.ip, 0), dst)
				slog.Info("mdns unregister", "domain", domain)
			}
		}
		for domain := range next {
			if !previous[domain] {
				_, _ = s.conn.WriteToUDP(responseTTL(domain, s.ip, 120), dst)
				slog.Info("mdns register", "domain", domain)
			}
		}
	}
	slog.Info("mdns records replaced", "count", len(next))
	return nil
}

// Probe 查询 A 记录并排除本机地址；无响应仅表示当前未发现冲突。
func (s *Service) Probe(ctx context.Context, domain string) (bool, error) {
	dst, _ := net.ResolveUDPAddr("udp4", multicastAddr)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: s.ip})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.waiters[domain] = append(s.waiters[domain], ch)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		list := s.waiters[domain]
		for i, entry := range list {
			if entry == ch {
				s.waiters[domain] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	query := question(domain)
	if _, err := conn.WriteToUDP(query, dst); err != nil {
		return false, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
	go func() {
		defer func() { _ = recover() }()
		buffer := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			for _, a := range answers(buffer[:n]) {
				if a.name == domain && !a.ip.Equal(s.ip) {
					select {
					case ch <- true:
					default:
					}
					return
				}
			}
		}
	}()
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case <-ch:
		slog.Warn("mdns conflict discovered", "domain", domain)
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (s *Service) monitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			domains := make([]string, 0, len(s.domains))
			for d := range s.domains {
				domains = append(domains, d)
			}
			s.mu.RUnlock()
			for _, d := range domains {
				conflict, err := s.Probe(ctx, d)
				if err != nil {
					continue
				}
				s.mu.Lock()
				wasConflict := s.conflicts[d]
				if conflict {
					s.conflicts[d] = true
				} else {
					delete(s.conflicts, d)
				}
				s.mu.Unlock()
				if wasConflict && !conflict && s.conn != nil {
					dst, _ := net.ResolveUDPAddr("udp4", multicastAddr)
					_, _ = s.conn.WriteToUDP(responseTTL(d, s.ip, 120), dst)
					slog.Info("mdns conflict cleared", "domain", d)
				}
			}
		}
	}
}

func (s *Service) serve(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 1500)
	dst, _ := net.ResolveUDPAddr("udp4", multicastAddr)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		for _, q := range questions(buf[:n]) {
			s.mu.RLock()
			active := s.domains[q] && !s.conflicts[q]
			s.mu.RUnlock()
			if active {
				_, _ = conn.WriteToUDP(response(q, s.ip), dst)
			}
		}
		for _, a := range answers(buf[:n]) {
			s.mu.Lock()
			newConflict := false
			if !a.ip.Equal(s.ip) {
				for _, ch := range s.waiters[a.name] {
					select {
					case ch <- true:
					default:
					}
				}
			}
			if s.domains[a.name] && !a.ip.Equal(s.ip) {
				newConflict = !s.conflicts[a.name]
				s.conflicts[a.name] = true
			}
			s.mu.Unlock()
			if newConflict {
				_, _ = conn.WriteToUDP(responseTTL(a.name, s.ip, 0), dst)
				slog.Warn("mdns runtime conflict", "domain", a.name, "remote_ip", a.ip.String())
			}
		}
	}
}

func encodeName(domain string) []byte {
	var out []byte
	for _, p := range strings.Split(domain, ".") {
		out = append(out, byte(len(p)))
		out = append(out, p...)
	}
	return append(out, 0)
}
func question(domain string) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[4:], 1)
	b = append(b, encodeName(domain)...)
	return append(b, 0, 1, 0, 1)
}
func response(domain string, ip net.IP) []byte { return responseTTL(domain, ip, 120) }
func responseTTL(domain string, ip net.IP, ttl uint32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[2:], 0x8400)
	binary.BigEndian.PutUint16(b[6:], 1)
	b = append(b, encodeName(domain)...)
	b = append(b, 0, 1, 0x80, 1, byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl), 0, 4)
	return append(b, ip.To4()...)
}

func readName(packet []byte, offset int) (string, int, bool) {
	var parts []string
	next := offset
	jumped := false
	for hops := 0; hops < 32; hops++ {
		if offset >= len(packet) {
			return "", 0, false
		}
		n := int(packet[offset])
		if n == 0 {
			if !jumped {
				next = offset + 1
			}
			return strings.Join(parts, "."), next, true
		}
		if n&0xc0 == 0xc0 {
			if offset+1 >= len(packet) {
				return "", 0, false
			}
			ptr := int(n&0x3f)<<8 | int(packet[offset+1])
			if !jumped {
				next = offset + 2
			}
			offset = ptr
			jumped = true
			continue
		}
		if n > 63 || offset+1+n > len(packet) {
			return "", 0, false
		}
		parts = append(parts, strings.ToLower(string(packet[offset+1:offset+1+n])))
		offset += 1 + n
		if !jumped {
			next = offset
		}
	}
	return "", 0, false
}
func questions(packet []byte) []string {
	if len(packet) < 12 {
		return nil
	}
	count := int(binary.BigEndian.Uint16(packet[4:]))
	offset := 12
	var out []string
	for i := 0; i < count && i < 100; i++ {
		name, next, ok := readName(packet, offset)
		if !ok || next+4 > len(packet) {
			return out
		}
		if binary.BigEndian.Uint16(packet[next:]) == 1 {
			out = append(out, name)
		}
		offset = next + 4
	}
	return out
}

type answer struct {
	name string
	ip   net.IP
}

func answers(packet []byte) []answer {
	if len(packet) < 12 {
		return nil
	}
	offset := 12
	qn := int(binary.BigEndian.Uint16(packet[4:]))
	an := int(binary.BigEndian.Uint16(packet[6:]))
	var out []answer
	for i := 0; i < qn && i < 100; i++ {
		_, next, ok := readName(packet, offset)
		if !ok || next+4 > len(packet) {
			return out
		}
		offset = next + 4
	}
	for i := 0; i < an && i < 100; i++ {
		name, next, ok := readName(packet, offset)
		if !ok || next+10 > len(packet) {
			return out
		}
		typ := binary.BigEndian.Uint16(packet[next:])
		length := int(binary.BigEndian.Uint16(packet[next+8:]))
		offset = next + 10 + length
		if offset > len(packet) {
			return out
		}
		if typ == 1 && length == 4 {
			out = append(out, answer{name, net.IP(packet[next+10 : offset]).To4()})
		}
	}
	return out
}
