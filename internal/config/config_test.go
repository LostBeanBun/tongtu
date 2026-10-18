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

// matchAny 判断 host 是否命中任一通配模式（与 rules.matchPattern 同规则：*.a.com 含裸域 a.com）。
func matchAny(host string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(p, "*.") {
			base := p[2:]
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		if p == host {
			return true
		}
	}
	return false
}

// TestDefaultReverseProxyCoversBlockedHosts 是「必须走无 SNI 反代」名单的契约测试。
//
// 名单来自实测（见 CHANGELOG 0.0.1）：这些域名真 SNI 直连会被阻断，
// 少写一个的后果就是用户打开 Steam 商店/社区一直转圈，所以在这里钉死。
func TestDefaultReverseProxyCoversBlockedHosts(t *testing.T) {
	cfg := Default()
	for _, host := range []string{
		"store.steampowered.com",  // 直连超时/断流，反代实测 200
		"avatars.steamstatic.com", // 直连 RST，反代实测正常
		"steamcommunity.com",      // 通配要覆盖裸域
		"www.steamcommunity.com",
		"help.steamcommunity.com",
		"github.com",
		"raw.githubusercontent.com",
		"gist.github.com",
	} {
		if !matchAny(host, cfg.MITM.Domains) {
			t.Errorf("默认反代名单缺少 %s", host)
		}
	}
	// 反向断言：绝不能写成 *.steampowered.com。
	// login.steampowered.com 剥离 SNI 后握手直接失败（实测 tls: internal error），
	// 用通配会把本来正常的登录页一起弄坏。
	if matchAny("login.steampowered.com", cfg.MITM.Domains) {
		t.Error("默认反代名单不该把 login.steampowered.com 卷进来（别写 *.steampowered.com）")
	}
	if matchAny("checkout.steampowered.com", cfg.MITM.Domains) {
		t.Error("checkout.steampowered.com 直连可用，不该反代")
	}
	// 兜底 IP：这几个域名的干净 DoH 答案不可达，没有兜底就只有超时。
	for _, host := range []string{
		"steamcommunity.com",
		"www.steamcommunity.com",
		"help.steamcommunity.com",
		"github.com",
		"gist.github.com",
	} {
		if len(cfg.DNS.StaticHosts[host]) == 0 {
			t.Errorf("默认兜底 IP 缺少 %s", host)
		}
	}
}
