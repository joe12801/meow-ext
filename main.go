package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[meow-ext] ")

	// 子命令用第一个位置参数，而不是 flag 子命令树：飞牛的启动脚本要能
	// 一眼看出在做什么（`meow-ext run` / `meow-ext restore`），也便于手工排障。
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configFlag := fs.String("config", "", "配置文件路径（默认 /etc/meow-ext/config.json）")
	timeout := fs.Duration("timeout", 40*time.Second, "启动时等待官方后端就绪的上限")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfgPath := ConfigPath(*configFlag)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误：%v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "run":
		os.Exit(runServer(cfg, cfgPath, *timeout))
	case "restore":
		os.Exit(restoreOfficial(cfg))
	case "status":
		os.Exit(printStatus(cfg))
	case "check":
		os.Exit(preflight(cfg))
	case "version":
		fmt.Println(Version)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n用法：meow-ext [run|restore|status|check|version] [--config 路径]\n", cmd)
		os.Exit(2)
	}
}

// runServer 是常驻主流程：接管入口、起 HTTP 服务、等信号退出。
func runServer(cfg Config, cfgPath string, timeout time.Duration) int {
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "创建状态目录失败：%v\n", err)
		return 1
	}

	// 单实例：两个 run 会抢同一个入口与同一个 gateway socket，
	// 表现为随机一半请求 502。宁可在启动时报错。
	unlockService, err := tryLock(cfg.StateDir + "/service.lock")
	if err != nil {
		fmt.Fprintln(os.Stderr, "已有一个 meow-ext 实例在运行")
		return 1
	}
	defer unlockService()

	srv := NewServer(cfg, cfgPath)
	take := NewTakeover(cfg)

	listener, err := take.Acquire()
	if err != nil {
		fmt.Fprintf(os.Stderr, "接管失败：%v\n", err)
		return 1
	}

	// 接管后先确认官方后端确实能从新路径连上。这一步失败就立刻还回去——
	// 让用户看到"飞牛音乐打不开"比看到"扩展启动失败"糟糕得多。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var probeErr error
	for {
		probeErr = srv.upstream.Probe(ctx)
		if probeErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			listener.Close()
			take.Release()
			fmt.Fprintf(os.Stderr, "官方后端无响应，已放弃接管：%v\n", probeErr)
			return 1
		case <-time.After(500 * time.Millisecond):
		}
	}

	// 管理页走独立的 gateway socket：飞牛桌面按 app/ui/config 的 gatewaySocket
	// 连它，与音乐入口互不影响（管理页打不开时音乐照常能听）。
	gateway, gwErr := listenUnix(cfg.GatewaySocket)
	if gwErr != nil {
		log.Printf("管理页 socket 建立失败（不影响音乐播放）：%v", gwErr)
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：取流是长连接，设了会把正在播放的曲子掐断。
		IdleTimeout: 120 * time.Second,
		ErrorLog:    log.New(os.Stderr, "[http] ", log.LstdFlags),
	}

	if gateway != nil {
		go func() {
			adminSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
			if err := adminSrv.Serve(gateway); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("管理页服务退出：%v", err)
			}
		}()
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// id 映射定期落盘：搜索时只在内存里登记，攒一批再写，
	// 避免每搜一次都产生一次磁盘 IO。
	stopPersist := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := srv.guids.Save(); err != nil {
					log.Printf("保存 id 映射失败：%v", err)
				}
			case <-stopPersist:
				return
			}
		}
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("已启动 版本=%s 官方=%s Meow=%s 在线搜索=%v",
		Version, cfg.UpstreamSocket, cfg.MeowURL, cfg.OnlineEnabled)

	var exitCode int
	select {
	case sig := <-sigCh:
		log.Printf("收到 %s，正在退出", sig)
	case err := <-serveErr:
		log.Printf("HTTP 服务异常退出：%v", err)
		exitCode = 1
	}

	close(stopPersist)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	httpSrv.Shutdown(shutdownCtx)
	if gateway != nil {
		gateway.Close()
	}
	// 先把监听关掉再恢复：否则官方 socket 被挪回 target 时，
	// 我们的监听器还占着同一个路径，会出现两个进程争抢入口的错觉。
	listener.Close()
	if err := srv.guids.Save(); err != nil {
		log.Printf("退出时保存 id 映射失败：%v", err)
	}
	if err := take.Release(); err != nil {
		log.Printf("恢复官方入口失败：%v", err)
		exitCode = 1
	}
	return exitCode
}

// listenUnix 建立 Unix socket 监听，权限放到 0666：
// 飞牛桌面网关以 root 运行，但管理页也可能被其他服务代理，宽一点更省事，
// 且该 socket 只存在于本机文件系统上。
func listenUnix(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("未配置 gateway socket 路径")
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o666)
	return l, nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// restoreOfficial 独立执行恢复：服务已经停了、或者启动脚本要强制清场时用。
func restoreOfficial(cfg Config) int {
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "创建状态目录失败：%v\n", err)
		return 1
	}
	take := NewTakeover(cfg)
	if err := take.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "恢复失败：%v\n", err)
		return 1
	}
	return 0
}

func printStatus(cfg Config) int {
	take := NewTakeover(cfg)
	out := map[string]any{
		"version": Version,
		"config": map[string]any{
			"path":         ConfigPath(""),
			"meow_url":     cfg.MeowURL,
			"online":       cfg.OnlineEnabled,
			"sources":      cfg.Sources,
			"gateway_sock": cfg.GatewaySocket,
		},
		"takeover": take.Status(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(out)
	return 0
}

// preflight 是安装/启动前的自检，输出可读结论而不是裸错误。
func preflight(cfg Config) int {
	ok := true

	targetState, _, err := inspectSocket(cfg.TargetSocket)
	switch {
	case err != nil:
		fmt.Printf("官方音乐入口   ✗ 无法检查 %s：%v\n", cfg.TargetSocket, err)
		ok = false
	case targetState == sockLive:
		fmt.Printf("官方音乐入口   ✓ %s 正在监听\n", cfg.TargetSocket)
	case targetState == sockStale:
		fmt.Printf("官方音乐入口   ! %s 是无监听的残留文件，接管时会清理\n", cfg.TargetSocket)
	default:
		fmt.Printf("官方音乐入口   ✗ %s %s：请先安装并启动飞牛音乐\n", cfg.TargetSocket, targetState)
		ok = false
	}

	take := NewTakeover(cfg)
	upState, _, _ := inspectSocket(cfg.UpstreamSocket)
	if upState == sockLive {
		fmt.Printf("官方后端副本   ✓ %s 已就位（上次接管未恢复）\n", cfg.UpstreamSocket)
	} else {
		fmt.Printf("官方后端副本   - %s %s\n", cfg.UpstreamSocket, upState)
	}
	_ = take

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := NewMeowClient(cfg).Health(ctx); err != nil {
		fmt.Printf("Meow 上游      ✗ %s 不可达：%v\n", cfg.MeowURL, err)
		// Meow 不可达不算硬失败：扩展装好之后再去配地址是常见流程。
		fmt.Println("               （可先完成安装，再到管理页填写地址）")
	} else {
		fmt.Printf("Meow 上游      ✓ %s 正常\n", cfg.MeowURL)
	}

	if ok {
		fmt.Println("\n自检通过，可以启动。")
		return 0
	}
	fmt.Println("\n自检未通过：请按上面的提示处理后重试。")
	return 1
}
