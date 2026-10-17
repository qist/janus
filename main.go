// janus —— OpenCode ↔ OpenAI/Anthropic/Responses 兼容桥。
//
// 本文件是仓库根的可执行入口（package main）。它只负责命令行解析、
// 配置加载与上游探活引导，真正的 HTTP 接口实现全部在 internal/api。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"janus/internal/api"
	"janus/internal/channel/opencode"
	"janus/internal/channel/workbuddy"
	"janus/internal/cli"
	"janus/internal/config"
	"janus/internal/db"
	"janus/internal/logx"
	"janus/internal/version"
)

func main() {
	// 版本查询：janus -v / --version / version
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Println(version.String())
			return
		case "models":
			// 列出上游可用模型，帮用户挑 BRIDGE_DEFAULT_MODEL
			cli.RunModels(os.Args[2:])
			return
		case "workbuddy-decrypt":
			// 在装有 WorkBuddy 桌面版的机器上把 5.6 加密凭据解成明文
			workbuddy.RunDecrypt(os.Args[2:])
			return
		}
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		// 配置写错就立刻退出，别带着半截配置跑
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(2)
	}
	log := logx.NewLogger(cfg.LogLevel)
	log.Infof("%s", version.String())
	if cfg.ConfigFile != "" {
		log.Infof("config file loaded: %s", cfg.ConfigFile)
	}

	// WorkBuddy provider：必须在 EnsureUpstream 之前初始化——OpenCode 拉起时
	// 生成的内联配置要包含 workbuddy provider（模型目录此刻就得就绪）。
	// 上下文档位（workbuddy_context）持久化在 DB 里，也必须赶在内联配置
	// 注入前读到，否则重启后回到 env 默认值。
	if !db.MemoryDSN(cfg.DBPath) {
		if d, err := db.Open(cfg.DBPath); err == nil {
			if v, ok := d.GetSetting("workbuddy_context"); ok && (v == "max" || v == "default") {
				cfg.WorkBuddyContext = v
			}
			if v, ok := d.GetSetting("opencode_context"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
					cfg.OpenCodeContext = n
				}
			}
			d.Close()
		}
	}
	workbuddy.Init(&cfg, log)

	// 本次进程拉起的 upstream PID（退出时回收，见 Server.Run）。
	var spawnedPID int
	if cfg.UpstreamAuto {
		// 自动发现 + 必要时自己拉起：OpenCode 桌面端端口/密码每次都变，
		// 没有在跑的（比如桌面端没开）就以随机端口拉一个，读 /proc 找到活的那个。
		dctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		ep, derr := opencode.Ensure(dctx, log, &cfg, cfg.AutostartUpstream, api.MCPNamePrefix)
		cancel()
		if derr != nil {
			log.Warnf("upstream auto-discovery failed: %v (will keep retrying in background)", derr)
		} else {
			cfg.Upstream, cfg.Username, cfg.Password = ep.Base, ep.User, ep.Pass
			if ep.Spawned {
				spawnedPID = ep.PID
			}
			log.Infof("auto-discovered OpenCode at %s (pid %d)", ep.Base, ep.PID)
		}
	} else if cfg.Password == "" {
		log.Errorf("OPENCODE_PASSWORD / OPENCODE_SERVER_PASSWORD is not set, and OPENCODE_URL is fixed")
		os.Exit(1)
	}
	if cfg.APIKey == "" {
		log.Warnf("BRIDGE_API_KEY is empty — anyone who can reach %s can spend your quota", cfg.Addr)
	}

	srv := api.NewServer(cfg, log)
	srv.SetManagedUpstream(spawnedPID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Run(ctx)
}
