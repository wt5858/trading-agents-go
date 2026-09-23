// Package config 负责加载与校验应用配置。
// 配置优先级：环境变量 > 配置文件 > 内置默认值，
// 这样容器部署时可以只靠环境变量覆盖，无需重新打包镜像。
package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/helpers/constants"
	"github.com/wt5858/trading-agents-go/pkg/mq"
)

type Config struct {
	App    App    `mapstructure:"app"`
	HTTP   HTTP   `mapstructure:"http"`
	MySQL  MySQL  `mapstructure:"mysql"`
	Mongo  Mongo  `mapstructure:"mongo"`
	Redis  Redis  `mapstructure:"redis"`
	AMQP   AMQP   `mapstructure:"amqp"`
	Auth   Auth   `mapstructure:"auth"`
	Queue  Queue  `mapstructure:"queue"`
	LLM    LLM    `mapstructure:"llm"`
	Market Market `mapstructure:"market"`
	Log    Log    `mapstructure:"log"`
}

type App struct {
	Name string `mapstructure:"name"`
	Env  string `mapstructure:"env"` // dev / test / prod
}

// IsProd 是四道防线共同的开关：拒绝 change-me 签名密钥、gin 切 ReleaseMode、
// 不挂载 Swagger、SQL 日志脱敏。它们全都只认 Env == "prod" 这一次精确比较。
//
// 所以 Env 必须是封闭取值，由 validate 在启动时把关：写成 "production" 而不是
// "prod" 不会有任何报错，只会让上面四样一起悄悄失效——一次拼写换来一个带调试
// 输出、公开 Swagger、日志里印着口令哈希、还接受硬编码密钥的生产实例。
func (a App) IsProd() bool { return a.Env == EnvProd }

const (
	EnvDev  = "dev"
	EnvTest = "test"
	EnvProd = "prod"
)

type HTTP struct {
	Host           string        `mapstructure:"host"`
	Port           int           `mapstructure:"port"`
	ReadTimeout    time.Duration `mapstructure:"read_timeout"`
	WriteTimeout   time.Duration `mapstructure:"write_timeout"`
	AllowedOrigins []string      `mapstructure:"allowed_origins"`
}

func (h HTTP) Addr() string { return fmt.Sprintf("%s:%d", h.Host, h.Port) }

type MySQL struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	User         string        `mapstructure:"user"`
	Password     string        `mapstructure:"password"`
	Database     string        `mapstructure:"database"`
	MaxOpenConns int           `mapstructure:"max_open_conns"`
	MaxIdleConns int           `mapstructure:"max_idle_conns"`
	ConnMaxLife  time.Duration `mapstructure:"conn_max_lifetime"`
}

// DSN 固定 parseTime=true，否则 GORM 会把 DATETIME 读成 []byte 而非 time.Time。
func (m MySQL) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		m.User, m.Password, m.Host, m.Port, m.Database)
}

type Mongo struct {
	URI      string        `mapstructure:"uri"`
	Database string        `mapstructure:"database"`
	Timeout  time.Duration `mapstructure:"timeout"`
	Enabled  bool          `mapstructure:"enabled"`
}

type Redis struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

// AMQP 是消息队列的接入配置。
//
// ===========================================================================
// 这里只有连接信息与可调参数，**没有拓扑**
// ===========================================================================
//
// 交换机、队列、路由键定义在 internal/helpers/constants/mq.go 里，是代码的一部分。
// 曾经它们也在配置里，但那是个错觉式的灵活：队列名同时还是处理器订阅时用的常量，
// 改了配置不改常量，得到的是一个「消费者活着但永远收不到消息」的服务——
// 不报错，只是那条链路静默地不工作。既然改一个必须同时改另一个，
// 它就不该有两份来源。
//
// 留在配置里的是那些**改了不会让系统自相矛盾**的东西：连到哪个 broker、
// 起几个消费者、失败等多久重投。它们全部可以用环境变量表达。
type AMQP struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	VHost    string `mapstructure:"vhost"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`

	// MaxRetries / RetryDelay 是队列没有单独配置时的默认重试策略。
	MaxRetries int           `mapstructure:"max_retries"`
	RetryDelay time.Duration `mapstructure:"retry_delay"`

	ReconnectDelay time.Duration `mapstructure:"reconnect_delay"`
	PublishTimeout time.Duration `mapstructure:"publish_timeout"`

	// 两个队列各自的可调参数。零值表示用内置拓扑里的值。
	DomainEvent     AMQPQueueTuning `mapstructure:"domain_event"`
	ScheduledJobDue AMQPQueueTuning `mapstructure:"scheduled_job_due"`
}

