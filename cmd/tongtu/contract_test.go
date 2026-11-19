package main

// README「1.0 兼容性承诺」（README.md 同名小节）是对外契约：1.0.0 之后只做
// 向后兼容的增量。这个文件把契约里能机器判定的部分钉成测试——命令树、
// 全局标志、退出码、数据位置、「不上传遥测」的出网白名单——任何一条被改坏，
// CI 直接红。要改契约本身：先改 README 的同名小节并抬 major，再改这里。
//
// 刻意不覆盖（README 已写明不在契约内）：__guard / __recover 等内部命令、
// 控制端口的形状、控件布局与绘制细节、日志措辞、tools/ 脚本输出。
// 也只检查「契约点名的还在」：新增命令与标志不受限制，这里不比对全集。
//
// 配置侧契约（version: 5、「配置说明」的键集合）见 internal/config/contract_test.go。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"tongtu/internal/config"
	"tongtu/internal/mitm"
)

// contractCommands 是 README 点名的命令树：一个都不能少、不能改名、不能隐藏。
// 值是必须存在的子命令。
var contractCommands = map[string][]string{
	"start":     nil,
	"stop":      nil,
	"status":    nil,
	"test":      nil,
	"version":   nil,
	"cert":      {"install", "status", "uninstall"},
	"autostart": {"on", "off", "status"},
}

// findSub 在 cmd 的子命令里按名字查找（不递归：契约只承诺到第二层）。
func findSub(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestContractCommands 守护契约里的「命令」：改名、删除、挪成隐藏命令都算破坏性变更。
// 反过来不设限——completion、help 这类新增命令不在契约里，允许出现。
func TestContractCommands(t *testing.T) {
	for name, subs := range contractCommands {
		cmd := findSub(rootCmd, name)
		if cmd == nil {
			t.Errorf("契约命令「tongtu %s」不存在（README「1.0 兼容性承诺」）", name)
			continue
		}
		if cmd.Hidden {
			t.Errorf("契约命令 %s 不能隐藏：用户要能从 tongtu --help 里找到它", name)
		}
		for _, sub := range subs {
			s := findSub(cmd, sub)
			if s == nil {
				t.Errorf("契约子命令「tongtu %s %s」不存在", name, sub)
				continue
			}
			if s.Hidden {
				t.Errorf("契约子命令「tongtu %s %s」不能隐藏", name, sub)
			}
		}
	}
}

// TestContractGlobalFlags 守护契约里的「全局标志」。
//
// --config 与四个启动开关挂在根命令的持久标志上：tongtu --hidden 与
// tongtu start --hidden 都要认（开机自启用的是不带子命令的根路径）。
// --json / --check 随子命令出现，必须挂在 README 点名的命令上。
func TestContractGlobalFlags(t *testing.T) {
	for _, name := range []string{"config", "no-sysproxy", "no-mitm", "no-tray", "hidden"} {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("缺少全局标志 --%s（README「1.0 兼容性承诺」）", name)
		}
	}
	if statusCmd.Flags().Lookup("json") == nil {
		t.Errorf("status 缺少契约标志 --json")
	}
	if versionCmd.Flags().Lookup("check") == nil {
		t.Errorf("version 缺少契约标志 --check")
	}
}

// TestContractExitCodeHelper 是 TestContractExitCodes 的子进程本体。
// 平时跑测试时它什么都不做；父测试用 TONGTU_CONTRACT_HELPER=1 把它单独拉起，
// 走与真实用户完全相同的 Execute() 路径，让退出码真正发生——
// 只断言「err == nil / err != nil」验不到 os.Exit 那一跳。
func TestContractExitCodeHelper(t *testing.T) {
	if os.Getenv("TONGTU_CONTRACT_HELPER") != "1" {
		return
	}
	rootCmd.SetArgs(strings.Fields(os.Getenv("TONGTU_CONTRACT_ARGS")))
	Execute()
	os.Exit(0) // Execute 正常返回即契约里的「成功为 0」
}

