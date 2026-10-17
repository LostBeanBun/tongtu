// Package mitm 管理「无 SNI 反代」所需的本地根证书（CA）及其安装状态。
//
// 安全边界：CA 私钥仅存放在本机 %APPDATA%\tongtu\ca.key；
// 反代只对配置里显式列出的域名解密，其余流量保持端到端隧道（不解密）。
package mitm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	caCertFile = "ca.crt"
	caKeyFile  = "ca.key"
)

// CA 是本地根证书及其签发能力。
type CA struct {
	dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// CertPaths 返回 CA 证书/私钥文件路径。
func CertPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile)
}

// LoadOrCreate 加载本地 CA；不存在时自动生成 ECDSA P-256 根证书。
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := CertPaths(dir)
	certPEM, cErr := os.ReadFile(certPath)
	keyPEM, kErr := os.ReadFile(keyPath)
	if cErr == nil && kErr == nil {
		ca, err := fromPEM(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		ca.dir = dir
		return ca, nil
	}
	if !os.IsNotExist(cErr) && !os.IsNotExist(kErr) {
		return nil, fmt.Errorf("读取本地 CA 失败: %w", errorsFirst(cErr, kErr))
	}
	// 私钥/证书任一缺失都重新生成，保证成对
	ca, certOut, keyOut, err := generate()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certOut, 0o644); err != nil {
		return nil, fmt.Errorf("写入 CA 证书失败: %w", err)
	}
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		return nil, fmt.Errorf("写入 CA 私钥失败: %w", err)
	}
	ca.dir = dir
	return ca, nil
}

func generate() (*CA, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("生成 CA 密钥失败: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Tongtu Local Root CA", Organization: []string{"Tongtu"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("签发 CA 证书失败: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &CA{cert: cert, key: key, pem: certPEM, leafs: map[string]*tls.Certificate{}}, certPEM, keyPEM, nil
}

func fromPEM(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("CA 证书 PEM 无效")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 CA 证书失败: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("CA 私钥 PEM 无效")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 CA 私钥失败: %w", err)
	}
	return &CA{cert: cert, key: key, pem: certPEM, leafs: map[string]*tls.Certificate{}}, nil
}

// Leaf 返回（并缓存）为 host 签发的服务器证书。
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return nil, fmt.Errorf("空主机名")
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leafs[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(397 * 24 * time.Hour),
		DNSNames:              []string{host},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("为 %s 签发证书失败: %w", host, err)
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}
	ca.leafs[host] = leaf
	return leaf, nil
}

// PEM 返回 CA 证书 PEM 内容。
func (ca *CA) PEM() []byte { return ca.pem }

// Thumbprint 返回 CA 证书的 SHA1 指纹（大写十六进制，certutil 查询用）。
func (ca *CA) Thumbprint() string {
	sum := sha1.Sum(ca.cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// Installed 检查 CA 是否已装入当前用户的受信任根证书存储。
func Installed(ca *CA) (bool, error) {
	if runtime.GOOS != "windows" {
		return false, fmt.Errorf("当前系统暂不支持证书存储检测")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "certutil", "-user", "-store", "Root", ca.Thumbprint()).CombinedOutput()
	if err != nil {
		return false, nil // certutil 未找到证书时返回非零退出码
	}
	return strings.Contains(strings.ToUpper(string(out)), ca.Thumbprint()), nil
}

// Install 将 CA 装入当前用户受信任根证书存储（无需管理员权限）。
func Install(ca *CA) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("当前系统暂不支持自动安装证书")
	}
	certPath, _ := CertPaths(ca.dir)
	if _, err := os.Stat(certPath); err != nil {
		// 找不到文件时用临时文件兜底
		tmp, terr := os.CreateTemp("", "tongtu-ca-*.crt")
		if terr != nil {
			return terr
		}
		certPath = tmp.Name()
		defer os.Remove(certPath)
		if _, werr := tmp.Write(ca.PEM()); werr != nil {
			tmp.Close()
			return werr
		}
		tmp.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "certutil", "-user", "-addstore", "Root", certPath).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("安装证书超时：可能弹出了需要点击确认的系统对话框\n  请重试并留意屏幕；或手动安装：双击 %s → 「安装证书」→ 存储位置选「当前用户」→ 「将所有的证书都放入下列存储」→「受信任的根证书颁发机构」", certPath)
	}
	if err != nil {
		return fmt.Errorf("certutil 安装失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall 将 CA 从当前用户受信任根证书存储移除（幂等）。
func Uninstall(ca *CA) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("当前系统暂不支持自动卸载证书")
	}
	ok, err := Installed(ca)
	if err != nil || !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "certutil", "-user", "-delstore", "Root", ca.Thumbprint()).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("删除证书超时：可能弹出了需要点击确认的系统对话框，请重试并留意屏幕")
	}
	if err != nil {
		return fmt.Errorf("certutil 删除失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func errorsFirst(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