// AMQPQueueTuning 是单个队列可以在部署期调整的参数。
//
// 它不包含名字、交换机、路由键——那些是拓扑，改了会让系统自相矛盾。
// 这里的每一项都只影响「跑多快、失败后等多久」，改错了最多是性能问题。
type AMQPQueueTuning struct {
	// Consumers 是本进程为这个队列起几个并发消费者。
	Consumers int `mapstructure:"consumers"`
	// Prefetch 是单个消费者未确认消息的上限。
	Prefetch int `mapstructure:"prefetch"`
	// MaxRetries 是处理失败后最多重投几次，超过则进死信队列。
	MaxRetries int `mapstructure:"max_retries"`
	// RetryDelay 是每次重投之前的等待时长。
	//
	// 改这个值需要先删掉旧的重试队列：它落在队列的 x-message-ttl 参数上，
	// 而 AMQP 不允许用不同参数重新声明一个已存在的队列，服务会直接启动失败。
	// 这是好事——它逼着改动被看见，而不是静默地不生效。
	RetryDelay time.Duration `mapstructure:"retry_delay"`
}

// applyTo 把非零的调整项盖到内置拓扑的队列声明上。
//
// 零值一律跳过：配置里没写就是没写，不该被解读成「设成 0」——
// 一个 consumers=0 的队列不会有任何消费者，而那正是「没配」最不该得到的结果。
func (t AMQPQueueTuning) applyTo(q mq.Queue) mq.Queue {
	if t.Consumers > 0 {
		q.Consumers = t.Consumers
	}
	if t.Prefetch > 0 {
		q.Prefetch = t.Prefetch
	}
	if t.MaxRetries > 0 {
		q.MaxRetries = t.MaxRetries
	}
	if t.RetryDelay > 0 {
		q.RetryDelay = t.RetryDelay
	}
	return q
}

// ConnectionInfo 投影出 pkg/mq 需要的连接信息。
func (a AMQP) ConnectionInfo() mq.ConnectionInfo {
	return mq.ConnectionInfo{
		Host:     a.Host,
		Port:     a.Port,
		VHost:    a.VHost,
		User:     a.User,
		Password: a.Password,
	}
}

func (a AMQP) Options(logger *zap.Logger) mq.Options {
	return mq.Options{
		Logger:         logger,
		MaxRetries:     a.MaxRetries,
		RetryDelay:     a.RetryDelay,
		ReconnectDelay: a.ReconnectDelay,
		PublishTimeout: a.PublishTimeout,
	}
}

// ExchangeList 返回内置拓扑里的交换机声明。
func (a AMQP) ExchangeList() []mq.Exchange { return constants.DefaultExchanges() }

// QueueList 返回内置拓扑里的队列声明，并盖上部署期的调整项。
func (a AMQP) QueueList() []mq.Queue {
	tuning := map[string]AMQPQueueTuning{
		constants.QueueDomainEvent:     a.DomainEvent,
		constants.QueueScheduledJobDue: a.ScheduledJobDue,
	}
	out := constants.DefaultQueues()
	for i, q := range out {
		if t, ok := tuning[q.Name]; ok {
			out[i] = t.applyTo(q)
		}
	}
	return out
}

type Auth struct {
	JWTSecret       string        `mapstructure:"jwt_secret"`
	AccessTokenTTL  time.Duration `mapstructure:"access_token_ttl"`
	RefreshTokenTTL time.Duration `mapstructure:"refresh_token_ttl"`
	BcryptCost      int           `mapstructure:"bcrypt_cost"`
	// BootstrapAdmin 在用户表为空时自动创建的管理员账号。
	//
	// 口令没有默认值，空口令表示「不要自动建号」。默认口令在这里是最坏的选择：
	// 它让「忘了配」和「配好了」表现得一模一样，而代价是一个用户名和口令
	// 全世界都知道的管理员账号——只要有人部署时没读这段注释就会踩到。
	BootstrapAdmin         string `mapstructure:"bootstrap_admin"`
	BootstrapAdminPassword string `mapstructure:"bootstrap_admin_password"`
}

