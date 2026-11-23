package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 接管的核心机制：飞牛音乐的 nginx 固定连 /var/run/trim_music.socket。
// 扩展要增强它，就得顶替这个路径——但官方进程仍持有那个 socket 的 inode，
// 只是路径被改了名，所以官方照常服务，扩展再把请求转回去。
//
// 顺序上有一处绝不能错：Go 的 net.Listen("unix", p) 会先 unlink 已存在的 p。
// 如果直接 listen 在 target 上，官方 socket 就被无声删掉了，等于把官方音乐
// 从系统里摘掉。所以必须"先在临时路径 listen，再 rename 就位"。

type socketState int

const (
	sockAbsent  socketState = iota // 路径不存在
	sockLive                       // 有进程在监听
	sockStale                      // socket 文件在，但没人监听（崩溃残留）
	sockForeign                    // 路径存在但不是 socket
)

func (s socketState) String() string {
	switch s {
	case sockAbsent:
		return "不存在"
	case sockLive:
		return "监听中"
	case sockStale:
		return "残留（无监听）"
	default:
		return "非 socket 文件"
	}
}

// inspectSocket 判断路径的当前状态。
//
// "是否有人监听"用一次真实连接来判断，而不是看文件存在与否：
// 进程被 kill -9 后 socket 文件会留在原地，看起来一切正常，实际连接全被拒。
func inspectSocket(path string) (socketState, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sockAbsent, 0, nil
		}
		return sockAbsent, 0, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return sockForeign, 0, nil
	}
	ino, _ := fileInode(path)

	conn, err := net.DialTimeout("unix", path, 400*time.Millisecond)
	if err == nil {
		conn.Close()
		return sockLive, ino, nil
	}
	return sockStale, ino, nil
}

// takeoverState 记录"这次接管做了什么"，用于重启后认领残留与安全恢复。
type takeoverState struct {
	PID           int       `json:"pid"`
	TargetInode   uint64    `json:"target_inode"`
	UpstreamInode uint64    `json:"upstream_inode"`
	TakenAt       time.Time `json:"taken_at"`
}

type Takeover struct {
	target   string
	upstream string
	stateDir string

	mu       sync.Mutex
	release  func()
	acquired bool
}

func NewTakeover(cfg Config) *Takeover {
	return &Takeover{
		target:   cfg.TargetSocket,
		upstream: cfg.UpstreamSocket,
		stateDir: cfg.StateDir,
	}
}

func (t *Takeover) statePath() string { return filepath.Join(t.stateDir, "takeover.json") }

func (t *Takeover) loadState() takeoverState {
	var st takeoverState
	data, err := os.ReadFile(t.statePath())
	if err != nil {
		return st
	}
	json.Unmarshal(data, &st)
	return st
}

func (t *Takeover) saveState(st takeoverState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.statePath())
}

func (t *Takeover) clearState() {
	os.Remove(t.statePath())
}

// pidAlive 判断进程是否还在。/proc 不存在说明容器里没挂 procfs，
// 此时保守地认为"可能还活着"，避免把真正的冲突当成残留。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return os.IsPermission(err)
	}
	return true
}

// Acquire 接管官方入口，返回已就位的监听器。
//
// 调用方拿到 listener 后直接用它服务 HTTP；关闭 listener 之后必须再调 Release，
// 否则官方入口会一直停在一个没人监听的路径上（用户表现为"飞牛音乐打不开"）。
func (t *Takeover) Acquire() (net.Listener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.acquired {
		return nil, errors.New("本实例已经接管过一次入口")
	}
	if err := os.MkdirAll(t.stateDir, 0o750); err != nil {
		return nil, fmt.Errorf("创建状态目录 %s 失败：%w", t.stateDir, err)
	}

	// 同一时刻只允许一个接管者。flock 在进程被杀时由内核释放，
	// 不会出现"崩溃一次就再也起不来"。
	unlock, err := tryLock(filepath.Join(t.stateDir, "takeover.lock"))
	if err != nil {
		return nil, errors.New("另一个 meow-ext 实例正在接管入口，本次退出")
	}
	t.release = unlock

	listener, err := t.acquireLocked()
	if err != nil {
		unlock()
		t.release = nil
		return nil, err
	}
	t.acquired = true
	return listener, nil
}

