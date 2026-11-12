#!/usr/bin/env python3
"""小浣熊账号登录：半自动走完「发起授权 → 浏览器登录 → 提交回调 → 入池 → 领新手礼包」。

直接运行即可（参数都写在下面的「配置」里，改完存盘跑就行）：

    python3 scripts/xhx_login_flow.py

命令行参数只是**临时覆盖**配置用（例如换一个面板地址），不传就用配置里的值。

为什么是半自动：发码那一步上游要阿里云无痕验证的令牌，令牌只能由浏览器里的官方页面签发
（服务端拿不到；带自动化标记的浏览器会被判 token 无效）。所以脚本负责所有能自动的部分，
浏览器里那几步（过验证 → 收验证码 → 点确认授权）由人来点。

与它的关系：另有一份**个人化的全闭环超集**（接码平台取号 + 自起干净 Chrome 自动发码 +
从接码 API 收码 + CSV/JSON 台账），见开发者本地的 xhx_login/ 目录。本文件是通用版：
不带任何第三方凭据、只用标准库，随时可以随仓库发出去；两份改一处不会自动同步，
本文件是"仓库版"，那份是"本地自动化版"。

注意：如果你在「配置」里填了面板 api_key，这个文件就带着密钥了 —— 别直接提交/推到
公开仓库，要开 PR 时把 API_KEY 清空即可（清空后脚本会退回读环境变量 XHX_API_KEY）。
"""

from __future__ import annotations

# ═══════════════════════════ 配置：改这里就够了 ═══════════════════════════
BASE = "http://10.0.1.36:7867"   # 面板地址
API_KEY = ""                      # 面板 api_key（面板「配置」页可看）。留空则依次找：
                                  #   环境变量 XHX_API_KEY → 同目录的 .xhx_key 文件（已被 git 忽略）
REALM = "cn"                      # 小浣熊只有国内版，别改

PHONE = ""                        # 可选：登录时用的手机号，只用于在屏幕上提醒你（不会自动填）
AUTO_OPEN_BROWSER = True          # 自动用默认浏览器打开授权页
CHECKIN_AFTER_LOGIN = True        # 入池后顺手签到一次（会把两笔一次性新手礼包一起领掉）
SHOW_TASKS_AFTER_CHECKIN = True   # 签到后打印「桌面端/移动端礼包领没领到」

CALLBACK_URL = ""                 # 已有回调链接就粘到这里，留空则运行时提示你粘贴
INSECURE = False                  # 面板走 https 自签证书时改 True
POLL_TIMEOUT = 90.0               # 换票轮询最长等多久（秒）
POLL_INTERVAL = 2.0               # 轮询间隔（秒）
HTTP_TIMEOUT = 30.0               # 单次 HTTP 超时（秒）
# ═════════════════════════════════════════════════════════════════════════

import argparse
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser

class Api:
    """面板 HTTP 客户端：统一带 api_key、统一的错误人话。"""

    def __init__(self, base: str, key: str, insecure: bool = False, timeout: float = 30.0):
        self.base = base.rstrip("/")
        self.key = key
        self.timeout = timeout
        self.ctx = ssl._create_unverified_context() if insecure else None

    def call(self, method: str, path: str, body: dict | None = None) -> dict:
        url = f"{self.base}{path}"
        data = json.dumps(body, ensure_ascii=False).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Accept", "application/json")
        req.add_header("Authorization", f"Bearer {self.key}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        # 空密钥面板会要求同源：带上 Origin 与面板自己一致（写操作校验用）。
        req.add_header("Origin", self.base)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout, context=self.ctx) as resp:
                raw = resp.read().decode("utf-8", "replace")
                return json.loads(raw) if raw.strip() else {}
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            try:
                payload = json.loads(raw)
            except json.JSONDecodeError:
                payload = {"error": raw or f"HTTP {e.code}"}
            payload.setdefault("http_status", e.code)
            raise PanelError(e.code, payload.get("error") or raw, payload) from None
        except urllib.error.URLError as e:
            raise PanelError(0, f"连不上面板 {self.base}：{e.reason}", {}) from None


class PanelError(Exception):
    def __init__(self, status: int, message: str, payload: dict):
        super().__init__(message)
        self.status = status
        self.message = message
        self.payload = payload


def parse_callback(raw: str) -> tuple[str, str]:
    """从回调链接里取 (code, state)。认三种形态：完整 URL、office-raccoon://、裸 ac_ 码。"""
    raw = raw.strip().strip('"').strip("'")
    if not raw:
        return "", ""
    if raw.startswith("ac_") and "?" not in raw and "://" not in raw:
        return raw, ""
    parsed = urllib.parse.urlparse(raw)
    query = urllib.parse.parse_qs(parsed.query)
    code = (query.get("code") or [""])[0].strip()
    state = (query.get("state") or [""])[0].strip()
    if not code:
        # 用户可能只截到了 code=ac_... 这一段
        m = re.search(r"(ac_[A-Za-z0-9_\-]+)", raw)
        if m:
            return m.group(1), state
    return code, state


