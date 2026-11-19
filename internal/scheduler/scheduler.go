// Package scheduler 定时任务：每日签到 + token 保活 + 周期余额刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻（只在这一条路径上解冻，见
// RunBalanceRefresh 的说明）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 时点集合与开关在 New 时定下，之后可以整体热替换（见 ApplySchedule）：面板保存配置时
// 调它，Run 里的等待会被唤醒并按新排程重算下一次时点，不需要重启进程。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client

	CheckinHours   []int // 每日签到时点，默认 [9]
	KeepaliveHours []int // token 保活时点，默认 [3]

	CheckinEnabled   bool // 默认 true
	KeepaliveEnabled bool // 默认 true

	// BalanceRefreshInterval > 0 时按它周期刷余额（0 = 不启动）。
	BalanceRefreshInterval time.Duration
}

// 面板签到可辨识的账号状态。
var (
	ErrNotFound = errors.New("account not found")
	ErrDisabled = errors.New("account disabled")
)

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
	mu  sync.RWMutex
	// wake 排程变更广播：ApplySchedule 关掉旧 chan 并换一个新的，所有等待者同时醒来。
	// 不用「带缓冲的 chan + 非阻塞发」：主循环、余额循环、补签循环三处都在等它，
	// 单 token 会被其中一个抢走、另两个白等一轮（实测第二次保存没有重排）。
	wakeMu sync.Mutex
	wakeCh chan struct{}
	// checkinLock / refreshLock 单飞锁：整点时点、每小时补签 ticker、面板按钮、启动补跑
	// 都可能同时进来。同一账号被两路送进 claim = 秒级二次 claim，而上游原注释说
	// 「秒级内的第二次 claim 会延长限流窗口」——所以同一任务同时只允许一轮。
	checkinLock sync.Mutex
	refreshLock sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{3}
	}
	return &Scheduler{cfg: cfg, wakeCh: make(chan struct{})}
}

// wakeup 取当前这一代的广播通道（每轮循环都要重新取，否则换代后等的还是已关闭的旧 chan）。
func (s *Scheduler) wakeup() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return s.wakeCh
}

// ApplySchedule 热替换排程（面板保存配置后调用），并广播给运行中的循环。
//
// 只换排程段：Pool/Upstream 是进程级依赖，不随配置改。时点落盘前已由 config 层校验
// （0-23、关闭的任务允许时点非法），这里不再二次校验。
func (s *Scheduler) ApplySchedule(checkinEnabled bool, checkinHours []int,
	keepaliveEnabled bool, keepaliveHours []int, balanceInterval time.Duration) {
	s.mu.Lock()
	s.cfg.CheckinEnabled = checkinEnabled
	s.cfg.CheckinHours = append([]int(nil), checkinHours...)
	s.cfg.KeepaliveEnabled = keepaliveEnabled
	s.cfg.KeepaliveHours = append([]int(nil), keepaliveHours...)
	s.cfg.BalanceRefreshInterval = balanceInterval
	s.mu.Unlock()
	s.wakeMu.Lock()
	close(s.wakeCh)
	s.wakeCh = make(chan struct{})
	s.wakeMu.Unlock()
}

// allHours 当前启用的全部触发时点（两个任务都关掉时为空 → 主循环无事可做）。
func (s *Scheduler) allHours() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []int
	if s.cfg.CheckinEnabled {
		out = append(out, s.cfg.CheckinHours...)
	}
	if s.cfg.KeepaliveEnabled {
		out = append(out, s.cfg.KeepaliveHours...)
	}
	return out
}

// Status 排程现状：面板显示「下次签到/保活几点」。时点是本地小时，关掉的任务不排。
type Status struct {
	CheckinEnabled     bool   `json:"checkin_enabled"`
	CheckinHours       []int  `json:"checkin_hours"`
	CheckinNext        string `json:"checkin_next,omitempty"` // RFC3339
	KeepaliveEnabled   bool   `json:"keepalive_enabled"`
	KeepaliveHours     []int  `json:"keepalive_hours"`
	KeepaliveNext      string `json:"keepalive_next,omitempty"`
	BalanceIntervalSec int    `json:"balance_interval_sec,omitempty"`
}

// Status 返回当前排程（只读快照）。上次执行不在服务端留痕（日志里有），这里只报下一次。
func (s *Scheduler) Status() Status {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		CheckinEnabled:     s.cfg.CheckinEnabled,
		CheckinHours:       s.cfg.CheckinHours,
		KeepaliveEnabled:   s.cfg.KeepaliveEnabled,
		KeepaliveHours:     s.cfg.KeepaliveHours,
		BalanceIntervalSec: int(s.cfg.BalanceRefreshInterval / time.Second),
	}
	if st.CheckinEnabled {
		if t := nextFire(now, s.cfg.CheckinHours); !t.IsZero() {
			st.CheckinNext = t.Format(time.RFC3339)
		}
	}
	if st.KeepaliveEnabled {
		if t := nextFire(now, s.cfg.KeepaliveHours); !t.IsZero() {
			st.KeepaliveNext = t.Format(time.RFC3339)
		}
	}
	return st
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			// 日历日 +1（Date 会归一 DST），不是 +24h：春季跳表那天 +24h 会把时点漂到
			// 隔壁小时（3/7 03:00 EST + 24h = 3/8 04:00 EDT），那一档就被静默跳过。
			t = time.Date(now.Year(), now.Month(), now.Day()+1, h, 0, 0, 0, now.Location())
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// shanghaiTZ 固定东八区：上游的「今天」按北京时间算，而进程可能跑在 UTC/别的时区，
// 用 time.Local 会在境外服务器上把「今天」算错一天（每日积分 00:00 发放的边界就在那儿）。
var shanghaiTZ = time.FixedZone("CST", 8*3600)

