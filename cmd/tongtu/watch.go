package main

// 配置文件热生效：改完 config.yaml 保存，正在运行的实例自动用上新规则。
//
// 只热更新「服务组 / 自定义站点 / 反代域名」（见 app.ReloadRules）；端口、DNS 端点、
// 日志级别这些要重建组件或重开进程的字段，改了仍需重启——文档里写清楚了，界面不撒谎。

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"tongtu/internal/app"
	"tongtu/internal/config"
)

// watchConfigFile 每两秒看一眼配置文件，变了就重新加载并应用到运行中的实例，直到 ctx 结束。
//
// 为什么轮询而不是文件系统通知：只需要秒级粒度，轮询没有额外依赖、也不会漏掉
// 「先写临时文件再改名」这种保存方式；mtime + 大小足够判断「内容可能变了」。
func watchConfigFile(ctx context.Context, a *app.App, path string, log *slog.Logger) {
	const (
		every  = 2 * time.Second
		settle = 400 * time.Millisecond
	)
	last := configStamp(path)
	lastErr := ""
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st := configStamp(path)
		if st == last {
			continue
		}
		// 编辑器可能正在写：等文件稳定下来再读，避免把半截 YAML 当成「配置写错了」报出来。
		time.Sleep(settle)
		if stable := configStamp(path); stable != st {
			continue // 还在写，下一轮再看
		}
		last = st

		next, err := config.Load(path)
		if err != nil {
			// 同一份错误只报一次：用户正在编辑时每两秒刷一条日志毫无意义。
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				log.Error("配置文件有误，本次改动未生效（继续使用原来的规则）", "err", err)
			}
			continue
		}
		lastErr = ""
		changed, err := a.ReloadRules(next)
		switch {
		case err != nil:
			log.Warn("重新应用配置失败（继续使用原来的规则）", "err", err)
		case changed:
			log.Info("配置已热生效", "services", next.Services,
				"自定义域名", len(next.ProxyPatterns()), "反代域名", len(next.MITMPatterns()))
		}
	}
}

// configStamp 是判断配置文件是否变化的轻量指纹（修改时间 + 大小）。
func configStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(fi.Size(), 10)
}
