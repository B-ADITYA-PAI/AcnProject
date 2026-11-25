package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Switch  SwitchConfig  `yaml:"switch"`
	Regions []Region      `yaml:"regions"`
	Traffic TrafficConfig `yaml:"traffic"`
}

type ServerConfig struct {
	ID       string `yaml:"id"`
	TCPPort  int    `yaml:"tcp_port"`
	UDPPort  int    `yaml:"udp_port"`
	HTTPPort int    `yaml:"http_port"`
}

type SwitchConfig struct {
	ClientIP       string        `yaml:"client_ip"`
	ServerIP       string        `yaml:"server_ip"`
	TCPLossPenalty time.Duration `yaml:"tcp_loss_penalty"`
}

type Region struct {
	Name    string        `yaml:"name"`
	CIDR    string        `yaml:"cidr"`
	Clients int           `yaml:"clients"`
	Delay   time.Duration `yaml:"delay"`
	Jitter  time.Duration `yaml:"jitter"`
	Loss    float64       `yaml:"loss"`

	prefix netip.Prefix `yaml:"-"`
}

func (r *Region) Prefix() netip.Prefix {
	return r.prefix
}

type TrafficConfig struct {
	Duration      time.Duration   `yaml:"duration"`
	RatePerRegion float64         `yaml:"rate_per_region"`
	Keys          int             `yaml:"keys"`
	ZipfAlpha     float64         `yaml:"zipf_alpha"`
	Mix           map[string]float64 `yaml:"mix"`
	Timeout       time.Duration   `yaml:"timeout"`
	Seed          int64           `yaml:"seed"`
	Output        string          `yaml:"output"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}

	applyEnvOverrides(&c)

	for i := range c.Regions {
		prefix, err := netip.ParsePrefix(c.Regions[i].CIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q for region %q: %v", c.Regions[i].CIDR, c.Regions[i].Name, err)
		}
		c.Regions[i].prefix = prefix
	}

	return &c, nil
}

func (c *Config) Lookup(ip netip.Addr) *Region {
	for i := range c.Regions {
		if c.Regions[i].prefix.Contains(ip) {
			return &c.Regions[i]
		}
	}
	return nil
}

// applyEnvOverrides lets the experiment runner change the traffic settings per
// run without editing config.yaml: ZIPF_ALPHA, DURATION (e.g. "60s"), SEED and
// RATE_PER_REGION.
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("ZIPF_ALPHA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Traffic.ZipfAlpha = f
		}
	}
	if v := os.Getenv("DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Traffic.Duration = d
		}
	}
	if v := os.Getenv("SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Traffic.Seed = n
		}
	}
	if v := os.Getenv("RATE_PER_REGION"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Traffic.RatePerRegion = f
		}
	}
}
