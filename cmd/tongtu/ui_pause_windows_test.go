//go:build windows

package main

import (
	"testing"
	"time"
)

// TestPauseMenuItems 暂停菜单的选项：顺序、文案、id 与时长必须一一对应。
//
// 这几样东西分散在三处用（菜单显示、反查时长、提示文案），错一个就会出现
// 「选了 30 分钟、提示说 15 分钟」这类问题，因此在这里锁死。
func TestPauseMenuItems(t *testing.T) {
	items := pauseMenuItems()
	if len(items) != len(pauseChoices) || len(items) == 0 {
		t.Fatalf("菜单项数量不对: %d", len(items))
	}
	want := []string{
		"暂停 15 分钟",
		"暂停 30 分钟",
		"暂停 1 小时",
		"暂停 2 小时",
		"暂停，直到手动恢复",
	}
	for i, w := range want {
		if items[i].Label != w {
			t.Fatalf("第 %d 项应为 %q，实际 %q", i, w, items[i].Label)
		}
	}
	if last := items[len(items)-1]; last.ID != "pause:0" {
		t.Fatalf("最后一项应是「一直暂停」，实际 id %q", last.ID)
	}
	for _, it := range items {
		c, ok := pauseChoiceByID(it.ID)
		if !ok {
			t.Fatalf("菜单项 id %q 反查不到时长", it.ID)
		}
		if c.Label != it.Label {
			t.Fatalf("id %q 的文案不一致：菜单 %q / 选项 %q", it.ID, it.Label, c.Label)
		}
	}
	if _, ok := pauseChoiceByID("pause:999h"); ok {
		t.Fatal("未知 id 不该反查成功")
	}
}

// TestHumanPause 时长文案：整小时写小时，其余写分钟，0 表示手动恢复。
func TestHumanPause(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "手动恢复"},
		{15 * time.Minute, "15 分钟"},
		{30 * time.Minute, "30 分钟"},
		{time.Hour, "1 小时"},
		{2 * time.Hour, "2 小时"},
		{90 * time.Minute, "90 分钟"},
	}
	for _, c := range cases {
		if got := humanPause(c.d); got != c.want {
			t.Fatalf("humanPause(%v) = %q，应为 %q", c.d, got, c.want)
		}
	}
}
