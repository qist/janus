package main

import (
	"strings"
	"testing"
)

// RunCommand 类工具的描述要追加异步引导（绕开客户端同步执行器楔死窗口）；
// 其余工具、以及配套工具被禁用时，原样返回。
func TestSteerAsyncCommandDesc(t *testing.T) {
	note := asyncCommandSteeringNote

	// 命中触发工具（大小写不敏感）→ 追加引导
	for _, name := range []string{"RunCommand", "run_command", "ExecuteCommand"} {
		got := steerAsyncCommandDesc(true, name, "run a command")
		if !strings.Contains(got, "run a command") || !strings.Contains(got, note) {
			t.Fatalf("%s: 引导未追加: %q", name, got)
		}
		if !strings.HasPrefix(got, "run a command") {
			t.Fatalf("%s: 原描述应在最前: %q", name, got)
		}
	}
	// 非触发工具 → 原样
	if got := steerAsyncCommandDesc(true, "Read", "read a file"); got != "read a file" {
		t.Fatalf("Read 描述被改: %q", got)
	}
	// 配套工具禁用 → 原样（避免引导模型调用不存在的状态查询工具）
	if got := steerAsyncCommandDesc(false, "RunCommand", "run a command"); got != "run a command" {
		t.Fatalf("禁用配套时不应追加: %q", got)
	}
	// 重复调用幂等（ tools/list 每轮都走这里，描述不能越拼越长）
	once := steerAsyncCommandDesc(true, "RunCommand", "run a command")
	twice := steerAsyncCommandDesc(true, "RunCommand", once)
	if once != twice {
		t.Fatal("重复追加应幂等")
	}
}
