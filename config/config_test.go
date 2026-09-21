package config

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

// 容器部署完全建立在「环境变量能覆盖配置」这个前提上。
// 这个前提一旦不成立，表现不是启动失败，而是服务拿着一份**默认配置**跑起来——
// 连的是 127.0.0.1 的数据库、用的是空密码。所以它值得被测试钉住。

func TestEnvOverridesEveryConnectionSetting(t *testing.T) {
	cases := map[string]struct {
		env  string
		want string
		get  func(*Config) string
	}{
		"mysql 主机": {"TA_MYSQL_HOST", "mysql", func(c *Config) string { return c.MySQL.Host }},
		"mongo 地址": {"TA_MONGO_URI", "mongodb://mongo:27017", func(c *Config) string { return c.Mongo.URI }},
		"redis 地址": {"TA_REDIS_ADDR", "redis:6379", func(c *Config) string { return c.Redis.Addr }},
		"amqp 主机":  {"TA_AMQP_HOST", "rabbitmq", func(c *Config) string { return c.AMQP.Host }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tc.env, tc.want)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("加载配置失败: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestEnvOverridesCredentialsWithoutDefaults 是这组测试里最要紧的一条。
//
// 凭据没有默认值（一个默认密码比没有密码更危险），而 viper 的 AutomaticEnv
// 只为「已知」的键查环境变量——没有默认值、配置文件又不在时，
// 环境变量会被静默忽略，服务拿着空密码去连库。
//
// 这个故障不会报错，只会在连接时给出一句看不懂的认证失败，
// 而所有人的第一反应都是「环境变量明明配了啊」。
func TestEnvOverridesCredentialsWithoutDefaults(t *testing.T) {
	cases := map[string]struct {
		env  string
		want string
		get  func(*Config) string
	}{
		"mysql 密码":   {"TA_MYSQL_PASSWORD", "s3cret", func(c *Config) string { return c.MySQL.Password }},
		"redis 密码":   {"TA_REDIS_PASSWORD", "r3dis", func(c *Config) string { return c.Redis.Password }},
		"amqp 密码":    {"TA_AMQP_PASSWORD", "r4bbit", func(c *Config) string { return c.AMQP.Password }},
		"jwt 密钥":     {"TA_AUTH_JWT_SECRET", "jwt-secret", func(c *Config) string { return c.Auth.JWTSecret }},
		"tushare 令牌": {"TA_MARKET_TUSHARE_TOKEN", "tu-token", func(c *Config) string { return c.Market.TushareToken }},
		"finnhub 令牌": {"TA_MARKET_FINNHUB_TOKEN", "fh-token", func(c *Config) string { return c.Market.FinnhubToken }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tc.env, tc.want)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("加载配置失败: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Fatalf("%s 被静默忽略了：得到 %q，want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestLoadWithoutConfigFileUsesDefaults 守住「纯环境变量部署」这条受支持的用法：
// 配置文件缺失不是错误。容器镜像里可以不带配置文件。
func TestLoadWithoutConfigFileUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("没有配置文件时应当能加载: %v", err)
	}
	if cfg.MySQL.Database == "" {
		t.Fatal("默认值应当生效")
	}
	// 内置拓扑必须能兜住 amqp 段整段缺失的情况，否则纯环境变量部署起不来。
	if len(cfg.AMQP.QueueList()) == 0 {
		t.Fatal("amqp 段缺失时应当回落到内置拓扑")
	}
}

// ---------------------------------------------------------------------------
// 纯环境变量配置 MQ 与 LLM
// ---------------------------------------------------------------------------

// TestAmqpTuningFromEnv 守住「队列参数能用环境变量调」。
// 拓扑（名字、交换机、路由键）不在配置里，因此这里能调的只有跑多快、等多久。
func TestAmqpTuningFromEnv(t *testing.T) {
	t.Setenv("TA_AMQP_SCHEDULED_JOB_DUE_CONSUMERS", "9")
	t.Setenv("TA_AMQP_SCHEDULED_JOB_DUE_RETRY_DELAY", "45s")
	t.Setenv("TA_AMQP_DOMAIN_EVENT_PREFETCH", "16")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	byName := map[string]mq.Queue{}
	for _, q := range cfg.AMQP.QueueList() {
		byName[q.Name] = q
	}

	job := byName[constants.QueueScheduledJobDue]
	if job.Consumers != 9 {
		t.Fatalf("消费者数应被环境变量覆盖，实际 %d", job.Consumers)
	}
	if job.RetryDelay != 45*time.Second {
		t.Fatalf("重投间隔应被环境变量覆盖，实际 %s", job.RetryDelay)
	}
	// 没配的项必须保留内置拓扑的值，而不是被清成 0。
	if job.Prefetch == 0 {
		t.Fatal("没配的项不该被清零——consumers=0 的队列不会有任何消费者")
	}

	ev := byName[constants.QueueDomainEvent]
	if ev.Prefetch != 16 {
		t.Fatalf("预取数应被环境变量覆盖，实际 %d", ev.Prefetch)
	}
	// 拓扑永远来自代码，不受环境变量影响。
	if ev.RoutingKey != constants.RoutingKeyDomainEvent || ev.Exchange != constants.ExchangeTradingAgents {
		t.Fatal("拓扑必须来自代码，不该被配置改动")
	}
}

func TestLLMProvidersFromEnvJSON(t *testing.T) {
	t.Setenv("TA_LLM_PROVIDERS_JSON",
		`[{"name":"deepseek","kind":"openai_compat","baseUrl":"https://api.deepseek.com/v1","apiKey":"sk-x","models":["deepseek-chat"],"enabled":true}]`)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	enabled := cfg.LLM.EnabledProviders()
	if len(enabled) != 1 {
		t.Fatalf("应当解析出 1 个可用供应商，实际 %d", len(enabled))
	}
	if enabled[0].Name != "deepseek" || enabled[0].APIKey != "sk-x" {
		t.Fatalf("供应商字段没有正确解析: %+v", enabled[0])
	}
}

// TestBrokenProvidersJSONFailsFast 守住「配坏了要立刻报错」。
// 静默忽略的表现是「配了密钥但所有分析都说没有可用模型」，
// 而那时没人会想到去看一个解析错误。
func TestBrokenProvidersJSONFailsFast(t *testing.T) {
	t.Setenv("TA_LLM_PROVIDERS_JSON", "{这不是 JSON")
	if _, err := Load(""); err == nil {
		t.Fatal("写坏的 providers_json 必须让启动失败")
	}
}

// TestMarketProvidersParsedFromCommaSeparatedEnv 钉住降级链顺序能用一个环境变量配出来。
//
// 靠的是 viper 默认 decode hook 里的 StringToSliceHookFunc(",")——那是个隐式行为，
// 一旦哪天 Unmarshal 换了自定义 DecodeHook 而忘了带上它，这里会退化成
// 「整串被当成一个源名」，然后每个名字都匹配不上、全被跳过，最后静默退回 mock。
func TestMarketProvidersParsedFromCommaSeparatedEnv(t *testing.T) {
	t.Setenv("TA_MARKET_PROVIDERS", "eastmoney,mock")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	want := []string{"eastmoney", "mock"}
	if len(cfg.Market.Providers) != len(want) {
		t.Fatalf("解析出 %v，期望 %v", cfg.Market.Providers, want)
	}
	for i, w := range want {
		if cfg.Market.Providers[i] != w {
			t.Fatalf("第 %d 个源是 %q，期望 %q（顺序即优先级，不能乱）", i, cfg.Market.Providers[i], w)
		}
	}
}

// TestEastmoneyRateDefaultsAreConservative 东财没有配额错误码，超了直接封 IP，
// 所以默认值必须保守。有人把默认调高时，应该是一次需要解释的改动。
func TestEastmoneyRateDefaultsAreConservative(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Market.EastmoneyRPS <= 0 || cfg.Market.EastmoneyRPS > 5 {
		t.Fatalf("默认 rps = %v，应当落在 (0, 5]", cfg.Market.EastmoneyRPS)
	}
	if cfg.Market.EastmoneyBurst != 1 {
		t.Fatalf("默认 burst = %d，应当是 1——突发正是触发封禁的形状", cfg.Market.EastmoneyBurst)
	}
}
