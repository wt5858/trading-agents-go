package httpx

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func timeoutAfterOneSecond() <-chan time.Time { return time.After(time.Second) }

// 测试里用的全部是明显假的占位凭证，不是任何真实密钥。
const (
	fakeAPIKey = "sk-fake000111222333444555666777888999aaa"
	fakeToken  = "fakefakefake0123456789abcdef0123456789ab" // 40 位，仿 Finnhub 形态
)

func TestRedactURLDropsQueryAndUserinfo(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"query 整段丢弃", "https://api.example.com/v1/quote?token=" + fakeToken, "https://api.example.com/v1/quote"},
		{"userinfo 丢弃", "https://" + fakeAPIKey + "@gw.example.com/v1/chat", "https://gw.example.com/v1/chat"},
		{"路径里的密钥打码", "https://gw.example.com/v1/" + fakeAPIKey + "/chat", "https://gw.example.com/v1/" + Placeholder + "/chat"},
		{"正常路径原样保留", "https://api.example.com/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"模型名不误伤", "https://api.example.com/v1beta/models/gemini-2.0-flash:generateContent", "https://api.example.com/v1beta/models/gemini-2.0-flash:generateContent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := RedactURL(u); got != c.want {
				t.Fatalf("RedactURL = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestRedactErrorStripsQuery 这是本次修复的核心：Client.Do 失败返回的 *url.Error
// 带着完整 URL，调用方 Wrap 之后会原样进日志。
func TestRedactErrorStripsQuery(t *testing.T) {
	orig := &url.Error{
		Op:  "Get",
		URL: "https://api.example.com/v1/quote?symbol=AAPL&token=" + fakeToken,
		Err: errors.New("dial tcp: i/o timeout"),
	}

	got := RedactError(orig)
	msg := got.Error()

	if strings.Contains(msg, fakeToken) {
		t.Fatalf("脱敏后仍含 token: %s", msg)
	}
	if !strings.Contains(msg, "api.example.com") {
		t.Fatalf("host 不该被抹掉，排查要靠它: %s", msg)
	}
	if !strings.Contains(msg, "i/o timeout") {
		t.Fatalf("底层错因必须保留: %s", msg)
	}

	// 形状要保住：调用方可能 errors.As 出 *url.Error 来看。
	var ue *url.Error
	if !errors.As(got, &ue) {
		t.Fatal("脱敏后应仍是 *url.Error")
	}
}

func TestRedactErrorPassesThroughOthers(t *testing.T) {
	plain := errors.New("某个普通错误")
	if got := RedactError(plain); got != plain {
		t.Fatalf("非 *url.Error 应原样返回，实际 %v", got)
	}
	if RedactError(nil) != nil {
		t.Fatal("nil 应返回 nil")
	}
}

// TestSanitizeBodyRedactsEchoedCredential 上游把我们发过去的密钥回显在错误体里——
// 这是最现实的泄密路径，也是精确匹配那一步专门要堵的场景。
func TestSanitizeBodyRedactsEchoedCredential(t *testing.T) {
	body := []byte(`{"error":{"message":"Incorrect API key provided: ` + fakeAPIKey + `","type":"invalid_request_error"}}`)

	got := SanitizeBody(body, 512, fakeAPIKey)

	if strings.Contains(got, fakeAPIKey) {
		t.Fatalf("回显的密钥没被脱掉: %s", got)
	}
	if !strings.Contains(got, "invalid_request_error") {
		t.Fatalf("上游错因必须保留，否则脱敏就等于把线索一起删了: %s", got)
	}
}

// TestSanitizeBodyRedactsUnknownSecrets 兜底那一步：上游泄漏的是别人的密钥，
// 我们没法做精确匹配，只能靠形态。
func TestSanitizeBodyRedactsUnknownSecrets(t *testing.T) {
	cases := map[string]string{
		"sk- 前缀":    `{"msg":"bad key sk-otherfake1234567890abcdefghij"}`,
		"Bearer 令牌": `{"msg":"got header Authorization: Bearer abc.def.ghi"}`,
		"超长随机串":     `{"msg":"token ` + fakeToken + ` rejected"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := SanitizeBody([]byte(body), 512)
			for _, leak := range []string{"sk-otherfake1234567890abcdefghij", "abc.def.ghi", fakeToken} {
				if strings.Contains(got, leak) {
					t.Fatalf("未脱敏: %s", got)
				}
			}
		})
	}
}

// TestSanitizeBodyRedactsBeforeTruncating 顺序很重要：先截断再替换的话，
// 一个正好跨在截断点上的密钥会被切成两半，前半段留在字符串里。
func TestSanitizeBodyRedactsBeforeTruncating(t *testing.T) {
	body := []byte(strings.Repeat("x", 20) + fakeAPIKey + strings.Repeat("y", 100))

	got := SanitizeBody(body, 40, fakeAPIKey)

	if strings.Contains(got, fakeAPIKey[:16]) {
		t.Fatalf("密钥前缀泄漏在截断结果里: %s", got)
	}
}

// TestSanitizeBodyKeepsShortValues 太短的 known 值不做全文替换，
// 否则会把响应里正常的文字打得七零八落。
func TestSanitizeBodyKeepsShortValues(t *testing.T) {
	got := SanitizeBody([]byte("rate limit exceeded"), 512, "it")
	if got != "rate limit exceeded" {
		t.Fatalf("短值不该触发替换，实际 %q", got)
	}
}

// TestStripBearerTerminates "Bearer " 后面直接跟分隔符时不能死循环。
func TestStripBearerTerminates(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- SanitizeBody([]byte(`{"a":"Bearer ","b":"Bearer "}`), 512) }()
	select {
	case <-done:
	case <-timeoutAfterOneSecond():
		t.Fatal("stripBearer 没有终止")
	}
}
