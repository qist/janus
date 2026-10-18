// janus —— 厂商无关的模型网关 + 可插拔 Agent 控制面。
//
// 本文件是仓库根的可执行入口（package main）。它只负责：
//  1. bootstrap（env：监听地址 / 数据库 / 主密钥 / 数据目录 / 日志级别）；
//  2. 打开数据库、加载运行期设置、生成首启 admin key；
//  3. 启动 HTTP 服务。
//
// 其余配置全部在数据库里，经 /api/v1/* 或 Web 面板修改。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"janus/internal/api"
	"janus/internal/channel/opencode"
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
			// 列出上游可用模型（走 OpenCode 目录），帮用户挑默认模型
			cli.RunModels(os.Args[2:])
			return
		}
	}

	cfg := config.Load()
	log := logx.NewLogger(cfg.LogLevel)
	log.Infof("%s", version.String())

	// ---- bootstrap：打开数据库 ----
	var store *db.Store
	if db.MemoryDSN(cfg.DBPath) {
		log.Warnf("JANUS_DB is empty/memory — persistence disabled, admin key will not survive restart")
	} else {
		// 主密钥：env > <data_dir>/master.key（首启生成 0600）
		source, err := db.ConfigureKey(cfg.DBKey, cfg.KeyFilePath())
		if err != nil {
			log.Errorf("cannot configure credential key: %v", err)
			os.Exit(2)
		}
		log.Infof("credential key: %s", source)

		opened, err := db.Open(cfg.DBPath)
		if err != nil {
			log.Errorf("cannot open database %s: %v", cfg.DBPath, err)
			os.Exit(2)
		}
		store = opened
		defer store.Close()

		// 运行期设置（settings.runtime）覆盖 bootstrap 默认值。
		if raw, ok := store.GetSettingRaw(db.SettingRuntime); ok {
			if err := cfg.ApplySettings(raw, json.Unmarshal); err != nil {
				log.Warnf("runtime settings ignored: %v", err)
			}
		}

		// 首启引导：admin 账号 + admin key（只打印一次，并写 bootstrap.key）。
		boot, err := store.EnsureBootstrap(cfg.BootstrapToken)
		if err != nil {
			log.Errorf("bootstrap failed: %v", err)
			os.Exit(2)
		}
		if boot.Created {
			if err := db.WriteBootstrapFile(cfg.KeyFilePath(), boot); err != nil {
				log.Warnf("cannot write bootstrap.key: %v", err)
			}
			if boot.Username != "" {
				log.Infof("[init] admin user: %s", boot.Username)
				log.Infof("[init] admin password: %s", boot.Password)
			}
			if boot.APIKey != "" {
				log.Infof("[init] admin key: %s", boot.APIKey)
			}
			log.Infof("[init] credentials written to %s", cfg.KeyFilePath())
		}
		log.Infof("persistence enabled: %s", cfg.DBPath)
	}

	// ---- 上游（OpenCode agent runtime）----
	// P2 之前 OpenCode 仍是唯一实现的 AgentRuntime，端点仍在 settings 里。
	var spawnedPID int
	if cfg.UpstreamAuto {
		// 自动发现 + 必要时自己拉起：OpenCode 桌面端端口/密码每次都变。
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
		log.Errorf("upstream password is not set, and upstream is fixed: configure it in /api/v1/settings")
		os.Exit(1)
	}

	srv := api.NewServer(cfg, log)
	srv.SetManagedUpstream(spawnedPID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Run(ctx)
}