// sameDayShanghai 两个时刻是否落在同一个北京日期。
func sameDayShanghai(a, b time.Time) bool {
	return a.In(shanghaiTZ).Format("2006-01-02") == b.In(shanghaiTZ).Format("2006-01-02")
}

// Run 主循环，阻塞直到 ctx 取消；余额刷新与补签扫描各起一条循环（周期与整点无关）。
//
// 排程可以热改（ApplySchedule 关旧 wakeCh 换代 → 下面的 <-wake 立刻返回）：收到就丢掉旧计划、
// 按新排程重算下一次时点。wake 每轮都要重新取（wakeup()），否则换代后等的还是已关闭的旧 chan。
// 注意 next 从**上一个计划时点**推进而不是每轮 time.Now()：任务耗时跨过相邻两档时点时，
// 用墙上时间重算会看到「下一档已经过去」直接跳到次日，把中间那档静默吞掉。
func (s *Scheduler) Run(ctx context.Context) {
	go s.runBalanceLoop(ctx)
	go s.runCheckinRetryLoop(ctx)
	var (
		hours     []int
		next      time.Time
		replanned bool // 刚被 wake 叫醒：排完新时点打一行日志，便于确认热改真的生效
	)
	for {
		wake := s.wakeup()
		if hours == nil {
			hours = s.allHours()
		}
		if len(hours) == 0 {
			// 两个任务都关了：没有时点要等，躺到退出或排程被改成有任务为止。
			select {
			case <-ctx.Done():
				return
			case <-wake:
				hours = nil
				replanned = true
			}
			continue
		}
		if next.IsZero() {
			next = nextFire(time.Now(), hours)
			if replanned {
				s.mu.RLock()
				ci, ch := s.cfg.CheckinEnabled, append([]int(nil), s.cfg.CheckinHours...)
				ka, kh := s.cfg.KeepaliveEnabled, append([]int(nil), s.cfg.KeepaliveHours...)
				s.mu.RUnlock()
				// 分开报两个任务：hours 是两者合并后的集合，混在一起看不出哪个任务在几点。
				log.Printf("[schedule] 排程已热更新：签到=%v 时点%v，保活=%v 时点%v，下次触发 %s",
					ci, ch, ka, kh, next.Format("2006-01-02 15:04"))
				replanned = false
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			// 排程改了：时点集合与开关都可能变，重算（保留「从计划时点推进」的语义
			// 不适用这里——旧计划已作废，按当下重新排）。
			timer.Stop()
			hours = nil
			next = time.Time{}
			replanned = true
		case <-timer.C:
			fired := next
			next = nextFire(fired, hours)
			h := fired.Hour()
			s.mu.RLock()
			doCheckin := s.cfg.CheckinEnabled && contains(s.cfg.CheckinHours, h)
			doKeepalive := s.cfg.KeepaliveEnabled && contains(s.cfg.KeepaliveHours, h)
			s.mu.RUnlock()
			if doCheckin {
				s.RunCheckinNow()
			}
			if doKeepalive {
				s.RunRefreshNow()
			}
		}
	}
}

// checkinRefreshSkew 签到前预刷新窗口：access token 剩不到这么久就先换一张。
const checkinRefreshSkew = 2 * time.Hour

// dailyGrantStaleAfter 「每日积分已停发」的判定窗口：最近一次 daily_grant 超过
// 这么多天没再出现，就认为上游不再给这个号发每日积分，签到轮次直接 skipped。
//
// 实测 2026-11-19：一批老号的最后一次 daily_grant 停在 11-12，之后每天都没有；
// 老逻辑（有历史就判「复核未通过」）会让它们每轮签到失败并记 5 分钟冷却，
// 面板长期挂着一片「签到冷却」——那不是账号在冷却，是规则没跟上上游。
const dailyGrantStaleAfter = 3 * 24 * time.Hour

// checkinGap / checkinGapJitter 多账号签到之间的间隔（基准 + 0..jitter 抖动）。
// 上游按设备/时段限流，背靠背连签容易吃到 9074「当前参与用户太多」——star620/TraeTools
// 用的是同一手（账号间 3~6s 随机间隔）。三条号也就多花 ~10s，且这是小时级任务。
// 变量而非常量：测试要把它置 0，不然每条用例白等十几秒。
var checkinGap, checkinGapJitter = 3 * time.Second, 3 * time.Second

// checkinPace 两个账号之间该等多久；<=0 表示不等（测试）。
func checkinPace() time.Duration {
	if checkinGap <= 0 {
		return 0
	}
	d := checkinGap
	if checkinGapJitter > 0 {
		d += time.Duration(rand.Int64N(int64(checkinGapJitter)))
	}
	return d
}

// bumpCheckinDevice 换一套设备指纹（设备维度限流/已领时用）。
// 失败只记日志：换指纹是补救动作，它失败不该把这次签到的结论改掉。
func (s *Scheduler) bumpCheckinDevice(uid string) {
	machineID, ok, err := s.cfg.Pool.RotateMachineID(uid)
	switch {
	case !ok:
		log.Printf("checkin device bump %s: 账号不在池里", uid)
	case err != nil:
		log.Printf("checkin device bump %s: machineId=%s 落盘失败: %v", uid, machineID, err)
	default:
		log.Printf("checkin device bump %s: 已换设备指纹 machineId=%s", uid, machineID)
	}
}

// isSessionDead 上游判定的「凭证失效」（401/1001 家族）：签到路径据此换票重试一次。
func isSessionDead(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead
}

// checkinRetryInterval 补签扫描间隔。
//
// 上游 claim 的 9074（「当前参与用户太多」）此前误判为纯时段性拥塞；2026-10-02 实测：
// 零星设备头会被系统性拒绝（三天 ~150 次全灭），完整客户端伪装头（headers.go ug 族）
// 修复后一次通过。每小时重扫保留作波动保险。
// 变量而非常量只为测试能把它调小。
var checkinRetryInterval = time.Hour

// runCheckinRetryLoop 当天补签：每小时扫一遍，没签上的账号在下一个整点还有机会。
//
// 反复扫是安全的：CheckinUID 先查 status，已签的账号直接跳过（幂等），签到域冷却中的账号
// 也会被 byUrgency 跳过——上游拥塞时不会退化成连打。
// 「进程起来先扫一遍」不在这里做：`catchUpCheckin` 已经负责启动补跑，重复调用会对上游连打两次。
// ponytail: 固定 1 小时。要更密/更疏就把它做成配置项。
func (s *Scheduler) runCheckinRetryLoop(ctx context.Context) {
	// 签到这个任务被关掉时也留在这条循环里等（wake 会把它叫醒）：否则面板打开签到后
	// 补签扫描要等到重启才出现。
	for {
		wake := s.wakeup()
		s.mu.RLock()
		on := s.cfg.CheckinEnabled
		s.mu.RUnlock()
		if !on {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		t := time.NewTimer(checkinRetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
		case <-t.C:
			s.RunCheckinNow()
		}
	}
}

// runBalanceLoop 周期刷新余额；周期是热项，每轮重读（<=0 = 暂停，等 wake 叫醒）。
func (s *Scheduler) runBalanceLoop(ctx context.Context) {
	for {
		wake := s.wakeup()
		s.mu.RLock()
		interval := s.cfg.BalanceRefreshInterval
		s.mu.RUnlock()
		if interval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
		case <-t.C:
			s.RunBalanceRefresh()
		}
	}
}

// entUsageOf 拉一次额度并写进池子，返回**口径感知**的快照。
//
// 用 UserEntUsageDetail 而不是老接口 UserEntUsage：国际版是**美元**计费、积分恒 0
// （2026-10-15 实测 is_dollar_usage_billing=true、basic_usage_limit=1、basic_usage_amount=0.00059），
// 老接口把国际号的额度一律读成 0，面板就显示成「无额度」——那不是上游的真实状态。
func (s *Scheduler) entUsageOf(uid string, a *auth.Auth) (upstream.EntUsage, error) {
	// 用带到期的口径：面板「到期」列与「积分到期提醒」都读 pool 里的 expire，
	// 而余额接口本身不返回到期时间（见 upstream.UserEntUsageDetailWithExpiry 的说明）。
	u, err := s.cfg.Upstream.UserEntUsageDetailWithExpiry(a)
	if err != nil {
		return u, err
	}
	s.cfg.Pool.SetBalance(uid, pool.Balance{
		Unit: u.Unit, Remain: u.Remain, Total: u.Total,
		UsedUSD: u.UsedUSD, LimitUSD: u.LimitUSD,
	}, u.Expire)
	s.cfg.Pool.SetExpireCredits(uid, u.ExpirePoints)
	return u, nil
}

// RunBalanceRefresh 刷新全部账号的积分与到期时间；返回查询失败的账号数。
//
// 与签到的区别：只读余额，不刷 token、**不解冻**。"余额 > 0 就解冻"是签到那一步的语义
// （用户手动签到就是为了解冻），套到周期刷新上会让熔断/降权/计划冷却每 5 分钟被清一次，
// 等于这些机制不存在。
func (s *Scheduler) RunBalanceRefresh() int {
	failed := 0
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 号与号之间留节奏：全池瞬间连发几十个余额请求会被网关按频率拦成 403
		// （实测 52 个号连查后，balance/setting_info 同时 403 一段时间）。
		if !first {
			time.Sleep(1500 * time.Millisecond)
		}
		first = false
		if _, err := s.entUsageOf(st.UID, a); err != nil {
			failed++
			continue
		}
	}
	if failed > 0 {
		log.Printf("balance refresh: %d 个账号查询失败", failed)
	}
	return failed
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
//
// 返回逐账号结果（面板据此回报「本次 +N 分 / 已签 / 失败原因」）与失败列表（日志与 HTTP 状态码用）。
func (s *Scheduler) RunCheckinNow() ([]CheckinResult, []error) {
	// 单飞：整点、补签 ticker、面板按钮、启动补跑四路并发时只放一轮进来。
	if !s.checkinLock.TryLock() {
		log.Printf("[checkin] 上一轮还在跑，跳过这一轮")
		return nil, nil
	}
	defer s.checkinLock.Unlock()
	results := []CheckinResult{} // 非 nil：面板拿到的是 [] 而不是 null
	var errs []error
	for i, uid := range s.byUrgency() {
		if i > 0 {
			time.Sleep(checkinPace()) // 第一个不等；最后一个签完也不用再等
		}
		// 面板「全部签到」也是用户主动点击：清掉当天限频标记，未领成功的礼包重问一次
		// （已领的号有 Granted 终态，不会重复打上游）。
		if a := s.cfg.Pool.AuthByUID(uid); a != nil {
			a.ResetGrantMarks()
		}
		res, err := s.CheckinUID(uid)
		results = append(results, res)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", uid, err))
		}
	}
	return results, errs
}