// Queue 是分析任务的排队与限流参数。
//
// # 这里为什么没有并发度与轮询间隔了
//
// 任务的取用已经交给消息队列：同时在跑几个由队列声明里的 Consumers 决定
// （见 constants.DefaultQueues），不再由应用侧起几个循环决定；也没有轮询，
// 消息是推过来的。原先的 worker_concurrency / poll_interval 因此删除，
// 留着只会让人以为改了它们有用。
//
// # 两个 TTL 为什么拆开
//
// 它们原先共用一个 visibility_timeout（15 分钟）——那是 Redis 队列的可见性
// 超时，顺手拿来当了这两个 Redis 键的存活时长。队列换掉之后这个名字已经没有
// 指代物了，而更糟的是那个值本身就不对：两者都必须**活得比一次分析长**，
// 15 分钟比 AnalysisMaxRuntime（30 分钟）还短，于是一次跑满的分析会在中途
// 丢掉进度快照、并让并发凭据提前过期把名额放出去。
type Queue struct {
	// MaxAttempts 是一个任务最多尝试几次，由领域层判定。
	MaxAttempts int `mapstructure:"max_attempts"`
	// SlotTTL 是并发凭据的存活时长。
	//
	// 它是名额泄漏的自愈机制：进程被 kill -9 时 Release 不会执行，没有 TTL
	// 那份名额就永久占着额度。因此它要覆盖「一个任务从提交到终态的最长时间」——
	// 包含排队、以及最多 MaxAttempts 次各自跑满 AnalysisMaxRuntime 的重试。
	// 宁可偏长：偏长只是让一次泄漏多占一会儿，偏短是稳定地放大并发。
	SlotTTL time.Duration `mapstructure:"slot_ttl"`
	// ProgressTTL 是进度快照在 Redis 里的存活时长。
	//
	// 终态在 MySQL 里，快照只服务于「正在跑的时候实时看进度」，因此不必长久保存，
	// 但必须长于一次分析——短于它的话，长任务跑到一半前端的进度条就空了。
	ProgressTTL time.Duration `mapstructure:"progress_ttl"`
	// UserConcurrency / GlobalConcurrency 是并发闸门的两级上限，0 表示不限。
	UserConcurrency   int `mapstructure:"user_concurrency"`
	GlobalConcurrency int `mapstructure:"global_concurrency"`
}

// LLMProvider 是一个大模型供应商的接入配置。
type LLMProvider struct {
	Name    string   `mapstructure:"name" json:"name"` // openai / deepseek / dashscope / anthropic / google / ollama ...
	Kind    string   `mapstructure:"kind" json:"kind"` // openai_compat / anthropic / google
	BaseURL string   `mapstructure:"base_url" json:"baseUrl"`
	APIKey  string   `mapstructure:"api_key" json:"apiKey"`
	Models  []string `mapstructure:"models" json:"models"`
	Enabled bool     `mapstructure:"enabled" json:"enabled"`
}

type LLM struct {
	DefaultModel string        `mapstructure:"default_model"`
	Timeout      time.Duration `mapstructure:"timeout"`
	Providers    []LLMProvider `mapstructure:"providers"`

	// 这里曾经有一个 max_parallel，声明了、给了默认值、然后没有任何代码读它。
	// 真正决定大模型并发的是三个扇出上限（AnalystFanOutLimit、RiskFanOutLimit、
	// ToolFanOutLimit）加上全局任务并发。删掉它的理由和 Queue 那段注释里
	// 删 worker_concurrency 是同一条：留着只会让人以为改了它有用。

	// ProvidersJSON 让供应商列表也能用一个环境变量表达：
	//
	//	TA_LLM_PROVIDERS_JSON='[{"name":"deepseek","kind":"openai_compat",...}]'
	//
	// 为什么要单开一个字段：供应商是一个**数组**，而环境变量是扁平的键值对，
	// 没有任何自然的方式把 N 个结构体塞进去。用 JSON 串是唯一不需要发明
	// 一套索引语法（PROVIDER_0_NAME、PROVIDER_1_NAME……）的办法，
	// 而那种语法写起来痛苦、读起来更痛苦，还没法整体复制粘贴。
	//
	// 它解析出来的内容会**覆盖** Providers，因为环境变量的优先级本来就更高。
	//
	// 也可以完全不配：供应商的运行期来源是配置中心（数据库），
	// 这里只是冷启动兜底，让一个全新部署在配置中心还空着时也能跑。
	ProvidersJSON string `mapstructure:"providers_json"`
}

