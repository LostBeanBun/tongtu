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

// Disable 在非 Windows 平台返回 ErrUnsupported。
func Disable(saved *Values) error { return ErrUnsupported }

// Active 始终返回 false。
func Active() bool { return false }
