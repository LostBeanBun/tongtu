package config

// README「1.0 兼容性承诺」的配置侧：`version: 5` 与「配置说明」里列出的键
// 是对外契约——键改名、删除或改变语义必须抬 major。这个文件把 README 的
// 承诺变成可执行断言：版本号三处一致（代码 / 契约段 / 配置说明样例）、
// 「配置说明」样例的键集合与 Config 结构体的 yaml 标签逐层相等、
// 未知键按契约「被忽略、下次保存时丢失」。
//
// CLI 侧契约（命令树、全局标志、退出码、数据位置）见 cmd/tongtu/contract_test.go。

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// readme 返回仓库根的 README；读不到（例如只拷了包目录）时跳过——
// 与 version_test.go 读 VERSION 的口径一致。
func readme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Skipf("读不到 README.md（%v），跳过（只在本仓库内有效）", err)
	}
	return string(raw)
}

// readmeSection 取 heading（整行精确匹配，含前导 #）之后、下一个标题之前的内容。
func readmeSection(t *testing.T, doc, heading string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("README 里找不到标题 %q", heading)
	}
	var b strings.Builder
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			break
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// readmeYamlBlock 取 heading 下第一个 ```yaml 代码块的内容。
func readmeYamlBlock(t *testing.T, doc, heading string) string {
	t.Helper()
	sec := readmeSection(t, doc, heading)
	const open = "```yaml"
	i := strings.Index(sec, open)
	if i < 0 {
		t.Fatalf("%s 下没有 %s 代码块", heading, open)
	}
	rest := sec[i+len(open):]
	j := strings.Index(rest, "```")
	if j < 0 {
		t.Fatalf("%s 的 %s 代码块没有闭合", heading, open)
	}
	return rest[:j]
}

// TestContractConfigVersionPinned 钉住配置版本：代码里的 CurrentVersion、
// 契约段承诺的 `version: 5`、「配置说明」样例里的 version 三处必须一致。
// 抬 CurrentVersion 属于破坏性变更（老配置要迁移），必须先改 README 并抬 major。
func TestContractConfigVersionPinned(t *testing.T) {
	if CurrentVersion != 5 {
		t.Errorf("CurrentVersion = %d，README 契约承诺的是 5；"+
			"改配置版本属于破坏性变更，先改 README「1.0 兼容性承诺」并抬 major", CurrentVersion)
	}

	doc := readme(t)

	// 契约段里的 `version: 5`
	sec := readmeSection(t, doc, "### 1.0 兼容性承诺（从 1.0.0 起）")
	m := regexp.MustCompile("`version: (\\d+)`").FindStringSubmatch(sec)
	if m == nil {
		t.Error("README 契约段里找不到 `version: N` 形式的承诺")
	} else if got, err := strconv.Atoi(m[1]); err != nil || got != CurrentVersion {
		t.Errorf("README 契约段写的是 version: %s，代码里 CurrentVersion = %d", m[1], CurrentVersion)
	}

	// 「配置说明」样例里的 version
	var sample map[string]any
	if err := yaml.Unmarshal([]byte(readmeYamlBlock(t, doc, "## 配置说明")), &sample); err != nil {
		t.Fatalf("README 配置说明的 yaml 样例解析失败: %v", err)
	}
	got, ok := sample["version"].(int)
	if !ok {
		t.Fatalf("配置说明样例缺少 version 键（实际类型 %T）", sample["version"])
	}
	if got != CurrentVersion {
		t.Errorf("配置说明样例写的是 version: %d，代码里 CurrentVersion = %d", got, CurrentVersion)
	}
}

// TestContractConfigKeysMatchDocs 「配置说明」样例列出的键就是对外契约：
// 两边逐层比对，README 少写（用户不知道有这个键）或多写（结构体已经没有
// 这个字段，样例在撒谎）都算契约漂移。新增键必须先写进 README。
func TestContractConfigKeysMatchDocs(t *testing.T) {
	doc := readme(t)
	var sample map[string]any
	if err := yaml.Unmarshal([]byte(readmeYamlBlock(t, doc, "## 配置说明")), &sample); err != nil {
		t.Fatalf("配置说明的 yaml 样例解析失败: %v", err)
	}
	checkContractKeys(t, "config", sample, reflect.TypeOf(Config{}))
}

// checkContractKeys 逐层比对 README 样例的键与结构体的 yaml 标签。
// 只在「字段本身是结构体」时向下递归：map（static_hosts 的域名键）与切片的
// 内容是取值不是配置键，不该拿来比。
func checkContractKeys(t *testing.T, path string, doc any, st reflect.Type) {
	t.Helper()
	dm, ok := doc.(map[string]any)
	if !ok {
		t.Fatalf("%s: README 样例里应是映射，实际 %T", path, doc)
	}
	want := map[string]reflect.Type{}
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue // 非持久化字段（UpgradedFrom）
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		want[name] = f.Type
	}
	for name := range want {
		if _, ok := dm[name]; !ok {
			t.Errorf("%s: README「配置说明」缺键 %q（结构体有、样例没写——用户就不知道它存在）", path, name)
		}
	}
	for name := range dm {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: README「配置说明」多出键 %q（结构体已没有对应字段——"+
				"键改名或删除属于破坏性变更）", path, name)
		}
	}
	for name, ft := range want {
		if ft.Kind() != reflect.Struct {
			continue
		}
		sub, ok := dm[name]
		if !ok {
			continue // 上面已经报过缺键
		}
		checkContractKeys(t, path+"."+name, sub, ft)
	}
}

// TestContractUnknownKeysIgnored 对应契约里的「未知键会被忽略，并在下次保存时
// 丢失（不要用它存自己的数据）」：老配置里混着已删除的键（例如 v5 移除的
// hosts 模式）时，加载不能报错；保存后它们自然消失，不会被悄悄写回去。
func TestContractUnknownKeysIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "version: 5\n" +
		"listen: 127.0.0.1:19180\n" +
		"hosts: [example.com]\n" + // v5 已移除的键
		"future_key: whatever\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("未知键不该让加载失败（契约：未知键被忽略）: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 逐个顶层键检查：不能用子串匹配（`hosts:` 会误中 `static_hosts:`）
	var saved map[string]any
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("保存后的配置解析失败: %v\n%s", err, data)
	}
	for _, k := range []string{"hosts", "future_key"} {
		if _, ok := saved[k]; ok {
			t.Errorf("保存后未知键 %q 应当丢失（契约写明会丢），实际还在:\n%s", k, data)
		}
	}
	if v, ok := saved["version"].(int); !ok || v != CurrentVersion {
		t.Errorf("保存后应保留 version: %d，实际 %v:\n%s", CurrentVersion, saved["version"], data)
	}
}
