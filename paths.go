package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// paths.go —— 跨平台默认路径。
//
// 默认值只在「没显式配置」时生效；设了 BRIDGE_DB / BRIDGE_WORKSPACES_DIR /
// OPENCODE_DB 时一律以配置为准。
//
// janus 自身的数据目录跟随各平台的惯例：
//
//	Linux:   $XDG_DATA_HOME 或 ~/.local/share   （不变）
//	macOS:   ~/Library/Application Support      （macOS 惯例）
//	Windows: %LOCALAPPDATA%                     （Windows 惯例）
//
// OpenCode 的库（OPENCODE_DB）另算：它是跨平台应用，沿用 XDG 约定，
// macOS 上也用 ~/.local/share/opencode，不套用 macOS 目录，避免猜错导致 /v1/usage 找不到库。

// janusDataHome janus 自身数据的根目录。
func janusDataHome() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "Library", "Application Support")
		}
	}
	return xdgDataHome()
}

// xdgDataHome 按 XDG 约定给出数据根（Linux 默认，也是 OpenCode 的约定）。
func xdgDataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share")
}

// janusDataDir janus 的默认数据目录（DB、workspaces 等）。
func janusDataDir() string {
	if h := janusDataHome(); h != "" {
		return filepath.Join(h, "janus")
	}
	return ""
}

// defaultDBPath 默认持久化库路径（BRIDGE_DB 未配置时）。
func defaultDBPath() string {
	if d := janusDataDir(); d != "" {
		return filepath.Join(d, "janus.db")
	}
	return ""
}

// defaultOpencodeDB 定位 OpenCode 的 SQLite 库（OPENCODE_DB 未配置时）。
func defaultOpencodeDB() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "opencode", "opencode.db")
		}
	}
	if h := xdgDataHome(); h != "" {
		return filepath.Join(h, "opencode", "opencode.db")
	}
	return ""
}

// defaultWorkspacesDir 远程场景 per-scope 的中性工作目录根（BRIDGE_WORKSPACES_DIR 未配置时）。
//
// Linux 保持 /var/lib/janus/workspaces（系统级、与 systemd 部署一致）；
// macOS / Windows 没有这个系统目录，改用各自的数据目录。
func defaultWorkspacesDir() string {
	switch runtime.GOOS {
	case "windows", "darwin":
		if d := janusDataDir(); d != "" {
			return filepath.Join(d, "workspaces")
		}
	}
	return "/var/lib/janus/workspaces"
}
