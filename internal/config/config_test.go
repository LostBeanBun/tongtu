package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLoadUpgradesOldConfig 旧版（无 version 字段）配置应被备份并按新默认值重建。
func TestLoadUpgradesOldConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	old := "listen: 127.0.0.1:9999\n" +
		"log:\n  level: debug\n" +
		"dns:\n  endpoints: ['https://1.1.1.1/dns-query', 'https://223.5.5.5/dns-query']\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("版本未升级: %d", cfg.Version)
	}
	if cfg.UpgradedFrom != 0 {
		t.Fatalf("UpgradedFrom 应为 0，实际 %d", cfg.UpgradedFrom)
	}
	for _, ep := range cfg.DNS.Endpoints {
		if strings.HasSuffix(ep, "223.5.5.5/dns-query") {
			t.Fatalf("旧的错误端点未被替换: %s", ep)
		}
	}
	if !cfg.MITM.Enabled || len(cfg.MITM.Domains) == 0 {
		t.Fatalf("MITM 默认配置缺失: %+v", cfg.MITM)
	}
	if _, err := os.Stat(path + ".v0.bak"); err != nil {
		t.Fatalf("缺少旧配置备份: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "version: "+strconv.Itoa(CurrentVersion)) {
		t.Fatal("新配置未写入版本号")
	}

	// 再次加载不应重复升级
	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("二次 Load: %v", err)
	}
	if cfg2.UpgradedFrom != 0 {
		t.Fatal("不应重复触发升级")
	}
}

// TestDefaultSanity 关键默认值回归保护。
func TestDefaultSanity(t *testing.T) {
	cfg := Default()
	if cfg.DNS.Endpoints[0] != "https://doh.pub/dns-query" {
		t.Fatalf("境内端点应排在首位: %s", cfg.DNS.Endpoints[0])
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
}
