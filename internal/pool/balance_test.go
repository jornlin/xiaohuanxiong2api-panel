package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// v1 balance 不含「今日发放额」；它的快照不能把当天已由流水算出的值清零，
// 但跨过午夜后的第一份未知快照必须清掉昨天的数。
func TestBalanceKeepsDailyTodayWithinDay(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{Data: auth.Data{UID: "u-1"}})

	p.SetBalance("u-1", Balance{Remain: 900, Daily: 600, DailyToday: 300, DailyTodayKnown: true}, 0)
	st, ok := p.Status("u-1")
	if !ok || st.DailyToday != 300 || !st.DailyTodayKnown {
		t.Fatalf("首次快照: %+v", st)
	}

	// v1 快照：没有 DailyToday 字段。
	p.SetBalance("u-1", Balance{Remain: 800, Daily: 500}, 0)
	st, _ = p.Status("u-1")
	if st.DailyToday != 300 || !st.DailyTodayKnown {
		t.Fatalf("同日未知快照不应清空今日发放额: %+v", st)
	}

	// 把日期改成昨天：再收到未知快照就必须清空，避免面板显示昨天的「今日已发」。
	p.mu.Lock()
	p.byUID["u-1"].dailyTodayDate = "2000-01-01"
	p.mu.Unlock()
	p.SetBalance("u-1", Balance{Remain: 700, Daily: 400}, 0)
	st, _ = p.Status("u-1")
	if st.DailyToday != 0 || st.DailyTodayKnown {
		t.Fatalf("跨日应清空今日发放额: %+v", st)
	}
}

// 选号用可扣费额度：total_chargeable_points 比账面 available 少时按可扣费算，
// 未知时回落到账面余额。
func TestSpendableCreditsPrefersChargeable(t *testing.T) {
	e := &entry{credits: 1000, chargeable: 200, chargeableKnown: true}
	if got := e.spendableCredits(); got != 200 {
		t.Fatalf("spendable = %d，期望 200", got)
	}
	e.chargeableKnown = false
	if got := e.spendableCredits(); got != 1000 {
		t.Fatalf("未知时应回落账面余额，得到 %d", got)
	}
	e.unit = "usd"
	if got := e.spendableCredits(); got != 0 {
		t.Fatalf("美元号应返回 0，得到 %d", got)
	}
}

// setting_info 回退快照没有冻结/归属字段：不能把上一份 v1 的「充值冻结」清掉，
// 且在可扣费未知时要继续把冻结额从可花费余额里扣掉。
func TestBalancePreservesExtrasOnFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{Data: auth.Data{UID: "u-1"}})

	p.SetBalance("u-1", Balance{
		Remain: 1000, Topup: 400, TopupFrozen: true, FrozenTopup: 400,
		Chargeable: 600, ChargeableKnown: true,
		OwnerType: "users", OwnerID: "u-1", ExtrasKnown: true,
	}, 0)

	// 回退快照：只有总额，ExtrasKnown=false。
	p.SetBalance("u-1", Balance{Remain: 900, Chargeable: 900}, 0)
	st, _ := p.Status("u-1")
	if !st.TopupFrozen || st.FrozenTopup != 400 {
		t.Fatalf("回退快照清掉了冻结标记: %+v", st)
	}
	if st.ChargeableKnown {
		t.Fatal("回退快照的可扣费应标为未知")
	}
	p.mu.RLock()
	spendable := p.byUID["u-1"].spendableCredits()
	p.mu.RUnlock()
	if spendable != 500 {
		t.Fatalf("可花费应扣除冻结 400（900-400=500），得到 %d", spendable)
	}
}

// HasHealthy 只回答「池子里有没有可用的号」：冷却/禁用不算，在途满仍算
// （后者正是 handler 值得短暂等待空位的场景）。
func TestHasHealthy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{Data: auth.Data{UID: "u-1"}})
	if !p.HasHealthy("") {
		t.Fatal("健康号应返回 true")
	}
	p.Disable("u-1", "test")
	if p.HasHealthy("") {
		t.Fatal("禁用号不应返回 true")
	}
}

// 上游首字延迟 EWMA：快账号的选号权重应高于持续慢的账号（新会话场景）。
func TestLatencyWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{Data: auth.Data{UID: "fast"}})
	p.Add(&auth.Auth{Data: auth.Data{UID: "slow"}})
	for i := 0; i < 5; i++ {
		p.NoteLatency("fast", 1200)
		p.NoteLatency("slow", 18000)
	}
	now := time.Now()
	p.mu.RLock()
	wf := p.weight(p.byUID["fast"], 0, now)
	ws := p.weight(p.byUID["slow"], 0, now)
	p.mu.RUnlock()
	if wf <= ws {
		t.Fatalf("快账号权重应更高：fast=%v slow=%v", wf, ws)
	}
}
