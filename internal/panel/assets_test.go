package panel

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestThemePreheat：首帧主题靠 theme.js 在 <style> 之前同步改 data-theme。
// 少了它或挪到 <style> 之后，浅色用户刷新会先闪一帧深色。
func TestThemePreheat(t *testing.T) {
	html := string(indexHTMLRaw)
	i := strings.Index(html, `src="theme.js"`)
	if i < 0 {
		t.Fatal("index.html 没有引用 theme.js")
	}
	if s := strings.Index(html, "<style>"); s < 0 || i > s {
		t.Error("theme.js 必须在 <style> 之前同步加载")
	}
	if strings.Contains(html, "<script>") {
		t.Error("页面不能有内联脚本：CSP script-src 'self' 会拦掉，主题就永远停在深色")
	}
	if a := strings.Index(html, `src="app.js"`); a < 0 || a > strings.Index(html, "</body>") {
		t.Error("app.js 应放在 </body> 之前，别让它阻塞首帧")
	}
	// 键名与判定规则必须与 app.js 同源，否则预热一套、app.js 再翻一套 = 二次闪烁。
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`LS_THEME\s*=\s*'([^']+)'`).FindSubmatch(app)
	if m == nil {
		t.Fatal("app.js 里找不到 LS_THEME")
	}
	if !strings.Contains(string(themeJS), "'"+string(m[1])+"'") {
		t.Errorf("theme.js 读的 localStorage 键与 app.js 的 LS_THEME(%q) 不一致", m[1])
	}
}

// TestAssetURLCarriesFingerprint：脚本 URL 带内容指纹，缓存回放旧 app.js 时 URL 必然不同。
func TestAssetURLCarriesFingerprint(t *testing.T) {
	html := string(indexHTML)
	for _, want := range []string{"src=\"app.js?v=" + fingerprint(appJS) + "\"", "src=\"theme.js?v=" + fingerprint(themeJS) + "\""} {
		if !strings.Contains(html, want) {
			t.Errorf("注入后的页面缺少 %s", want)
		}
	}
}

// TestStatsStripSingleRow：KPI 条靠 auto-fit 排成一行，写死列数会让多出来的格
// （国际额度的美元格）单独掉到第二行，破坏与基准一致的「单行等分 + 细分割线」。
func TestStatsStripSingleRow(t *testing.T) {
	html := string(indexHTMLRaw)
	i := strings.Index(html, ".stats {")
	if i < 0 {
		t.Fatal("index.html 里找不到 .stats 规则")
	}
	rule := html[i : i+strings.Index(html[i:], "}")]
	if !strings.Contains(rule, "auto-fit") {
		t.Errorf(".stats 应使用 auto-fit 自适应列数，实际规则：%s", rule)
	}
}

// TestPanelScriptsParse 面板的 JS 是 go:embed 进去的，**没有构建期校验**：
// 一个漏写的 async（里面却有 await）就能让整个 app.js 解析失败，而 Go 侧一切正常、
// 只有浏览器里才炸（2026-11-12 真踩过：复制按钮忘了 async）。
// 这里用 node --check 过一遍语法；没装 node 就跳过（本地/CI 大多有）。
func TestPanelScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("没装 node，跳过 JS 语法检查")
	}
	for _, f := range []string{"app.js", "theme.js"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		cmd := exec.Command("node", "--check", "-")
		cmd.Stdin = strings.NewReader(string(raw))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s 语法错误（node --check）：%s", f, strings.TrimSpace(string(out)))
		}
	}
}
