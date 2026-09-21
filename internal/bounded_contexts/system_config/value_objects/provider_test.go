package value_objects

import (
	"testing"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// TestNewProviderKind 的重点是「未知取值必须报错」这一半。
// 静默回落到 openai_compat 的话，一个写错的 kind 会一路潜伏到第一次真实模型调用，
// 那时的报错信息离配置现场隔着几十个文件。
func TestNewProviderKind(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    ProviderKind
		wantErr bool
	}{
		{name: "openai 兼容", raw: "openai_compat", want: KindOpenAICompat},
		{name: "anthropic", raw: "anthropic", want: KindAnthropic},
		{name: "google", raw: "google", want: KindGoogle},
		{name: "大小写与空白归一化", raw: "  Anthropic  ", want: KindAnthropic},

		{name: "拼写错误必须报错而不是回落", raw: "anthropi", wantErr: true},
		{name: "厂商名不是协议类型", raw: "openai", wantErr: true},
		{name: "deepseek 是厂商不是协议", raw: "deepseek", wantErr: true},
		{name: "空值同样报错", raw: "", wantErr: true},
		{name: "纯空白同样报错", raw: "   ", wantErr: true},
		{name: "带连字符的近似写法", raw: "openai-compat", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewProviderKind(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝 %q，却得到 %q", tc.raw, got)
				}
				if got != "" {
					t.Fatalf("报错时不应返回任何取值，却得到 %q", got)
				}
				if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
					t.Fatalf("错误码 = %s, 期望 %s", code, custom_errors.CodeInvalidArgument)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("得到 %q, 期望 %q", got, tc.want)
			}
			if !got.Valid() {
				t.Fatalf("%q 应当是合法取值", got)
			}
		})
	}
}

func TestProviderKindValid(t *testing.T) {
	if ProviderKind("openai").Valid() {
		t.Fatal("未知取值不应判为合法")
	}
	if !KindOpenAICompat.Valid() {
		t.Fatal("openai_compat 应当合法")
	}
}

func TestNewProviderName(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "常规名称", raw: "deepseek", want: "deepseek"},
		{name: "大写转小写", raw: "DeepSeek", want: "deepseek"},
		{name: "允许短横线与下划线", raw: "my-gateway_v2", want: "my-gateway_v2"},
		{name: "空值报错", raw: "  ", wantErr: true},
		{name: "斜杠会破坏模型路由，必须拒绝", raw: "acme/openai", wantErr: true},
		{name: "空格必须拒绝", raw: "open ai", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewProviderName(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝 %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("得到 %q, 期望 %q", got.String(), tc.want)
			}
		})
	}
}

func TestNewEndpointURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "空表示使用官方默认地址", raw: "", want: ""},
		{name: "https 正常", raw: "https://api.openai.com/v1", want: "https://api.openai.com/v1"},
		{name: "尾部斜杠被裁剪", raw: "https://api.openai.com/v1/", want: "https://api.openai.com/v1"},
		{name: "内网 http 允许", raw: "http://127.0.0.1:11434", want: "http://127.0.0.1:11434"},
		{name: "漏掉 scheme 必须报错", raw: "api.openai.com/v1", wantErr: true},
		{name: "非 http 协议报错", raw: "ftp://api.openai.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewEndpointURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝 %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("得到 %q, 期望 %q", got.String(), tc.want)
			}
		})
	}
}
