package value_objects

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// 一把足够长的样本密钥。middle 是它中间那段——只要任何输出里出现了 middle，
// 就说明密钥泄露了，而且这个判定比「输出等于原值」更严格：
// 截断一半的密钥同样是泄露。
const (
	sampleSecret = "sk-proj-A1B2C3D4E5F6G7H8wxyz"
	sampleMiddle = "A1B2C3D4E5F6G7H8"
	sampleMask   = "sk-…wxyz"
)

func TestSecretValueMasked(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "空值如实返回空串，便于管理台显示未配置", raw: "", want: ""},
		{name: "极短值整体打码", raw: "abc", want: "****"},
		{name: "刚好低于阈值仍整体打码", raw: "12345678901", want: "****"},
		{name: "达到阈值露出首尾", raw: "sk-12345wxyz", want: "sk-…wxyz"},
		{name: "长密钥露出首尾", raw: sampleSecret, want: sampleMask},
		{name: "非 ASCII 前缀不越界", raw: "AIzaSyD-1234567890abcdef", want: "AIz…cdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := RehydrateSecret(tc.raw)
			if got := s.Masked(); got != tc.want {
				t.Fatalf("Masked() = %q, 期望 %q", got, tc.want)
			}
			if got := s.String(); got != tc.want {
				t.Fatalf("String() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// TestSecretValueNeverLeaks 穷举密钥可能被「顺手打印/序列化」的每一条路径。
// 这些路径是真实事故的来源：一次 zap.Any、一次 fmt.Errorf("%+v")、
// 一次把实体直接塞进响应，都会走到其中之一。
func TestSecretValueNeverLeaks(t *testing.T) {
	secret := RehydrateSecret(sampleSecret)

	// 一个「长得像响应体」的结构体：密钥作为导出字段嵌在里面，
	// 这是实际最容易泄露的形态。
	type providerResponse struct {
		Name   string      `json:"name"`
		APIKey SecretValue `json:"apiKey"`
		Nested struct {
			Key SecretValue `json:"key"`
		} `json:"nested"`
	}
	resp := providerResponse{Name: "openai", APIKey: secret}
	resp.Nested.Key = secret

	marshalled, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	marshalledSecret, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	cases := []struct {
		name string
		got  string
	}{
		{name: "String()", got: secret.String()},
		{name: "Masked()", got: secret.Masked()},
		{name: "fmt %v", got: fmt.Sprintf("%v", secret)},
		{name: "fmt %+v", got: fmt.Sprintf("%+v", secret)},
		{name: "fmt %s", got: fmt.Sprintf("%s", secret)},
		{name: "fmt %q", got: fmt.Sprintf("%q", secret)},
		{name: "fmt %#v", got: fmt.Sprintf("%#v", secret)},
		{name: "fmt 指针 %v", got: fmt.Sprintf("%v", &secret)},
		{name: "fmt.Errorf 包装", got: fmt.Errorf("调用失败: %v", secret).Error()},
		{name: "json.Marshal 自身", got: string(marshalledSecret)},
		{name: "json.Marshal 嵌入结构体", got: string(marshalled)},
		{name: "结构体 %v", got: fmt.Sprintf("%v", resp)},
		{name: "结构体 %+v", got: fmt.Sprintf("%+v", resp)},
		{name: "结构体指针 %+v", got: fmt.Sprintf("%+v", &resp)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.got, sampleMiddle) {
				t.Fatalf("密钥中段泄露: %s", tc.got)
			}
			if strings.Contains(tc.got, sampleSecret) {
				t.Fatalf("密钥明文泄露: %s", tc.got)
			}
			if !strings.Contains(tc.got, "sk-…wxyz") && !strings.Contains(tc.got, "****") {
				t.Fatalf("输出里没有出现掩码，可能走了未预期的路径: %s", tc.got)
			}
		})
	}
}

// TestSecretValueShortNeverLeaks 短密钥走的是另一条分支（整体打码），单独覆盖。
func TestSecretValueShortNeverLeaks(t *testing.T) {
	const short = "tiny-key"
	secret := RehydrateSecret(short)

	outputs := []string{
		secret.String(),
		fmt.Sprintf("%v", secret),
		fmt.Sprintf("%+v", secret),
		fmt.Sprintf("%#v", secret),
	}
	b, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	outputs = append(outputs, string(b))

	for i, got := range outputs {
		if strings.Contains(got, short) {
			t.Fatalf("第 %d 个输出泄露了短密钥: %s", i, got)
		}
	}
}

// TestSecretValueExpose 确认刻意留出的明文出口仍然可用——
// 掩码类型如果连密钥都取不出来就没法用了。
func TestSecretValueExpose(t *testing.T) {
	if got := RehydrateSecret(sampleSecret).Expose(); got != sampleSecret {
		t.Fatalf("Expose() = %q, 期望 %q", got, sampleSecret)
	}
}

// TestNewSecretRejectsMask 覆盖「GET 到掩码再原样 PUT 回来」这条最常见的误操作。
func TestNewSecretRejectsMask(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "掩码星号形态", raw: "****", wantErr: true},
		{name: "掩码首尾形态", raw: sampleMask, wantErr: true},
		{name: "正常密钥", raw: sampleSecret, wantErr: false},
		{name: "空值（本地部署无密钥）", raw: "", wantErr: false},
		{name: "两端空白被裁剪", raw: "  " + sampleSecret + "  ", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSecret(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝掩码输入，却通过了")
				}
				if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
					t.Fatalf("错误码 = %s, 期望 %s", code, custom_errors.CodeInvalidArgument)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if want := strings.TrimSpace(tc.raw); got.Expose() != want {
				t.Fatalf("Expose() = %q, 期望 %q", got.Expose(), want)
			}
		})
	}
}

func TestSecretValueIsZero(t *testing.T) {
	if !(SecretValue{}).IsZero() {
		t.Fatal("零值应当是 IsZero")
	}
	if RehydrateSecret(sampleSecret).IsZero() {
		t.Fatal("有值时不应是 IsZero")
	}
}
