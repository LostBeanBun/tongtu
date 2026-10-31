package app

// custom.go 实现自定义站点的增删与「是否走反代」的切换（窗口「站点」面板的底层）。
//
// 为什么放在 app 层：改一个域名要同时做三件事——校验写法、写回配置、重建白名单并
// 让 hosts / 反代立即跟上（见 applyRulesLocked）。任何一件漏掉，界面就会显示
// 「已添加」而实际没生效，因此统一收在这里，而不是让界面直接改 config 再自己拼。

import (
	"fmt"
	"strings"

	"tongtu/internal/config"
	"tongtu/internal/rules"
)

// CustomSite 是一条自定义站点（窗口面板的一行）。
type CustomSite struct {
	Pattern string `json:"pattern"`
	MITM    bool   `json:"mitm"` // 是否同时走无 SNI 反代
}

// CustomSites 返回当前全部自定义站点：普通白名单与反代名单合并成一行一条。
func (a *App) CustomSites() []CustomSite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return customSitesOf(a.cfg.Custom)
}

// customSitesOf 把配置里的两个名单合并成界面要的列表。
//
// 只出现在反代名单里的域名（用户手工写进 config.yaml 的）也要显示：否则界面里
// 看不到、也删不掉，只能去改文件——这正是不该让用户做的事。
func customSitesOf(c config.CustomConfig) []CustomSite {
	mitm := make(map[string]bool, len(c.MITMDomains))
	for _, d := range c.MITMDomains {
		mitm[d] = true
	}
	out := make([]CustomSite, 0, len(c.Domains)+len(c.MITMDomains))
	seen := make(map[string]bool, len(c.Domains)+len(c.MITMDomains))
	for _, d := range c.Domains {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, CustomSite{Pattern: d, MITM: mitm[d]})
	}
	for _, d := range c.MITMDomains {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, CustomSite{Pattern: d, MITM: true})
	}
	return out
}

// AddCustomSite 校验并添加一个自定义站点，返回规范化后的域名。
//
// 只加进普通白名单；要让它同时走无 SNI 反代，用 SetCustomSiteMITM。
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

// RemoveCustomSite 删除一个自定义站点（白名单与反代名单一起删）。
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
		Domains:     removeString(a.cfg.Custom.Domains, p),
		MITMDomains: removeString(a.cfg.Custom.MITMDomains, p),
	}
	if len(next.Domains) == len(a.cfg.Custom.Domains) &&
		len(next.MITMDomains) == len(a.cfg.Custom.MITMDomains) {
		return fmt.Errorf("%s 不在自定义站点里", p)
	}
	if err := a.applyCustomLocked(next); err != nil {
		return err
	}
	a.log.Info("已删除自定义站点", "domain", p)
	return nil
}

// SetCustomSiteMITM 切换某个自定义站点是否走无 SNI 反代。
//
// 打开时确保它同时进白名单：反代域名也必须先过本地代理，否则流量根本不会到反代。
func (a *App) SetCustomSiteMITM(pattern string, on bool) error {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return fmt.Errorf("域名不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ln == nil || a.stopped {
		return fmt.Errorf("服务未启动")
	}
	if !containsString(a.cfg.Custom.Domains, p) && !containsString(a.cfg.Custom.MITMDomains, p) {
		return fmt.Errorf("%s 不在自定义站点里", p)
	}
	next := config.CustomConfig{
		Domains:     append([]string(nil), a.cfg.Custom.Domains...),
		MITMDomains: append([]string(nil), a.cfg.Custom.MITMDomains...),
	}
	if on {
		if !containsString(next.Domains, p) {
			next.Domains = append(next.Domains, p)
		}
		if !containsString(next.MITMDomains, p) {
			next.MITMDomains = append(next.MITMDomains, p)
		}
	} else {
		next.MITMDomains = removeString(next.MITMDomains, p)
	}
	if err := a.applyCustomLocked(next); err != nil {
		return err
	}
	a.log.Info("自定义站点的反代设置已更新", "domain", p, "mitm", on)
	return nil
}

// applyCustomLocked 应用新的自定义站点集合（需持有 a.mu）：
// 重建规则集、让 hosts / 反代立即跟上，并写回配置文件。
func (a *App) applyCustomLocked(next config.CustomConfig) error {
	rs, err := rules.Load(a.svcIDs)
	if err != nil {
		return err
	}
	patterns := append(append([]string(nil), next.Domains...), next.MITMDomains...)
	rs.AddPatterns(patterns)
	a.applyRulesLocked(rs, a.svcIDs, next, a.cfg.MITM.Domains)
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
