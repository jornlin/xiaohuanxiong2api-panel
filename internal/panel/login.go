package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

const (
	loginTTL = 15 * time.Minute
)

var errLoginSession = errors.New("登录链接已失效，请回面板重新点「添加账号」")

// errLoginSave 换票成功但凭证落盘失败：授权码已经花掉，重试换票没有意义。
var errLoginSave = errors.New("登录成功但账号凭证保存失败")

// loginDone 是一次已完成会话的结果（state → 账号）。只服务「同一条回调被重复提交」这一种情况：
// 授权码一次性，重复提交得幂等回应成功，而不是甩一句「链接失效」把人吓回去重开登录。
type loginDone struct {
	UID      string
	Nickname string
	At       time.Time
}

// loginSession 是一次登录流程（一个账号一个全新设备号）。
//
// 两种形态：Qr != nil 时走微信扫码轮询（不需要任何回调地址，服务器/局域网部署都能用）；
// 否则走 Qr == nil 的环回回调（浏览器登录后打 /auth/callback）。
type loginSession struct {
	Machine string
	Realm   string
	Login   *upstream.DeviceLogin
	Qr      *upstream.QrLogin
	At      time.Time
	// Busy 上一次 poll 还没回来：前端 3s 一次比上游慢时会重叠，重叠会并发打同一个 nonce。
	Busy bool
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newMachineID 生成账号的设备指纹（UUID4，与官方客户端同形）。
func newMachineID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	}, "-")
}

// authCallback 接官方授权页的回调（无需 API key：这是浏览器跳转，不是面板前端调用）。
//
// 只做三件事：核对 state 属于哪个未完成的登录会话、把 code 交给它、后台主动换票
// （不再等前端轮询）。浏览器那边一提交就立刻回一个干净的成功页，换票在后台跑。
//
// 安全边界：state 是 32 字节随机数，只存在内存里、5~15 分钟内一次性有效；code 本身也是
// 一次性授权码（上游 300 秒过期）。就算有人扫到这个端点，没有 state 也塞不进账号。
func (p *Panel) authCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	// 官方页在用户取消/失败时也可能带 error 回来。
	if msg := query.Get("error"); msg != "" {
		writeCallbackPage(w, http.StatusBadRequest, "授权未完成", msg)
		return
	}
	if state == "" || code == "" {
		writeCallbackPage(w, http.StatusBadRequest, "回调参数不全", "缺少 state 或 code")
		return
	}
	p.loginMu.Lock()
	sess, ok := p.logins[state]
	p.loginMu.Unlock()
	if !ok || sess.Login == nil || time.Since(sess.At) > loginTTL {
		writeCallbackPage(w, http.StatusGone, "登录会话已失效", "请回到面板重新点「添加账号」")
		return
	}
	duplicate := sess.Login.Pending()
	if err := sess.Login.AcceptCallback(state, code); err != nil {
		writeCallbackPage(w, http.StatusBadRequest, "无法接收授权", err.Error())
		return
	}
	if duplicate {
		// 与「粘贴回调」撞车：同一个码已经被收下，换票可能正在进行；这里再推一次
		// （finishLoginSession 有 Busy 闸门，重复调用是安全的），照样回成功页。
		go p.autoFinishLogin(state)
		writeCallbackPage(w, http.StatusOK, "这条回调已经收到过了", "换票正在进行，回到面板即可（账号会自动出现）")
		return
	}
	log.Printf("panel: 收到浏览器授权回调 state=%.8s…", state)
	// 换票不能只靠前端轮询：面板页面可能已刷新/关闭（loginID 丢了就再没人 poll），
	// 那样授权码会一直躺在会话里，账号永远不入池。这里后台主动换一次；前端轮询仍然
	// 幂等（finishLoginSession 会先看 Busy/已完成）。
	go p.autoFinishLogin(state)
	writeCallbackPage(w, http.StatusOK, "授权成功", "可以关闭这个页面，回到面板即可（账号会自动出现）")
}

