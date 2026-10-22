package main

// 「检查更新」的界面侧实现（托盘菜单调用）。
//
// 只查询 GitHub 最新 Release，然后告诉用户「已是最新」或「有新版本」；下载交给浏览器。
// 通途是绿色单文件，自动替换正在运行的 exe 需要处理文件占用、回滚与校验，
// 为一个个人项目引入这套机制得不偿失——但要让人知道有新版本，这个入口必须有。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tongtu/internal/tray"
	"tongtu/internal/update"
)

// checkUpdateDialog 查询并弹框报告结果。由托盘在独立 goroutine 里调用：
// 弹模态框会阻塞调用线程，绝不能放在托盘的消息循环里。
func checkUpdateDialog(log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if log != nil {
		log.Info("正在检查更新…")
	}
	res, err := update.Default().Check(ctx, Version)
	switch {
	case errors.Is(err, update.ErrNoRelease):
		if log != nil {
			log.Info("检查更新：仓库还没有发布版本")
		}
		messageBox("通途 Tongtu — 检查更新",
			"仓库还没有发布版本。\n\n当前版本 v"+Version, mbOK|mbIconInformation|mbTopMost)
	case err != nil:
		if log != nil {
			log.Warn("检查更新失败", "err", err)
		}
		messageBox("通途 Tongtu — 检查更新失败",
			err.Error()+"\n\n请检查网络后重试。", mbOK|mbIconError|mbTopMost)
	case res.Newer:
		if log != nil {
			log.Info("检查更新：发现新版本", "current", res.Current, "latest", res.Latest)
		}
		msg := fmt.Sprintf("当前 v%s，最新 v%s。\n\n是否打开下载页面？", res.Current, res.Latest)
		if messageBox("通途 Tongtu — 发现新版本", msg,
			mbYesNo|mbIconQuestion|mbSetForeground|mbTopMost) != messageBoxYes {
			return
		}
		if err := tray.OpenPath(res.URL); err != nil {
			messageBox("通途 Tongtu — 打开下载页失败",
				err.Error()+"\n\n请手动打开：\n"+res.URL, mbOK|mbIconError)
			return
		}
		if log != nil {
			log.Info("已在浏览器中打开下载页面", "url", res.URL)
		}
	default:
		if log != nil {
			log.Info("检查更新：已是最新版本", "latest", res.Latest)
		}
		messageBox("通途 Tongtu — 检查更新",
			"已是最新版本（v"+res.Latest+"）。", mbOK|mbIconInformation|mbTopMost)
	}
}
