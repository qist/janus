package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"janus/internal/channel/opencode"
	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/logx"
	"janus/internal/mcpbridge"
	"janus/internal/modelx"
	"janus/internal/upstream"
)

// RunModels 打印上游（OpenCode）可用的模型，帮用户挑一个填进默认模型。
// 用法：janus models [--json]
//
// 运行期设置全部在数据库里：这里先读 settings.runtime，再回落到 bootstrap 默认值。
func RunModels(args []string) {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "-h", "--help", "help":
			fmt.Println("用法: janus models [--json]")
			fmt.Println("列出上游所有可用模型（含价格/能力），用于挑选默认模型。")
			return
		}
	}

	cfg := config.Load()
	if !db.MemoryDSN(cfg.DBPath) {
		if opened, err := db.Open(cfg.DBPath); err == nil {
			if raw, ok := opened.GetSettingRaw(db.SettingRuntime); ok {
				if err := cfg.ApplySettings(raw, json.Unmarshal); err != nil {
					fmt.Fprintln(os.Stderr, "runtime settings ignored: "+err.Error())
				}
			}
			opened.Close()
		}
	}
	log := logx.NewLogger("warn")

	if cfg.UpstreamAuto {
		// CLI 只查询模型：优先复用已在跑的实例，避免每次 `janus models` 都多拉一个。
		cfg.ReuseExternal = true
		// 与主程序一致：先发现已在跑的 OpenCode，没有就按配置自动拉起
		dctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		ep, derr := opencode.Ensure(dctx, log, &cfg, cfg.AutostartUpstream, mcpbridge.NamePrefix)
		cancel()
		if derr != nil {
			fmt.Fprintln(os.Stderr, "upstream auto-discovery/autostart failed: "+derr.Error())
			fmt.Fprintln(os.Stderr, "提示：先安装/启动 OpenCode，或在 /api/v1/settings 配置 upstream。")
			os.Exit(1)
		}
		cfg.Upstream, cfg.Username, cfg.Password = ep.Base, ep.User, ep.Pass
		if ep.Spawned {
			// CLI 自己拉起的实例用完即收，避免 `janus models` 成为孤儿制造机。
			defer func() { opencode.Terminate(ep.PID, log) }()
		}
	}

	up := upstream.New(cfg, log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	list, err := up.ListModels(ctx, cfg.Directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot list models: "+err.Error())
		os.Exit(1)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "上游没有返回任何模型（检查 settings.upstream / directory）")
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
				"free":          modelx.IsFree(m),
				"usable_by_api": modelx.UsableByAPI(m),
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
		case m.ProviderID == "opencode" && modelx.IsFree(m):
			note = "免费额度经 API 会 403"
		case !m.Capabilities.Tools:
			note = "不支持工具"
		}
		fmt.Fprintf(tw, "%s/%s\t%.2f\t%.2f\t%s\t%s\t%s\t%s\n",
			m.ProviderID, m.ID, c.Input, c.Output,
			humanContext(m.Limit.Context), yesNo(m.Capabilities.Tools),
			yesNo(modelx.UsableByAPI(m)), note)
	}
	tw.Flush()

	// 把 Claude Code 档位的自动挑选结果直接亮出来，用户一眼就知道"不配也行"。
	fmt.Println()
	fmt.Println("Claude Code 档位（未配 model_routes 时的启发式自动挑选）：")
	for _, t := range modelx.TierAliases {
		if ref, ok := modelx.PickTier(list, t.Tier); ok {
			fmt.Printf("  %-18s → %s\n", t.Alias, ref.String())
		} else {
			fmt.Printf("  %-18s → （没有可用模型）\n", t.Alias)
		}
	}
	fmt.Println()
	fmt.Println("想固定用某一个模型（多数用户的选择）：")
	fmt.Println("  PATCH /api/v1/settings {\"default_model\": \"<上面某一行的 provider/id>\"}")
	fmt.Println("  # 所有 Claude Code 档位都会走它；也可用 /api/v1/routes 分档位映射")
}

func firstCost(m upstream.OCModel) upstream.OCCost {
	if len(m.Cost) == 0 {
		return upstream.OCCost{}
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