// writeCallbackPage 给浏览器看的极简结果页：不引外部资源、不回显 code。
func writeCallbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>`+html.EscapeString(title)+
		`</title><body style="font:15px/1.6 system-ui,Segoe UI,Microsoft YaHei,sans-serif;padding:56px;color:#222">`+
		`<h2 style="margin:0 0 8px">`+html.EscapeString(title)+`</h2><p style="color:#555;margin:0">`+
		html.EscapeString(detail)+`</p></body>`)
}

// callbackPort 从环回回调基址里取端口（只用于前端提示文案）。
func callbackPort(base string) string {
	if u, err := url.Parse(base); err == nil && u.Port() != "" {
		return u.Port()
	}
	return ""
}

// isLoopbackRequest 面板页面是否从本机回环地址打开的（决定环回回调能否自动完成）。
func isLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
}

// loginCode 收人工粘贴的回调链接：面板跑在服务器/局域网时，官方页那条 127.0.0.1 回调
// 浏览器到不了（地址是写死的），用户把地址栏整条链接（或 office-raccoon:// 链接、裸 code）
// 粘进来即可，效果与环回回调完全一样。
//
// 两条路径都**由服务端自己换票**（不再把换票责任交给前端轮询）：
// state 命中未完成会话 → 把码交给该会话并当场换票；命中不了（换机/会话过期/只有 code）
// → 直接换票入池。换票失败（网络抖动）才退回 action=session 让前端轮询重试。
func (p *Panel) loginCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	code, state, ok := upstream.ParseCallbackCode(req.URL)
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"认不出这条链接：请粘 http://127.0.0.1:<端口>/auth/callback?code=…&state=…、"+
				"office-raccoon://auth/callback?code=… 或 ac_ 开头的授权码")
		return
	}
	if state != "" {
		p.loginMu.Lock()
		sess, found := p.logins[state]
		p.loginMu.Unlock()
		if found && sess.Login != nil && time.Since(sess.At) <= loginTTL {
			// 这次提交之前会话手上是不是已经有码了：有 = 同一链接被点了第二次。
			duplicate := sess.Login.Pending()
			if err := sess.Login.AcceptCallback(state, code); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if !duplicate {
				log.Printf("panel: 收到粘贴的授权回调 state=%.8s…", state)
			} else {
				log.Printf("panel: 重复提交同一条回调 state=%.8s…（幂等放行）", state)
			}
			// 服务端自己换票，不再把换票责任交给前端轮询：页面刷新过（loginID 清空）
			// 或轮询对象不是这条会话时，前端根本不会来 poll —— 以前这里只回「已接收」，
			// 码就一直躺着，账号永远不入池（用户看到的是「提交了没反应，再点提示正在处理」）。
			// 换票通常不到 1 秒，同步做掉最直接；失败再交回轮询重试。
			uid, nick, done, retryable, ferr := p.finishLoginSession(state)
			if done {
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "action": "imported", "uid": uid, "nickname": nick, "duplicate": duplicate,
				})
				return
			}
			if ferr != nil && !retryable {
				if errors.Is(ferr, errLoginSave) {
					writeErr(w, http.StatusInternalServerError, ferr.Error())
					return
				}
				writeErr(w, http.StatusGone,
					"授权码无效或已被使用（一次性码，同一条链接提交两次就会这样）：请回面板重新发起登录")
				return
			}
			if ferr != nil {
				log.Printf("panel: 会话 %.8s 粘贴回调换票暂时失败（交回轮询重试）：%v", state, ferr)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "action": "session", "id": state, "duplicate": duplicate,
			})
			return
		}
	}
	// 会话已经完成过（同一条链接被再次提交，且会话已被换票流程删掉）：直接回报上次的结果。
	// 授权码一次性，重复提交本就该是幂等成功，不是「链接失效」。
	if uid, nick, done := p.completedLogin(state); done {
		log.Printf("panel: 重复提交已完成的回调 state=%.8s… → uid=%s（幂等）", state, uid)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "action": "imported", "uid": uid, "nickname": nick, "duplicate": true,
		})
		return
	}
	// 没有对应会话：自己换票（PrepareAccount 与「导入凭证」同一条管线）。
	creds, err := p.cfg.Upstream.PrepareAccount(r.Context(), map[string]any{"code": code}, newMachineID())
	if err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "imported", "uid": uid, "nickname": nick})
}

// completedLogin 查某个 state 的会话是否已经成功换票入池，返回当时的 uid/昵称。
// 记到「会话完成」为止，之后同一条回调再提交就能幂等回报成功，而不是报「链接失效」。
func (p *Panel) completedLogin(state string) (uid, nickname string, ok bool) {
	if state == "" {
		return "", "", false
	}
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	done, found := p.loginDone[state]
	if !found {
		return "", "", false
	}
	if time.Since(done.At) > loginTTL {
		delete(p.loginDone, state)
		return "", "", false
	}
	return done.UID, done.Nickname, true
}

// markLoginDone 记一次会话成功（finishLoginSession 换票入池后调用）。
func (p *Panel) markLoginDone(state, uid, nickname string) {
	if state == "" {
		return
	}
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	if p.loginDone == nil {
		p.loginDone = map[string]loginDone{}
	}
	// 顺手清掉过期条目：这张表只为「重复提交」服务，不该无限长。
	for k, v := range p.loginDone {
		if time.Since(v.At) > loginTTL {
			delete(p.loginDone, k)
		}
	}
	p.loginDone[state] = loginDone{UID: uid, Nickname: nickname, At: time.Now()}
}

// loginQrStart 开一次微信扫码登录：返回会话 id、二维码内容、二维码 PNG 地址。
//
// 比环回回调强的地方：二维码里编的是**公开网页**（{origin}/login/mp?code=…），
// 微信扫完服务端把结果挂到我们自造的 code 上，面板轮询取票。所以面板放在服务器/
// 局域网、甚至用户用手机扫码都能完成，不再受「官方页只认 127.0.0.1」的限制。
func (p *Panel) loginQrStart(w http.ResponseWriter, r *http.Request) {
	machine := newMachineID()
	qr, err := upstream.NewQrLogin(p.cfg.Upstream, machine)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	for id, sess := range p.logins {
		if time.Since(sess.At) > loginTTL {
			delete(p.logins, id)
		}
	}
	p.logins[qr.State()] = loginSession{Machine: machine, Realm: upstream.RegionCN, Qr: qr, At: time.Now()}
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      qr.State(),
		"code":    qr.QRCode(),
		"content": qr.QRContent(),
		"png":     "/panel/api/login/qr.png?code=" + url.QueryEscape(qr.QRCode()),
	})
}

// loginQrImage 把扫码链接渲染成 PNG（二维码由面板自己画，不依赖任何外部服务）。
//
// 为什么服务端画而不是前端画：前端要么引 CDN 库（面板要能离线/内网用），要么
// 塞一份 QR 编码器进 app.js（几百行纠错码逻辑，维护成本高于收益）。
func (p *Panel) loginQrImage(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" || len(code) > 64 {
		writeErr(w, http.StatusBadRequest, "缺少 code")
		return
	}
	content := p.cfg.Upstream.QrContent(code)
	png, err := qrcode.Encode(content, qrcode.Medium, 320)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "二维码生成失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store") // code 一次性，缓存住会展示过期码
	_, _ = w.Write(png)
}

// loginStart 生成设备授权链接：{id, url}。用户在自己浏览器里打开 url 完成授权。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	// 小浣熊目前只有国内版（RegionGlobal 也 == "cn"）。国际版分支先留着：
	// 等官方出国际站/国际客户端，把这里改回「按 req.Realm 分流」即可。
	_ = req.Realm
	realm := upstream.RegionCN
	machine := newMachineID()
	// 环回回调：浏览器登录完由官方授权页把 code 打到面板自己的 /auth/callback，
	// 全程不需要官方客户端、也不需要人工粘贴 code。
	login, err := upstream.NewDeviceLogin(p.cfg.Upstream, realm, machine, p.cfg.CallbackBase)
	if err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	// 顺手清掉过期会话，避免面板长开时 map 无限增长。
	for id, sess := range p.logins {
		if time.Since(sess.At) > loginTTL {
			delete(p.logins, id)
		}
	}
	p.logins[login.State()] = loginSession{Machine: machine, Realm: realm, Login: login, At: time.Now()}
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    login.State(),
		"url":   login.AuthURL(),
		"realm": realm,
		// manual：当前页面不是从本机 127.0.0.1 打开的（局域网 IP / 域名 / 反代），
		// 官方页那条 127.0.0.1 回调浏览器到不了，前端应引导用户粘贴回调链接。
		"manual":        !isLoopbackRequest(r),
		"callback_port": callbackPort(p.cfg.CallbackBase),
	})
}

// loginPoll 查一次授权结果：未完成 → {done:false}（前端每 3s 再问）；
// 会话不存在/过期 → 410（前端停止轮询）；完成 → 落盘入池并回 {done:true}。
//
// 做成「一次一查」而不是服务端长等：设备流没有回调，verifier 又只能在服务端，
// 所以必须有人轮询——让浏览器按固定节拍问，用户什么时候授权完都在下一次问到时收号，
// 不需要再点一次「我已授权」。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	p.loginMu.Unlock()
	if !ok {
		// 会话已经换票入池（被 loginCode / 浏览器回调主动完成）：重复轮询按幂等回报成功。
		if uid, nick, done := p.completedLogin(id); done {
			writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": true, "uid": uid, "nickname": nick})
			return
		}
		writeErr(w, http.StatusGone, errLoginSession.Error())
		return
	}
	// 扫码会话有自己的状态机（等待扫码 / 已扫待确认），与「授权码换票」分开处理。
	if sess.Qr != nil {
		p.pollQrSession(w, r, id)
		return
	}
	uid, nick, done, _, err := p.finishLoginSession(id)
	if err != nil {
		// 换票被「业务层面」拒绝 = 这个码废了（一次性码重复使用、过期、伪造），
		// 再轮询也是白轮。会话已被 finishLoginSession 删掉，前端据此停轮询。
		// 只有网络层/5xx 才交给前端继续重试——那种情况授权可能已在浏览器侧完成。
		if errors.Is(err, errLoginSave) {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		var be *upstream.BusinessError
		if errors.Is(err, upstream.ErrCodeExpired) || errors.As(err, &be) || errors.Is(err, errLoginSession) {
			writeErr(w, http.StatusGone, "授权码无效或已被使用（一次性码，同一条链接提交两次就会这样）：请回面板重新发起登录")
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": true, "uid": uid, "nickname": nick})
}

// pollQrSession 处理扫码会话的一轮轮询（与「授权码换票」分开：状态机在 QrLogin 里）。
func (p *Panel) pollQrSession(w http.ResponseWriter, r *http.Request, id string) {
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	if ok && sess.Busy {
		// 上一次查询还没回来（上游慢）：本轮直接说「没完成」，避免并发打同一个 nonce。
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	if ok {
		sess.Busy = true
		p.logins[id] = sess
	}
	p.loginMu.Unlock()
	if !ok || time.Since(sess.At) > loginTTL {
		writeErr(w, http.StatusGone, errLoginSession.Error())
		return
	}
	// 失败路径复位闸门，供下一次轮询继续（成功路径已 delete 掉会话）。
	defer func() {
		p.loginMu.Lock()
		if s, still := p.logins[id]; still {
			s.Busy = false
			p.logins[id] = s
		}
		p.loginMu.Unlock()
	}()

	creds, done, err := sess.Qr.Poll(r.Context())
	if errors.Is(err, upstream.ErrQrCanceled) {
		// 用户取消/二维码过期：这是终态，停掉前端轮询比一直转圈诚实。
		p.loginMu.Lock()
		delete(p.logins, id)
		p.loginMu.Unlock()
		writeErr(w, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done {
		// 扫码没有回调事件：把上游状态（等待扫码 / 已扫码待确认）当进度显示。
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "qr_status": sess.Qr.LastStatus()})
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.loginMu.Lock()
	delete(p.logins, id)
	p.loginMu.Unlock()
	p.markLoginDone(id, uid, nick)
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": true, "uid": uid, "nickname": nick})
}

// finishLoginSession 主动把会话手上的授权码换成凭证并入池。
//
// 这是「换票不再依赖前端轮询」的核心：浏览器回调 / 粘贴回调收到码后就由服务端自己
// 换票，前端的 login/poll 只是幂等兜底。以前换票只发生在前端轮询里——页面一刷新
// （loginID 清空）或轮询对象不是这条会话，码就永远躺在服务端，账号永远不入池，
// 用户只看到「正在换票」却等不到结果，只能重走一遍授权。
//
// 返回语义：
//   - done=true：已入池，uid/nickname 有效；
//   - err != nil 且 retryable=false：终态（码已废/会话没了），会话已删；
//   - err != nil 且 retryable=true：暂时失败，码保留，可再试；
//   - 三者皆空：已有一次 poll 在飞（Busy），等那一轮。
func (p *Panel) finishLoginSession(id string) (uid, nick string, done bool, retryable bool, err error) {
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	if ok && time.Since(sess.At) > loginTTL {
		delete(p.logins, id)
		ok = false
	}
	if !ok {
		p.loginMu.Unlock()
		if uid, nick, ok := p.completedLogin(id); ok {
			return uid, nick, true, false, nil
		}
		return "", "", false, false, errLoginSession
	}
	if sess.Busy {
		p.loginMu.Unlock()
		return "", "", false, true, nil
	}
	if sess.Qr != nil || sess.Login == nil {
		// 扫码会话由 login/poll 的状态机驱动，不在这里换票。
		p.loginMu.Unlock()
		return "", "", false, true, nil
	}
	sess.Busy = true
	p.logins[id] = sess
	p.loginMu.Unlock()
	// 失败路径复位闸门，供下一次轮询/提交继续（成功路径已 delete 掉会话）。
	defer func() {
		p.loginMu.Lock()
		if s, still := p.logins[id]; still {
			s.Busy = false
			p.logins[id] = s
		}
		p.loginMu.Unlock()
	}()

	creds, exchanged, perr := sess.Login.Poll(context.Background())
	if perr != nil {
		var be *upstream.BusinessError
		if errors.Is(perr, upstream.ErrCodeExpired) || errors.As(perr, &be) {
			p.loginMu.Lock()
			delete(p.logins, id)
			p.loginMu.Unlock()
			log.Printf("panel: 会话 %.8s 换票被拒（%v），结束该会话", id, perr)
			return "", "", false, false, perr
		}
		// 网络层/5xx：码保留，交给下一次重试（授权可能已在浏览器侧完成）。
		return "", "", false, true, perr
	}
	if !exchanged {
		return "", "", false, true, nil
	}
	uid, nick, serr := p.saveAccount(creds)
	if serr != nil {
		// 票已经换掉了（Poll 内部已置 spent、码已清）：再重试换票没有意义。
		// 按终态回报「落盘失败」，让用户看到真实原因并重新授权，而不是一直转圈。
		p.loginMu.Lock()
		delete(p.logins, id)
		p.loginMu.Unlock()
		return "", "", false, false, fmt.Errorf("%w：%v", errLoginSave, serr)
	}
	p.loginMu.Lock()
	delete(p.logins, id)
	p.loginMu.Unlock()
	// 记下这次会话的结果：同一 state 的回调再被提交（用户点了第二次、或浏览器回调与
	// 手工粘贴同时到达）时，loginCode 直接幂等回报这个账号，而不是报「已经用过了」。
	p.markLoginDone(id, uid, nick)
	return uid, nick, true, false, nil
}

// autoFinishLogin 在后台把会话手上的授权码换掉（浏览器回调路径）。
// 失败只打日志：前端轮询还会再试，用户在弹窗里也能看到最终错误。
func (p *Panel) autoFinishLogin(state string) {
	uid, _, done, retryable, err := p.finishLoginSession(state)
	switch {
	case done:
		log.Printf("panel: 会话 %.8s 已在服务端换票入池 uid=%s", state, uid)
	case err != nil && !retryable:
		log.Printf("panel: 会话 %.8s 换票失败（终态）：%v", state, err)
	case err != nil:
		log.Printf("panel: 会话 %.8s 换票暂时失败（保留授权码等重试）：%v", state, err)
	}
}

// saveAccount 把一个已确认的凭证落盘并入池（同一 uid 直接覆盖旧文件）。
func (p *Panel) saveAccount(creds *auth.Auth) (uid, nickname string, err error) {
	if creds.UID == "" {
		return "", "", errors.New("上游没有返回用户标识，无法保存账号")
	}
	creds.FilePath = p.cfg.AuthDir + "/" + auth.FileNameFor(creds.UID)
	if err := creds.SaveAtomic(); err != nil {
		return "", "", err
	}
	p.cfg.Pool.Add(creds)
	log.Printf("panel: 账号已入池 uid=%s realm=%s", creds.UID, creds.Realm())
	return creds.UID, creds.Nickname, nil
}

// ---------------------------------------------------------------- 手机号 + 验证码

// smsNations 可选区号。上游拉不到时用内置兜底表（见 upstream.SmsNations），
// 所以这里不会因为一次网络抖动让登录页不可用。
func (p *Panel) smsNations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nations": p.cfg.Upstream.SmsNations()})
}

// smsVerify 校验面板来的手机号/区号/验证码，返回归一后的值。
// 只做形状校验（上游还有一层），目的是让明显的笔误在本地就报清楚。
func smsVerify(nation, phone string) (string, string, error) {
	nation = strings.TrimSpace(nation)
	phone = strings.TrimSpace(phone)
	if nation == "" {
		nation = "86"
	}
	if !digitOnly(nation) || len(nation) > 4 {
		return "", "", errors.New("区号只能是 1-4 位数字")
	}
	if !digitOnly(phone) || len(phone) < 5 || len(phone) > 20 {
		return "", "", errors.New("手机号只能是 5-20 位数字（不要带区号、空格或 +）")
	}
	return nation, phone, nil
}

func digitOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// loginSmsSend 发登录验证码。
//
// 走的是官方网页端同一条通道（auth 域的 /send_sms，手机号必须密文，见
// upstream.EncryptSmsField）。captcha_param 是官方页阿里云滑块的回执，可空——
// 上游真要它就会回业务码，面板把上游原话照实报出来，不编造「已发送」。
func (p *Panel) loginSmsSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nation  string `json:"nation_code"`
		Phone   string `json:"phone"`
		Captcha string `json:"captcha_param"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	nation, phone, err := smsVerify(req.Nation, req.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.cfg.Upstream.SendSmsCode(nation, phone, strings.TrimSpace(req.Captcha)); err != nil {
		log.Printf("panel: 发送验证码失败 +%s: %v", maskPhone(phone), err)
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	log.Printf("panel: 已请求发送验证码 +%s", maskPhone(phone))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "sent"})
}

