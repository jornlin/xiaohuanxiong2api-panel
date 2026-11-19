package upstream

import "testing"

// 分类判据必须「宁可漏判，不可误伤」：误判的代价是给一个好号写 12 小时计划冷却、
// 或者把它记成登录态失效直到自动禁用。
func TestClassifyNoFalsePenalty(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		// 上下文太长 ≠ 额度不足（曾把 insufficient 单独当额度信号）
		{"context window insufficient", 400,
			`{"code":4001,"message":"context window is insufficient for this model"}`, ErrClient},
		{"max_tokens exceeded", 400,
			`{"error":{"message":"max_tokens exceeded: 80001 > 80000"}}`, ErrClient},
		// 真的额度不足：要能认出来
		{"积分不足", 400, `{"code":14018,"message":"积分不足"}`, ErrPlanLimit},
		{"credits exhausted", 400, `{"message":"credits exhausted"}`, ErrPlanLimit},
		{"insufficient credit", 402, `{"message":"insufficient credit balance"}`, ErrPlanLimit},
		// 403 是权限（某接口只对网页会话开放、模型权限不足），不是登录态失效：
		// 判成 session dead 会让好号在连续三次 403 后被自动禁用（bills v1 的 403 就是实例）。
		{"403 forbidden", 403, `{"message":"insufficient permissions to access this resource"}`, ErrClient},
		// 403 也可能是上游排队/网关限流的投放方式（高并发下的批量 403）：
		// 判成 ErrClient 会让 handler fail-fast，必须归 ErrBusy 走等待/重试。
		{"403 queue", 403, `{"message":"model is queued","serviceAvailable":false}`, ErrBusy},
		{"403 Queue", 403, `{"message":"request isQueued, retryAfterSeconds":15}`, ErrBusy},
		{"403 throttle", 403, `{"message":"too many requests, rate limit exceeded"}`, ErrBusy},
		{"403 busy", 403, `{"message":"服务繁忙，请稍后重试"}`, ErrBusy},
		// 额度信号优先于 403 的忙判据：该罚计划冷却，不能当排队到处重试。
		{"403 quota", 403, `{"message":"积分不足，服务繁忙"}`, ErrPlanLimit},
		{"429 queue", 429, `{"message":"busy","serviceAvailable":false}`, ErrBusy},
		{"401", 401, `{"message":"unauthorized"}`, ErrSessionDead},
		// 499 是调用方断开：不能换号重发
		{"499 abort", 499, `client has aborted request`, ErrClient},
		{"408", 408, `timeout`, ErrClient},
		{"429", 429, `slow down`, ErrSoftRate},
		{"502", 502, `bad gateway`, ErrGateway},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d) = %v，期望 %v", c.name, c.status, got, c.want)
		}
	}
}

// 流内文案分类与 HTTP 同一张表：同一报文在两条路径上「罚不罚号」必须一致。
//
// 两者允许的差异只有「不罚号的那一侧怎么叫」：HTTP 会给 4xx 一个 ErrClient（好据实回给
// 调用方），流内没有状态码、认不出来就归 ErrNone（模型/参数问题）。罚号的三种
// （计划冷却 / 软限流 / 登录态失效）则必须逐格一致——否则同一个报文在流式与非流式
// 下会给账号记两种不同的账。
func TestStreamClassifyMatchesHTTP(t *testing.T) {
	penalty := func(k ErrKind) bool {
		return k == ErrPlanLimit || k == ErrSoftRate || k == ErrSessionDead
	}
	// status 给「这条报文真会伴随的状态码」：流内没有状态码，比的是「认不认得出罚号」。
	cases := []struct {
		status int
		body   string
	}{
		{400, `{"message":"context window is insufficient for this model"}`},
		{400, `{"message":"max_tokens exceeded"}`},
		{400, `{"message":"积分不足"}`},
		{401, `{"message":"unauthorized"}`},
		{400, `{"message":"insufficient credit balance"}`},
		{429, `{"message":"rate limit exceeded"}`},
	}
	for _, c := range cases {
		a, b := Classify(c.status, c.body), classifyErrorText(c.body)
		if penalty(a) != penalty(b) || (penalty(a) && a != b) {
			t.Errorf("报文 %s（HTTP %d）: HTTP=%v 流内=%v（罚号口径应一致）", c.body, c.status, a, b)
		}
	}
	// 特别是 "token" 单独出现，不能再被判成登录态失效。
	if got := classifyErrorText(`max_tokens exceeded`); got == ErrSessionDead {
		t.Errorf("max_tokens 报文被误判成登录态失效")
	}
	if got := classifyErrorText(`context window is insufficient for this model`); got == ErrPlanLimit {
		t.Errorf("上下文过长被误判成额度不足")
	}
}

// 200 信封里的 statusCodeValue=403 是上游「排队」的另一种投递形状：
// 必须与 HTTP 403 一样归 ErrBusy，而不是当限流去软冷却账号。
func TestStreamEnvelope403IsBusy(t *testing.T) {
	for _, raw := range []string{
		`{"message":"model is queued","statusCodeValue":403}`,
		`{"error":{"message":"busy","statusCode":403}}`,
		`{"status_code":403,"message":"暂不可服务"}`,
	} {
		if got := classifyErrorText(raw); got != ErrBusy {
			t.Errorf("classifyErrorText(%s) = %v，期望 ErrBusy", raw, got)
		}
	}
}

// statusCodeValue 在外层 chunk 上，parseSSELine 必须按 status 兜底：
// 200 信封 + 403 是排队，不能漏判成普通错误。
func TestParseSSEEnvelope403(t *testing.T) {
	ev := parseSSELine(`{"error":{"message":"暂不可服务"},"statusCodeValue":403}`)
	if ev.Kind != sseError || ev.Status != 403 || ev.Class != ErrBusy {
		t.Fatalf("事件 = %+v，期望 sseError/403/ErrBusy", ev)
	}
	// 额度不足仍按额度处理，不被 403 覆盖。
	ev = parseSSELine(`{"error":{"message":"积分不足","statusCodeValue":403}}`)
	if ev.Class != ErrPlanLimit {
		t.Fatalf("额度错误被 403 覆盖成 %v", ev.Class)
	}
}

// serviceAvailable/retryAfterSeconds 在外层 chunk：解析时要读到，
// 否则队列「永远排不上」的会白等两轮 15s。
func TestParseSSEOuterQueueFields(t *testing.T) {
	ev := parseSSELine(`{"error":{"message":"busy"},"statusCodeValue":403,` +
		`"serviceAvailable":false,"retryAfterSeconds":20}`)
	if ev.Class != ErrBusy || !ev.ServiceUnavailable || ev.RetryAfterSecs != 20 {
		t.Fatalf("事件 = %+v，期望 Busy/hopeless/20s", ev)
	}
}
