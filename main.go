package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 版本查询：janus -v / --version / version
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Println(versionString())
			return
		case "models":
			// 列出上游可用模型，帮用户挑 BRIDGE_DEFAULT_MODEL
			runModels(os.Args[2:])
			return
		}
	}

	cfg, err := LoadConfig()
	if err != nil {
		// 配置写错就立刻退出，别带着半截配置跑
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(2)
	}
	log := NewLogger(cfg.LogLevel)
	log.Infof("%s", versionString())
	if cfg.ConfigFile != "" {
		log.Infof("config file loaded: %s", cfg.ConfigFile)
	}

	if cfg.UpstreamAuto {
		// 自动发现 + 必要时自己拉起：OpenCode 桌面端端口/密码每次都变，
		// 没有在跑的（比如桌面端没开）就以随机端口拉一个，读 /proc 找到活的那个。
		dctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		ep, derr := EnsureUpstream(dctx, log, &cfg, cfg.AutostartUpstream)
		cancel()
		if derr != nil {
			log.Warnf("upstream auto-discovery failed: %v (will keep retrying in background)", derr)
		} else {
			cfg.Upstream, cfg.Username, cfg.Password = ep.Base, ep.User, ep.Pass
			log.Infof("auto-discovered OpenCode at %s (pid %d)", ep.Base, ep.PID)
		}
	} else if cfg.Password == "" {
		log.Errorf("OPENCODE_PASSWORD / OPENCODE_SERVER_PASSWORD is not set, and OPENCODE_URL is fixed")
		os.Exit(1)
	}
	if cfg.APIKey == "" {
		log.Warnf("BRIDGE_API_KEY is empty — anyone who can reach %s can spend your quota", cfg.Addr)
	}

	srv := NewServer(cfg, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 事件流：单连接长驻
	go srv.bus.Run(ctx)

	// 上游探活 + 端点自动切换（扛住 OpenCode 重启）
	go srv.WatchUpstream(ctx, cfg.UpstreamAuto)

	// 启动自检
	go func() {
		time.Sleep(500 * time.Millisecond)
		ic, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := srv.up.Ping(ic); err != nil {
			log.Warnf("upstream %s not reachable yet: %v", srv.up.endpoint(), err)
		} else {
			log.Infof("upstream %s ok", srv.up.endpoint())
		}
	}()

	// 模型列表预热：OpenCode 刚起来时模型列表可能还是空的，重试几次再放弃。
	go func() {
		const attempts = 6
		var err error
		for i := 1; i <= attempts; i++ {
			mc, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err = srv.models.Get(mc, srv.up, cfg.Directory, true)
			cancel()
			if err == nil {
				if i > 1 {
					log.Infof("model list warmup ok (attempt %d)", i)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		log.Warnf("model list warmup failed after %d attempts: %v", attempts, err)
	}()

	// janitor：回收闲置会话
	go srv.janitor(ctx)

	// 权限请求兜底扫描：agent 会在两次请求之间异步发起权限请求，
	// 事件流可能落空档，定时扫一遍待审批的、属于本桥的请求并自动应答。
	go srv.sweepPermissions(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要保持打开
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Infof("janus listening on %s (upstream=%s, dir=%s, agent=%s)",
			cfg.Addr, srv.up.endpoint(), cfg.Directory, cfg.Agent)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Infof("shutting down…")
		srv.bus.Close()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Warnf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("listen: %v", err)
			os.Exit(1)
		}
	}
	log.Infof("bye")
}

// janitor 定期清理 store 里过期的会话，并同步删除上游 session。
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()

	// 被分桶淘汰的 session 立即删；channel 满时靠 TTL 兜底
	drainOrphans := func() {
		for {
			select {
			case sid := <-s.store.orphans:
				s.deleteSession(sid)
			default:
				return
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case sid := <-s.store.orphans:
			s.deleteSession(sid)
		case <-t.C:
			drainOrphans()
			for _, sid := range s.store.GC() {
				s.deleteSession(sid)
			}
			s.sweepTools(ctx)
			s.responses.gc()
			if s.db != nil {
				// 会话映射 / 历史快照保留时长（默认 7 天）
				ttl := s.cfg.ConvTTL
				if ttl <= 0 {
					ttl = 7 * 24 * time.Hour
				}
				s.db.gcConvs(time.Now().Add(-ttl).Unix())
			}
			s.perKeyLimit.GC()
			s.globalLimit.GC()
		}
	}
}

func (s *Server) deleteSession(sid string) {
	if sid == "" {
		return
	}
	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.up.DeleteSession(dctx, sid); err != nil {
		s.log.Debugf("delete session %s: %v", sid, err)
	} else {
		s.log.Infof("janitor deleted session %s", sid)
	}
	// 无论上游删除是否成功，都要清掉库里对该 session 的引用，
	// 否则之后会拿死 session 去打上游（502 Session not found）。
	if s.db != nil {
		s.db.clearSession(sid)
	}
}

var _ = fmt.Sprintf
