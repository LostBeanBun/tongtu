package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"tongtu/internal/app"
	"tongtu/internal/config"
)

// 崩溃 / 强杀防护（「监护进程」）。
//
// 问题：通途会接管系统代理（注册表指向 127.0.0.1:19180）与 hosts（若干域名指向
// 127.0.0.1）。如果通途被强杀、崩溃或断电，这些设置会留在系统里指向一个已经
// 不存在的本地代理——表现就是「浏览器上不了网」。优雅退出时通途会自己还原，
// 但崩溃时没有机会执行任何代码。
//
// 方案：在发生接管时额外拉起一个同名 exe 的子进程作为监护：
//
//	tongtu.exe __guard --guard-pid <主进程 PID> --guard-token <本轮令牌>
//
//	1. 监护进程只做一件事：等待主进程退出（等它的进程句柄），
//	   醒来后调用 app.RecoverStale —— 还原系统代理、移除 hosts 托管块；
//	2. RecoverStale 是幂等的：主进程若已优雅还原（状态文件已删除），它就是空操作；
//	3. 令牌文件 guard.json 记录「当前这轮接管」的令牌。若期间已有新实例接管
//	   （令牌被改写），旧监护进程什么都不做，避免把新实例的状态清掉；
//	4. 主进程被强杀 / 崩溃时，进程句柄立刻进入已通知状态，监护进程几毫秒内完成还原，
//	   用户几乎察觉不到连接中断。
//
// 断电 / 蓝屏这类无法执行任何代码的情况，靠下次启动时的 app.RecoverStale 兜底。

// guardInfo 记录当前轮次的监护信息（写入 guard.json）。
type guardInfo struct {
	PID   int       `json:"pid"`
	Token string    `json:"token"`
	At    time.Time `json:"at"`
}

// guardStatePath 返回监护令牌文件路径。
func guardStatePath() string { return filepath.Join(config.Dir(), "guard.json") }

// guardLogPath 返回监护进程的日志文件（调试用；监护进程没有界面）。
func guardLogPath() string { return filepath.Join(config.Dir(), "guard.log") }

// guardLog 追加一行监护日志（尽力而为，失败忽略）。
func guardLog(format string, args ...any) {
	if err := config.EnsureDir(); err != nil {
		return
	}
	f, err := os.OpenFile(guardLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// readGuardInfo 读取监护令牌文件。
func readGuardInfo() (guardInfo, error) {
	var gi guardInfo
	data, err := os.ReadFile(guardStatePath())
	if err != nil {
		return gi, err
	}
	if err := json.Unmarshal(data, &gi); err != nil {
		return gi, err
	}
	return gi, nil
}

// foreignLiveSession 报告「系统代理 / hosts 是否正被另一个仍在运行的实例持有」，并返回它的 pid。
//
// guard.json 里的 pid 是那一轮实例的主进程：它还活着，就说明系统状态由它负责。此时绝不能做
// 「遗留清理」（app.RecoverStale）——那会把别人刚接管好的系统代理还原掉，而对方界面照旧显示
// 「已启用」，用户完全不察觉。
//
// 实测场景：用另一份配置（换控制端口）跑第二个实例时，第二个实例的 stop 兜底清理把第一个
// 实例的系统代理清空了——兜底本来只该处理「崩溃遗留」，不该误伤还活着的实例。
func foreignLiveSession() (int, bool) {
	gi, err := readGuardInfo()
	if err != nil {
		return 0, false
	}
	return foreignGuardOwner(gi, os.Getpid(), processAlive)
}

// foreignGuardOwner 是 foreignLiveSession 的纯函数内核：令牌里的 pid 有效、不是自己、而且还活着。
func foreignGuardOwner(gi guardInfo, self int, alive func(int) bool) (int, bool) {
	if gi.PID <= 0 || gi.PID == self || !alive(gi.PID) {
		return 0, false
	}
	return gi.PID, true
}

// guardRunner 管理监护进程的生命周期（同一时刻只需要一个）。
type guardRunner struct {
	mu      sync.Mutex
	token   string
	started bool
	alive   bool
}

var guard = &guardRunner{}

// claimGuard 在启动早期占用本轮令牌：写入 guard.json 并返回令牌。
//
// 必须早于 app.RecoverStale 与接管动作：这样上一轮可能还活着的监护进程会
// 立刻发现令牌已经变了，从而不会误清新实例刚刚建立的状态。
func claimGuard() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		buf = []byte(fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()))
	}
	token := hex.EncodeToString(buf)
	guard.mu.Lock()
	guard.token = token
	guard.mu.Unlock()

	if err := config.EnsureDir(); err != nil {
		return token
	}
	gi := guardInfo{PID: os.Getpid(), Token: token, At: time.Now()}
	data, err := json.Marshal(gi)
	if err != nil {
		return token
	}
	_ = os.WriteFile(guardStatePath(), data, 0o644)
	return token
}

