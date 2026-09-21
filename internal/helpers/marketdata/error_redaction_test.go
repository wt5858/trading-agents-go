package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 明显伪造的占位 token，不是任何真实凭证。
const fakeProviderToken = "faketoken0123456789abcdefghijklmnopqrstu"

// TestUpstreamBodyDoesNotLeakEchoedToken 覆盖最现实的泄密路径：
// 上游把收到的凭证回显在错误响应体里，而这个响应体会被拼进错误信息、
// 再被调用方原样打进日志。
//
// 这里从 provider 的公开入口进，而不是直接测 truncate——要钉住的是
// 「调用点确实把 token 传下去了」，而不只是「工具函数会脱敏」。
// 少传那个参数，脱敏一样静悄悄地失效。
func TestUpstreamBodyDoesNotLeakEchoedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模仿真实网关：把收到的凭证抄回错误体里。
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":40001,"msg":"invalid token: ` + fakeProviderToken + `"}`))
	}))
	defer srv.Close()

	p := NewTushareProvider(fakeProviderToken, srv.Client())
	p.endpoint = srv.URL

	_, err := p.call(context.Background(), "stock_basic", nil, "")
	if err == nil {
		t.Fatal("期望返回错误")
	}

	msg := err.Error()
	if strings.Contains(msg, fakeProviderToken) {
		t.Fatalf("错误信息里泄漏了 token: %s", msg)
	}
	// 脱敏不能把线索一起删掉，否则这条错误就没用了。
	if !strings.Contains(msg, "invalid token") {
		t.Fatalf("上游错因应保留: %s", msg)
	}
}

// TestTransportErrorDoesNotLeakQueryToken Do 失败时返回的 *url.Error 带着完整 URL。
// finnhub 今天把 token 放在头里，但 URL 里一旦出现凭证，这条路径就是泄密口。
func TestTransportErrorDoesNotLeakQueryToken(t *testing.T) {
	// 指向一个必然连不上的地址，强制走 Do 的错误分支。
	p := NewFinnhubProvider(fakeProviderToken, &http.Client{})
	p.baseURL = "http://127.0.0.1:1/api/v1?token=" + fakeProviderToken

	var out any
	err := p.get(context.Background(), "/quote", nil, &out)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if strings.Contains(err.Error(), fakeProviderToken) {
		t.Fatalf("错误信息里泄漏了 query 中的 token: %s", err.Error())
	}
}

// TestSupportsUnchanged 顺带钉一下这次改动没碰到行为：脱敏只该影响错误文本。
func TestSupportsUnchanged(t *testing.T) {
	if !NewTushareProvider("x", nil).Supports(shared_vo.MarketCN) {
		t.Fatal("tushare 应支持 A 股")
	}
	if NewFinnhubProvider("x", nil).Supports(shared_vo.MarketCN) {
		t.Fatal("finnhub 不该支持 A 股")
	}
}
