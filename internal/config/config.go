// Package config 负责 tongtu 配置文件的加载、默认值与持久化。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentVersion 是配置文件版本：低于该版本的配置会被备份并按新默认值重建。
const CurrentVersion = 4

// DNSConfig 控制 DoH 解析行为。
type DNSConfig struct {
	Endpoints   []string `yaml:"endpoints"`
	TimeoutSec  int      `yaml:"timeout_sec"`
	CacheMinSec int      `yaml:"cache_min_sec"`
	CacheMaxSec int      `yaml:"cache_max_sec"`
	// RemoteHosts 是远程优选 IP 列表（hosts 格式）来源，作为 DoH 之外的候选。
	// 用于对抗 DNS 污染：社区实测可用的 IP 往往优于被污染/绕远的 DoH 结果。
	RemoteHosts []string `yaml:"remote_hosts"`
	// StaticHosts 是静态优选 IP（host -> IP 列表），优先级最高。用于远程列表
	// 覆盖不到、或 DoH 稳定返回不可用 IP 的域名（如 gist.github.com）。
	StaticHosts map[string][]string `yaml:"static_hosts"`
}

// MITMConfig 控制「无 SNI 反代」（本地解密 + 出站剥离 SNI）行为。
type MITMConfig struct {
	Enabled  bool     `yaml:"enabled"`   // 是否启用无 SNI 反代
	AutoCert bool     `yaml:"auto_cert"` // 首次启动时提示安装本地根证书
	Domains  []string `yaml:"domains"`   // 需要反代的域名（支持 "*.example.com" 通配）
}

// LogConfig 控制日志输出。
type LogConfig struct {
	Level string `yaml:"level"` // debug / info / warn / error
	File  string `yaml:"file"`  // 空表示仅输出到控制台
}