// resolveProviders 把 JSON 形式的供应商列表解析进 Providers。
//
// 解析失败直接让启动失败，不静默回退：一个写坏了的 TA_LLM_PROVIDERS_JSON
// 如果被忽略，表现是「配了密钥但所有分析都说没有可用模型」——
// 而那时没人会想到去看一个解析错误。
func (l *LLM) resolveProviders() error {
	if strings.TrimSpace(l.ProvidersJSON) == "" {
		return nil
	}
	var providers []LLMProvider
	if err := json.Unmarshal([]byte(l.ProvidersJSON), &providers); err != nil {
		return fmt.Errorf("解析 llm.providers_json 失败: %w", err)
	}
	l.Providers = providers
	return nil
}

// EnabledProviders 过滤出配置了密钥的供应商。
// Ollama 本地部署不需要密钥，因此单独放行。
func (l LLM) EnabledProviders() []LLMProvider {
	out := make([]LLMProvider, 0, len(l.Providers))
	for _, p := range l.Providers {
		if !p.Enabled {
			continue
		}
		if p.APIKey == "" && p.Name != "ollama" {
			continue
		}
		out = append(out, p)
	}
	return out
}

type Market struct {
	// Providers 是降级链的顺序，优先级从高到低，逗号分隔（TA_MARKET_PROVIDERS=eastmoney,mock）。
	//
	// 顺序写在配置里而不是代码里，是因为「哪个源优先」取决于部署环境：
	// 有 Tushare 积分的部署想让它打头（它是唯一有行业/地区/上市日期的源），
	// 没积分的部署只能靠东财。名字不认识就跳过并告警，不会让服务起不来。
	//
	// 注意这个顺序对所有取数操作共用。不必按操作分别配置：Supports() 会跳过不支持
	// 该市场的源，没有某个批量端点的源会返回 ErrBatchUnsupported 让链子继续往下走，
	// 两者合起来已经能做到按操作自动分流。
	Providers []string `mapstructure:"providers"`

	TushareToken string        `mapstructure:"tushare_token"`
	FinnhubToken string        `mapstructure:"finnhub_token"`
	Timeout      time.Duration `mapstructure:"timeout"`
	// EnableMock 为真时把 mock 数据源挂到兜底位置，让系统在无任何密钥时也能跑通。
	//
	// 它与 Providers 是「与」的关系：mock 要生效，既要在 Providers 里列出来，
	// 也要这个开关为真（或者一个真实源都没配上）。
	EnableMock bool `mapstructure:"enable_mock"`

	// EastmoneyRPS / EastmoneyBurst 是对东方财富的出站速率上限。
	//
	// 这不是性能参数，是生存参数：东财没有配额错误码，超了直接掐 TCP 连接乃至封 IP，
	// 而且没有任何响应能告诉你被封了。实测境外出口 IP 以约 0.67 次/秒持续请求，
	// 八次里六次拿到空响应。调大它之前先想清楚被封之后靠什么恢复。
	//
	// 按 2 次/秒估算：A 股全市场快照 60 页约 30 秒，美股 139 页约 70 秒；
	// 日 K 回补是每标的一次调用，5900 只要 50 分钟——那是后台作业，可以接受。
	// burst 取 1 而不是更大：突发正是触发封禁的那个形状。
	EastmoneyRPS   float64 `mapstructure:"eastmoney_rps"`
	EastmoneyBurst int     `mapstructure:"eastmoney_burst"`

	// SyncFanOutLimit 是全量同步时逐标的取数的并发上限。
	//
	// 它由数据源配额决定而不是核数：Tushare 免费档约 500 次/分钟，Finnhub 免费档 60 次/分钟。
	// 东财则由上面的 EastmoneyRPS 在数据源内部限速，本项对它只起「同时在飞的请求数」
	// 这一个作用——真正的节流在 marketdata/throttle.go。
	SyncFanOutLimit int `mapstructure:"sync_fan_out_limit"`
	// SyncChunkSize 是同步的分片大小：每处理这么多标的落一次库并推进断点。
	// 太大则崩溃后回退太多，太小则落库过于频繁。
	SyncChunkSize int `mapstructure:"sync_chunk_size"`
}

type Log struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"` // json / console
	File   string `mapstructure:"file"`   // 留空只输出到 stdout

	// SQL 控制是否逐条打印 SQL、Redis 命令与 Mongo 命令。
	//
	// 它是独立开关而不是复用 level，因为这两件事的粒度差着几个量级：
	// 想看 SQL 就把全局降到 debug 的话，会同时打开每个库的调试输出，
	// 日志量大到没法看——于是下一步就是把它整个关掉，然后再也不打开。
	SQL bool `mapstructure:"sql"`

	// SQLArgs 控制 SQL 里的参数值是留明文还是脱敏。
	//
	// GORM 打出来的 SQL 是参数内联之后的，也就是说邮箱、口令哈希、持仓金额
	// 会原样出现在日志里。默认值按环境取（生产脱敏、非生产明文），
	// 显式配置优先——见 applyLogDefaults。
	SQLArgs bool `mapstructure:"sql_args"`
}

