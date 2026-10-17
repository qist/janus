// Package prompt 负责把协议层消息压平成上游 agent 能吃的 prompt 文本，
// 以及生成/剥离工具活动注释。
//
// 这一层与 HTTP 无关：三套对外 API（Chat / Responses / Anthropic）都复用同一套
// 压平规则，保证同一条对话在不同协议下发给上游的内容一致。
package prompt

import (
	"fmt"
	"strings"

	"janus/internal/protocol/shared"
	"janus/internal/upstream"
)

// annotationPattern 是本包注入正文的工具注释标记。
const annotationPattern = "<opencode-tool>"

// Flatten 把多条 messages 压平成 OpenCode prompt 的一段纯文本。
//
// OpenCode 的 prompt 只接受 string，所以必须自己做角色分段。
// 历史重放时，只有"新增的那部分"会被压平发送。
func Flatten(msgs []shared.ChatMessage) string {
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
func FlattenDelta(delta []shared.ChatMessage) string {
	if len(delta) == 1 && delta[0].Role == "user" && len(delta[0].ToolCalls) == 0 {
		// 去掉 agent 注入的工具注释，避免污染上游上下文
		return StripToolAnnotations(delta[0].Content.Text)
	}
	return Flatten(delta)
}

// StripToolAnnotations 移除本桥注入的 <opencode-tool> 注释块。
func StripToolAnnotations(s string) string {
	if !strings.Contains(s, annotationPattern) {
		return s
	}
	var sb strings.Builder
	rest := s
	for {
		i := strings.Index(rest, annotationPattern)
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
// MCP 工具的真实调用已由工具桥以 tool_calls 下发给客户端，注解属噪声。
const mcpPlaceholderTool = "execute"

// IsMCPPlaceholderTool 判断事件里的工具名是不是 MCP 的占位名。
func IsMCPPlaceholderTool(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), mcpPlaceholderTool)
}

// ToolAnnotation 生成注入 content 的工具活动注释（用于展示调用入参）。
func ToolAnnotation(name, input string) string {
	return annotation(name, input, 400)
}

// ToolResultAnnotation 生成工具结果的注释。结果往往比入参长（文件内容、
// 命令输出、子代理结论），所以单独给更大的上限。
func ToolResultAnnotation(name, result string) string {
	return annotation(name, result, 1500)
}

func annotation(name, body string, max int) string {
	in := strings.TrimSpace(body)
	if len([]rune(in)) > max {
		in = string([]rune(in)[:max]) + "…"
	}
	in = strings.ReplaceAll(in, "</opencode-tool>", "")
	return "\n\n" + annotationPattern + " " + name + ": " + in + "</opencode-tool>\n\n"
}

// ToolResultText 从工具结果事件里提取可读文本（优先 content，其次 error）。
func ToolResultText(d upstream.EvtToolResult) string {
	var sb strings.Builder
	for _, c := range d.Content {
		if c.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(c.Text)
	}
	s := strings.TrimSpace(sb.String())
	if s == "" {
		s = strings.TrimSpace(d.Error)
	}
	return s
}
