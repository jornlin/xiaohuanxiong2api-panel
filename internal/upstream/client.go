package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// 上游地址（可被 config.upstream 覆盖）。
const (
	LLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	AuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	PointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	Origin     = "https://xiaohuanxiong.com"

	// ChatPath / CatalogPath 挂在 LLM 网关下；RefreshPath 挂在鉴权域名下。
	ChatPath        = "/chat/completions"
	CatalogPath     = "/model_catalog"
	RefreshPath     = "/refresh"
	LoginCodePath   = "/login_with_authorization_code"
	BalancePath     = "/balance"
	UserInfoPath    = "/user_info"
	EntitlementPath = "/entitlement_info"
	// 每日 300 分是服务端 00:00 自动发放、午夜自动清零（bills 流水实锤），
	// 不存在「调 setting_info 触发发放」这种客户端侧开关——旧 DailyGrantPath
	// 是对抓包的误读。登录领取动作是 desktop 域的 grant（幂等，领过了回
	// granted:false），流水查询是 points 域的 bills。
	DesktopBase     = Origin + "/api/web/desktop/v1"
	PointsGrantPath = "/login/points/grant"
	// 手机端首登奖励是**另一条端点、另一笔额度**：桌面端早已领过的号，手机端照样能
	// 拿满（实测确认）。所以一次性福利要两条都领，只在账号页展示桌面端那笔会漏钱。
	MobileGrantPath = "/login/points/grant"
	PointsBillsPath = "/bills"
	// limit 必须给足：流水按时间倒序，重度使用一天几十条 -N 消费记录会把
	// 今日 +300 的 daily_grant 挤出前 10 条，签到状态就会误判 false。
	PointsBillsQuery = "paging.limit=100&type=all&paging.offset=0"

	// 流水查询的现行端点是 **v2**：v1 /points/v1/bills 对 Bearer 令牌一律 403
	// （2026-11-19 实测，官网前端也早已切到 v2）。v2 用 cursor 分页，type=income
	// 只取发放记录（每日积分/礼包），不会像 type=all 那样被几百条消耗记录淹没。
	PointsBillsV2Path  = "/bills" // 拼在 pointsV2Base()（…/points/v2）后面
	PointsBillsV2Query = "paging.limit=50&type=income"

	// RegionGlobal / RegionCN 保留基线的地区常量：小浣熊只有一个站
	// （xiaohuanxiong.com），面板上的「地区」选择已注释掉（index.html 的 .realm-row），
	// login/start 固定按 RegionCN 建会话。等官方出国际版客户端再放开那块 UI，
	// pool 的分区选号/美元额度逻辑一直在（休眠状态），不需要重写。
	RegionGlobal = "cn"
	RegionCN     = "cn"

	// DefaultConfigName 是上游默认对话模型（目录里拿不到时用它兜底）。
	// v1.1.0 抓包确认：model_catalog 的 default_model = raccoon-8c4485
	// （raccoon-chat-ml-5-5 不在目录里，但上游实测仍接受，保留作别名目标）。
	DefaultConfigName     = "raccoon-8c4485"
	DefaultConfigNameIntl = DefaultConfigName

	// DefaultClientVersion 是出站 x-client-version 头的内置值（v1.1.0 抓包实测）；
	// 官方升版本后可用 config.upstream.client_version 覆盖，不必重编译。
	DefaultClientVersion = "v1.1.0"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 与账号无关（模型/参数/业务码）
	ErrPlanLimit                    // 额度不足 → 计划硬冷却
	ErrSoftRate                     // 限流 → 软冷却
	ErrSessionDead                  // 登录态失效 → 连续达阈值才禁用
	ErrNotFound                     // 404 → 短冷却
	ErrServer                       // 5xx → 熔断计数
	ErrClient                       // 其余 4xx → 降权（本仓按「不罚号」处理）
	ErrBusy                         // 上游排队 → 换号不罚号
	ErrReasoningLoop                // 思考链复读（模型侧问题）→ 中断且不罚号
	ErrGateway                      // 502/504 网关超时 → 不罚号、不换号
)

// String 供日志与面板展示。
func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrBusy:
		return "busy"
	case ErrReasoningLoop:
		return "reasoning_loop"
	case ErrGateway:
		return "gateway"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind           ErrKind
	Status         int
	Msg            string
	Busy           bool // 排队态：可原地重试
	RetryAfterSecs int64
	// ServiceUnavailable = 排队体里 serviceAvailable:false：等也没用，立刻回错。
	ServiceUnavailable bool
	// Throttled = 网关频率/风控拒绝（不是模型排队）：退避交给全局闸门，
	// 单次重试间隔可以很短，不必按模型的 15s 排队窗口等。
	Throttled bool
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Msg
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝（code != 0）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string { return e.Message }

