package main

import (
	"fmt"
	"strings"
)

// Flatten 把 OpenAI 的多条 messages 压平成 OpenCode prompt 的一段纯文本。
//
// OpenCode 的 prompt 只接受 string，所以必须自己做角色分段。
// 历史重放时，只有"新增的那部分"会被压平发送。

const toolAnnotationPattern = "<opencode-tool>"

func Flatten(msgs []ChatMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		body := strings.TrimRight(m.Content.Text, "\n")
		switch m.Role {
		case "system":
			if body == "" {
				continue
			}
			sb.WriteString("[SYSTEM]\n")
			sb.WriteString(body)
			sb.WriteString("\n\n")
		case "user":
			sb.WriteString("[USER]\n")
			sb.WriteString(body)
			sb.WriteString("\n\n")
		case "assistant":
			if len(m.ToolCalls) > 0 {
				sb.WriteString("[ASSISTANT TOOL_CALLS]\n")
				for _, tc := range m.ToolCalls {
					name, args := "", "{}"
					if tc.Function != nil {
						name = tc.Function.Name
						args = tc.Function.Arguments
					}
					fmt.Fprintf(&sb, "%s %s\n", name, args)
				}
				sb.WriteString("\n")
			}
			if body != "" {
				sb.WriteString("[ASSISTANT]\n")
				sb.WriteString(body)
				sb.WriteString("\n\n")
			}
		case "tool":
			sb.WriteString("[TOOL RESULT")
			if m.Name != "" {
				sb.WriteString(" " + m.Name)
			}
			sb.WriteString("]\n")
			sb.WriteString(body)
			sb.WriteString("\n\n")
		default:
			if body != "" {
				fmt.Fprintf(&sb, "[%s]\n%s\n\n", strings.ToUpper(m.Role), body)
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// FlattenDelta 只压平新增部分；如果新增部分的第一条就是 user，
// 就不必再打角色标签，读起来更自然。
func FlattenDelta(delta []ChatMessage) string {
	if len(delta) == 1 && delta[0].Role == "user" && len(delta[0].ToolCalls) == 0 {
		// 去掉 agent 注入的工具注释，避免污染上游上下文
		return stripToolAnnotations(delta[0].Content.Text)
	}
	return Flatten(delta)
}

// stripToolAnnotations 移除本 bridge 注入的 <opencode-tool> 注释块。
func stripToolAnnotations(s string) string {
	if !strings.Contains(s, toolAnnotationPattern) {
		return s
	}
	var sb strings.Builder
	rest := s
	for {
		i := strings.Index(rest, toolAnnotationPattern)
		if i < 0 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:i])
		j := strings.Index(rest[i:], "</opencode-tool>")
		if j < 0 {
			// 没找到闭合标签，丢掉残缺部分
			break
		}
		rest = rest[i+j+len("</opencode-tool>"):]
	}
	return strings.TrimSpace(sb.String())
}

// mcpPlaceholderTool 是 OpenCode 事件里给 MCP 工具起的通用名。
// MCP 工具的真实调用已由 tool bridge 以 tool_calls 下发给客户端，注解属噪声。
const mcpPlaceholderTool = "execute"

func isMCPPlaceholderTool(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), mcpPlaceholderTool)
}

// ToolAnnotation 生成注入 content 的工具活动注释。
func ToolAnnotation(name, input string) string {
	in := strings.TrimSpace(input)
	if len([]rune(in)) > 400 {
		in = string([]rune(in)[:400]) + "…"
	}
	in = strings.ReplaceAll(in, "</opencode-tool>", "")
	return "\n\n" + toolAnnotationPattern + " " + name + ": " + in + "</opencode-tool>\n\n"
}