// byUrgency 探一轮余额，按最早到期时间升序返回待签 uid；无到期信息的排最后。
// 探失败不剔除账号（仍要去签，失败原因由 CheckinUID 报出来），只是没有紧迫度信息。
// ponytail: 每账号多一次只读探询换准确排序；号多了改成复用上一轮落盘的 expire。
func (s *Scheduler) byUrgency() []string {
	type row struct {
		uid    string
		expire int64
	}
	rows := make([]row, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		// 签到域冷却中的账号跳过本轮：上游拥塞时反复重试只会更堵。
		if s.cfg.Pool.CheckinCooling(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 签到只依赖 access_token（v1.1.0 凭证可能没有 refresh_token——CheckinUID
		// 已放开前置否决，这里同步别再按票筛掉，否则自动签到永远排不进该号）。
		// 所有号都要进签到队列：小浣熊的每日积分发放对所有账号一致（
		// CheckinUID 会按「上游未开」如实跳过，而不是这里直接不排队）。
		//
		// 到期时间直接用池里的持久化快照（st.Expire），**不再逐号探一次余额**：
		// 原来这轮探询 + CheckinStatus + 收尾刷新 = 每号 3 次余额请求，37 个号
		// 一轮就是上百次，正是上游网关对 balance/setting_info 返回 403 的原因。
		rows = append(rows, row{uid: st.UID, expire: st.Expire})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].expire, rows[j].expire
		if a == 0 {
			return false // 无到期信息排最后
		}
		if b == 0 {
			return true
		}
		return a < b
	})
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.uid)
	}
	return uids
}