// ErrEmptyStream 上游一个字都没吐（可换号重试，但不罚号）。
var ErrEmptyStream = errors.New("上游返回空流")

// ErrCheckinAlready 今日已领取（上游幂等，不是失败）。
var ErrCheckinAlready = errors.New("今日已领取")

// ErrCodeExpired 授权码已失效：提示重新发起，不要重试。
var ErrCodeExpired = errors.New("授权码已失效，请重新发起登录")

// CodeExpired 是上游「授权码失效」的业务码。
const CodeExpired = 200035

// IsCheckinRateLimited 小浣熊签到没有专用限流码。
func IsCheckinRateLimited(error) bool { return false }

// 失败分类的判据表（HTTP 与流内共用，别各写一份）。
//
// 教训来自同类实现的审计记录：`insufficient` 单独出现太宽——"context window is
// insufficient for this model" 也会命中，于是给一个只是上下文太长的**好号**写 12 小时
// 计划冷却；而 `token` 单独出现同样过宽——"max_tokens exceeded" 会把号记成登录态失效，
// 连续几次就自动禁用。两条都是「静默误伤可用账号」，比漏判贵得多。
//
// 方向：宁可漏判（不罚号、只换号重试），不可误伤。

// quotaSignal 文案里是否**确实**在说额度/余额不足。
func quotaSignal(lower string) bool {
	if strings.Contains(lower, "14018") || strings.Contains(lower, "credits exhausted") {
		return true
	}
	if strings.Contains(lower, "insufficient") {
		for _, w := range []string{"point", "credit", "balance", "quota", "余额", "积分", "额度"} {
			if strings.Contains(lower, w) {
				return true
			}
		}
	}
	for _, pair := range [][2]string{
		{"积分", "不足"}, {"积分", "耗尽"}, {"额度", "不足"}, {"额度", "耗尽"},
		{"余额", "不足"}, {"余额", "耗尽"}, {"quota", "exceeded"}, {"quota", "exhausted"},
	} {
		if strings.Contains(lower, pair[0]) && strings.Contains(lower, pair[1]) {
			return true
		}
	}
	return false
}

