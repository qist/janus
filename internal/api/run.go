package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"
)

// Run 启动 HTTP 服务并阻塞直到 ctx 被取消（信号/进程退出）。
//
// 它负责把上游事件流、探活、模型预热、会话回收、权限兜底扫描等后台协程都拉起来，
// 然后监听 cfg.Addr。真正读配置 / 解析命令行参数的入口在仓库根的 main 包，
// 这里只关心"Server 已经构造好、该怎么跑"。
func (s *Server) Run(ctx context.Context) {
	cfg := s.cfg
	log := s.log

	// 事件流：单连接长驻
	go s.bus.Run(ctx)

	// 上游探活 + 端点自动切换（扛住 OpenCode 重启）
	go s.opencode.Watch(ctx, cfg.UpstreamAuto)

	// 启动自检
	go func() {
		time.Sleep(500 * time.Millisecond)
		ic, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := s.up.Ping(ic); err != nil {
			log.Warnf("upstream %s not reachable yet: %v", s.up.Base(), err)
		} else {
			log.Infof("upstream %s ok", s.up.Base())
		}
	}()

	// 模型列表预热：OpenCode 刚起来时模型列表可能还是空的，重试几次再放弃。
	go func() {
		const attempts = 6
		var err error
		for i := 1; i <= attempts; i++ {
			mc, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err = s.models.Get(mc, s.up, cfg.Directory, true)
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
	go s.janitor(ctx)

	// 权限请求兜底扫描：agent 会在两次请求之间异步发起权限请求，
	// 事件流可能落空档，定时扫一遍待审批的、属于本桥的请求并自动应答。
	go s.sweepPermissions(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要保持打开
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Infof("janus listening on %s (upstream=%s, dir=%s, agent=%s)",
			cfg.Addr, s.up.Base(), cfg.Directory, cfg.Agent)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Infof("shutting down…")
		s.bus.Close()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			log.Warnf("shutdown: %v", err)
		}
		// 收掉自己拉起的 opencode（没它的话 nohup/裸进程部署下会留孤儿）。
		// 注意放在 HTTP 收尾之后、进程退出之前。
		s.opencode.StopManaged()
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.opencode.StopManaged()
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
				// 会话映射 / 历史快照保留时长（默认 7 天）；<0 = 永不清理
				if ttl := s.cfg.ConvTTL; ttl >= 0 {
					s.db.GCConvs(time.Now().Add(-ttl).Unix())
				}
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
		s.db.ClearSession(sid)
	}
}