// CheckinResult 单个账号的签到结果，给面板如实回报用：
// 用户签完得知道「本次拿了多少分」，而不是只看到一句「签到完成」。
type CheckinResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Status   string `json:"status"`            // ok | already | skipped（上游未开）| failed
	Credits  int64  `json:"credits,omitempty"` // 本次到账（仅 ok 非 0，取上游 status 告知的可领额度）
	Reason   string `json:"reason,omitempty"`  // 失败原因原文
	// Grants 是这一轮顺带结算的一次性新手礼包（各 3000 分）。**不计入 Credits**：
	// Credits 是每日签到的口径，礼包混进去会让面板把一次性收入报成当天签到收益。
	Grants []NewcomerGrant `json:"grants,omitempty"`
}

// CheckinUID 签到并按剩余积分解冻单个账号。禁用返回 ErrDisabled。
// 返回的 CheckinResult 说明这次到底发生了什么（含本次拿到的积分），错误路径同样带结果。
func (s *Scheduler) CheckinUID(uid string) (CheckinResult, error) {
	res := CheckinResult{UID: uid}
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return res, ErrNotFound
	}
	res.Nickname = st.Nickname
	if st.Disabled {
		return res, ErrDisabled
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return res, ErrNotFound
	}
	// 没有 refresh_token 不再一票否决：v1.1.0 的 box-agent 凭证只有 access_token
	// （实测 auth.json 不再落 refresh），签到/查询本身用 access_token 就够。
	// 预刷新逻辑自己会跳过未到期票；真到期又无票时，RefreshTokenIfNeeded 报错、
	// 后面 status 调用如实暴露「请重新登录」，不在这里提前拦。
	// 每日发放是服务端 00:00 自动入账（bills 流水实锤），CheckinStatus 以流水里
	// 当天有无 daily_grant 为准；enable=false 只在「上游把该账号额度关了」时出现。
	// 签到前把 access token 换新的：9 点签到离上一次保活（凌晨 3 点）有段距离，拿一张
	// 已过期的票只会白跑一轮（照 wild-work：2h 内到期就先刷）。刷失败不在这里报错——真有
	// 问题会让下面的 status 调用如实暴露，那条错误信息比「刷新失败」更贴题。
	// 但**真刷了就得落盘**：上游此时已轮换 refreshToken，盘上留旧票的话，进程在下次成功
	// 刷新前重启 → 该号被判 session 失效并自动禁用（要人工重登）。
	if refreshed, err := s.cfg.Upstream.RefreshTokenIfNeeded(a, checkinRefreshSkew); err != nil {
		log.Printf("checkin refresh %s: %v", uid, err)
	} else if refreshed {
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("checkin refresh %s: 新票落盘失败: %v", uid, serr)
		}
	}
	cst, serr := s.cfg.Upstream.CheckinStatus(a)
	checkedIn, reward, enable := cst.CheckedIn, cst.Credits, cst.Enable
	// 收尾落池用的余额：默认复用签到前的状态查询，复核拿到更新的快照时覆盖。
	finalCredits, finalKnown := cst.Credits, cst.CreditsKnown
	// token 被别处轮换/提前吊销 → status 报 session 失效：换一张新票再查一次，别把
	// 「票过期」记成签到失败（这条只重试一次，重试前必须真的换到了新票）。
	if isSessionDead(serr) {
		if rerr := s.cfg.Upstream.RefreshToken(a); rerr != nil {
			log.Printf("checkin token refresh %s: %v", uid, rerr)
		} else {
			// RefreshToken 一定轮换了票，同样必须落盘（理由同上）。
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("checkin token refresh %s: 新票落盘失败: %v", uid, serr)
			}
			if cst, serr = s.cfg.Upstream.CheckinStatus(a); serr == nil {
				checkedIn, reward, enable = cst.CheckedIn, cst.Credits, cst.Enable
				log.Printf("checkin status %s: status ok after token refresh", uid)
			}
		}
	}
	var checkinErr error
	var desktopGrant *NewcomerGrant
	// desktopTried：本轮 default 分支已经打过桌面端那条端点（每日领取与礼包同一条）。
	// 打了就不再在同一轮里补打一次——秒级重复请求正是上游风控盯的东西。
	desktopTried := false
	switch {
	case serr != nil:
		checkinErr = serr
		log.Printf("checkin status %s: %v", uid, serr)
	case cst.GrantErr != nil:
		// 流水接口不可用（v1 已废弃返回 403 / 网络抖动）：签到状态**未知**。
		// 不要判失败、不要记冷却——接口问题不该变成全池的「签到失败 + 冷却」循环
		// （2026-11-19 实测：13/15 个号因此每轮失败并反复冷却）。
		res.Status = "skipped"
		res.Reason = "签到状态查询失败（积分流水接口不可用）：" + cst.GrantErr.Error()
		log.Printf("checkin %s: skipped（流水查询失败：%v）", uid, cst.GrantErr)
	case checkedIn:
		res.Status = "already"
		log.Printf("checkin %s: already checked in", uid)
	case !enable:
		// 上游把签到关了这个账号（enable=false）：什么都没做，如实报出来。
		res.Status = "skipped"
		log.Printf("checkin %s: upstream disabled (enable=false)", uid)
	default:
		// 从未发过每日积分还要再分两种：当天新注册的号是 pending（上游 00:00 的批次
		// 今天还没轮到它），不是「未开通」。只在 ever=false 时查一次注册时间，老号
		// 不需要多打这一枪。
		//
		// 注意：**不要**因为「最近 N 天没发」就跳过领取。客户端的实证（2026-11-19）：
		// 一个 11-13 起就没再收到 daily_grant 的号，用户在桌面客户端打开后当场补发了
		// 11-13 到今天的一整串 300 分——领取动作就是下面这次 grant。判定「停发」只能
		// 放在**领取之后**（拿到结果再说），提前跳过等于把本来能补发的号直接放弃。
		if !cst.EverGranted {
			if created, ok := s.cfg.Upstream.UserCreatedAt(a); ok && sameDayShanghai(created, time.Now()) {
				res.Status = "pending"
				res.Reason = "上游今日尚未发放每日积分（" + created.In(shanghaiTZ).Format("2006-01-02 15:04") +
					" 注册的新号，首批在次日 00:00 发放）"
				log.Printf("checkin %s: pending（%s 注册的新号，等次日 00:00 首批每日积分）",
					uid, created.In(shanghaiTZ).Format("2006-01-02 15:04"))
			} else {
				res.Status = "skipped"
				res.Reason = "上游未给该账号开通每日积分（发放流水里没有 daily_grant），无需签到"
				log.Printf("checkin %s: skipped（从未有过 daily_grant）", uid)
			}
			break
		}
		var claimErr error
		// 桌面端那笔礼包就是被这次"每日登录领取"顺带领掉的：把结果留好，
		// 否则面板只会看到移动端那笔，看着像漏领。
		// granted=true 且 popup.source=daily 时，上游就是把当天/欠发的每日积分补上了。
		//
		// 每个号每天最多问一次：这个端点是网关风控最敏感的一条（一天几十上百次
		// 重复请求会换来 X-TT-System-Error: 3 的 403）。日期标记落盘，重启也不会重问。
		if !a.DesktopGrantDone() {
			desktopGrant, claimErr = s.claimWithRetry(a)
			desktopTried = true
			if claimErr == nil {
				a.MarkDesktopGrantDone(desktopGrant != nil && desktopGrant.Granted)
				if serr := a.SaveAtomic(); serr != nil {
					log.Printf("checkin claim %s: 问询标记落盘失败: %v", uid, serr)
				}
			}
			if claimErr == nil && desktopGrant != nil {
				log.Printf("checkin claim %s: granted=%v", uid, desktopGrant.Granted)
			}
		}
		switch {
		case claimErr == nil:
			// 交给下面的复核判定
		case errors.Is(claimErr, upstream.ErrCheckinAlready):
			// 9095「今日已签到」**不能**当「本账号已签」：上游这条信息是**设备维度**的，
			// 同一台设备可能已经替另一个账号签过（Trae2api-cn 原注释：只有 status 能定论）。
			// 不换设备的话下一轮还会回 9095——所以顺手换一套指纹，再由复核给结论。
			s.bumpCheckinDevice(uid)
			log.Printf("checkin %s: claim 9095（设备维度已签）→ 换设备指纹，以 status 复核", uid)
		default:
			checkinErr = claimErr
			log.Printf("checkin claim %s: %v", uid, claimErr)
			// 9074「当前参与用户太多」同样是设备维度限流：换一套指纹让**下一轮**脱开旧标记。
			// 这里刻意不立刻重试——上游原注释说秒级内第二次 claim 会延长限流窗口
			// （Trae2api-cn trae_client.py），补签扫描/手动点击自然带新身份再试。
			if upstream.IsCheckinRateLimited(claimErr) {
				s.bumpCheckinDevice(uid)
			}
		}
		// 复核（claim 成功与 9095 两条路径都要走）：只有 status 说 checked_in 才算签上。
		// claim 回 code 0 不等于真签上（上游偶发只回执不落账）；9095 更是设备维度信息。
		if checkinErr == nil {
			vst, verr := s.cfg.Upstream.CheckinStatus(a)
			if verr != nil {
				checkinErr = fmt.Errorf("签到复核失败：%w", verr)
				log.Printf("checkin verify %s: %v", uid, verr)
			} else if vst.GrantErr != nil {
				// 复核同样依赖流水：查不到就别判失败（接口问题不是账号问题）。
				res.Status = "skipped"
				res.Reason = "签到复核跳过：积分流水接口不可用（" + vst.GrantErr.Error() + "）"
				log.Printf("checkin verify %s: skipped（流水查询失败：%v）", uid, vst.GrantErr)
			} else if !vst.CheckedIn {
				// 当天新注册的号：每日积分发放是上游 00:00 的定时任务，今天这批根本还没发，
				// bills 里不会有 daily_grant。这不是签到失败（claim 该拿的登录奖励已拿到），
				// 报 failed 还会给它记一次 5 分钟签到冷却、把面板刷成一脸错。
				if created, ok := s.cfg.Upstream.UserCreatedAt(a); ok && sameDayShanghai(created, time.Now()) {
					res.Status = "pending"
					res.Reason = "上游今日尚未发放每日积分（" + created.In(shanghaiTZ).Format("2006-01-02 15:04") +
						" 注册的新号，首批在次日 00:00 发放）"
					log.Printf("checkin %s: pending（%s 注册的新号，等次日 00:00 首批每日积分）",
						uid, created.In(shanghaiTZ).Format("2006-01-02 15:04"))
				} else if !vst.LastGrant.IsZero() && time.Since(vst.LastGrant) > dailyGrantStaleAfter {
					// 已经尝试过 grant（见上面的 default 分支），上游仍没补发：如实报
					// 「近期未发放」，但不记失败/冷却——这已经不是客户端能改变的状态。
					res.Status = "skipped"
					res.Reason = fmt.Sprintf("已尝试领取，但上游近期未再发放每日积分（最近一次 %s，已 %d 天）",
						vst.LastGrant.In(shanghaiTZ).Format("2006-01-02"), int(time.Since(vst.LastGrant).Hours()/24))
					log.Printf("checkin verify %s: skipped（已尝试领取，上游仍未发放；最近一次 %s）",
						uid, vst.LastGrant.In(shanghaiTZ).Format("2006-01-02"))
				} else {
					checkinErr = &upstream.BusinessError{Message: "签到复核未通过：上游仍报未签到"}
					log.Printf("checkin verify %s: status still checked_in=false", uid)
				}
			} else {
				res.Status, res.Credits = "ok", reward
				log.Printf("checkin %s: ok +%d", uid, reward)
			}
			// 复核拿到的最新余额覆盖签到前的快照（免掉收尾那次额外余额请求）。
			if verr == nil && vst.CreditsKnown {
				finalCredits, finalKnown = vst.Credits, true
			}
		}
	}
	// 一次性新手礼包（桌面端/移动端各 3000 分）在这里无条件补一次：
	// 每日分是上游 00:00 自动发的，上面几乎总走「already」短路，claim 分支
	// 根本不执行——礼包不能跟着签到结果一起被跳过（见 claimNewcomerGrants）。
	grants := s.claimNewcomerGrants(a, false, desktopTried)
	if desktopGrant != nil {
		grants = append([]NewcomerGrant{*desktopGrant}, grants...)
	}
	if len(grants) > 0 {
		res.Grants = grants
	}
	if res.Status == "" {
		res.Status = "failed"
	}
	if checkinErr != nil {
		res.Reason = checkinErr.Error()
	}
	// 余额快照优先复用本次 CheckinStatus 的结果，**不再额外打一次余额接口**：
	// 签到轮次原来每号要查三次余额（排序探询、状态查询、收尾刷新），37 个号一轮
	// 上百次——正是上游网关对 balance/setting_info 返回 X-TT-System-Error:3 的原因。
	if finalKnown {
		expire := int64(0)
		if st, ok := s.cfg.Pool.Status(uid); ok {
			expire = st.Expire
		}
		s.cfg.Pool.SetBalance(uid, pool.Balance{
			Unit: "credit", Remain: finalCredits, Total: finalCredits,
		}, expire)
		if checkinErr == nil {
			// 「签到解冻」只在成功路径上做：失败路径也调它，等于一次失败签到
			// 顺手把 12h 计划冷却冲掉。
			s.cfg.Pool.ReenableIfCredits(uid, finalCredits)
			s.cfg.Pool.ClearCheckinCooldown(uid)
			return res, nil
		}
	} else if checkinErr == nil {
		// 两个余额接口都受限：跳过快照，签到结果照常返回，不刷 403 噪音。
		s.cfg.Pool.ClearCheckinCooldown(uid)
		return res, nil
	}
	// 签到失败：记一次签到域冷却，避免下一轮对拥塞的上游反复重试
	// （9074「当前参与用户太多」是上游拥塞，重试只会更堵）。
	// 只挡签到轮次，不进网关选号——对话额度跟签到接口拥塞无关。
	if d, ok := checkinCooldown(checkinErr); ok {
		s.cfg.Pool.CooldownCheckin(uid, d, checkinErr.Error())
	}
	return res, checkinErr
}