// sessionDeadSignal 文案里是否**确实**在说登录态失效（不含泛化的 "token"）。
func sessionDeadSignal(lower string) bool {
	for _, s := range []string{
		"unauthorized", "invalid token", "token expired", "token is expired",
		"token_expired", "authorization_verify_error", "invalid_grant",
		"登录态已失效", "凭证已失效", "请重新登录",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态与正文判定错误类别。
//
// 文案判据与流内的 classifyErrorText 共用 quotaSignal / sessionDeadSignal（见上）：
// 各写一份必然漂移，同一报文会在 HTTP 与 SSE 两条路径上被判成不同类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 认证先判：401 才是登录态失效。
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}
	// 额度信号必须在状态码分支之前：上游会用 403/429 投递「积分不足」，
	// 若先按 403 判成忙/客户端错误，该账号就永远进不了计划冷却。
	if quotaSignal(lower) || status == http.StatusPaymentRequired {
		return ErrPlanLimit
	}
	switch {
	// 403 是权限/风控（例如某个接口只对网页会话开放，或模型权限不足），
	// 判成 session dead 会让好号在连续三次 403 后被自动禁用（bills v1 的 403 就是活例）。
	// 403 归「调用方侧」，不罚号。
	case status == http.StatusForbidden:
		// 403 有两种来源，必须分开：
		//   1. 排队/网关限流/风控：上游把「模型暂不可服务」也用 403 投递；
		//   2. 真正的权限问题（接口只对网页会话开放、模型无权限）。
		// 早先在 switch 里直接把 403 归 ErrClient，于是 (1) 被 handler 当请求错误
		// fail-fast 原样回给客户端——高并发时表现为「批量 403、请求大批量中断」。
		// 先看报文里的忙信号；没有才按权限错误处理。
		if busySignal(body) || throttleSignal(body) {
			return ErrBusy
		}
		return ErrClient
	case status == http.StatusTooManyRequests:
		// 429 默认为账号级限流（要软冷却）；但上游排队体也是 429 时就别罚号。
		if queueUnavailable(body) {
			return ErrBusy
		}
		return ErrSoftRate
	case status == http.StatusGatewayTimeout:
		return ErrGateway
	case status == http.StatusRequestTimeout || status == 499:
		// 499（nginx「client closed request」）/408：调用方在响应完成前断开，多为请求体
		// 过大或上游超时。换号重发只会照样失败、把每个号都记一次错，所以归「客户端侧」。
		return ErrClient
	case status == http.StatusBadGateway:
		return ErrGateway
	case status >= 500:
		return ErrServer
	case status == http.StatusNotFound:
		return ErrNotFound
	}
	if queueUnavailable(body) {
		return ErrBusy
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 访问小浣熊上游。四个基址都可由配置/测试覆盖。
type Client struct {
	// HTTP 用于短 JSON 请求（续期/模型/签到/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，只用 ResponseHeaderTimeout
	// 兜底「上游一直不返首字节」的悬挂。
	StreamHTTP *http.Client

	timeoutMu   sync.RWMutex
	idleTimeout time.Duration

	hostMu sync.RWMutex
	llm    string
	auth   string
	points string
	origin string

	uaMu          sync.RWMutex
	userAgent     string
	clientVersion string

	catMu   sync.RWMutex
	catalog []ModelInfo
	catAt   time.Time

	flightMu sync.Mutex
	flights  map[string]*refreshFlight

	// 余额接口熔断：v1 /points/v1/balance 从 2026-11-19 下午起对 Bearer 令牌返回 403。
	// 每轮签到/刷新都对 30+ 个号重打一次只会持续刺激上游风控，所以 403 后静默 30 分钟，
	// 期间用最后一次成功快照（没有快照就如实报错）。
	quotaMu           sync.Mutex
	quotaBlockedUntil time.Time
	// billsBlockedUntil v1 /bills 403 后的静默窗口（30 分钟）：v1 余额收紧通常与
	// v1 流水同批（2026-11-19 实测）。签到与到期推算已全走 v2；继续每号重打 v1
	// 只是白送请求，还会把网关风控窗口越撑越长。
	billsBlockedUntil time.Time
	// settingBlockedUntil setting_info 也 403 后的短熔断（10 分钟）：两个余额接口
	// 同时被网关拦时，全池逐号重打只是继续喂风控。
	settingBlockedUntil time.Time
	quotaCache          map[string]EntUsage

	// 网关级暂缓（global gate）：上游按频率打回 403/429 时，不是某一个号的问题，
	// 继续全池重试只会把风控窗口越撑越长。这里给所有出站对话请求加一道共享闸门：
	// 命中限流后短暂暂停新请求（指数退避，封顶 10s），请求是排队等待而不是直接被拒。
	throttleMu      sync.Mutex
	gateUntil       time.Time
	nextSlot        time.Time
	throttleStreak  int
	lastThrottle    time.Time
	lastThrottleLog time.Time
	lastRejectLog   time.Time
}

// New 生产默认值：连接池共享，短请求 120s 兜底、长流只看首字节超时。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		llm:        LLMBase,
		auth:       AuthBase,
		points:     PointsBase,
		origin:     Origin,
		flights:    map[string]*refreshFlight{},
		quotaCache: map[string]EntUsage{},
	}
}

// SetTimeouts 热改超时：short 是短请求总超时，header 是首字节超时，idle 是流内空闲上限。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()
	if short > 0 && c.HTTP != nil {
		c.HTTP.Timeout = short
	}
	if header > 0 {
		for _, hc := range []*http.Client{c.HTTP, c.StreamHTTP} {
			if hc == nil {
				continue
			}
			if tr, ok := hc.Transport.(*http.Transport); ok {
				tr.ResponseHeaderTimeout = header
			}
		}
	}
	c.idleTimeout = idle
}

// SetIdentity 覆盖出站 User-Agent 与客户端版本头（空 = 用内置默认）。
func (c *Client) SetIdentity(userAgent, clientVersion string) {
	c.uaMu.Lock()
	c.userAgent = strings.TrimSpace(userAgent)
	v := strings.TrimSpace(clientVersion)
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v // 配置里写 1.1.0 和写 v1.1.0 都行，头里统一带 v（照客户端实测）
	}
	c.clientVersion = v
	c.uaMu.Unlock()
}

// currentVersion 返回 x-client-version 头取值：配置覆盖优先，否则内置抓包实测值。
func (c *Client) currentVersion() string {
	c.uaMu.RLock()
	defer c.uaMu.RUnlock()
	if c.clientVersion != "" {
		return c.clientVersion
	}
	return DefaultClientVersion
}

