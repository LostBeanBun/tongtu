// Package rules 定义可加速服务的域名白名单规则。
package rules

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed data/steam.json
var steamJSON []byte

//go:embed data/github.json
var githubJSON []byte

// Service 表示一组可加速的网络服务（如 Steam、GitHub）。
type Service struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Domains []string `json:"domains"` // 代理白名单，支持 "*.example.com" 通配（含裸域）
	Hosts   []string `json:"hosts"`   // hosts 模式需要精确覆盖的主机名
}

var builtin = map[string][]byte{
	"steam":  steamJSON,
	"github": githubJSON,
}

// Set 是已启用服务的匹配集合。
type Set struct {
	services []Service
	custom   []string // 自定义域名（来自配置 custom.domains / custom.mitm_domains）
}

// Load 按 id 列表加载内置规则。
func Load(ids []string) (*Set, error) {
	set := &Set{}
	for _, id := range ids {
		raw, ok := builtin[id]
		if !ok {
			return nil, fmt.Errorf("未知服务组 %q，可用: %s", id, strings.Join(BuiltinIDs(), ", "))
		}
		var svc Service
		if err := json.Unmarshal(raw, &svc); err != nil {
			return nil, fmt.Errorf("解析内置规则 %s 失败: %w", id, err)
		}
		set.services = append(set.services, svc)
	}
	if len(set.services) == 0 {
		return nil, fmt.Errorf("未启用任何服务组")
	}
	return set, nil
}

// BuiltinIDs 返回全部内置服务组 id（升序）。
func BuiltinIDs() []string {
	ids := make([]string, 0, len(builtin))
	for id := range builtin {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AddPatterns 追加自定义域名模式（支持 "*.example.com"，含裸域），用于「加速自己的站点」。
//
// 与内置服务组的区别：这些域名不进服务组菜单、也不会被默认值合并覆盖；
// 重复添加是幂等的（同一个模式只保留一份）。
func (s *Set) AddPatterns(patterns []string) {
	have := make(map[string]bool, len(s.custom))
	for _, p := range s.custom {
		have[normalize(p)] = true
	}
	for _, raw := range patterns {
		p := normalize(raw)
		if p == "" || have[p] {
			continue
		}
		have[p] = true
		s.custom = append(s.custom, p)
	}
}

// CustomPatterns 返回自定义模式的副本（界面展示与测试用）。
func (s *Set) CustomPatterns() []string {
	return append([]string(nil), s.custom...)
}

// Services 返回已启用服务的副本。
func (s *Set) Services() []Service {
	return append([]Service(nil), s.services...)
}

// Name 返回内置服务组的显示名（未知 id 或解析失败时返回 id 本身）。
// 界面要给用户看「加速 Steam」这样的名字，而不是内部的 id。
func Name(id string) string {
	raw, ok := builtin[id]
	if !ok {
		return id
	}
	var svc Service
	if err := json.Unmarshal(raw, &svc); err != nil || svc.Name == "" {
		return id
	}
	return svc.Name
}

// Match 判断 host（不含端口）是否命中白名单。
func (s *Set) Match(host string) bool {
	host = normalize(host)
	if host == "" {
		return false
	}
	for i := range s.services {
		for _, pattern := range s.services[i].Domains {
			if matchPattern(host, pattern) {
				return true
			}
		}
	}
	for _, pattern := range s.custom {
		if matchPattern(host, pattern) {
			return true
		}
	}
	return false
}

// MatchPatterns 判断 host（不含端口）是否命中任一通配模式（支持 "*.example.com"）。
func MatchPatterns(host string, patterns []string) bool {
	host = normalize(host)
	if host == "" {
		return false
	}
	for _, pattern := range patterns {
		if matchPattern(host, pattern) {
			return true
		}
	}
	return false
}

// Hosts 返回去重后的 hosts 覆盖名单（保持规则内顺序）。
//
// 自定义域名的精确主机名也会进来（hosts 模式才能覆盖它们）；
// 通配模式无法枚举，只能靠系统代理模式生效——这一点在文档里写清楚。
func (s *Set) Hosts() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(h string) {
		h = normalize(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for i := range s.services {
		for _, h := range s.services[i].Hosts {
			add(h)
		}
	}
	for _, p := range s.custom {
		if !strings.Contains(p, "*") {
			add(p)
		}
	}
	return out
}

func normalize(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

func matchPattern(host, pattern string) bool {
	pattern = normalize(pattern)
	if strings.HasPrefix(pattern, "*.") {
		base := pattern[2:]
		return host == base || strings.HasSuffix(host, "."+base)
	}
	return host == pattern
}
