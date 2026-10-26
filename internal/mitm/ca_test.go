package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadOrCreateGeneratesPersistentPair(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("首次生成 CA: %v", err)
	}
	if ca == nil {
		t.Fatal("LoadOrCreate 返回 nil")
	}

	certPath, keyPath := CertPaths(dir)
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("读取 CA 证书文件: %v", err)
	}
	if !bytes.Equal(certPEM, ca.PEM()) {
		t.Fatal("内存中的 CA PEM 与落盘文件不一致")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取 CA 私钥文件: %v", err)
	}
	if !bytes.Contains(keyPEM, []byte("BEGIN EC PRIVATE KEY")) {
		t.Fatalf("私钥文件不是 EC PRIVATE KEY PEM: %q", keyPEM)
	}

	if !ca.cert.IsCA || !ca.cert.BasicConstraintsValid {
		t.Fatal("生成的根证书必须带 CA 基本约束")
	}
	if ca.cert.Subject.CommonName != "Tongtu Local Root CA" {
		t.Fatalf("根证书 CN 错误: %q", ca.cert.Subject.CommonName)
	}
	if len(ca.cert.Subject.Organization) != 1 || ca.cert.Subject.Organization[0] != "Tongtu" {
		t.Fatalf("根证书 O 错误: %v", ca.cert.Subject.Organization)
	}
	if ca.cert.SerialNumber == nil || ca.cert.SerialNumber.Sign() <= 0 {
		t.Fatal("根证书序列号必须为正数")
	}
	if !ca.cert.NotAfter.After(ca.cert.NotBefore) {
		t.Fatalf("根证书有效期异常: %v ~ %v", ca.cert.NotBefore, ca.cert.NotAfter)
	}
	if ca.cert.NotAfter.Before(time.Now().Add(9 * 365 * 24 * time.Hour)) {
		t.Fatalf("根证书有效期应不少于 9 年，实际到 %v", ca.cert.NotAfter)
	}

	pub, ok := ca.cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("根证书公钥类型错误: %T", ca.cert.PublicKey)
	}
	if !pub.Equal(&ca.key.PublicKey) {
		t.Fatal("落盘私钥与证书公钥不匹配")
	}

	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("重新加载 CA: %v", err)
	}
	if !bytes.Equal(again.PEM(), ca.PEM()) {
		t.Fatal("重新加载应复用已有证书")
	}
	if again.Thumbprint() != ca.Thumbprint() {
		t.Fatal("重新加载的证书指纹不一致")
	}
}

func TestCAThumbprintIsSHA1OfDER(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}
	sum := sha1.Sum(ca.cert.Raw)
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	got := ca.Thumbprint()
	if got != want {
		t.Fatalf("指纹错误: got %q want %q", got, want)
	}
	if len(got) != 40 {
		t.Fatalf("SHA1 指纹应为 40 个十六进制字符，实际 %d", len(got))
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("指纹不是合法十六进制: %v", err)
	}
}

func TestLeafIssuesVerifiableCertificate(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}

	leaf, err := ca.Leaf("Example.COM.")
	if err != nil {
		t.Fatalf("签发叶证书: %v", err)
	}
	if len(leaf.Certificate) != 2 {
		t.Fatalf("叶证书链应包含叶证书 + CA，实际 %d 张", len(leaf.Certificate))
	}
	if !bytes.Equal(leaf.Certificate[1], ca.cert.Raw) {
		t.Fatal("证书链第二张应为本地 CA")
	}
	if leaf.PrivateKey == nil {
		t.Fatal("叶证书缺少私钥")
	}

	cert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("解析叶证书: %v", err)
	}
	if cert.Subject.CommonName != "example.com" {
		t.Fatalf("叶证书 CN 应已归一化: %q", cert.Subject.CommonName)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "example.com" {
		t.Fatalf("叶证书 DNSNames 错误: %v", cert.DNSNames)
	}
	if cert.IsCA {
		t.Fatal("叶证书不应是 CA")
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("叶证书 KeyUsage 错误: %v", cert.KeyUsage)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("叶证书 ExtKeyUsage 错误: %v", cert.ExtKeyUsage)
	}
	if !cert.NotAfter.After(time.Now()) {
		t.Fatalf("叶证书已过期: %v", cert.NotAfter)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.PEM()) {
		t.Fatal("CA PEM 无法加入证书池")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com"}); err != nil {
		t.Fatalf("叶证书无法用本地 CA 验证: %v", err)
	}

	cached, err := ca.Leaf("example.com")
	if err != nil {
		t.Fatalf("再次签发叶证书: %v", err)
	}
	if cached != leaf {
		t.Fatal("同一主机名应命中证书缓存")
	}
	other, err := ca.Leaf("raw.githubusercontent.com")
	if err != nil {
		t.Fatalf("签发另一张叶证书: %v", err)
	}
	if other == leaf {
		t.Fatal("不同主机名不应复用同一张叶证书")
	}
}

func TestLeafRejectsEmptyHost(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}
	for _, host := range []string{"", "   ", ".", "\t"} {
		if _, err := ca.Leaf(host); err == nil {
			t.Errorf("主机名 %q 应被拒绝", host)
		}
	}
}