// SetAgentHosts 覆盖推理网关与鉴权基址（config.upstream.llm_base / auth_base）。
func (c *Client) SetAgentHosts(llm, authBase string) {
	c.hostMu.Lock()
	c.llm = orDefault(strings.TrimRight(strings.TrimSpace(llm), "/"), LLMBase)
	c.auth = orDefault(strings.TrimRight(strings.TrimSpace(authBase), "/"), AuthBase)
	c.hostMu.Unlock()
}

// SetAPIHosts 覆盖积分与主站基址（测试与自建镜像用）。
func (c *Client) SetAPIHosts(points, origin string) {
	c.hostMu.Lock()
	c.points = orDefault(strings.TrimRight(strings.TrimSpace(points), "/"), PointsBase)
	c.origin = orDefault(strings.TrimRight(strings.TrimSpace(origin), "/"), Origin)
	c.hostMu.Unlock()
}

// AgentHosts 返回当前网关基址（面板展示「生效地址」用）。
func (c *Client) AgentHosts() (llm, authBase string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm, c.auth
}

// Origins 返回积分与主站基址（面板展示用）。
func (c *Client) Origins() (points, origin string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points, c.origin
}

func (c *Client) llmBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm
}

func (c *Client) authBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.auth
}

func (c *Client) pointsBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points
}

// pointsV2Base 积分域 v2（流水现行端点）。默认真实地址以 /points/v1 结尾，
// 换成 /v2；测试/自建镜像把 points 覆盖成裸基址时直接拼 /v2。
func (c *Client) pointsV2Base() string {
	base := strings.TrimRight(c.pointsBase(), "/")
	if strings.HasSuffix(base, "/v1") {
		return strings.TrimSuffix(base, "/v1") + "/v2"
	}
	return base + "/v2"
}

func (c *Client) originBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.origin
}

// desktopBase 桌面端专属域（登录领取积分等）：挂在主站 origin 下，
// 与积分/鉴权域同生共死，config 覆盖主站时一起跟着走。
func (c *Client) desktopBase() string {
	return c.originBase() + "/api/web/desktop/v1"
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.httpClient()
}

func (c *Client) currentUA() string {
	c.uaMu.RLock()
	defer c.uaMu.RUnlock()
	if c.userAgent != "" {
		return c.userAgent
	}
	return "xhx2api-panel"
}

type apiResponse struct {
	Status int
	Body   []byte
}

// do 发一次请求并读回响应体（上限 4MiB）。
func (c *Client) do(req *http.Request) (*apiResponse, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.currentUA())
	}
	c.timeoutMu.RLock()
	resp, err := c.httpClient().Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取小浣熊响应失败"}
	}
	return &apiResponse{Status: resp.StatusCode, Body: raw}, nil
}

// doJSON 发请求：body 非 nil 时按 JSON 序列化，headers 原样写入。
func (c *Client) doJSON(method, url string, body any, headers map[string]string) (*apiResponse, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return c.do(req)
}

// payloadOf 校验 2xx 并拆掉 {code,message,data} 信封，返回 data 节点。
func payloadOf(resp *apiResponse, action string) (map[string]any, error) {
	if resp.Status < 200 || resp.Status >= 300 {
		hint := ""
		switch resp.Status {
		case 401:
			hint = "，登录态可能已失效"
		case 403:
			// 403 是权限/接口受限（v1 金额接口对 Bearer 收紧就是活例），不是登录态失效。
			hint = "，接口权限受限（不是登录态失效）"
		case 429:
			hint = "，请稍后重试"
		}
		return nil, &Error{
			Kind:   Classify(resp.Status, string(resp.Body)),
			Status: resp.Status,
			Msg:    fmt.Sprintf("小浣熊 %s失败（HTTP %d）%s: %s", action, resp.Status, hint, truncate(errorText(resp.Body), 200)),
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: fmt.Sprintf("小浣熊 %s未返回有效 JSON 对象", action)}
	}
	if code, ok := intOf(envelope["code"]); ok && code != 0 {
		return nil, &BusinessError{Code: code, Message: orDefault(text(envelope, "message", "msg"), fmt.Sprintf("小浣熊 %s被拒绝（code=%d）", action, code))}
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// errorText 从错误体里取人类可读文案。
func errorText(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return strings.TrimSpace(string(body))
	}
	if msg := text(payload, "message", "msg", "detail"); msg != "" {
		return msg
	}
	if inner, ok := payload["error"].(map[string]any); ok {
		return text(inner, "message")
	}
	return strings.TrimSpace(string(body))
}

// ---------------------------------------------------------------------------
// 续期

type refreshFlight struct {
	done  chan struct{}
	token string
	exp   int64
	uid   string
	err   error
}

// RefreshToken 无条件续期一次。
func (c *Client) RefreshToken(a *auth.Auth) error {
	_, err := c.refresh(a)
	return err
}

// RefreshTokenIfNeeded 仅在 skew 内将过期时续期；返回是否真的续了。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	if !a.NeedsRefresh(skew) {
		return false, nil
	}
	return c.refresh(a)
}

