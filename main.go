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
		// 自动发现：OpenCode 桌面端端口/密码每次都变，读 /proc 找到活的那个
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ep, derr := Discover(dctx, probeEndpoint)
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

	// 模型列表预热
	go func() {
		mc, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if _, err := srv.models.Get(mc, srv.up, cfg.Directory, true); err != nil {
			log.Warnf("model list warmup failed: %v", err)
		}
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
				// 会话映射保留 7 天（够跨重启续链，也不至于无限增长）
				s.db.gcConvs(time.Now().Add(-7 * 24 * time.Hour).Unix())
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
		return
	}
	s.log.Infof("janitor deleted session %s", sid)
}

var _ = fmt.Sprintf
