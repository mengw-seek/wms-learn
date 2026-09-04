package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gowms/internal/app"
	"gowms/internal/bootstrap"
	"gowms/internal/pkg/config"
	"gowms/internal/pkg/log"
)

func main() {
	// 1. 加载配置
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}
	// 2. 日志
	log.Init(cfg.Log.Level)
	logger := log.L()

	// 3. DB / Redis / 迁移
	db, err := bootstrap.InitDB(cfg)
	if err != nil {
		logger.Error("init db failed", "err", err)
		os.Exit(1)
	}
	if err := bootstrap.Migrate(db); err != nil {
		logger.Error("migrate failed", "err", err)
		os.Exit(1)
	}
	rdb := bootstrap.InitRedis(cfg)

	// 4. 组装依赖
	application, err := app.New(cfg, db, rdb)
	if err != nil {
		logger.Error("assemble app failed", "err", err)
		os.Exit(1)
	}
	// 5. 后台任务：Excel 导入悬挂补偿（每 2 分钟扫描；随服务关停退出）
	compensatorCtx, compensatorCancel := context.WithCancel(context.Background())
	defer compensatorCancel()
	application.InboundService.StartCompensator(compensatorCtx)

	// 6. 启动 HTTP 服务（优雅关停）
	router := application.NewRouter()
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
		Handler: router,
	}
	go func() {
		logger.Info("wms server started", "port", cfg.Server.Port, "mode", cfg.Server.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown failed", "err", err)
	}
	compensatorCancel() // HTTP 关停后停止补偿扫描
	logger.Info("bye")
}
