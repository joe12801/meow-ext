package main

import (
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir 返回一个短路径临时目录。
//
// Unix socket 路径有 108 字节硬上限，t.TempDir() 生成的路径在测试名较长时
// 会超限，表现出的是"bind: invalid argument"这种与逻辑无关的失败。
func shortDir(t *testing.T) string {
	t.Helper()
	dir := fmt.Sprintf("/tmp/meowt-%d-%d", os.Getpid(), rand.Intn(100000))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建临时目录失败：%v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fakeOfficial 在指定路径起一个假的"官方音乐后端"。
func fakeOfficial(t *testing.T, path string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("建立假官方后端失败：%v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"data":{"list":[],"total":0},"path":%q}`, r.URL.Path)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l
}

func testTakeover(t *testing.T) (Config, *Takeover) {
	t.Helper()
	dir := shortDir(t)
	cfg := DefaultConfig()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.TargetSocket = filepath.Join(dir, "target.sock")
	cfg.UpstreamSocket = filepath.Join(dir, "upstream.sock")
	cfg.DataDir = dir
	return cfg, NewTakeover(cfg)
}

func TestInspectSocketStates(t *testing.T) {
	dir := shortDir(t)

	if st, _, _ := inspectSocket(filepath.Join(dir, "nope.sock")); st != sockAbsent {
		t.Fatalf("不存在的路径应为 absent，实际 %v", st)
	}

	live := filepath.Join(dir, "live.sock")
	l := fakeOfficial(t, live)
	if st, _, _ := inspectSocket(live); st != sockLive {
		t.Fatalf("有监听的 socket 应为 live，实际 %v", st)
	}

	// 关掉监听但保留文件，模拟进程被 kill -9：文件还在，连接全被拒。
	// 注意 Go 的 UnixListener 默认会在 Close 时 unlink 文件，
	// 必须显式关掉这个行为，否则造不出"残留"这个状态。
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if st, _, _ := inspectSocket(live); st != sockStale {
		t.Fatalf("无监听的残留 socket 应为 stale，实际 %v", st)
	}

	plain := filepath.Join(dir, "plain.txt")
	os.WriteFile(plain, []byte("x"), 0o600)
	if st, _, _ := inspectSocket(plain); st != sockForeign {
		t.Fatalf("普通文件应为 foreign，实际 %v", st)
	}
}

func TestAcquireAndReleaseRoundTrip(t *testing.T) {
	cfg, take := testTakeover(t)
	official := fakeOfficial(t, cfg.TargetSocket)

	l, err := take.Acquire()
	if err != nil {
		t.Fatalf("接管失败：%v", err)
	}
	if l == nil {
		t.Fatal("接管应返回可用监听器")
	}

	// 接管后：target 是我们的监听器，upstream 是官方。
	if st, _, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("接管后 target 应在监听，实际 %v", st)
	}
	if st, _, _ := inspectSocket(cfg.UpstreamSocket); st != sockLive {
		t.Fatalf("接管后 upstream 应是官方监听，实际 %v", st)
	}
	// 官方进程仍持有同一个 inode，只是路径变了——用 upstream 路径继续服务。
	resp, err := http.Get("http://unix/music/api/v1/search/track")
	_ = resp
	if err == nil {
		resp.Body.Close()
	}

	if err := take.Release(); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	l.Close()

	// 恢复后：target 应重新是官方（它一直在监听 upstream，被 rename 回来）。
	if st, ino, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("恢复后 target 应回到官方监听，实际 %v", st)
	} else if ino == 0 {
		t.Fatal("恢复后 target 的 inode 无效")
	}
	official.Close()
}

func TestAcquireRestoresOnListenerClose(t *testing.T) {
	// 这是最容易出事的路径：我们的监听器关掉之后，如果没调 Release，
	// target 就成了一个没人监听的残留文件，用户表现为"飞牛音乐打不开"。
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)

	l, err := take.Acquire()
	if err != nil {
		t.Fatalf("接管失败：%v", err)
	}
	l.Close()
	if err := take.Release(); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if st, _, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("恢复后 target 应可连接，实际 %v", st)
	}
}

func TestAcquireRefusesWhenOfficialAbsent(t *testing.T) {
	_, take := testTakeover(t)
	// 官方音乐没装/没启动时，接管只会让入口指向一个空后端。
	if _, err := take.Acquire(); err == nil {
		t.Fatal("官方入口不存在时应拒绝接管，实际成功")
	} else if !strings.Contains(err.Error(), "飞牛音乐") {
		t.Fatalf("错误信息应提示用户去启动飞牛音乐，实际：%v", err)
	}
}