// startGuard 在确实接管了系统状态（系统代理 / hosts）时拉起监护进程。
// 可重复调用：同一轮只拉起一个，监护进程若已退出会重新拉起。
func startGuard(log *slog.Logger) {
	guard.mu.Lock()
	if guard.token == "" {
		guard.mu.Unlock()
		return
	}
	if guard.started {
		alive := guard.alive
		guard.mu.Unlock()
		if alive {
			return
		}
	}
	guard.started = true
	token := guard.token
	guard.alive = true
	guard.mu.Unlock()

	self, err := os.Executable()
	if err != nil {
		guard.markDead()
		if log != nil {
			log.Warn("崩溃防护：找不到自身可执行文件", "err", err)
		}
		return
	}
	cmd := exec.Command(self, guardSubcommand,
		"--guard-pid", fmt.Sprint(os.Getpid()),
		"--guard-token", token)
	// 监护进程要能在主进程被杀之后继续活着：
	//   - CREATE_NO_WINDOW：不创建控制台（GUI 子系统本身也不会有）；
	//   - CREATE_BREAKAWAY_FROM_JOB：万一主进程在会「关闭即杀子进程」的作业对象里，
	//     监护进程也能脱身（作业对象不允许脱离时会失败，此时退回普通启动）。
	cmd.SysProcAttr = guardSysProcAttr(true)
	err = cmd.Start()
	if err != nil {
		cmd.SysProcAttr = guardSysProcAttr(false)
		err = cmd.Start()
	}
	if err != nil {
		guard.markDead()
		if log != nil {
			log.Warn("崩溃防护：拉起监护进程失败（异常退出时将无法自动还原系统代理）", "err", err)
		}
		return
	}
	guardLog("监护进程已启动 pid=%d（主进程 %d）", cmd.Process.Pid, os.Getpid())
	// 等待子进程结束：它退出后（例如被误杀）轮询会重新拉起。
	go func() {
		_ = cmd.Wait()
		guard.markDead()
	}()
}

func (g *guardRunner) markDead() {
	g.mu.Lock()
	g.alive = false
	g.mu.Unlock()
}

// guardNeeded 报告当前是否已经接管了需要保护的系统状态。
func guardNeeded(a *app.App) bool {
	if a == nil {
		return false
	}
	st := a.Status()
	return st.SystemProxy || st.Hosts
}

// watchGuard 轮询「是否已接管系统状态」，一旦发生就确保监护进程在跑。
// 托盘里手动打开系统代理 / hosts 之后也由它补上监护。
func watchGuard(a *app.App, log *slog.Logger, quit <-chan struct{}) {
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for {
		if guardNeeded(a) {
			startGuard(log)
		}
		select {
		case <-quit:
			return
		case <-t.C:
		}
	}
}

// guardSubcommand 是监护进程的子命令名（不出现在帮助里）。
const guardSubcommand = "__guard"

var (
	flagGuardPID   int
	flagGuardToken string
)

var guardCmd = &cobra.Command{
	Use:    guardSubcommand,
	Short:  "内部命令：等待主进程退出后还原系统代理与 hosts（崩溃防护）",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		runGuard(flagGuardPID, flagGuardToken)
		return nil
	},
}

// runGuard 是监护进程的主体：等主进程退出，然后按需还原。
func runGuard(pid int, token string) {
	if pid <= 0 || token == "" {
		return
	}
	guardLog("监护进程启动 watch pid=%d", pid)

	if !waitProcessExit(pid) {
		// 拿不到父进程句柄（多半是它已经不在了）→ 直接还原
		guardLog("无法等待父进程 %d（可能已退出），立即还原", pid)
	} else {
		guardLog("父进程 %d 已退出，检查是否需要还原", pid)
	}

	gi, err := readGuardInfo()
	if err != nil {
		guardLog("读取令牌失败（%v），跳过还原", err)
		return
	}
	if !guardStillOurs(gi, pid, token) {
		guardLog("令牌已更换（新实例已接管：pid=%d），跳过还原", gi.PID)
		return
	}
	guardLog("开始还原系统代理与 hosts")
	app.RecoverStale(nil)
	_ = os.Remove(guardStatePath())
	guardLog("还原完成")
}

// guardStillOurs 判断令牌文件里的这一轮是否仍然属于「我们要守护的那个进程」。
//
// 只要期间已经有新实例接管（pid / 令牌对不上），就说明当前系统状态由新实例负责，
// 旧监护进程必须什么都不做——否则会把新实例刚建立的状态清掉。
func guardStillOurs(gi guardInfo, pid int, token string) bool {
	return gi.PID == pid && gi.Token == token
}

// isGuardInvocation 报告本次进程是不是监护进程（用于跳过控制台附加等启动动作）。
func isGuardInvocation() bool {
	for _, a := range os.Args[1:] {
		if a == guardSubcommand || a == "--"+guardSubcommand {
			return true
		}
	}
	return false
}

func init() {
	guardCmd.Flags().IntVar(&flagGuardPID, "guard-pid", 0, "被监护的进程 PID")
	guardCmd.Flags().StringVar(&flagGuardToken, "guard-token", "", "本轮监护令牌")
	rootCmd.AddCommand(guardCmd)
}
