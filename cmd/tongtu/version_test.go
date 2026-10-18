package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVersionMatchesVERSIONFile 保证 VERSION 文件与 version.go 里的默认值不漂移。
//
// 版本号是发布产物的一部分：两个人各改一处、或者只改了 build.ps1 的参数，
// 结果就是「exe 里写的 0.0.3、tag 打的 v0.0.4」这种事后很难查的问题。
func TestVersionMatchesVERSIONFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Skipf("读不到 VERSION 文件（%v），跳过（只在本仓库内有效）", err)
	}
	want := strings.TrimSpace(string(raw))
	if want == "" {
		t.Fatal("VERSION 文件是空的")
	}
	if want != Version {
		t.Errorf("版本号漂移：VERSION=%q，version.go 默认值=%q（改版本请用 .\\bump.ps1）", want, Version)
	}
}

// TestVersionIsSemver 版本号必须是 x.y.z 形式，方便打 tag、写 CHANGELOG 与自动解析。
func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Errorf("版本号必须是 x.y.z 形式（当前 %q）", Version)
	}
}