func TestAcquireRefusesForeignFile(t *testing.T) {
	cfg, take := testTakeover(t)
	// 有人（或某个 bug）在入口路径上放了个普通文件：绝不能删。
	os.WriteFile(cfg.TargetSocket, []byte("important"), 0o600)
	if _, err := take.Acquire(); err == nil {
		t.Fatal("入口被非 socket 文件占用时应拒绝接管")
	}
	if data, _ := os.ReadFile(cfg.TargetSocket); string(data) != "important" {
		t.Fatal("不该动那个非 socket 文件")
	}
}

func TestAcquireReclaimsStaleTarget(t *testing.T) {
	cfg, take := testTakeover(t)
	// 场景：上次进程被 kill -9，target 残留成无人监听的 socket，
	// 而官方仍在 upstream 上正常服务。此时必须能直接接管，不能要求人工介入。
	official := fakeOfficial(t, cfg.UpstreamSocket)
	stale, err := net.Listen("unix", cfg.TargetSocket)
	if err != nil {
		t.Fatal(err)
	}
	// 保留文件才能模拟 kill -9 之后的现场。
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	l, err := take.Acquire()
	if err != nil {
		t.Fatalf("应能回收残留并接管，实际：%v", err)
	}
	if st, _, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("接管后 target 应在监听，实际 %v", st)
	}
	l.Close()
	if err := take.Release(); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if st, _, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("恢复后 target 应回到官方，实际 %v", st)
	}
	official.Close()
}

func TestReleaseIsIdempotent(t *testing.T) {
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)

	l, err := take.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := take.Release(); err != nil {
		t.Fatal(err)
	}
	l.Close()
	// 启动脚本可能重复调用 restore，第二次不能报错、更不能把官方 socket 弄丢。
	if err := take.Release(); err != nil {
		t.Fatalf("重复恢复不应报错：%v", err)
	}
	if st, _, _ := inspectSocket(cfg.TargetSocket); st != sockLive {
		t.Fatalf("重复恢复后 target 仍应可用，实际 %v", st)
	}
}

func TestAcquireTwiceOnSameInstanceFails(t *testing.T) {
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)

	l, err := take.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		l.Close()
		take.Release()
	}()
	if _, err := take.Acquire(); err == nil {
		t.Fatal("同一实例重复接管应被拒绝")
	}
}

func TestStatusReportsBothEndpoints(t *testing.T) {
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)

	l, err := take.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		l.Close()
		take.Release()
	}()

	st := take.Status()
	target, ok := st["target"].(map[string]any)
	if !ok || target["state"] != "监听中" {
		t.Fatalf("status 未如实反映 target 状态：%v", st)
	}
	if pid, _ := st["recorded_pid"].(int); pid != os.Getpid() {
		t.Fatalf("status 应记录当前进程 pid，实际 %v", st["recorded_pid"])
	}
	// 接管生效时 proxy_active 必须为 true。这里断言的是"别的进程（比如
	// status 子命令）读同一份状态也要看到 true"——用 os.Getpid() 比较时
	// 只有服务进程自己能看到 true，管理页就会一直显示"未代理"。
	if active, _ := st["proxy_active"].(bool); !active {
		t.Fatalf("接管生效时 proxy_active 应为 true：%v", st)
	}
}

func TestStatusProxyInactiveAfterRelease(t *testing.T) {
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)
	l, err := take.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err := take.Release(); err != nil {
		t.Fatalf("还原失败：%v", err)
	}
	st := take.Status()
	if active, _ := st["proxy_active"].(bool); active {
		t.Fatalf("还原后 proxy_active 应为 false：%v", st)
	}
}

func TestStateFileWritten(t *testing.T) {
	cfg, take := testTakeover(t)
	fakeOfficial(t, cfg.TargetSocket)
	l, err := take.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		l.Close()
		take.Release()
	}()

	// 状态文件用于崩溃后认领残留，必须落盘。
	if _, err := os.Stat(take.statePath()); err != nil {
		t.Fatalf("接管状态未落盘：%v", err)
	}
	take.Release()
	if _, err := os.Stat(take.statePath()); !os.IsNotExist(err) {
		t.Fatal("恢复后应清理状态文件")
	}
	_ = time.Now
}