// checkinCooldown 按签到失败类型决定冷却时长。
// 业务失败（HTTP 200 + 非 0 业务码）不会因重试而改变，短冷却只为不再连打上游；
// plan 权益不足要等很久，给长冷却。
func checkinCooldown(err error) (time.Duration, bool) {
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		if be.Code == 1005 {
			return 12 * time.Hour, true
		}
		return 5 * time.Minute, true
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate {
		return time.Minute, true
	}
	return 0, false
}

// claimWithRetry 只在网络层失败时重试一次。
// 业务失败与 HTTP 错误不会因重试而改变，重试只会成倍增加上游请求、触发风控。
//
// 这里只负责「每日登录领取」这一件事（9095 换指纹、9074 限流的判定都靠它的返回值）。
// 一次性新手礼包不在这里领：同一轮里再领一次就是秒级重复请求，正是本文件反复强调要避免的
// （见下面 claimNewcomerGrants 的说明）。打过的痕迹只记「端点给了答复」这一种，
// 网络层失败留给下一轮——否则一次抖动会把礼包永久判成已结算。
// 返回桌面端那笔礼包的领取结果（nil = 端点没给出答复，交给后面的统一礼包步骤再试）。
func (s *Scheduler) claimWithRetry(a *auth.Auth) (*NewcomerGrant, error) {
	granted, err := s.cfg.Upstream.CheckinClaim(a)
	if err == nil {
		a.MarkDesktopGrantDone(granted)
		return &NewcomerGrant{Key: "desktop_login", Granted: granted}, nil
	}
	if !isNetworkError(err) {
		return nil, err
	}
	time.Sleep(time.Second)
	granted, err = s.cfg.Upstream.CheckinClaim(a)
	if err != nil {
		return nil, err
	}
	a.MarkDesktopGrantDone(granted)
	return &NewcomerGrant{Key: "desktop_login", Granted: granted}, nil
}

