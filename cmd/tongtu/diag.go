package main

// 「复制诊断」：把排查问题需要的上下文一次性拼好，用户直接粘贴给别人即可。
//
// 只放事实（版本、开关、监听地址、近期日志），不放完整配置——配置项太多，
// 反而让人抓不到重点；而且诊断信息可能被贴到公开的地方，越少越好。

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/autostart"
	"tongtu/internal/config"
	"tongtu/internal/mitm"
)

// diagLogLines 是诊断信息里附带的近期日志行数：足够看到一次操作的来龙去脉，
// 又不至于把剪贴板塞爆。
const diagLogLines = 60

// buildDiagnostics 生成一份「可以直接贴给别人看」的诊断文本。
func buildDiagnostics(a *app.App, cfg *config.Config) string {
	st := a.Status()
	var b strings.Builder
	fmt.Fprintf(&b, "通途 Tongtu %s 诊断信息\n", Version)
	fmt.Fprintf(&b, "生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "系统: %s/%s · 管理员=%v\n", runtime.GOOS, runtime.GOARCH, autostart.IsAdmin())
	fmt.Fprintf(&b, "配置: %s\n", a.ConfigPath())
	if path := cfg.LogFilePath(); path != "" {
		fmt.Fprintf(&b, "日志: %s\n", path)
	} else {
		b.WriteString("日志: 未启用文件日志（log.file: off）\n")
	}
	fmt.Fprintf(&b, "监听: %s（系统代理=%v · hosts=%v · 反代=%v）\n",
		st.Listen, st.SystemProxy, st.Hosts, st.MITM)
	if st.Running {
		fmt.Fprintf(&b, "运行时长: %s%s\n", humanUptime(st.UptimeSec), pauseWord(st.Paused))
	}
	if custom := cfg.ProxyPatterns(); len(custom) > 0 {
		// 「我的站点没加速」第一件事就是确认规则到底读进去了没有
		fmt.Fprintf(&b, "自定义站点: %s\n", strings.Join(custom, "、"))
	}
	if mitm := cfg.Custom.MITMDomains; len(mitm) > 0 {
		fmt.Fprintf(&b, "自定义反代: %s\n", strings.Join(mitm, "、"))
	}
	if st.ProxyNote != "" {
		fmt.Fprintf(&b, "系统代理说明: %s\n", st.ProxyNote)
	}
	if st.MITMNote != "" {
		fmt.Fprintf(&b, "反代说明: %s\n", st.MITMNote)
	}
	certWord := "未安装"
	if ok, err := mitm.CertInstalled(config.Dir()); err != nil {
		certWord = "状态未知（" + err.Error() + "）"
	} else if ok {
		certWord = "已安装"
	}
	fmt.Fprintf(&b, "根证书: %s\n", certWord)
	fmt.Fprintf(&b, "服务组: %s\n", strings.Join(st.Services, "、"))
	fmt.Fprintf(&b, "流量: 请求 %d · 隧道 %d · 活跃 %d · ↓%s ↑%s\n",
		st.Proxy.Requests, st.Proxy.Tunnels, st.Proxy.Active,
		humanBytes(st.Proxy.BytesIn), humanBytes(st.Proxy.BytesOut))

	if data, err := json.MarshalIndent(st, "", "  "); err == nil {
		b.WriteString("\n状态快照:\n")
		b.Write(data)
		b.WriteByte('\n')
	}
	if sink := logDisplay(); sink != nil {
		if recent := sink.recentText(diagLogLines); recent != "" {
			fmt.Fprintf(&b, "\n最近窗口日志（最多 %d 行）:\n%s", diagLogLines, recent)
		}
	}
	return b.String()
}

// pauseWord 在诊断信息里给「运行时长」补一句是否处于暂停，省得把「暂停中」当成故障排查。
func pauseWord(paused bool) string {
	if !paused {
		return ""
	}
	return "（当前已暂停：对外影响已撤回）"
}