func (t *Takeover) acquireLocked() (net.Listener, error) {
	prev := t.loadState()

	targetState, targetIno, err := inspectSocket(t.target)
	if err != nil {
		return nil, fmt.Errorf("检查 %s 失败：%w", t.target, err)
	}
	upState, _, err := inspectSocket(t.upstream)
	if err != nil {
		return nil, fmt.Errorf("检查 %s 失败：%w", t.upstream, err)
	}

	switch targetState {
	case sockAbsent:
		return nil, fmt.Errorf("没有找到官方音乐入口 %s：请先在应用中心安装并启动飞牛音乐", t.target)
	case sockForeign:
		return nil, fmt.Errorf("%s 已存在且不是 socket 文件，为避免误删已停止接管，请人工检查", t.target)
	case sockLive:
		// 活着的监听者是谁？如果 inode 与上次接管记录一致且那个进程还在，
		// 说明就是另一个自己——两个实例抢同一个入口只会互相踩。
		if prev.TargetInode == targetIno && pidAlive(prev.PID) && prev.PID != os.Getpid() {
			return nil, fmt.Errorf("入口已被另一个 meow-ext 实例（pid %d）接管，请先停止它", prev.PID)
		}
	case sockStale:
		// 崩溃残留。只有当官方确实还在 upstream 上时才继续，
		// 否则说明官方音乐本身没在跑，此时接管只会让用户面对一个全 502 的入口。
		if upState != sockLive {
			return nil, fmt.Errorf("官方音乐入口 %s 是一个没有监听的残留文件，且未发现官方后端在 %s；"+
				"请在应用中心重启飞牛音乐后重试", t.target, t.upstream)
		}
	}

	// 判断 upstream 上那个活着的监听者能不能被解释。
	//
	// 两种情况都正常：
	//   - target 是残留、upstream 活着：上次接管后进程崩了，官方一直在 upstream 上服务
	//   - target 不存在、upstream 活着：同上，只是残留文件也被清掉了
	// 只有 target 也活着时才可疑——那说明官方仍在入口上，而 upstream 另有一个
	// 我们无法解释的监听者。此时宁可停下，也不能 rename 覆盖掉它。
	if targetState == sockLive && upState == sockLive {
		return nil, fmt.Errorf("%s 已有监听者，同时 %s 也被占用；"+
			"可能是上次异常退出留下的孤儿监听，请先停止飞牛音乐并清理该文件后重试",
			t.target, t.upstream)
	}

	// 第一步：在私有临时目录里把监听器建好。此步不影响官方。
	staging := filepath.Join(t.stateDir, "listen.sock")
	os.Remove(staging)
	listener, err := net.Listen("unix", staging)
	if err != nil {
		return nil, fmt.Errorf("在 %s 建立监听失败：%w", staging, err)
	}
	// 让飞牛音乐（可能以非 root 用户跑）与 nginx 都能连上代理。
	if err := os.Chmod(staging, 0o666); err != nil {
		listener.Close()
		return nil, fmt.Errorf("设置 %s 权限失败：%w", staging, err)
	}

	rollback := func() {
		listener.Close()
		os.Remove(staging)
	}

	// 第二步：清掉挡路的残留（此时已确认 target 不是活着的官方监听）。
	if targetState == sockStale {
		if err := os.Remove(t.target); err != nil {
			rollback()
			return nil, fmt.Errorf("清理残留入口 %s 失败：%w", t.target, err)
		}
	}
	if upState == sockStale {
		if err := os.Remove(t.upstream); err != nil {
			rollback()
			return nil, fmt.Errorf("清理残留上游 %s 失败：%w", t.upstream, err)
		}
	}

	// 第三步：官方入口改名到 upstream。rename 不改变 inode，
	// 官方进程手里的监听继续有效，只是路径变了。
	movedOfficial := false
	if targetState == sockLive {
		if err := os.Rename(t.target, t.upstream); err != nil {
			rollback()
			return nil, fmt.Errorf("把官方入口移到 %s 失败：%w", t.upstream, err)
		}
		movedOfficial = true
	}

	// 第四步：把我们的监听器挪到入口位置。rename 一个已 bind 的 socket 文件
	// 是安全的——监听队列与 inode 都不受影响，客户端立刻就能连上。
	if err := os.Rename(staging, t.target); err != nil {
		if movedOfficial {
			// 回滚：把官方挪回去。这一步必须成功，否则飞牛音乐就真的断了。
			if rbErr := os.Rename(t.upstream, t.target); rbErr != nil {
				rollback()
				return nil, fmt.Errorf("接管失败（%v），且回滚官方入口也失败（%v）；"+
					"请手动把 %s 改回 %s", err, rbErr, t.upstream, t.target)
			}
		}
		rollback()
		return nil, fmt.Errorf("把代理就位到 %s 失败：%w", t.target, err)
	}

	newIno, _ := fileInode(t.target)
	newUpIno, _ := fileInode(t.upstream)
	if err := t.saveState(takeoverState{
		PID:           os.Getpid(),
		TargetInode:   newIno,
		UpstreamInode: newUpIno,
		TakenAt:       time.Now(),
	}); err != nil {
		// 状态文件写不进去不算致命：它只影响下次崩溃后能否自动认领残留，
		// 而正常恢复看的是 upstream 的实际状态。
		fmt.Fprintf(os.Stderr, "[takeover] 警告：写入接管状态失败：%v\n", err)
	}

	fmt.Printf("[takeover] 已接管 %s（官方后端移至 %s）\n", t.target, t.upstream)
	return listener, nil
}

