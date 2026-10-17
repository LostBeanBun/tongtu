# 第三方组件与致谢

通途（Tongtu）自身的代码（代理、DoH 解析、hosts 接管、MITM 反代、窗口与托盘）为独立编写，
网络与系统能力直接调用 Go 标准库与 Windows API；下列组件以 Go 模块形式引入，**未修改其源码**。

感谢这些项目的作者与维护者。分发本项目的二进制时，请一并保留本文件与各组件许可证的声明。

## 运行时依赖

| 模块 | 版本 | 许可证 | 用途 | 主页 |
| --- | --- | --- | --- | --- |
| `fyne.io/systray` | v1.12.2 | Apache-2.0 | 系统托盘图标与右键菜单 | <https://github.com/fyne-io/systray> |
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 | 命令行解析与子命令框架 | <https://github.com/spf13/cobra> |
| `github.com/spf13/pflag` | v1.0.9 | BSD-3-Clause | GNU 风格参数解析（cobra 依赖） | <https://github.com/spf13/pflag> |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | Win32 / 注册表 / 控制台等系统调用绑定 | <https://pkg.go.dev/golang.org/x/sys> |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT + Apache-2.0 | 配置文件（config.yaml）解析 | <https://github.com/go-yaml/yaml> |
| `github.com/godbus/dbus/v5` | v5.1.0 | BSD-2-Clause | systray 的间接依赖（Linux / BSD 侧） | <https://github.com/godbus/dbus> |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | Apache-2.0 | cobra 的间接依赖（Windows 侧） | <https://github.com/inconshreveable/mousetrap> |
| Go 语言与标准库 | go1.27 | BSD-3-Clause | 运行时，以及 `crypto`、`net`、`net/http` 等标准库 | <https://go.dev/> |

版本以 `go.mod` / `go.sum` 为准。核对依赖树与许可证文本：

```powershell
go list -m all        # 依赖树（含版本）
go env GOMODCACHE     # 依赖源码与各自的 LICENSE 就在这个目录下
```

上述许可证都属于宽松（permissive）协议，与本项目采用的 GPL-3.0 兼容：
Apache-2.0 / BSD-2-Clause / BSD-3-Clause / MIT 的代码都可以被 GPL-3.0 作品静态链接与再分发，
条件是**保留其版权与许可证声明**（本文件即为满足该要求而存在）。

## 开发与构建工具（不进入发布产物）

| 工具 | 许可证 | 用途 |
| --- | --- | --- |
| `github.com/akavel/rsrc` | MIT | 把 `internal/icon/icon.ico` 编译成 exe 资源段（`cmd/tongtu/rsrc_windows_amd64.syso`） |
| `honnef.co/go/tools`（staticcheck） | MIT | 静态检查（`go run honnef.co/go/tools/cmd/staticcheck@latest ./...`） |

## 数据来源与公共服务（非代码依赖，均可替换或离线）

- **GitHub520**（<https://github.com/521xueweihan/GitHub520>）：社区维护的 GitHub 优选 IP 列表，
  本项目的「远程优选 IP」默认读取它的公开数据（`raw.hellogithub.com`）。
  该仓库**未声明开源许可证**，因此本项目只在**运行时读取**其公开数据、不再分发其内容；
  你可以在配置里把 `dns.remote_hosts` 换成自己的数据源。
- **公共 DoH 解析端点**：`doh.pub`（腾讯）、`dns.alidns.com` / `223.5.5.5`（阿里）、
  `1.1.1.1`（Cloudflare）、`8.8.8.8`（Google）。这些是各厂商提供的公开服务，按其各自条款使用；
  本工具的解析可以完全离线配置（`dns.static_hosts` + 自定义 `dns.endpoints`）。
