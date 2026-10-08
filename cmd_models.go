package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

// runModels 打印 Janus（= 上游 OpenCode）可用的模型，帮用户挑一个填进
// BRIDGE_DEFAULT_MODEL。用法：janus models [--json]
//
// 设计意图：普通用户通常只想指定一个订阅里的模型，不需要逐个映射。
// 配一个 BRIDGE_DEFAULT_MODEL，所有 Claude Code 档位都走它；这个命令就是
// 用来"从一堆订阅模型里挑那一个"的。
func runModels(args []string) {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "-h", "--help", "help":
			fmt.Println("用法: janus models [--json]")
			fmt.Println("列出上游所有可用模型（含价格/能力），用于挑选 BRIDGE_DEFAULT_MODEL。")
			return
		}
	}

	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error: "+err.Error())
		os.Exit(2)
	}
	log := NewLogger("warn")

	if cfg.UpstreamAuto {
		// CLI 只查询模型：优先复用已在跑的实例，避免每次 `janus models` 都多拉一个。
		cfg.ReuseExternal = true
		// 与主程序一致：先发现已在跑的 OpenCode，没有就按配置自动拉起
		dctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		ep, derr := EnsureUpstream(dctx, log, &cfg, cfg.AutostartUpstream)
		cancel()
		if derr != nil {
			fmt.Fprintln(os.Stderr, "upstream auto-discovery/autostart failed: "+derr.Error())
			fmt.Fprintln(os.Stderr, "提示：先安装/启动 OpenCode，或设置 OPENCODE_URL / OPENCODE_PASSWORD。")
			os.Exit(1)
		}
		cfg.Upstream, cfg.Username, cfg.Password = ep.Base, ep.User, ep.Pass
		if ep.Spawned {
			// CLI 自己拉起的实例用完即收，避免 `janus models` 成为孤儿制造机。
			defer func() { terminateUpstream(ep.PID, log) }()
		}
	}

	up := NewUpstream(cfg, log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := up.ListModels(ctx, cfg.Directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot list models: "+err.Error())
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "上面没有返回任何模型（检查 OPENCODE_URL / directory）")
		os.Exit(1)
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].ProviderID != list[j].ProviderID {
			return list[i].ProviderID < list[j].ProviderID
		}
		return list[i].ID < list[j].ID
	})

	if asJSON {
		out := make([]map[string]any, 0, len(list))
		for _, m := range list {
			out = append(out, map[string]any{
				"model":         m.ProviderID + "/" + m.ID,
				"provider":      m.ProviderID,
				"id":            m.ID,
				"enabled":       m.Enabled,
				"tools":         m.Capabilities.Tools,
				"input":         m.Capabilities.Input,
				"context":       m.Limit.Context,
				"cost_input":    firstCost(m).Input,
				"cost_output":   firstCost(m).Output,
				"free":          isFreeModel(m),
				"usable_by_api": usableByAPI(m),
			})
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "模型\t输入$/M\t输出$/M\t上下文\t工具\t可用\t说明")
	for _, m := range list {
		c := firstCost(m)
		note := ""
		switch {
		case !m.Enabled:
			note = "已禁用"
		case m.ProviderID == "opencode" && isFreeModel(m):
			note = "免费额度经 API 会 403"
		case !m.Capabilities.Tools:
			note = "不支持工具"
		}
		fmt.Fprintf(tw, "%s/%s\t%.2f\t%.2f\t%s\t%s\t%s\t%s\n",
			m.ProviderID, m.ID, c.Input, c.Output,
			humanContext(m.Limit.Context), yesNo(m.Capabilities.Tools),
			yesNo(usableByAPI(m)), note)
	}
	tw.Flush()

	// 把 Claude Code 档位的自动挑选结果直接亮出来，用户一眼就知道"不配也行"。
	fmt.Println()
	fmt.Println("Claude Code 档位（未配 BRIDGE_MODEL_MAP 时的启发式自动挑选）：")
	for _, t := range anthropicTierAliases {
		if ref, ok := pickTierModel(list, t.Tier); ok {
			fmt.Printf("  %-18s → %s\n", t.Alias, ref.String())
		} else {
			fmt.Printf("  %-18s → （没有可用模型）\n", t.Alias)
		}
	}
	fmt.Println()
	fmt.Println("想固定用某一个模型（多数用户的选择）：")
	fmt.Println("  BRIDGE_DEFAULT_MODEL=<上面某一行的 provider/id>")
	fmt.Println("  # 所有 Claude Code 档位都会走它；也可用 BRIDGE_MODEL_MAP 分档位映射")
}

// usableByAPI 粗略判断模型能否经 Janus 的 API 实际调用。
// 已知 opencode 提供商的免费额度经 API 调用会 403
// （free tier can only be used from within OpenCode），故排除。
func usableByAPI(m OCModel) bool {
	if !m.Enabled || !m.Capabilities.Tools {
		return false
	}
	if m.ProviderID == "opencode" && isFreeModel(m) {
		return false
	}
	return true
}

func firstCost(m OCModel) OCCost {
	if len(m.Cost) == 0 {
		return OCCost{}
	}
	return m.Cost[0]
}

func humanContext(n int) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	default:
		return fmt.Sprint(n)
	}
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
