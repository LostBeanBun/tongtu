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
