package config

import (
	"os"
	"strconv"
	"time"
)

// Config 将部署参数集中管理，业务层不直接读取环境变量。
type Config struct {
	ListenAddr       string
	DataDir          string
	OpenRestyBin     string
	SessionTTL       time.Duration
	MDNSProbeTimeout time.Duration
	GatewayIPv4      string
}

func Load() Config {
	return Config{
		ListenAddr:       get("GATEWAY_LISTEN", ":8080"),
		DataDir:          get("GATEWAY_DATA_DIR", "/data"),
		OpenRestyBin:     get("GATEWAY_OPENRESTY_BIN", "openresty"),
		SessionTTL:       seconds("GATEWAY_SESSION_TTL_SECONDS", 12*3600),
		MDNSProbeTimeout: seconds("GATEWAY_MDNS_PROBE_SECONDS", 2),
		GatewayIPv4:      os.Getenv("GATEWAY_IPV4"),
	}
}

func get(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func seconds(key string, fallback int) time.Duration {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil && value > 0 {
		return time.Duration(value) * time.Second
	}
	return time.Duration(fallback) * time.Second
}