// ErrRoundBusy 说明已经有一轮签到/领取在跑：同一账号被两路同时送进 claim 就是秒级二次请求，
// 会延长上游 9074 的限流窗口（本文件反复强调的那条）。面板据此回 409 让用户稍等。
var ErrRoundBusy = errors.New("上一轮签到/领取还在跑，稍等几秒再试")

// NewcomerGrant 是一笔一次性新手礼包的领取结果（面板如实回报用）。
// 一笔一次失败：Error 非空时 Granted 无意义，且账号上的「已问过」标记没落，
// 下一轮签到会自己重试。
type NewcomerGrant struct {
	UID     string `json:"uid,omitempty"` // 面板按号回报时填
	Key     string `json:"key"`           // desktop_login | mobile_login
	Granted bool   `json:"granted"`       // false = 上游说这个号早领过了
	Error   string `json:"error,omitempty"`
}

// claimNewcomerGrants 领两笔一次性新手礼包：桌面端 + 移动端，各 3000 分。
//
// 必须独立成一步、且不依赖签到结果：每日 300 分是上游 00:00 自动入账的，
// CheckinStatus 几乎总是 checked_in=true，签到流程走「already」短路、根本到不了
// claim 分支——礼包挂在 claim 成功路径上等于一辈子领不到（uid 7982218 实测：
// 只有注册礼包 + 桌面端到账，移动端那 3000 从没被试过）。所以每轮签到都无条件
// 试一次，用账号上的「已问过」标记压住重复请求。
//
// skipDesktop 表示本轮已经打过桌面端那条端点（每日领取与它是同一条
// login/points/grant，见 claimWithRetry）：同一轮里再打一次就是秒级重复请求，
// 与「9074 风控时不要连着打」的口径冲突，所以跳过；下一轮标记若还没落就自然重试。
//
// force=true 先抹掉标记再问，给面板的手动领取用（显式点击不该被进程内标记拦下），
// 此时 skipDesktop 一并忽略。
func (s *Scheduler) claimNewcomerGrants(a *auth.Auth, force, skipDesktop bool) []NewcomerGrant {
	if force {
		a.ResetGrantMarks()
	}
	var out []NewcomerGrant
	if !a.DesktopGrantDone() && !(skipDesktop && !force) {
		granted, err := s.cfg.Upstream.ClaimDesktopGrant(a)
		switch {
		case err != nil:
			log.Printf("desktop-grant %s: %v", a.UID, err)
			out = append(out, NewcomerGrant{Key: "desktop_login", Error: err.Error()})
		default:
			a.MarkDesktopGrantDone(granted)
			// 落盘：否则重启后同一天还会对所有号再打一轮（风控来源）。
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("desktop-grant %s: 问询标记落盘失败: %v", a.UID, serr)
			}
			log.Printf("desktop-grant %s: granted=%v", a.UID, granted)
			out = append(out, NewcomerGrant{Key: "desktop_login", Granted: granted})
		}
	}
	if !a.MobileGrantDone() {
		granted, err := s.cfg.Upstream.ClaimMobileGrant(a)
		switch {
		case err != nil:
			log.Printf("mobile-grant %s: %v", a.UID, err)
			out = append(out, NewcomerGrant{Key: "mobile_login", Error: err.Error()})
		default:
			a.MarkMobileGrantDone(granted)
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("mobile-grant %s: 问询标记落盘失败: %v", a.UID, serr)
			}
			log.Printf("mobile-grant %s: granted=%v", a.UID, granted)
			out = append(out, NewcomerGrant{Key: "mobile_login", Granted: granted})
		}
	}
	return out
}