// Load 读取配置。path 为空时按 ./configs/config.yaml 查找。
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath(".")
	}

	// TA_HTTP_PORT 这样的环境变量会覆盖 http.port。
	v.SetEnvPrefix("TA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		// 配置文件缺失不是错误：纯环境变量部署是受支持的用法。
		var notFound viper.ConfigFileNotFoundError
		if !isNotFound(err, &notFound) {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	if err := bindEnvOverrides(v); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	// 归一化必须在 applyLogDefaults 之前：那一步已经开始读 IsProd 了。
	cfg.App.Env = strings.ToLower(strings.TrimSpace(cfg.App.Env))
	applyLogDefaults(v, &cfg)
	if err := cfg.LLM.resolveProviders(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// envOnlyKeys 是没有默认值、也未必出现在配置文件里的配置项。
//
// 它们清一色是凭据。凭据不该有默认值——一个默认密码比没有密码更危险，
// 因为它会让「忘了配」看起来像「配好了」。
var envOnlyKeys = []string{
	"mysql.password",
	"redis.password",
	"amqp.password",
	"auth.jwt_secret",
	"auth.bootstrap_admin_password",
	"market.tushare_token",
	"market.finnhub_token",
}

// bindEnvOverrides 让每一个配置项都能被环境变量覆盖。
//
// ===========================================================================
// 为什么 AutomaticEnv 一个人不够用
// ===========================================================================
//
// viper 的 AutomaticEnv 只会为它**已知**的键去查环境变量，而「已知」的定义是
// 「有默认值，或出现在配置文件里」。两者都不满足的键，环境变量会被**静默忽略**——
// 不报错、不警告，只是取到零值。
//
// 这条规则最坑的地方在于它恰好打在凭据上：`mysql.password` 没有默认值
// （密码不该有默认值），纯环境变量部署时配置文件也不在，于是
// `TA_MYSQL_PASSWORD=xxx` 什么也不做，服务拿着空密码去连库。
// 而密码恰恰是容器部署里最常用环境变量注入的东西。
//
// 显式 BindEnv 把这件事钉死：AllKeys 覆盖默认值与配置文件里的全部键，
// envOnlyKeys 补上那几个两者都没有的凭据。
func bindEnvOverrides(v *viper.Viper) error {
	for _, key := range append(v.AllKeys(), envOnlyKeys...) {
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("绑定环境变量 %s 失败: %w", key, err)
		}
	}
	return nil
}

func isNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if e, ok := err.(viper.ConfigFileNotFoundError); ok {
		*target = e
		return true
	}
	return false
}

// applyLogDefaults 补上那个取值依赖环境的默认项。
//
// log.sql_args 不能像别的键那样走 v.SetDefault：默认值要按 app.env 取，
// 而 app.env 本身也来自这份配置，setDefaults 跑的时候还不知道。
//
// 于是这里在 Unmarshal 之后补，并且**只在用户没有显式配置时**才补——
// 判据是 IsSet 而不是「值是否为零」，否则显式写 sql_args=false 的非生产环境
// 会被这段代码悄悄改回 true，而那正是它想关掉的东西。
//
// 这个键刻意不进 setDefaults：一旦设了默认值，IsSet 就恒为 true，
// 「用户配没配过」这个问题就再也问不出来了。
func applyLogDefaults(v *viper.Viper, cfg *Config) {
	if v.IsSet("log.sql_args") {
		return
	}
	// 生产默认脱敏。SQL 是参数内联后的，明文等于把邮箱、口令哈希、
	// 持仓金额写进日志——日志的留存期限和访问范围都和数据库不是一回事。
	cfg.Log.SQLArgs = !cfg.App.IsProd()
}

func (c *Config) validate() error {
	switch c.App.Env {
	case EnvDev, EnvTest, EnvProd:
	default:
		return fmt.Errorf("app.env 只能是 %s / %s / %s，当前是 %q", EnvDev, EnvTest, EnvProd, c.App.Env)
	}
	if c.Auth.JWTSecret == "" || c.Auth.JWTSecret == "change-me" {
		if c.App.IsProd() {
			return fmt.Errorf("生产环境必须设置 auth.jwt_secret")
		}
	}
	if c.MySQL.Database == "" {
		return fmt.Errorf("必须配置 mysql.database")
	}
	return nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("app.name", "trading-agents")
	v.SetDefault("app.env", "dev")

	v.SetDefault("http.host", "0.0.0.0")
	v.SetDefault("http.port", 8080)
	v.SetDefault("http.read_timeout", "30s")
	// 写超时要大于一次完整分析的时间上限，否则 SSE 长连接会被服务端掐断。
	v.SetDefault("http.write_timeout", "10m")
	v.SetDefault("http.allowed_origins", []string{"*"})

	v.SetDefault("mysql.host", "127.0.0.1")
	v.SetDefault("mysql.port", 3306)
	v.SetDefault("mysql.user", "root")
	v.SetDefault("mysql.database", "trading_agents")
	v.SetDefault("mysql.max_open_conns", 50)
	v.SetDefault("mysql.max_idle_conns", 10)
	v.SetDefault("mysql.conn_max_lifetime", "1h")

	v.SetDefault("mongo.uri", "mongodb://127.0.0.1:27017")
	v.SetDefault("mongo.database", "trading_agents")
	v.SetDefault("mongo.timeout", "10s")
	v.SetDefault("mongo.enabled", true)

	v.SetDefault("redis.addr", "127.0.0.1:6379")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 6)

	v.SetDefault("amqp.host", "127.0.0.1")
	v.SetDefault("amqp.port", 5672)
	v.SetDefault("amqp.vhost", "/")
	v.SetDefault("amqp.user", "guest")
	v.SetDefault("amqp.password", "guest")
	v.SetDefault("amqp.max_retries", 3)
	v.SetDefault("amqp.retry_delay", "30s")
	v.SetDefault("amqp.reconnect_delay", "5s")
	v.SetDefault("amqp.publish_timeout", "10s")
	// 两个队列的可调参数。给 0 值只是为了让这些键「已知」，
	// 从而能被环境变量覆盖；真正的默认值在内置拓扑里（constants.DefaultQueues）。
	for _, q := range []string{"domain_event", "scheduled_job_due"} {
		v.SetDefault("amqp."+q+".consumers", 0)
		v.SetDefault("amqp."+q+".prefetch", 0)
		v.SetDefault("amqp."+q+".max_retries", 0)
		v.SetDefault("amqp."+q+".retry_delay", "0s")
	}

	v.SetDefault("auth.jwt_secret", "change-me")
	v.SetDefault("auth.access_token_ttl", "2h")
	v.SetDefault("auth.refresh_token_ttl", "720h")
	v.SetDefault("auth.bcrypt_cost", 12)
	v.SetDefault("auth.bootstrap_admin", "admin")
	// bootstrap_admin_password 刻意没有默认值，见 envOnlyKeys。

	v.SetDefault("queue.max_attempts", 3)
	// 两个 TTL 都必须大于 constants.AnalysisMaxRuntime（30m），理由见 Queue 的注释。
	// 6h 覆盖「排队 + 三次各跑满 30 分钟的重试」还有富余；2h 同理覆盖单个任务的
	// 全生命周期，又不会让已完成任务的快照长期滞留在 Redis 里。
	v.SetDefault("queue.slot_ttl", "6h")
	v.SetDefault("queue.progress_ttl", "2h")
	v.SetDefault("queue.user_concurrency", 3)
	v.SetDefault("queue.global_concurrency", 20)

	v.SetDefault("llm.providers_json", "")

	v.SetDefault("llm.default_model", "deepseek-chat")
	v.SetDefault("llm.timeout", "180s")

	v.SetDefault("market.timeout", "30s")
	v.SetDefault("market.enable_mock", true)
	// 默认顺序里 tushare 在东财之前：它是唯一提供行业/地区/上市日期的源，
	// 排后面就永远轮不到它去填这几列。没配 token 时它自动不进链，东财顶上。
	v.SetDefault("market.providers", []string{"tushare", "eastmoney", "finnhub", "mock"})
	v.SetDefault("market.eastmoney_rps", 2.0)
	v.SetDefault("market.eastmoney_burst", 1)
	v.SetDefault("market.sync_fan_out_limit", 8)
	v.SetDefault("market.sync_chunk_size", 200)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "console")
	// 逐条打印 SQL / Redis / Mongo 命令，默认开。
	// log.sql_args 故意不在这里给默认值，理由见 applyLogDefaults。
	v.SetDefault("log.sql", true)
}