func TestLeafConcurrentReturnsSameCertificate(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}
	first, err := ca.Leaf("concurrent.example")
	if err != nil {
		t.Fatalf("首次签发: %v", err)
	}

	const n = 32
	out := make([]*tls.Certificate, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = ca.Leaf("Concurrent.Example.")
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发签发失败（%d）: %v", i, errs[i])
		}
		if out[i] != first {
			t.Fatalf("并发签发应命中同一缓存（%d）", i)
		}
	}
}

func TestLoadOrCreateRegeneratesIncompletePair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove string
	}{
		{"缺少私钥", caKeyFile},
		{"缺少证书", caCertFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			first, err := LoadOrCreate(dir)
			if err != nil {
				t.Fatalf("生成 CA: %v", err)
			}
			if err := os.Remove(filepath.Join(dir, tc.remove)); err != nil {
				t.Fatalf("删除 %s: %v", tc.remove, err)
			}

			second, err := LoadOrCreate(dir)
			if err != nil {
				t.Fatalf("缺失文件后重新加载: %v", err)
			}
			if bytes.Equal(first.PEM(), second.PEM()) {
				t.Fatal("证书/私钥不成对时应重新生成")
			}
			for _, name := range []string{caCertFile, caKeyFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Fatalf("重新生成后 %s 应存在: %v", name, err)
				}
			}
		})
	}
}

func TestLoadOrCreateRejectsCorruptFiles(t *testing.T) {
	validDir := t.TempDir()
	ca, err := LoadOrCreate(validDir)
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}
	certPEM := ca.PEM()
	keyPEM, err := os.ReadFile(filepath.Join(validDir, caKeyFile))
	if err != nil {
		t.Fatalf("读取私钥: %v", err)
	}

	for _, tc := range []struct {
		name string
		cert []byte
		key  []byte
		want string
	}{
		{"证书不是 PEM", []byte("garbage"), keyPEM, "证书 PEM 无效"},
		{"私钥不是 PEM", certPEM, []byte("garbage"), "私钥 PEM 无效"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, caCertFile), tc.cert, 0o644); err != nil {
				t.Fatalf("写入证书: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, caKeyFile), tc.key, 0o600); err != nil {
				t.Fatalf("写入私钥: %v", err)
			}
			_, err := LoadOrCreate(dir)
			if err == nil {
				t.Fatal("损坏的 CA 文件应返回错误")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应包含 %q，实际为 %v", tc.want, err)
			}
		})
	}
}

func TestLoadOrCreateFailsWhenDirIsFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ca-dir")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("准备同名文件: %v", err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("CA 目录位置被文件占用时应返回错误")
	}
}

func TestCertPaths(t *testing.T) {
	dir := filepath.Join("some", "ca", "dir")
	certPath, keyPath := CertPaths(dir)
	if certPath != filepath.Join(dir, "ca.crt") {
		t.Fatalf("证书路径错误: %q", certPath)
	}
	if keyPath != filepath.Join(dir, "ca.key") {
		t.Fatalf("私钥路径错误: %q", keyPath)
	}
}

func TestInstallPromptMentionsSafetyAndPath(t *testing.T) {
	certPath := filepath.Join("C:", "Users", "me", "AppData", "Roaming", "tongtu", "ca.crt")
	got := InstallPrompt(certPath)
	for _, want := range []string{certPath, "当前用户", "ca.crt", "tongtu cert uninstall"} {
		if !strings.Contains(got, want) {
			t.Errorf("安装说明应包含 %q，实际为:\n%s", want, got)
		}
	}
}

// TestEnsureInstalledDoesNotInstallWithoutConsent 用户拒绝时不能偷偷装证书，
// 但必须先把完整说明（含证书路径）交到调用方。
func TestEnsureInstalledDoesNotInstallWithoutConsent(t *testing.T) {
	dir := t.TempDir()
	var prompt string
	installed, err := EnsureInstalled(dir, func(p string) bool {
		prompt = p
		return false
	})
	if err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if installed {
		t.Fatal("用户拒绝后不应报告已安装")
	}
	certPath, _ := CertPaths(dir)
	if !strings.Contains(prompt, certPath) {
		t.Fatalf("询问文字应包含证书路径 %q，实际:\n%s", certPath, prompt)
	}
	if !strings.Contains(prompt, "当前用户") {
		t.Fatalf("询问文字应说明安装范围，实际:\n%s", prompt)
	}
}

// TestEnsureInstalledCheckOnly 没有询问回调时只检查、不安装。
func TestEnsureInstalledCheckOnly(t *testing.T) {
	installed, err := EnsureInstalled(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if installed {
		t.Fatal("新生成的 CA 不应已安装")
	}
}

// TestCertStoreUnsupportedOnNonWindows 非 Windows 平台必须明确报「不支持」，而不是假装成功。
func TestCertStoreUnsupportedOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 支持当前用户证书存储")
	}
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("生成 CA: %v", err)
	}
	if _, err := Installed(ca); err == nil {
		t.Error("Installed 在非 Windows 平台应返回错误")
	}
	if err := Install(ca); err == nil {
		t.Error("Install 在非 Windows 平台应返回错误")
	}
	if err := Uninstall(ca); err == nil {
		t.Error("Uninstall 在非 Windows 平台应返回错误")
	}
}
