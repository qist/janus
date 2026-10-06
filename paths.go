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
// Linux/macOS 保持原有的 XDG 约定不变；Windows 用 %LOCALAPPDATA%，避免在
// 用户主目录下生成 `C:\Users\<name>\.local\share\...` 这种非 Windows 习惯的路径。

// dataHome 返回 per-user 数据目录：
//
//	Windows: %LOCALAPPDATA%
//	其它:    $XDG_DATA_HOME 或 ~/.local/share
func dataHome() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
	}
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
	if h := dataHome(); h != "" {
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
	if h := dataHome(); h != "" {
		return filepath.Join(h, "opencode", "opencode.db")
	}
	return ""
}

// defaultWorkspacesDir 远程场景 per-scope 的中性工作目录根（BRIDGE_WORKSPACES_DIR 未配置时）。
//
// Linux 保持 /var/lib/janus/workspaces（系统级、与 systemd 部署一致）；
// Windows 没有 /var/lib，改用 %LOCALAPPDATA%\janus\workspaces。
func defaultWorkspacesDir() string {
	if runtime.GOOS == "windows" {
		if d := janusDataDir(); d != "" {
			return filepath.Join(d, "workspaces")
		}
	}
	return "/var/lib/janus/workspaces"
}
