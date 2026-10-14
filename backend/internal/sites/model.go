package sites

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

type Site struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Domain       string `json:"domain"`
	TargetHost   string `json:"targetHost"`
	TargetPort   int    `json:"targetPort"`
	ListenPort   int    `json:"listenPort"`
	Protocol     string `json:"protocol"`
	Enabled      bool   `json:"enabled"`
	ConfigMode   string `json:"configMode"`
	CustomConfig string `json:"customConfig,omitempty"`
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate 限制生成配置中的输入，避免注入额外 OpenResty 指令。
func (s *Site) Validate() error {
	s.Domain = strings.ToLower(strings.TrimSpace(s.Domain))
	parts := strings.Split(s.Domain, ".")
	if len(parts) != 2 || parts[1] != "local" || !label.MatchString(parts[0]) {
		return errors.New("DOMAIN_INVALID")
	}
	if s.Name == "" || len(s.Name) > 100 || s.TargetPort < 1 || s.TargetPort > 65535 {
		return errors.New("CONFIG_INVALID")
	}
	if s.Protocol != "HTTP" && s.Protocol != "WEBSOCKET" && s.Protocol != "SSH" {
		return errors.New("CONFIG_INVALID")
	}
	if s.Protocol == "SSH" {
		if s.ListenPort < 1 || s.ListenPort > 65535 || s.ListenPort == 80 || s.ConfigMode != "GENERATED" {
			return errors.New("CONFIG_INVALID")
		}
	} else if s.ListenPort != 0 {
		return errors.New("CONFIG_INVALID")
	}
	if net.ParseIP(s.TargetHost) == nil && !validTargetHost(s.TargetHost) {
		return errors.New("CONFIG_INVALID")
	}
	if s.ConfigMode != "GENERATED" && s.ConfigMode != "CUSTOM" {
		return errors.New("CONFIG_INVALID")
	}
	if s.ConfigMode == "CUSTOM" && strings.TrimSpace(s.CustomConfig) == "" {
		return errors.New("CONFIG_INVALID")
	}
	return nil
}

func validTargetHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}