// refresh 单飞：同一份 refresh_token 并发只打一次上游（上游每次轮换，并发会让一副票作废）。
func (c *Client) refresh(a *auth.Auth) (bool, error) {
	return c.refreshWith(a, c.clientIDHeaders())
}

// refreshWeb 用 web 头续期，换一张 **web 会话**票。
//
// 为什么要单独一条：2026-11-20 实测，每日 300 分不是桌面端 grant 端点发放的，
// 而是账号第一次以 web 会话调 setting_info 时惰性入账（流水 client_type=web）；
// 桌面会话调 setting_info 被网关 403（X-TT-System-Error:3）。会话类型由续期请求
// 的头决定，所以先 web 续期再 setting_info 才能触发入账。
func (c *Client) refreshWeb(a *auth.Auth) (bool, error) {
	return c.refreshWith(a, c.webHeaders())
}

func (c *Client) refreshWith(a *auth.Auth, headers map[string]string) (bool, error) {
	refreshToken := a.RefreshTokenValue()
	if refreshToken == "" {
		return false, &Error{Kind: ErrClient, Status: 400, Msg: "该账号没有 refresh_token，无法续期，请重新登录"}
	}
	key := a.UID + "|" + refreshToken
	c.flightMu.Lock()
	if c.flights == nil {
		c.flights = map[string]*refreshFlight{}
	}
	if flight, ok := c.flights[key]; ok {
		c.flightMu.Unlock()
		<-flight.done
		if flight.err != nil {
			return false, flight.err
		}
		a.SetTokens(flight.token, "", flight.exp)
		applyRefreshedUID(a, flight.uid)
		return true, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.flightMu.Unlock()

	token, refresh, exp, uid, err := c.refreshOnceWith(a, headers)
	flight.token, flight.exp, flight.uid, flight.err = token, exp, uid, err
	close(flight.done)
	c.flightMu.Lock()
	delete(c.flights, key)
	c.flightMu.Unlock()
	if err != nil {
		return false, err
	}
	a.SetTokens(token, refresh, exp)
	applyRefreshedUID(a, uid)
	return true, nil
}

// applyRefreshedUID 用续期响应补 uid：小浣熊的 access token 未必是 JWT（也可能只是不透明串），
// uid 只有上游自己知道，缺了就没法落盘成账号。
func applyRefreshedUID(a *auth.Auth, uid string) {
	if uid == "" || a.UID != "" {
		return
	}
	a.UID = uid
	if a.Nickname == "" {
		a.Nickname = uid
	}
}

// refreshOnceWith 真的打一次续期接口并返回新票（不写回账号）。
func (c *Client) refreshOnceWith(a *auth.Auth, headers map[string]string) (token, refresh string, exp int64, uid string, err error) {
	resp, err := c.doJSON(http.MethodPost, c.authBase()+RefreshPath,
		map[string]string{"refresh_token": a.RefreshTokenValue()}, headers)
	if err != nil {
		return "", "", 0, "", err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return "", "", 0, "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "小浣熊刷新令牌已失效，请重新登录该账号"}
	}
	data, err := payloadOf(resp, "凭证续期")
	if err != nil {
		return "", "", 0, "", err
	}
	token = text(data, "access_token", "accessToken", "token")
	if token == "" {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期响应缺少令牌，旧凭证未被覆盖"}
	}
	uid = text(data, "user_id", "uid", "userId")
	if uid != "" && a.UID != "" && uid != a.UID {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期返回了不同账号，旧凭证未被覆盖"}
	}
	// 上游可能不轮换 refresh_token：没有新值就沿用旧的（SetTokens 收到空串即不动它）。
	refresh = text(data, "refresh_token", "refreshToken")
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		if ms := millisOf(data[key]); ms > 0 {
			exp = ms
			break
		}
	}
	return token, refresh, exp, uid, nil
}

// ---------------------------------------------------------------------------
// 用户资料

