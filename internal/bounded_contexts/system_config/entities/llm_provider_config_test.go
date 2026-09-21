package entities

import (
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/domain_events"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

func buildProvider(t *testing.T, name string, key string, models []string, enabled bool) *LLMProviderConfig {
	t.Helper()
	providerName, err := value_objects.NewProviderName(name)
	if err != nil {
		t.Fatalf("构造供应商名称失败: %v", err)
	}
	secret, err := value_objects.NewSecret(key)
	if err != nil {
		t.Fatalf("构造密钥失败: %v", err)
	}
	p, err := RegisterProvider(
		providerName,
		value_objects.KindOpenAICompat,
		value_objects.EndpointURL{},
		secret,
		models,
		0,
		enabled,
	)
	if err != nil {
		t.Fatalf("注册供应商失败: %v", err)
	}
	return p
}

// TestUsableInvariant 覆盖聚合真正的不变式：这家供应商现在能不能接活。
// 三个条件缺一不可，外加 ollama 这条业务例外。
func TestUsableInvariant(t *testing.T) {
	cases := []struct {
		name    string
		vendor  string
		key     string
		models  []string
		enabled bool
		want    bool
	}{
		{
			name:   "启用且密钥模型齐备则可用",
			vendor: "deepseek", key: "sk-1234567890abcdef",
			models: []string{"deepseek-chat"}, enabled: true, want: true,
		},
		{
			name:   "停用的供应商一律不可用",
			vendor: "deepseek", key: "sk-1234567890abcdef",
			models: []string{"deepseek-chat"}, enabled: false, want: false,
		},
		{
			name:   "云厂商缺密钥不可用：放进路由表只会在首次调用时 401",
			vendor: "deepseek", key: "",
			models: []string{"deepseek-chat"}, enabled: true, want: false,
		},
		{
			name:   "没有模型不可用：那是个永远匹配不上的空条目",
			vendor: "deepseek", key: "sk-1234567890abcdef",
			models: nil, enabled: true, want: false,
		},
		{
			name:   "ollama 本地部署没有密钥仍然可用（业务例外）",
			vendor: LocalProviderName, key: "",
			models: []string{"qwen2.5:7b"}, enabled: true, want: true,
		},
		{
			name:   "ollama 停用后同样不可用：例外只豁免密钥，不豁免启用态",
			vendor: LocalProviderName, key: "",
			models: []string{"qwen2.5:7b"}, enabled: false, want: false,
		},
		{
			name:   "ollama 没有模型同样不可用：例外只豁免密钥，不豁免模型",
			vendor: LocalProviderName, key: "",
			models: nil, enabled: true, want: false,
		},
		{
			name:   "ollama 配了密钥也不影响可用",
			vendor: LocalProviderName, key: "whatever-token-1234",
			models: []string{"qwen2.5:7b"}, enabled: true, want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := buildProvider(t, tc.vendor, tc.key, tc.models, tc.enabled)
			if got := p.Usable(); got != tc.want {
				t.Fatalf("Usable() = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

func TestIsLocal(t *testing.T) {
	if !buildProvider(t, LocalProviderName, "", []string{"qwen2.5:7b"}, true).IsLocal() {
		t.Fatal("ollama 应当被识别为本地部署")
	}
	if buildProvider(t, "openai", "sk-1234567890abcdef", []string{"gpt-4o"}, true).IsLocal() {
		t.Fatal("openai 不是本地部署")
	}
}

// TestEnableDisableRejectsNoOp 确认重复启停报 Conflict：
// 那意味着有人的状态变更被覆盖了，值得让调用方看见。
func TestEnableDisableRejectsNoOp(t *testing.T) {
	p := buildProvider(t, "deepseek", "sk-1234567890abcdef", []string{"deepseek-chat"}, true)

	err := p.Enable()
	if err == nil {
		t.Fatal("重复启用应当报错")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
		t.Fatalf("错误码 = %s, 期望 %s", code, custom_errors.CodeConflict)
	}
	if p.HasPendingEvents() {
		t.Fatal("被拒绝的状态迁移不应产生领域事件")
	}

	if err := p.Disable(); err != nil {
		t.Fatalf("首次停用不应报错: %v", err)
	}
	if p.Enabled {
		t.Fatal("停用后 Enabled 应为 false")
	}
	evts := p.GetAllPendingEvents()
	if len(evts) != 1 || evts[0].Name() != domain_events.OnProviderDisabledEventName {
		t.Fatalf("应当恰好产生一条停用事件, 实际 %v", evts)
	}

	err = p.Disable()
	if err == nil {
		t.Fatal("重复停用应当报错")
	}
	if code := custom_errors.CodeOf(err); code != custom_errors.CodeConflict {
		t.Fatalf("错误码 = %s, 期望 %s", code, custom_errors.CodeConflict)
	}
}

// TestRotateKeyEventCarriesNoSecret 是本上下文的安全底线：
// 事件会流向日志和消息队列，它绝不能带上密钥。
func TestRotateKeyEventCarriesNoSecret(t *testing.T) {
	const freshKey = "sk-proj-A1B2C3D4E5F6G7H8wxyz"

	p := buildProvider(t, "deepseek", "sk-old-1234567890", []string{"deepseek-chat"}, true)
	secret, err := value_objects.NewSecret(freshKey)
	if err != nil {
		t.Fatalf("构造密钥失败: %v", err)
	}
	if err := p.RotateKey(secret); err != nil {
		t.Fatalf("轮换密钥失败: %v", err)
	}

	evts := p.GetAllPendingEvents()
	if len(evts) != 1 {
		t.Fatalf("应当恰好产生一条轮换事件, 实际 %d 条", len(evts))
	}
	payload, err := evts[0].ToJson()
	if err != nil {
		t.Fatalf("事件序列化失败: %v", err)
	}
	if strings.Contains(payload, "A1B2C3D4E5F6G7H8") {
		t.Fatalf("密钥泄露进了领域事件: %s", payload)
	}
}

func TestRotateKeyRejectsEmptyForCloudProvider(t *testing.T) {
	p := buildProvider(t, "deepseek", "sk-old-1234567890", []string{"deepseek-chat"}, true)
	if err := p.RotateKey(value_objects.SecretValue{}); err == nil {
		t.Fatal("云厂商不允许把密钥轮换成空值，应当报错")
	}

	// 本地部署没有密钥这一说，清空是合法的。
	local := buildProvider(t, LocalProviderName, "", []string{"qwen2.5:7b"}, true)
	if err := local.RotateKey(value_objects.SecretValue{}); err != nil {
		t.Fatalf("本地部署清空密钥不应报错: %v", err)
	}
}

// TestRotateKeySameValueIsNoOp 确认「没改动的保存」不会污染审计流水。
func TestRotateKeySameValueIsNoOp(t *testing.T) {
	const key = "sk-1234567890abcdef"
	p := buildProvider(t, "deepseek", key, []string{"deepseek-chat"}, true)
	same, err := value_objects.NewSecret(key)
	if err != nil {
		t.Fatalf("构造密钥失败: %v", err)
	}
	if err := p.RotateKey(same); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if p.HasPendingEvents() {
		t.Fatal("密钥未变化时不应产生轮换事件")
	}
}

func TestSetModelsRejectsEmpty(t *testing.T) {
	p := buildProvider(t, "deepseek", "sk-1234567890abcdef", []string{"deepseek-chat"}, true)
	if err := p.SetModels(nil); err == nil {
		t.Fatal("清空模型列表应当报错，停用供应商才是正确做法")
	}
	if err := p.SetModels([]string{"  ", ""}); err == nil {
		t.Fatal("全是空白的模型列表等同于清空，应当报错")
	}
}

// TestSetModelsNormalizes 确认去重与排序生效：
// 同一组模型必须有唯一的存储形态，否则只是调换顺序也会产生一条假的变更记录。
func TestSetModelsNormalizes(t *testing.T) {
	p := buildProvider(t, "deepseek", "sk-1234567890abcdef", []string{"deepseek-chat"}, true)
	if err := p.SetModels([]string{" deepseek-reasoner ", "deepseek-chat", "deepseek-chat"}); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	want := []string{"deepseek-chat", "deepseek-reasoner"}
	if len(p.Models) != len(want) {
		t.Fatalf("模型列表 = %v, 期望 %v", p.Models, want)
	}
	for i := range want {
		if p.Models[i] != want[i] {
			t.Fatalf("模型列表 = %v, 期望 %v", p.Models, want)
		}
	}
}