// ClaimNewcomerNow 领单个账号的一次性新手礼包（面板按钮，强制问上游）。
// 与签到轮共用单飞锁：正在跑一轮签到时就别挤进去，返回 ErrRoundBusy 让面板提示稍等。
func (s *Scheduler) ClaimNewcomerNow(uid string) ([]NewcomerGrant, error) {
	if !s.checkinLock.TryLock() {
		return nil, ErrRoundBusy
	}
	defer s.checkinLock.Unlock()
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return nil, ErrNotFound
	}
	if st.Disabled {
		return nil, ErrDisabled
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return nil, ErrNotFound
	}
	grants := s.claimNewcomerGrants(a, true, false)
	for i := range grants {
		grants[i].UID = uid
	}
	return grants, nil
}

// RunNewcomerNow 对全池领一遍一次性新手礼包（面板按钮）。禁用的跳过。
//
// 与签到轮共用单飞锁 + 账号之间留 checkinPace 的节奏：全池一次点下去就是 2×N 个请求
// 连打同一批上游端点，那正是 9074 的成因（RunCheckinNow 一直在这么做，这里照抄）。
func (s *Scheduler) RunNewcomerNow() ([]NewcomerGrant, error) {
	if !s.checkinLock.TryLock() {
		return nil, ErrRoundBusy
	}
	defer s.checkinLock.Unlock()
	out := []NewcomerGrant{}
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if !first {
			time.Sleep(checkinPace())
		}
		first = false
		for _, g := range s.claimNewcomerGrants(a, true, false) {
			g.UID = st.UID
			out = append(out, g)
		}
	}
	return out, nil
}

