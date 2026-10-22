//go:build !windows

// Package autostart 在非 Windows 平台上不提供开机自启。
package autostart

import "errors"

var errUnsupported = errors.New("开机自启仅支持 Windows")

// Info 描述当前的开机自启配置。
type Info struct {
	RunCmd  string `json:"run_cmd"`
	TaskCmd string `json:"task_cmd"`
}

// IsAdmin 在非 Windows 平台恒为 false。
func IsAdmin() bool { return false }

// CommandLine 生成自启命令行。
func CommandLine(exe string) string { return exe + " start --hidden" }

// EnableRun 在非 Windows 平台返回不支持。
func EnableRun(string) error { return errUnsupported }

// DisableRun 在非 Windows 平台为空实现。
func DisableRun() error { return nil }

// RunCommand 在非 Windows 平台返回空。
func RunCommand() (string, error) { return "", nil }

// EnableTask 在非 Windows 平台返回不支持。
func EnableTask(string) error { return errUnsupported }

// DisableTask 在非 Windows 平台为空实现。
func DisableTask() error { return nil }

// TaskCommand 在非 Windows 平台返回空。
func TaskCommand() string { return "" }

// Query 在非 Windows 平台返回空配置。
func Query() (Info, error) { return Info{}, nil }

// RecoverCommand 在非 Windows 平台也生成同样的命令行（便于测试与诊断）。
func RecoverCommand(exe string) string { return `"` + exe + `" __recover` }

// SetRunOnce 在非 Windows 平台返回不支持。
func SetRunOnce(string) error { return errUnsupported }

// ClearRunOnce 在非 Windows 平台为空实现。
func ClearRunOnce() error { return nil }

// RunOnceCommand 在非 Windows 平台返回空。
func RunOnceCommand() (string, error) { return "", nil }