// GetUserInfo 用上游 GET {auth_base}/user_info 补齐 uid/昵称（抓包实测存在，
// 200 返回 {data:{id,name,...}}）；接口不可用时退回 JWT claims，保证不比以前差。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	if token := a.JWT(); token != "" {
		if resp, uerr := c.doJSON(http.MethodGet, c.authBase()+UserInfoPath, nil,
			map[string]string{"Accept": "application/json", "Authorization": "Bearer " + token}); uerr == nil {
			if data, perr := payloadOf(resp, "用户资料"); perr == nil {
				uid = text(data, "id", "user_id", "uid")
				nickname = text(data, "name", "nickname")
				enterpriseID = text(data, "office_org_name", "org_code")
				if uid != "" {
					return uid, nickname, enterpriseID, nil
				}
			}
		}
	}
	token := a.JWT()
	uid = auth.JWTUserID(token)
	nickname = a.Nickname
	if uid == "" && a.UID != "" {
		return a.UID, nickname, "", nil
	}
	if uid == "" {
		return "", "", "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "无法从 token 解析用户标识，请重新登录"}
	}
	return uid, nickname, "", nil
}

// ---------------------------------------------------------------------------
// 推理

// ChatStream 把 OpenAI 请求体发给小浣熊的 chat/completions。
//
// 上游就是 OpenAI 形状：body 原样转发（model 已由 handler 定成规范 id），
// 返回未读的 SSE 响应体供 Stream/Aggregate 消费；非 2xx 时返回分类后的 *Error。
//
// turnTitle 非空时作为 X-RACCOON-Turn-Title 发出去。这不是装饰：上游会把标题写进
// 积分流水的 event_name（「任务消耗 · <title>」），于是每一笔扣费都能反查到是哪一次
// 请求——积分对账（见 upstream.RecentBills / panel.reconcileCredits）完全靠它。
func (c *Client) ChatStream(a *auth.Auth, body []byte, turnTitle string) (io.ReadCloser, int, []byte, error) {
	url := c.llmBase() + ChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造小浣熊上游请求失败"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", c.currentUA())
	// 标题用于积分对账：上游把它拼进扣费流水的 event_name（「任务消耗 · <title>」）。
	// 头名沿用抓包里的写法（v1.1.0 客户端发 X-RACCOON-Title），空标题就不发。
	if title := strings.TrimSpace(turnTitle); title != "" {
		req.Header.Set("X-RACCOON-Title", title)
	}
	if token := a.JWT(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	c.timeoutMu.RLock()
	resp, err := c.streamClient().Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		uid := ""
		if a != nil {
			uid = a.UID
		}
		// 先记拒绝样本并更新全局闸门，再分类：403 的正文是判断「排队/风控/权限」
		// 的唯一依据，不能既不进日志也不进闸门。
		xtt := resp.Header.Get("X-TT-System-Error")
		throttled := c.noteChatReject(resp.StatusCode, uid, xtt, raw)
		_ = resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		// X-TT-System-Error 是网关级限流标记，正文可能不含任何关键字；
		// Classify 只看得到正文，这里用头上的信号把 403/429 归为可等待的忙。
		if xtt != "" && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) {
			kind = ErrBusy
		}
		msg := truncate(errorText(raw), 300)
		if msg == "" {
			msg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, raw, &Error{Kind: kind, Status: resp.StatusCode, Msg: msg,
			RetryAfterSecs: retryAfterOf(string(raw)), ServiceUnavailable: queueUnavailable(string(raw)),
			Throttled: throttled}
	}

	c.NoteUpstreamOK()

	// stream:false 时上游回的是普通 JSON（不是 SSE）：包成一帧 SSE 交给同一套管道，
	// 下游（handler/Aggregate）就只用认一种线上形状。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游响应失败"}
		}
		if event := parseSSELine(string(raw)); event.Kind == sseError {
			return nil, event.Status, raw, &Error{Kind: event.Class, Status: event.Status,
				Msg: orDefault(event.Message, event.Raw), RetryAfterSecs: event.RetryAfterSecs,
				ServiceUnavailable: event.ServiceUnavailable}
		}
		frame := deltaFrameOf(raw)
		stream := "data: " + string(frame) + "\n\ndata: [DONE]\n\n"
		return io.NopCloser(strings.NewReader(stream)), resp.StatusCode, nil, nil
	}

	// HTTP 200 也可能是错误：上游会用一帧 {"error":{...}} 表达限流/额度。
	// 只看状态码会把它当成功，账号也就没法换——所以先窥一帧再交出去。
	br := newPeekReader(resp.Body)
	first, err := br.peekFrame()
	if err != nil {
		_ = resp.Body.Close()
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游首帧失败"}
	}
	if first.Kind == sseError {
		_ = resp.Body.Close()
		return nil, first.Status, []byte(first.Raw), &Error{Kind: first.Class, Status: first.Status,
			Msg: orDefault(first.Message, first.Raw), RetryAfterSecs: first.RetryAfterSecs,
			ServiceUnavailable: first.ServiceUnavailable}
	}
	return br.stream(), resp.StatusCode, nil, nil
}