// Config 是 tongtu 的完整配置。
type Config struct {
	Version         int        `yaml:"version"`               // 配置文件版本
	Listen          string     `yaml:"listen"`                // 本地代理监听地址
	Control         string     `yaml:"control"`               // 控制 API 监听地址（仅回环）
	SystemProxy     bool       `yaml:"system_proxy"`          // 启动时接管系统代理
	Hosts           bool       `yaml:"hosts"`                 // 启动时接管 hosts
	Services        []string   `yaml:"services"`              // 启用的服务组 id
	HostsRefreshMin int        `yaml:"hosts_refresh_minutes"` // hosts IP 刷新周期
	DNS             DNSConfig  `yaml:"dns"`
	MITM            MITMConfig `yaml:"mitm"`
	Log             LogConfig  `yaml:"log"`

	// UpgradedFrom 记录本次加载触发配置升级前的旧版本号（0 表示未升级）。
	UpgradedFrom int `yaml:"-"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Version:         CurrentVersion,
		Listen:          "127.0.0.1:19180",
		Control:         "127.0.0.1:19181",
		SystemProxy:     true,
		Hosts:           true,
		Services:        []string{"steam", "github"},
		HostsRefreshMin: 30,
		DNS: DNSConfig{
			// 境内可达的端点在前（1.1.1.1/8.8.8.8 在境内常被拒连，仅作后备）；
			// 223.5.5.5 的 DoH 路径是 /resolve（旧版误用 /dns-query 导致 400）。
			Endpoints: []string{
				"https://doh.pub/dns-query",
				"https://223.5.5.5/resolve",
				"https://dns.alidns.com/resolve",
				"https://1.1.1.1/dns-query",
				"https://8.8.8.8/dns-query",
			},
			TimeoutSec:  5,
			CacheMinSec: 60,
			CacheMaxSec: 600,
			// GitHub520：社区维护的 GitHub 优选 IP（raw.hellogithub.com 境内可达）。
			RemoteHosts: []string{
				"https://raw.hellogithub.com/hosts",
			},
			// 下列域名的 SNI 被封锁，或 DoH 会稳定返回不可用/被污染的 IP；
			// 静态给出实测可用候选，作为 DoH 全部失败/被污染时的兜底。
			StaticHosts: map[string][]string{
				"gist.github.com":    {"140.82.112.3", "140.82.113.3", "140.82.114.3"},
				"steamcommunity.com": {"104.89.103.51", "184.30.89.225"},
				"github.com":         {"140.82.112.3", "140.82.113.3", "140.82.114.3"},
			},
		},
		MITM: MITMConfig{
			Enabled:  true,
			AutoCert: true,
			Domains: []string{
				"*.steamcommunity.com",
				"raw.githubusercontent.com",
				"gist.github.com",
				"github.com",
			},
		},
		Log: LogConfig{Level: "info"},
	}
}

// Dir 返回配置与状态文件目录（%APPDATA%\tongtu）。
func Dir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "tongtu")
}

// DefaultPath 返回默认配置文件路径。
func DefaultPath() string {
	return filepath.Join(Dir(), "config.yaml")
}

// EnsureDir 创建配置目录。
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0o755)
}

// Load 加载配置。path 为空时使用默认路径；文件不存在时写入并返回默认配置。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if e := EnsureDir(); e == nil {
				_ = Save(path, cfg)
			}
			return cfg, nil
		}
		return nil, err
	}
	// 先解析到零值结构体判断版本（默认值自带 CurrentVersion，直接覆盖会漏判旧文件）
	fileCfg := &Config{}
	if err := yaml.Unmarshal(data, fileCfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	if fileCfg.Version < CurrentVersion {
		// 旧版配置：备份原文件后按新默认值重建（v2 修正了 DoH 端点等关键默认项）
		bak := fmt.Sprintf("%s.v%d.bak", path, fileCfg.Version)
		_ = os.WriteFile(bak, data, 0o644)
		cfg = Default()
		cfg.UpgradedFrom = fileCfg.Version
		if e := EnsureDir(); e == nil {
			_ = Save(path, cfg)
		}
	} else {
		// 当前版本：文件值覆盖默认值（保持缺失字段使用默认）
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return cfg, nil
}

// Save 将配置写入 path。
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Validate 校验并补全配置中的零值。
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen 不能为空")
	}
	if c.Control == "" {
		return fmt.Errorf("control 不能为空")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("services 不能为空")
	}
	if len(c.DNS.Endpoints) == 0 {
		return fmt.Errorf("dns.endpoints 不能为空")
	}
	if len(c.DNS.RemoteHosts) == 0 {
		c.DNS.RemoteHosts = Default().DNS.RemoteHosts
	}
	if len(c.DNS.StaticHosts) == 0 {
		c.DNS.StaticHosts = Default().DNS.StaticHosts
	} else {
		// 合并新增的内置默认项，保留用户已有配置
		for h, ips := range Default().DNS.StaticHosts {
			if _, ok := c.DNS.StaticHosts[h]; !ok {
				c.DNS.StaticHosts[h] = ips
			}
		}
	}
	if c.DNS.TimeoutSec <= 0 {
		c.DNS.TimeoutSec = 5
	}
	if c.DNS.CacheMinSec <= 0 {
		c.DNS.CacheMinSec = 60
	}
	if c.DNS.CacheMaxSec <= c.DNS.CacheMinSec {
		c.DNS.CacheMaxSec = c.DNS.CacheMinSec * 10
	}
	if c.HostsRefreshMin <= 0 {
		c.HostsRefreshMin = 30
	}
	if len(c.MITM.Domains) == 0 {
		c.MITM.Domains = Default().MITM.Domains
	} else {
		// 合并新增的内置默认反代域名，保留用户已有配置
		have := make(map[string]bool, len(c.MITM.Domains))
		for _, d := range c.MITM.Domains {
			have[d] = true
		}
		for _, d := range Default().MITM.Domains {
			if !have[d] {
				c.MITM.Domains = append(c.MITM.Domains, d)
			}
		}
	}
	return nil
}

// DNSTimeout 返回单次 DoH 查询超时。
func (c *Config) DNSTimeout() time.Duration {
	return time.Duration(c.DNS.TimeoutSec) * time.Second
}

// HostsRefresh 返回 hosts 刷新周期。
func (c *Config) HostsRefresh() time.Duration {
	return time.Duration(c.HostsRefreshMin) * time.Minute
}
