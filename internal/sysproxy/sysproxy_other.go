//go:build !windows

// 非 Windows 平台的占位实现，保证交叉编译与单元测试可在其他平台进行。
package sysproxy

import "errors"

// ErrUnsupported 表示当前平台不支持系统代理接管。
var ErrUnsupported = errors.New("系统代理接管仅支持 Windows")

// Values 是平台无关的占位类型。
type Values struct{}

// StatePath 返回占位路径。
func StatePath() string { return "" }

// SavedAddr 非 Windows 平台没有系统代理接管状态。
func SavedAddr() (string, bool) { return "", false }

// Enable 在非 Windows 平台返回 ErrUnsupported。
func Enable(addr string) (*Values, error) { return nil, ErrUnsupported }

// Outcome 说明 Disable 实际做了什么（非 Windows 平台的占位实现）。
type Outcome int

const (
	// Restored 表示已按原值还原系统代理。
	Restored Outcome = iota
	// NotHeld 表示系统代理不归我们管，注册表未被改动。
	NotHeld
	// DisabledDeadProxy 表示状态文件损坏，只关掉了指向失效本地端口的代理。
	DisabledDeadProxy
)

// Disable 在非 Windows 平台返回 ErrUnsupported。
func Disable(saved *Values) (Outcome, error) { return NotHeld, ErrUnsupported }

// Retarget 在非 Windows 平台返回 ErrUnsupported（没有系统代理可换口）。
func Retarget(addr string) (bool, error) { return false, ErrUnsupported }

// Active 始终返回 false。
func Active() bool { return false }

// Held 在非 Windows 平台始终报告「未持有」。
func Held(addr string) (bool, error) { return false, nil }