def read_key_file() -> str:
    """读同目录的 .xhx_key（一行）。这样无参直接跑不必把密钥写进脚本本身。"""
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".xhx_key")
    try:
        with open(path, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


def step(msg: str) -> None:
    print(f"\n\033[1m▸ {msg}\033[0m")


def ok(msg: str) -> None:
    print(f"  \033[32m✓\033[0m {msg}")


def warn(msg: str) -> None:
    print(f"  \033[33m!\033[0m {msg}")


def fail(msg: str) -> None:
    print(f"  \033[31m✗\033[0m {msg}")


def start_login(api: Api, realm: str) -> dict:
    """发起一次授权会话，返回 {id, url, ...}。"""
    out = api.call("POST", "/panel/api/login/start", {"realm": realm})
    if not out.get("url"):
        raise PanelError(0, f"面板没有返回授权链接：{out}", out)
    return out


def submit_callback(api: Api, raw: str) -> dict:
    """提交回调链接。返回面板响应（action=imported/session）。"""
    return api.call("POST", "/panel/api/login/code", {"url": raw})


def poll_until_done(api: Api, session_id: str, timeout: float = POLL_TIMEOUT) -> dict | None:
    """轮询会话直到账号入池。返回 {uid,nickname} 或 None（超时）。"""
    deadline = time.time() + timeout
    last_note = 0.0
    while time.time() < deadline:
        try:
            out = api.call("GET", f"/panel/api/login/poll?id={urllib.parse.quote(session_id)}")
        except PanelError as e:
            if e.status == 410:  # 会话已失效/终态失败
                raise
            if time.time() - last_note > 10:
                warn(f"轮询出错（继续重试）：{e.message}")
                last_note = time.time()
            time.sleep(POLL_INTERVAL)
            continue
        if out.get("done"):
            return out
        time.sleep(POLL_INTERVAL)
    return None


def show_accounts(api: Api, highlight: str = "") -> list[dict]:
    out = api.call("GET", "/panel/api/overview")
    accounts = out.get("accounts") or []
    print(f"  {'':2}{'uid':<12}{'昵称':<18}{'积分':>8}  状态")
    for a in accounts:
        mark = " ← 新" if highlight and str(a.get("uid")) == str(highlight) else ""
        state = "已禁用" if a.get("disabled") else ("冷却中" if a.get("cooling") else "可用")
        print(f"  {'':2}{str(a.get('uid')):<12}{str(a.get('nickname') or ''):<18}"
              f"{a.get('credits', 0):>8}  {state}{mark}")
    return accounts


def show_tasks(api: Api) -> None:
    """任务状态：桌面端/移动端礼包到底领没领到，看这里而不是猜余额。"""
    out = api.call("GET", "/panel/api/tasks")
    names = {
        "register_pack": "新人注册礼包", "desktop_login": "桌面端礼包",
        "mobile_login": "移动端礼包", "active_7d": "活跃 7 天", "first_payment": "首次付费",
    }
    for row in out.get("accounts") or []:
        if row.get("error"):
            warn(f"{row.get('uid')} 任务状态查询失败：{row['error']}")
            continue
        items = (row.get("new_user_tasks") or []) + (row.get("invitee_tasks") or [])
        done = sum(1 for t in items if t.get("status") == "completed")
        detail = "、".join(f"{names.get(t['key'], t['key'])}={'已领' if t['status'] == 'completed' else t['status']}"
                          for t in items)
        print(f"  {row.get('uid')}  新手任务 {done}/{len(items)}：{detail}")


def main() -> int:
    ap = argparse.ArgumentParser(
        description="小浣熊账号半自动登录（参数写在脚本顶部「配置」里；命令行只用于临时覆盖）")
    ap.add_argument("--base", default=os.environ.get("XHX_BASE") or BASE, help=f"面板地址（配置：{BASE}）")
    ap.add_argument("--key", default=os.environ.get("XHX_API_KEY") or API_KEY, help="面板 api_key")
    ap.add_argument("--realm", default=REALM)
    ap.add_argument("--phone", default=PHONE, help="登录用的手机号（仅用于提醒）")
    ap.add_argument("--url", default=CALLBACK_URL, help="已有回调链接时直接传，跳过交互式粘贴")
    ap.add_argument("--no-open", action="store_true", default=not AUTO_OPEN_BROWSER,
                    help="不自动打开浏览器（配置 AUTO_OPEN_BROWSER=False 等价）")
    ap.add_argument("--checkin", action="store_true", default=CHECKIN_AFTER_LOGIN,
                    help="入池后立刻签到一次（含两笔新手礼包）")
    ap.add_argument("--no-checkin", dest="checkin", action="store_false", help="这次不签到")
    ap.add_argument("--insecure", action="store_true", default=INSECURE)
    args = ap.parse_args()

    key = args.key or read_key_file()
    if not key:
        print("缺少面板密钥：把 api_key 填进脚本顶部配置的 API_KEY，"
              "或写进 scripts/.xhx_key（一行），或用 --key / 环境变量 XHX_API_KEY", file=sys.stderr)
        return 2
    api = Api(args.base, key, insecure=args.insecure, timeout=HTTP_TIMEOUT)

    try:
        step("1) 连通性自检")
        acc_before = [a.get("uid") for a in show_accounts(api)]
        uids_before = {str(u) for u in acc_before}

        step("2) 发起授权会话")
        sess = start_login(api, args.realm)
        session_id, auth_url = sess["id"], sess["url"]
        ok(f"会话 id {session_id[:12]}…（回调地址写死 127.0.0.1:{sess.get('callback_port')}，"
           f"浏览器打不开是正常的，链接本身就是要复制的回调载体）")
        print(f"  授权页：\033[4m{auth_url}\033[0m")

        raw = args.url.strip()
        if raw:
            ok("使用 --url 传入的回调链接")
        else:
            step("3) 在浏览器里完成登录（这几步必须人工：验证码要过阿里云无痕验证）")
            print("      1) 打开授权页 → 点「登录并继续」")
            print(f"      2) 输入手机号{('（' + args.phone + '）') if args.phone else ''} → 发送验证码"
                  f" → 勾选同意 → 填验证码 → 登录")
            print("      3) 点「确认授权」→ 浏览器跳到 http://127.0.0.1:… （打不开是正常的）")
            print("      4) 把地址栏那条完整链接复制回来")
            if not args.no_open:
                if webbrowser.open(auth_url):
                    ok("已用默认浏览器打开授权页")
                else:
                    warn("没能自动打开浏览器，请手动打开上面的链接")
            while True:
                try:
                    raw = input("\n  粘贴回调链接（或 ac_ 开头的授权码，直接回车重来，q 退出）：").strip()
                except (EOFError, KeyboardInterrupt):
                    print()
                    return 130
                if raw.lower() in ("q", "quit", "exit"):
                    return 130
                if raw:
                    break
                warn("没读到内容，再试一次（复制时别漏掉开头的 http://）")

        code, state = parse_callback(raw)
        if not code:
            fail(f"这条链接里找不到授权码：{raw[:120]}")
            return 1
        ok(f"解析到 code={code[:12]}…" + (f" state={state[:12]}…" if state else "（没有 state）"))

        step("4) 提交回调，等账号入池")
        try:
            out = submit_callback(api, raw)
        except PanelError as e:
            # 提交失败先别急着下结论：授权码是一次性的，账号很可能已经被另一条路径
            # （另一次提交、浏览器自动回调、上一次重试）加进池子了。以账号池为准。
            warn(f"面板回报：{e.message}")
            step("复查账号池（授权码一次性，先看账号是否已经进去了）")
            accounts = show_accounts(api)
            new = [a for a in accounts if str(a.get("uid")) not in uids_before]
            if new:
                ok(f"确有新账号入池：{new[0].get('uid')}（{new[0].get('nickname')}）"
                   f"—— 上面那条报错可以忽略，链接已经生效过")
                return 0
            fail("池子里没有新账号。这条链接的授权码已经作废，请重新发起一次登录")
            return 1

        if out.get("action") == "imported":
            ok(f"已入池：{out.get('nickname') or out.get('uid')}（uid={out.get('uid')}）")
            new_uid = str(out.get("uid") or "")
        else:
            ok("授权码已交给登录会话，正在换票…" + ("（这条链接之前提交过，属于重复提交）"
                                              if out.get("duplicate") else ""))
            try:
                done = poll_until_done(api, session_id, timeout=POLL_TIMEOUT)
            except PanelError as e:
                fail(f"会话结束：{e.message}")
                if e.status == 410:
                    warn("多半是授权码已被消费（链接提交过两次）。回面板重新发起一次登录即可。")
                return 1
            if not done:
                fail(f"等 {int(POLL_TIMEOUT)} 秒还没换到票。面板可能仍在处理，稍后刷新看看；"
                     f"持续不动就重新发起登录")
                return 1
            new_uid = str(done.get("uid") or "")
            ok(f"已入池：{done.get('nickname') or new_uid}（uid={new_uid}）")

        step("5) 核对结果")
        show_accounts(api, highlight=new_uid)
        if args.checkin:
            print()
            step("顺带签到（每日积分 + 两笔一次性新手礼包）")
            try:
                res = api.call("POST", "/panel/api/checkin", {})
                for r in res.get("checkin") or []:
                    line = f"{r.get('nickname') or r.get('uid')}  {r.get('status')}"
                    if r.get("credits"):
                        line += f"  +{r['credits']}"
                    if r.get("grants"):
                        got = [f"{g['key']}={g.get('granted')}" for g in r["grants"] if not g.get("error")]
                        if got:
                            line += "  礼包：" + "、".join(got)
                    if r.get("reason"):
                        line += f"  （{r['reason']}）"
                    print(f"  {line}")
            except PanelError as e:
                warn(f"签到失败（不影响入池）：{e.message}")
            if SHOW_TASKS_AFTER_CHECKIN:
                print()
                show_tasks(api)
        return 0
    except PanelError as e:
        fail(e.message)
        if e.status == 401:
            warn("密钥不对：面板「配置」页里能看到 api_key")
        return 1


if __name__ == "__main__":
    sys.exit(main())