// 短信登录的本地限流参数：一个号 10 分钟内最多 8 次尝试。
// 这条端点上游不设人机校验（实测），不加上限就是一个对着已知手机号猜验证码的入口。
const (
	smsLoginWindow = 10 * time.Minute
	smsLoginMax    = 8
)

// allowSmsAttempt 记一次尝试并判断是否超限。返回剩余可试次数与是否放行。
func (p *Panel) allowSmsAttempt(phone string) (int, bool) {
	p.smsMu.Lock()
	defer p.smsMu.Unlock()
	if p.smsAttempts == nil { // 防御：别的构造路径忘了初始化时也不该 panic
		p.smsAttempts = map[string][]time.Time{}
	}
	now := time.Now()
	kept := make([]time.Time, 0, smsLoginMax)
	for _, t := range p.smsAttempts[phone] {
		if now.Sub(t) < smsLoginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= smsLoginMax {
		p.smsAttempts[phone] = kept
		return 0, false
	}
	kept = append(kept, now)
	p.smsAttempts[phone] = kept
	return smsLoginMax - len(kept), true
}

// loginSms 手机号 + 验证码登录，成功即落盘入池。
//
// 登录接入与其它几条路径完全一致：拿凭证 → saveAccount（落 auths/ + 入池）。
// 验证码由用户自己从官方页面/客户端获取时也走这里——**这条端点不需要滑块**，
// 所以「官网取码 + 面板登录」在任何部署形态下都能用。
func (p *Panel) loginSms(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nation string `json:"nation_code"`
		Phone  string `json:"phone"`
		Code   string `json:"sms_code"`
		// Invite 可选：只在**注册那一刻**有效（上游没有补绑接口）。
		// 未注册的号在登录即注册时带上它，邀请码的主人才能拿到奖励。
		Invite string `json:"invite_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	nation, phone, err := smsVerify(req.Nation, req.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	code := strings.TrimSpace(req.Code)
	if !digitOnly(code) || len(code) > 8 {
		writeErr(w, http.StatusBadRequest, "验证码只能是数字")
		return
	}
	if left, ok := p.allowSmsAttempt(phone); !ok {
		log.Printf("panel: 验证码登录尝试过于频繁 +%s（%d 分钟内已 %d 次）",
			maskPhone(phone), int(smsLoginWindow.Minutes()), smsLoginMax)
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("同一个号码 %d 分钟内最多试 %d 次，请稍后再来（或重新获取验证码）",
				int(smsLoginWindow.Minutes()), smsLoginMax))
		return
	} else if left <= 2 {
		log.Printf("panel: 验证码登录尝试接近上限 +%s（本窗口还剩 %d 次）", maskPhone(phone), left)
	}
	invite := strings.TrimSpace(req.Invite)
	if invite != "" && (len(invite) < 8 || len(invite) > 64) {
		writeErr(w, http.StatusBadRequest, "邀请码长度不对（8-64 位）")
		return
	}
	creds, err := p.cfg.Upstream.LoginWithSms(nation, phone, code, invite)
	if err != nil {
		log.Printf("panel: 验证码登录失败 +%s: %v", maskPhone(phone), err)
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	// 设备指纹：与其它登录路径同形（UUID4），落盘前补上。
	if err := creds.CompleteIdentity(newMachineID()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 没有 refresh_token 的账号到期后无法自动续期（别处登录路径对这种情况是直接拒绝的，
	// 短信登录这条上游可能不给 refresh_token）：不拦，但必须当场说清楚。
	noRefresh := creds.RefreshTokenValue() == ""
	warn := ""
	if noRefresh {
		warn = "上游没返回 refresh_token：这个号能用到 access token 过期，之后需要重新登录"
		log.Printf("panel: 验证码登录成功 +%s uid=%s —— 上游没给 refresh_token，到期后需重新登录",
			maskPhone(phone), uid)
	}
	if invite != "" {
		log.Printf("panel: 验证码登录 +%s uid=%s 已带邀请码（注册归因）", maskPhone(phone), uid)
	}
	log.Printf("panel: 验证码登录成功 +%s uid=%s", maskPhone(phone), uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "action": "imported", "uid": uid, "nickname": nick,
		"invite_code":           invite,
		"refresh_token_missing": noRefresh,
		"warn":                  warn,
	})
}

// maskPhone 日志里只留后四位：手机号是个人标识，日志面板会被整屏看。
func maskPhone(phone string) string {
	if len(phone) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}

// loginRefresh 用一条 refreshToken 直登（最稳的免粘贴入口）。
func (p *Panel) loginRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
		Realm        string `json:"realm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	rt := strings.TrimSpace(req.RefreshToken)
	if rt == "" {
		writeErr(w, http.StatusBadRequest, "请填写 refreshToken")
		return
	}
	// 小浣熊的 refresh_token 是上游直给的 JWT，没有 Qoder 那种 token|uid|machine 打包串。
	creds := &auth.Auth{Data: auth.Data{RefreshToken: rt}}
	if err := p.completeByRefresh(r.Context(), creds); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if creds.UID == "" {
		writeErr(w, http.StatusBadGateway, "上游没有返回用户标识，无法保存账号")
		return
	}
	if err := creds.CompleteIdentity(newMachineID()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
}

// completeByRefresh 续期一次并补齐 uid/昵称（refresh 响应可能不带 user_id，再查 userinfo）。
func (p *Panel) completeByRefresh(ctx context.Context, creds *auth.Auth) error {
	if err := p.cfg.Upstream.RefreshToken(creds); err != nil {
		return err
	}
	uid, nick, _, err := p.cfg.Upstream.GetUserInfo(creds)
	if err != nil {
		if creds.UID == "" {
			return err
		}
		return nil
	}
	if uid != "" {
		if creds.UID != "" && creds.UID != uid {
			return errors.New("续期返回了不同账号，旧凭证未被覆盖")
		}
		creds.UID = uid
	}
	if nick != "" {
		creds.Nickname = nick
	}
	return nil
}