// RunCheckinOne 单号签到（面板行内「签到」）：同样要先拿到单飞锁，
// 免得和整点那一轮把同一个账号同时送进 claim。
func (s *Scheduler) RunCheckinOne(uid string) (CheckinResult, error) {
	if !s.checkinLock.TryLock() {
		return CheckinResult{UID: uid}, ErrRoundBusy
	}
	defer s.checkinLock.Unlock()
	// 手动签到的语义是「现在就领」：把当天限频标记清掉，未领成功的礼包立刻重问。
	// 已领成功的号有 Granted 终态，不受影响，不会重复打上游。
	if a := s.cfg.Pool.AuthByUID(uid); a != nil {
		a.ResetGrantMarks()
	}
	return s.CheckinUID(uid)
}

// isNetworkError 判断是否传输层失败：http.Client 会把传输错误包成 *url.Error，
// 而 HTTP 4xx/5xx 与业务失败走的是 *upstream.Error / *upstream.BusinessError。
func isNetworkError(err error) bool {
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// CatchUp 启动补跑：已过当天最早的签到时点就补签一次。
// 排程定在 9 点而进程 10 点才起来是常态，不补跑等于当天整天不签。
//
// 不记台账：CheckinUID 先探上游状态，已签到不会重复 claim，
// 代价是进程当天反复重启时会多几次只读探询。
func (s *Scheduler) CatchUp() {
	s.mu.RLock()
	enabled := s.cfg.CheckinEnabled
	hours := append([]int(nil), s.cfg.CheckinHours...)
	s.mu.RUnlock()
	if !enabled || len(hours) == 0 {
		return
	}
	earliest := slices.Min(hours)
	if h := time.Now().Hour(); h < earliest {
		return
	}
	log.Printf("checkin catch-up: 已过 %d 点签到小时，补跑一轮", earliest)
	if _, errs := s.RunCheckinNow(); len(errs) != 0 {
		log.Printf("checkin catch-up: %d 个账号失败", len(errs))
	}
}

// RunRefreshNow 立即对所有账号刷新 token（保活：不看剩余有效期，每天无条件刷一遍）。
//
// 为什么不按 RefreshSkew 只刷快过期的：access token 活 14 天，按「过期前 24h」刷等于
// refresh token 链十天半个月没人碰；上游一旦让它过期就只能重登（掉线）。
// session 失效不立刻杀号：连续 SessionDeadThreshold 次才禁用，成功即清零。
func (s *Scheduler) RunRefreshNow() {
	// 单飞：与签到同理，保活不该被两路同时跑（同一账号两次 ExchangeToken 会互相轮换掉票）。
	if !s.refreshLock.TryLock() {
		log.Printf("[keepalive] 上一轮还在跑，跳过这一轮")
		return
	}
	defer s.refreshLock.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("refresh %s: 连续 %d 次 session 失效 — 禁用", st.UID, pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}
