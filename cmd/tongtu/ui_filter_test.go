package main

import (
	"strings"
	"testing"
)

// 日志过滤是纯函数，因此不依赖窗口，任何平台都能跑。
//
// 它要解决的问题很具体：日志里混着「一条记录的正文行」与「缩进的属性续行」，
// 过滤时不能把一条记录拦腰截断。

func plainLines(texts ...string) []logLine {
	out := make([]logLine, 0, len(texts))
	for _, s := range texts {
		out = append(out, logLine{Spans: []logSpan{{Style: styleText, Text: s}}})
	}
	return out
}

func lineTexts(lines []logLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.plain())
	}
	return out
}

func TestFilterLinesNoConditionKeepsAll(t *testing.T) {
	lines := plainLines("a", "b", "c")
	got := filterLines(lines, "", false)
	if len(got) != 3 {
		t.Fatalf("没有过滤条件时应原样返回，得到 %d 行", len(got))
	}
}

func TestFilterLinesKeyword(t *testing.T) {
	lines := plainLines(
		"已启用系统代理模式",
		"服务组已加载 groups=[steam github]",
		"隧道已建立 target=steamcommunity.com:443",
	)
	got := lineTexts(filterLines(lines, "STEAM", false))
	if len(got) != 2 {
		t.Fatalf("关键字应不区分大小写命中 2 行，得到 %v", got)
	}
	if !strings.Contains(got[0], "服务组") || !strings.Contains(got[1], "隧道") {
		t.Errorf("命中的行不对：%v", got)
	}
}

func TestFilterLinesContinuationFollowsHead(t *testing.T) {
	// 一条记录的正文行 + 两条缩进续行；关键字只出现在续行上。
	lines := []logLine{
		{Spans: []logSpan{{Style: styleText, Text: "写入 hosts 需要管理员权限"}}},
		{Spans: []logSpan{{Style: styleMuted, Text: logIndent}, {Style: styleText, Text: "err=Access is denied."}}},
		{Spans: []logSpan{{Style: styleMuted, Text: logIndent}, {Style: styleText, Text: "path=C:\\Windows\\hosts"}}},
		{Spans: []logSpan{{Style: styleText, Text: "隧道已建立"}}},
	}
	// 关键字命中第一条的续行：正文行没命中 → 整条都不该留下（否则会看到孤立的属性行）
	got := filterLines(lines, "Access", false)
	if len(got) != 0 {
		t.Errorf("正文行没命中时，续行不该单独留下：%v", lineTexts(got))
	}
	// 关键字命中正文行：续行要跟着留下
	got = filterLines(lines, "hosts 需要", false)
	if len(got) != 3 {
		t.Errorf("正文行命中时续行应跟随，得到 %d 行：%v", len(got), lineTexts(got))
	}
}

func TestFilterLinesOnlyWarn(t *testing.T) {
	lines := []logLine{
		{Spans: []logSpan{{Style: styleText, Text: "已启用系统代理模式"}}},
		{Spans: []logSpan{{Style: styleMuted, Text: "· "}, {Style: styleText, Text: "listen=127.0.0.1:19180"}}},
		{Spans: []logSpan{{Style: styleWarn, Text: "警告"}, {Style: styleText, Text: " 写入 hosts 需要管理员权限"}}},
		{Spans: []logSpan{{Style: styleMuted, Text: logIndent}, {Style: styleDanger, Text: "err="}, {Style: styleText, Text: "Access is denied."}}},
		{Spans: []logSpan{{Style: styleDanger, Text: "错误"}, {Style: styleText, Text: " 提权后仍未成功"}}},
	}
	got := filterLines(lines, "", true)
	if len(got) != 3 {
		t.Fatalf("只看警告应留下「警告 + 其续行 + 错误」共 3 行，得到 %d 行：%v", len(got), lineTexts(got))
	}
	if !strings.Contains(lineTexts(got)[0], "警告") {
		t.Errorf("第一行应是警告行：%v", lineTexts(got))
	}
	if !strings.Contains(lineTexts(got)[1], "Access is denied") {
		t.Errorf("警告行的续行应当跟随：%v", lineTexts(got))
	}
}

func TestFilterLinesOnlyWarnAndKeyword(t *testing.T) {
	lines := []logLine{
		{Spans: []logSpan{{Style: styleText, Text: "信息 已启用系统代理模式"}}},
		{Spans: []logSpan{{Style: styleWarn, Text: "警告"}, {Style: styleText, Text: " 写入 hosts 失败"}}},
		{Spans: []logSpan{{Style: styleWarn, Text: "警告"}, {Style: styleText, Text: " 反代证书未安装"}}},
	}
	got := filterLines(lines, "hosts", true)
	if len(got) != 1 || !strings.Contains(lineTexts(got)[0], "hosts") {
		t.Errorf("两个条件应同时生效（只看警告 + 关键字 hosts）：%v", lineTexts(got))
	}
}

func TestHasWarnSpan(t *testing.T) {
	if hasWarnSpan(logLine{Spans: []logSpan{{Style: styleText, Text: "ok"}}}) {
		t.Error("普通行不该算警告")
	}
	if !hasWarnSpan(logLine{Spans: []logSpan{{Style: styleText, Text: "x"}, {Style: styleDanger, Text: "y"}}}) {
		t.Error("含错误片段的行应算警告")
	}
}
