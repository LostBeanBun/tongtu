package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"tongtu/internal/config"
	"tongtu/internal/mitm"
)

var certCmd = &cobra.Command{
	Use:   "cert",
	Short: "管理「无 SNI 反代」的本地根证书",
	Long: `管理无 SNI 反代所需的本地根证书（CA）：
  - 证书与私钥保存在 %APPDATA%\tongtu\（ca.crt / ca.key）
  - 安装目标为【当前用户】受信任的根证书存储，无需管理员权限
  - 仅对配置 mitm.domains 列出的域名解密，其余流量端到端隧道不解密`,
}

var certInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "生成（如不存在）并安装本地根证书",
	RunE: func(cmd *cobra.Command, args []string) error {
		ca, err := mitm.LoadOrCreate(config.Dir())
		if err != nil {
			return err
		}
		if ok, _ := mitm.Installed(ca); ok {
			fmt.Println("本地根证书已安装（无需重复操作）")
			printCertInfo(ca)
			return nil
		}
		if err := mitm.Install(ca); err != nil {
			return err
		}
		fmt.Println("已安装本地根证书到当前用户存储 ✓")
		printCertInfo(ca)
		return nil
	},
}

var certUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "从当前用户存储移除本地根证书（幂等）",
	RunE: func(cmd *cobra.Command, args []string) error {
		ca, err := mitm.LoadOrCreate(config.Dir())
		if err != nil {
			return err
		}
		if err := mitm.Uninstall(ca); err != nil {
			return err
		}
		fmt.Println("已移除本地根证书（如从未安装则无影响）")
		return nil
	},
}

var certStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "查看证书状态",
	RunE: func(cmd *cobra.Command, args []string) error {
		ca, err := mitm.LoadOrCreate(config.Dir())
		if err != nil {
			return err
		}
		installed, err := mitm.Installed(ca)
		if err != nil {
			return err
		}
		if installed {
			fmt.Println("状态: 已安装 ✓")
		} else {
			fmt.Println("状态: 未安装（无 SNI 反代不会启用）")
		}
		printCertInfo(ca)
		return nil
	},
}

func printCertInfo(ca *mitm.CA) {
	certPath, keyPath := mitm.CertPaths(config.Dir())
	tp := ca.Thumbprint()
	fmt.Printf("  指纹(SHA1): %s\n", tp)
	fmt.Printf("  证书:       %s\n", certPath)
	fmt.Printf("  私钥:       %s\n", keyPath)
	fmt.Printf("  提示:       Firefox 需在「设置→隐私与安全→证书」手动导入 %s\n", certPath)
}

func init() {
	certCmd.AddCommand(certInstallCmd, certUninstallCmd, certStatusCmd)
	rootCmd.AddCommand(certCmd)
}
