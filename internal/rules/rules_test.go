package rules

import "testing"

func TestMatch(t *testing.T) {
	set, err := Load([]string{"steam", "github"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		host string
		want bool
	}{
		{"steamcommunity.com", true},
		{"www.steamcommunity.com", true},
		{"login.steamcommunity.com", true},
		{"store.steampowered.com", true},
		{"api.steampowered.com", true},
		{"cdn.steamstatic.com", true},
		{"community.fastly.steamstatic.com", true},
		{"github.com", true},
		{"api.github.com", true},
		{"gist.github.com", true},
		{"codeload.github.com", true},
		{"raw.githubusercontent.com", true},
		{"avatars.githubusercontent.com", true},
		{"private-user-images.githubusercontent.com", true},
		{"github.githubassets.com", true},
		{"user.github.io", true},
		{"STEAMCOMMUNITY.COM", true},  // 大小写不敏感
		{"steamcommunity.com.", true}, // 尾部点
		{"example.com", false},
		{"notsteamcommunity.com", false},     // 不应误伤同尾缀域名
		{"steampowered.com.evil.com", false}, // 后缀伪装
		{"steamcommunity.com.evil.com", false},
		{"evil.com/steamcommunity.com", false},
	}
	for _, c := range cases {
		if got := set.Match(c.host); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestHostsList(t *testing.T) {
	set, err := Load([]string{"steam", "github"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hosts := set.Hosts()
	if len(hosts) == 0 {
		t.Fatal("Hosts() 为空")
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h] {
			t.Errorf("重复条目: %s", h)
		}
		seen[h] = true
	}
	for _, want := range []string{"steamcommunity.com", "github.com", "raw.githubusercontent.com"} {
		if !seen[want] {
			t.Errorf("缺少 %s", want)
		}
	}
}

func TestLoadUnknownService(t *testing.T) {
	if _, err := Load([]string{"nope"}); err == nil {
		t.Fatal("未知服务组应报错")
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("空服务组应报错")
	}
}

// TestHostsCoverSteamStoreHosts 提醒：hosts 名单要覆盖到「必须走反代」的主机。
//
// 反代域名在 hosts 模式下会写 127.0.0.1（见 app.mitmLocalNamesFor），
// 名单里少了某个主机，Steam 客户端走 hosts 时就绕不过 SNI 封锁。
func TestHostsCoverSteamStoreHosts(t *testing.T) {
	set, err := Load([]string{"steam"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hosts := make(map[string]bool)
	for _, h := range set.Hosts() {
		hosts[h] = true
	}
	for _, want := range []string{
		"store.steampowered.com",  // 商店：直连被 SNI 阻断，走反代
		"avatars.steamstatic.com", // 头像：直连 RST，走反代
		"steamcommunity.com",
		"www.steamcommunity.com",
		"help.steamcommunity.com",
	} {
		if !hosts[want] {
			t.Errorf("hosts 名单缺少 %s", want)
		}
	}
}

// TestAddPatterns 自定义站点：精确域名与通配都要能命中；重复添加是幂等的。
func TestAddPatterns(t *testing.T) {
	set, err := Load([]string{"github"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	set.AddPatterns([]string{"My.Site.com", "*.example.com", "my.site.com", ""})
	set.AddPatterns([]string{"*.example.com"}) // 重复添加不应产生第二份

	if got := set.CustomPatterns(); len(got) != 2 || got[0] != "my.site.com" || got[1] != "*.example.com" {
		t.Fatalf("自定义模式未规范化/去重: %v", got)
	}
	cases := map[string]bool{
		"my.site.com":      true,
		"MY.SITE.COM":      true, // 大小写不敏感
		"api.my.site.com":  false,
		"a.example.com":    true, // 通配含裸域
		"example.com":      true,
		"b.a.example.com":  true,
		"notexample.com":   false, // 后缀伪装
		"example.com.evil": false,
		"github.com":       true, // 内置规则不受影响
	}
	for host, want := range cases {
		if got := set.Match(host); got != want {
			t.Errorf("Match(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestCustomHosts 精确主机名进 hosts 覆盖；通配无法枚举，只靠系统代理模式（不进 hosts）。
func TestCustomHosts(t *testing.T) {
	set, err := Load([]string{"github"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	set.AddPatterns([]string{"my.site.com", "*.example.com"})
	hosts := make(map[string]bool)
	for _, h := range set.Hosts() {
		hosts[h] = true
	}
	if !hosts["my.site.com"] {
		t.Error("hosts 名单应包含自定义的精确域名")
	}
	if hosts["*.example.com"] || hosts["example.com"] {
		t.Error("通配模式不该进 hosts（无法枚举，会写出一个无意义的主机名）")
	}
	if !hosts["github.com"] {
		t.Error("内置服务组的 hosts 不应被自定义规则顶掉")
	}
}
