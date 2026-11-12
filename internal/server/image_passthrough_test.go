package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// 图片不许被静默降级（2026-11-12 补，思路借鉴 loomy2api-panel 的 05ed9f0/4cbe114）：
//
// loomy 那边踩过的坑是**两条桥接把图片压成 "[image]" 文本** —— 上游只看到文字，
// 模型回「我看不到图片」，而客户端毫不知情。小浣熊这边看着没这个 bug（messages.go /
// responses.go 都构造标准 image_url 块），但**一条相关断言都没有**：哪天有人图省事
// 把内容字符串化，测试也不会红。这里把四条入口各钉一条。
//
// 另外记一笔上游的形态差异（loomy 的实测，2026-11）：个别 deepseek 系只认
// `<image_base64>` 标记、不认标准 image_url；DeepSeek-V4.1-Flash 之后反过来 ——
// 标记会被当纯文本内联（prompt_tokens 涨到 10 万级）而看不见图，标准 image_url 才对。
// 本仓一律透传标准 image_url，没有标记改写；这条测试同时也是"别乱加改写"的护栏。

const testDataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

// outboundImages 递归收集出站 chat 请求体里所有的图片 URL。
// 不假设图片挂在哪条消息上（tool_result 的图会被挪到另一条 user 消息里）。
func outboundImages(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		if u, ok := t["image_url"].(map[string]any); ok {
			if s, ok := u["url"].(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if s, ok := t["image_url"].(string); ok && s != "" { // 有的客户端直接给字符串
			out = append(out, s)
		}
		for _, sub := range t {
			out = append(out, outboundImages(sub)...)
		}
	case []any:
		for _, sub := range t {
			out = append(out, outboundImages(sub)...)
		}
	}
	return out
}

// 出站请求体里不许出现被压扁的图片占位（"[image]" 之类）。
func assertNoImagePlaceholder(t *testing.T, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	for _, bad := range []string{"[image]", "[图片]", "<image>"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("图片被降级成了 %q 文本（上游只会看到文字）：%s", bad, raw)
		}
	}
}

// OpenAI 入口：content 数组里的 data URI 图片必须原样出现在出站请求里。
func TestOpenAIImageURLPassthrough(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/chat/completions", `{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user",
		"content":[{"type":"text","text":"这张图什么颜色"},{"type":"image_url","image_url":{"url":"`+testDataURI+`"}}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	imgs := outboundImages(fake.lastBody)
	if len(imgs) != 1 || imgs[0] != testDataURI {
		t.Errorf("出站图片 url = %v，期望原样透传 %q", imgs, testDataURI)
	}
	assertNoImagePlaceholder(t, fake.lastBody)
}

// Anthropic 入口：Claude Code 的 image 块（source.base64）要变成标准 image_url。
func TestAnthropicImageBlockKept(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/messages", `{"model":"raccoon-chat-ml-5-5","max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"看图"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	imgs := outboundImages(fake.lastBody)
	want := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	if len(imgs) != 1 || imgs[0] != want {
		t.Errorf("出站图片 = %v，期望 %q（Anthropic source.base64 → image_url data URI）", imgs, want)
	}
	assertNoImagePlaceholder(t, fake.lastBody)
}

// Anthropic 入口：tool_result 里嵌套的图片（Claude Code 读截图后的结果）不能丢，
// 也不能留在 tool 消息里 —— 得挪到一条 user 消息带图。
func TestAnthropicToolResultImageKept(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/messages", `{"model":"raccoon-chat-ml-5-5","max_tokens":64,
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"text","text":"截图如下"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}]}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	imgs := outboundImages(fake.lastBody)
	if len(imgs) != 1 {
		t.Fatalf("工具结果里的图片丢了：出站图片 = %v（出站体 %v）", imgs, fake.lastBody)
	}
	assertNoImagePlaceholder(t, fake.lastBody)
}

// Responses 入口：Codex 的 input_image 要变成标准 image_url。
func TestResponsesInputImageKept(t *testing.T) {
	h, fake := newFakeHandler(t)
	rec := post(t, h, "/v1/responses", `{"model":"raccoon-chat-ml-5-5","input":[{"type":"message","role":"user",
		"content":[{"type":"input_text","text":"看图"},{"type":"input_image","image_url":"`+testDataURI+`"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	imgs := outboundImages(fake.lastBody)
	if len(imgs) != 1 || imgs[0] != testDataURI {
		t.Errorf("出站图片 = %v，期望 %q（Responses input_image → image_url）", imgs, testDataURI)
	}
	assertNoImagePlaceholder(t, fake.lastBody)
}
