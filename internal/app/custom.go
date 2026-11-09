package app

// custom.go 实现自定义站点的增删（窗口「站点」面板的底层）。
//
// 为什么放在 app 层：改一个域名要同时做三件事——校验写法、写回配置、重建白名单并
// 让新规则立即生效（见 applyRulesLocked）。任何一件漏掉，界面就会显示「已添加」
// 而实际没生效，因此统一收在这里，而不是让界面直接改 config 再自己拼。

import (
	"fmt"
	"strings"

	"tongtu/internal/config"
	"tongtu/internal/rules"
)

// CustomSite 是一条自定义站点（窗口面板的一行）。
type CustomSite struct {
	Pattern string `json:"pattern"`
}

// CustomSites 返回当前全部自定义站点。
func (a *App) CustomSites() []CustomSite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return customSitesOf(a.cfg.Custom)
}

// customSitesOf 把配置里的名单转成界面要的列表。
func customSitesOf(c config.CustomConfig) []CustomSite {
	out := make([]CustomSite, 0, len(c.Domains))
	seen := make(map[string]bool, len(c.Domains))
	for _, d := range c.Domains {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, CustomSite{Pattern: d})
	}
	return out
}

// AddCustomSite 校验并添加一个自定义站点，返回规范化后的域名。
func (a *App) AddCustomSite(raw string) (string, error) {
	p, err := config.NormalizePattern(raw)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil || a.stopped {
		return "", fmt.Errorf("服务未启动")
	}
	for _, d := range a.cfg.Custom.Domains {
		if d == p {
			return p, fmt.Errorf("%s 已经在自定义站点里", p)
		}
	}
	next := a.cfg.Custom
	next.Domains = append(append([]string(nil), next.Domains...), p)
	if err := a.applyCustomLocked(next); err != nil {
		return "", err
	}
	a.log.Info("已添加自定义站点", "domain", p)
	return p, nil
}

// RemoveCustomSite 删除一个自定义站点。
func (a *App) RemoveCustomSite(pattern string) error {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return fmt.Errorf("域名不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil || a.stopped {
		return fmt.Errorf("服务未启动")
	}
	next := config.CustomConfig{
		Domains: removeString(a.cfg.Custom.Domains, p),
	}
	if len(next.Domains) == len(a.cfg.Custom.Domains) {
		return fmt.Errorf("%s 不在自定义站点里", p)
	}
	if err := a.applyCustomLocked(next); err != nil {
		return err
	}
	a.log.Info("已删除自定义站点", "domain", p)
	return nil
}

// applyCustomLocked 应用新的自定义站点集合（需持有 a.mu）：
// 重建规则集、让新规则立即生效，并写回配置文件。
func (a *App) applyCustomLocked(next config.CustomConfig) error {
	rs, err := rules.Load(a.svcIDs)
	if err != nil {
		return err
	}
	rs.AddPatterns(next.Domains)
	a.applyRulesLocked(rs, a.svcIDs, next)
	a.saveConfigLocked()
	return nil
}

// containsString 报告 list 里是否有 s（大小写不敏感，配置里存的一律是小写）。
func containsString(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// removeString 返回去掉 s 之后的新切片（不改动入参，避免共享底层数组带来的意外）。
func removeString(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if strings.EqualFold(v, s) {
			continue
		}
		out = append(out, v)
	}
	return out
}