// PrepareBody 是入站体的轻量规整钩子：小浣熊原样透传，这里不改任何字段。
func PrepareBody(src []byte) []byte { return src }

var (
	retryAfterRE   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	waitTimeRE     = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	serviceAvailRE = regexp.MustCompile(`"serviceAvailable"\s*:\s*false`)
)

// queueUnavailable 看排队体里的 serviceAvailable:false——上游自己说此刻不可服务。
func queueUnavailable(raw string) bool {
	flat := strings.ReplaceAll(raw, `\`, "")
	return serviceAvailRE.MatchString(flat)
}

// busySignal 判断报文是不是「上游忙/排队」而不是请求本身有问题。
// 与 throttleSignal 的区别：这里的信号很强（排队字段/错误码），任何下文都认；
// throttleSignal 更宽（限流文案），只在 403/429 这类状态码上才拿来判。
func busySignal(raw string) bool {
	flat := strings.ReplaceAll(raw, `\`, "")
	lower := strings.ToLower(flat)
	if queueUnavailable(flat) {
		return true
	}
	for _, s := range []string{"isqueued", "queued", "in queue", "10605", "排队", "\"queue\"", "queue_position"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// throttleSignal 判断 403/429 的报文是否在说「频率/风控」——高并发下上游网关
// （X-TT-System-Error:3）会这样打回。这类拒绝与账号健康、请求参数都无关，
// 应当暂缓出站后重试，不能原样回给客户端。
func throttleSignal(raw string) bool {
	lower := strings.ToLower(strings.ReplaceAll(raw, `\`, ""))
	for _, s := range []string{
		"x-tt-system-error", "too many requests", "rate limit", "rate_limit",
		"too frequent", "requests are too frequent", "request frequency",
		"频繁", "限流", "风控", "繁忙", "busy", "throttl",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// minSendGap 是全局出站最小间隔：重启/客户端重连时几百个请求会同时涌进来，
// 上游按频率直接 429（实测一次重启能让 130+ 个号同时被限流）。把出站切成
// 最多 1 次/秒，尖峰被摊平，账号就不会成批冷却。
const minSendGap = time.Second

// maxSendSlotWait 是单个出站名额的最长排队时间：重启后 138 个请求同时到达时，
// 严格按 1s 排队会让最后一个等 2 分多钟，超过客户端/代理超时。到 30s 就先放行
// （宁可让 429 闸门再兜底，也不能把请求挂死）。
const maxSendSlotWait = 30 * time.Second

// WaitSendSlot 按全局最小间隔排一个出站名额（调用方在每次 attempt 前调用）。
// 名额先占后等：并发调用者按到达顺序排队，不会一起醒来再撞一次。
func (c *Client) WaitSendSlot(ctx context.Context) {
	c.throttleMu.Lock()
	now := time.Now()
	if c.nextSlot.Before(now) {
		c.nextSlot = now
	}
	wait := c.nextSlot.Sub(now)
	c.nextSlot = c.nextSlot.Add(minSendGap)
	c.throttleMu.Unlock()
	if wait <= 0 {
		return
	}
	if wait > maxSendSlotWait {
		wait = maxSendSlotWait
	}
	timer := time.NewTimer(wait)
	select {
	case <-ctx.Done():
		timer.Stop()
	case <-timer.C:
	}
}

// NoteUpstreamOK 一次成功出站。只有长时间（60s）没有再见到限流才把退避档位清零，
// 避免「失败-成功-失败」来回抖动时档位重置、闸门失去作用。
func (c *Client) NoteUpstreamOK() {
	c.throttleMu.Lock()
	if c.throttleStreak != 0 && time.Since(c.lastThrottle) > 30*time.Second {
		c.throttleStreak = 0
	}
	c.throttleMu.Unlock()
}

// WaitGate 阻塞到全局闸门放开（或 ctx 结束）。handler 在每次出站尝试前调用：
// 上游限流期间请求是「等一会儿再发」，而不是立刻拿到 403。
func (c *Client) WaitGate(ctx context.Context) {
	c.WaitGateUntil(ctx, time.Now().Add(30*time.Second))
}

// WaitGateUntil 与 WaitGate 相同，但共享调用方给的截止时间：一个请求内部的
// 多次重试/换号共用同一份等待预算，不会每步各等 30s。
func (c *Client) WaitGateUntil(ctx context.Context, deadline time.Time) {
	for {
		if !time.Now().Before(deadline) {
			return
		}
		c.throttleMu.Lock()
		until := c.gateUntil
		c.throttleMu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			return
		}
		if left := time.Until(deadline); d > left {
			d = left
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// noteChatReject 记录一次上游拒绝（采样），并视是否为限流信号更新全局闸门。
//
// 采样日志是排障的关键：403 的报文以前既没进请求记录也没进日志，只能看到
// 「客户端批量 403」，无法判断是排队、风控还是真的权限问题。
func (c *Client) noteChatReject(status int, uid, xtt string, raw []byte) (throttled bool) {
	now := time.Now()
	body := truncate(string(raw), 300)
	// 全局闸门覆盖两类：
	//   1. 403 + X-TT-System-Error / 频率文案：网关级风控；
	//   2. 429（线上 body code=200108「您的模型请求频率超限」）：并发尖峰时上游会对
	//      **整池**同时打回 429（实测一次重启后 130/138 个号同时被限流）。只按账号
	//      软冷却会不断缩池 → 剩余号压力更大 → 更多 429，形成级联。必须全局限速。
	// 403 的模型排队（isQueued/serviceAvailable）仍不拉闸门：那是单模型窗口。
	throttled = status == http.StatusTooManyRequests ||
		(status == http.StatusForbidden && (xtt != "" || throttleSignal(body)))

	c.throttleMu.Lock()
	logNow := now.Sub(c.lastRejectLog) >= 5*time.Second
	if logNow {
		c.lastRejectLog = now
	}
	if !throttled {
		c.throttleMu.Unlock()
		if logNow {
			log.Printf("[upstream] chat 拒绝 HTTP %d uid=%s xtt=%q body=%s", status, uid, xtt, body)
		}
		return
	}
	// 30s 没再见限流就从 1s 重新起退；否则档位递增，封顶 2s。
	// 封顶不能太大：429 在高并发下会持续出现，10s 的全景暂停会把正常吞吐压到
	// 个位数请求/分钟（实测级联恢复期更糟）。2s 的节奏足以打散并发尖峰。
	if now.Sub(c.lastThrottle) > 30*time.Second {
		c.throttleStreak = 0
	}
	c.lastThrottle = now
	if c.throttleStreak < 6 {
		c.throttleStreak++
	}
	delay := time.Second << (c.throttleStreak - 1)
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	if until := now.Add(delay); until.After(c.gateUntil) {
		c.gateUntil = until
	}
	streak := c.throttleStreak
	c.throttleMu.Unlock()

	if logNow {
		log.Printf("[upstream] 网关限流 HTTP %d uid=%s xtt=%q，暂停新出站 %s（连续第 %d 次）body=%s",
			status, uid, xtt, delay, streak, body)
	}
	return throttled
}

// retryAfterOf 抓上游建议的重试间隔（秒）。
func retryAfterOf(raw string) int64 {
	flat := strings.ReplaceAll(raw, `\`, "")
	for _, re := range []*regexp.Regexp{retryAfterRE, waitTimeRE} {
		if m := re.FindStringSubmatch(flat); len(m) == 2 {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// millisOf 把数字/数字字符串归一成毫秒时间戳（0 = 没有）。
func millisOf(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return normalizeMillis(int64(typed))
	case int64:
		return normalizeMillis(typed)
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return normalizeMillis(n)
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func normalizeMillis(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 100_000_000_000 {
		return value * 1000
	}
	return value
}

// truncate 截断到 n 个 rune（不切坏多字节字符）。
func truncate(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n])
}

// text 取第一个非空的字符串/数字字段。
func text(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			return value.String()
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

// intOf 把 JSON 数字/数字字符串归一成 int。
func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// deltaFrameOf 把非流式 chat.completion 摊成同形状的一帧 chunk（message → delta），
// 让下游只用处理增量帧。解析失败时原样返回，交给 Aggregate 报空流。
func deltaFrameOf(raw []byte) []byte {
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return raw
	}
	choices, _ := frame["choices"].([]any)
	if len(choices) == 0 {
		return raw
	}
	if choice, ok := choices[0].(map[string]any); ok {
		if message, ok := choice["message"].(map[string]any); ok {
			choice["delta"] = message
			delete(choice, "message")
			if _, has := choice["index"]; !has {
				choice["index"] = 0
			}
		}
	}
	frame["object"] = "chat.completion.chunk"
	out, err := json.Marshal(frame)
	if err != nil {
		return raw
	}
	return out
}
