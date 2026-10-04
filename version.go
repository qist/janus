package main

import "fmt"

// 版本信息。构建时用 -ldflags 注入（见 Makefile），源码里保持 dev 默认值：
//
//	go build -ldflags "-X main.Version=v0.1.0 -X main.Commit=abc1234 -X main.Date=2026-10-04T00:00:00Z"
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// versionString 返回人类可读的一行版本信息。
func versionString() string {
	return fmt.Sprintf("Janus %s (commit %s, built %s)", Version, Commit, Date)
}