// Release 把官方入口还回去。可重复调用。
func (t *Takeover) Release() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var problems []string
	upState, _, err := inspectSocket(t.upstream)
	if err != nil {
		problems = append(problems, "检查上游失败："+err.Error())
	}

	switch upState {
	case sockAbsent:
		// 从没接管过，或者已经恢复过了。只需确认入口本身没被我们留下空壳。
		if st, _, _ := inspectSocket(t.target); st == sockStale {
			os.Remove(t.target)
		}
	case sockStale:
		// 官方进程已经没了，upstream 只是残留。把入口路径清干净，
		// 让飞牛音乐下次启动能正常 bind，而不是撞上一个僵尸文件。
		os.Remove(t.upstream)
		if st, _, _ := inspectSocket(t.target); st == sockStale {
			os.Remove(t.target)
		}
		problems = append(problems, "官方音乐后端已不在运行，入口已清理；请到应用中心重新启动飞牛音乐")
	case sockLive:
		// 正常恢复：先撤掉入口（我们自己的 socket），再把官方挪回来。
		if st, _, _ := inspectSocket(t.target); st == sockLive || st == sockStale {
			if err := os.Remove(t.target); err != nil && !os.IsNotExist(err) {
				// 删不掉就直接 rename 覆盖：rename 会替换目标路径，
				// 残留文件不会挡住官方入口。
				fmt.Fprintf(os.Stderr, "[takeover] 清理入口失败（将直接覆盖）：%v\n", err)
			}
		}
		if err := os.Rename(t.upstream, t.target); err != nil {
			problems = append(problems, fmt.Sprintf("把官方入口从 %s 恢复到 %s 失败：%v", t.upstream, t.target, err))
		}
	}

	t.clearState()
	if t.release != nil {
		t.release()
		t.release = nil
	}
	t.acquired = false

	if len(problems) > 0 {
		return errors.New(problems[0])
	}
	fmt.Printf("[takeover] 已把 %s 还给官方音乐\n", t.target)
	return nil
}

// Status 输出接管现状，供 status 子命令与管理页排障。
func (t *Takeover) Status() map[string]any {
	targetState, targetIno, _ := inspectSocket(t.target)
	upState, upIno, _ := inspectSocket(t.upstream)
	st := t.loadState()
	return map[string]any{
		"target": map[string]any{
			"path":  t.target,
			"state": targetState.String(),
			"inode": targetIno,
		},
		"upstream": map[string]any{
			"path":  t.upstream,
			"state": upState.String(),
			"inode": upIno,
		},
		"recorded_pid":      st.PID,
		"recorded_pid_live": pidAlive(st.PID),
		"taken_at":          st.TakenAt,
		// 这里不能用 os.Getpid() 判断：status 子命令是另一个进程，
		// 拿它跟接管记录比必然不等，于是管理页永远显示"未代理"——一个
		// 只会误导人的状态位。改用"入口在监听 + 记录里的进程还活着"，
		// 在服务进程内与 status 进程里都能给出同一个正确答案。
		"proxy_active": targetState == sockLive && pidAlive(st.PID),
	}
}