// TestContractExitCodes 守护契约里的「退出码：成功为 0，失败为非 0」。
// 脚本只判断「是否为 0」，所以这里只要求符号正确，不断言具体非零值。
func TestContractExitCodes(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("拿不到自身可执行文件: %v", err)
	}
	cases := []struct {
		name     string
		args     string
		wantZero bool
	}{
		// version 不读配置、不联网（--check 才联网），没有副作用
		{"成功为 0", "version", true},
		{"未知命令为非 0", "definitely-not-a-contract-command", false},
		{"未知标志为非 0", "--definitely-not-a-contract-flag", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(self, "-test.run=^TestContractExitCodeHelper$")
			cmd.Env = append(os.Environ(),
				"TONGTU_CONTRACT_HELPER=1",
				"TONGTU_CONTRACT_ARGS="+c.args)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("子进程启动失败: %v", err)
				}
				code = ee.ExitCode()
			}
			if c.wantZero && code != 0 {
				t.Errorf("成功应退出 0，实际 %d：%s", code, out)
			}
			if !c.wantZero && code == 0 {
				t.Errorf("失败应退出非 0：%s", out)
			}
		})
	}
}

// TestContractDataLocations 守护契约里的「数据位置」：配置、CA、日志都在
// %APPDATA%\tongtu，覆盖 exe 不会动它们。只承诺位置，不承诺文件名不变。
func TestContractDataLocations(t *testing.T) {
	base := t.TempDir()
	t.Setenv("APPDATA", base)

	wantDir := filepath.Join(base, "tongtu")
	if got := config.Dir(); got != wantDir {
		t.Errorf("数据目录 = %q，契约要求 %q", got, wantDir)
	}
	if got := config.DefaultPath(); got != filepath.Join(wantDir, "config.yaml") {
		t.Errorf("默认配置路径 = %q，应落在数据目录", got)
	}
	if got := config.Default().LogFilePath(); got != filepath.Join(wantDir, "tongtu.log") {
		t.Errorf("默认日志路径 = %q，应落在数据目录", got)
	}
	// CA 与私钥：cert install / stop / 反代都从同一个目录读写（见 cmd/tongtu/cert.go）
	certPath, keyPath := mitm.CertPaths(config.Dir())
	if filepath.Dir(certPath) != wantDir || filepath.Dir(keyPath) != wantDir {
		t.Errorf("CA 应落在数据目录: cert=%q key=%q", certPath, keyPath)
	}
}

// allowedOutboundHosts 是契约「不上传任何遥测数据」的出网白名单：
// 非测试 Go 源码里出现的每个 http(s) 主机名都必须在这里点名——DoH 端点、
// 远程优选 IP 列表、GitHub 更新检查、本地控制 API。
// 想新增一个出网目标，必须先改这张表（留下可审查的记录）；
// 悄悄塞一个上报地址进来，这个测试过不去。
var allowedOutboundHosts = map[string]bool{
	"doh.pub":             true, // DoH 端点
	"223.5.5.5":           true,
	"dns.alidns.com":      true,
	"1.1.1.1":             true, // 境内不可达，仅作后备端点
	"8.8.8.8":             true,
	"raw.hellogithub.com": true, // 远程优选 IP 列表（GitHub520）
	"api.github.com":      true, // 只读查询最新 Release（version --check / 托盘检查更新）
	"github.com":          true, // Release 页面链接（交给浏览器打开，程序不下载）
	"127.0.0.1":           true, // 本地控制 API、系统代理回环
	"localhost":           true,
	"::1":                 true,
	"[::1]":               true,
}

// outboundURLRe 只认「以 scheme 开头」的字面量：`"http://" + addr` 这种拼接
// 的字面量部分（恰好是 "http://"）不会命中，不会误报成本地请求。
var outboundURLRe = regexp.MustCompile(`^(?:https?)://([A-Za-z0-9.-]+)`)

// TestContractOutboundHosts 把「不上传任何遥测数据」变成可执行的出网白名单：
// 扫描仓库里所有非测试 Go 源码的字符串字面量，出现的每个 http(s) 主机名
// 都必须在 allowedOutboundHosts 里点过名。动态拼出来的目标（用户输入的域名、
// 配置里的端点）不在此列——它们的去向由 DoH / 代理逻辑各自负责。
func TestContractOutboundHosts(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	fset := token.NewFileSet()
	seen := map[string]bool{} // 已报告的主机（同一目标在多处出现只报一次）
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist": // 构建产物与版本库不含源码契约
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("解析 %s 失败: %w", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			m := outboundURLRe.FindStringSubmatch(s)
			if m == nil {
				return true
			}
			host := strings.ToLower(m[1])
			if !allowedOutboundHosts[host] {
				if _, dup := seen[host]; !dup {
					seen[host] = true
					t.Errorf("出网目标 %q（%s）不在契约白名单里："+
						"契约承诺「不上传任何遥测数据」——确认它不是上报地址后，"+
						"把它加进 allowedOutboundHosts 留档",
						host, fset.Position(lit.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
}
