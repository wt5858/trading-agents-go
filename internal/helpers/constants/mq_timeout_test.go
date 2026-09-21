package constants

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// AnalysisMaxRuntime 有一半在 Go 之外：broker 的 consumer_timeout 是 RabbitMQ
// 自己的配置，只能写在 docker-compose.yml 里。两边靠注释互指是不够的——
// 注释拦不住任何人，而两个值一旦分家，现场表现是「长任务偶尔被重投」或者
// 「巡检偶尔杀掉健康任务」，两种都极难顺藤摸瓜查回这两个数字上。
//
// 因此把这条跨文件的耦合做成断言：改了一边没改另一边，这里会红。
func TestConsumerTimeoutMatchesAnalysisMaxRuntime(t *testing.T) {
	// 从本包目录回到仓库根。
	path := filepath.Join("..", "..", "..", "docker-compose.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}

	re := regexp.MustCompile(`-rabbit\s+consumer_timeout\s+(\d+)`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatal("docker-compose.yml 里找不到 consumer_timeout 设置。" +
			"它不能省略：省掉就等于把长任务的重投时机交给 RabbitMQ 的版本默认值")
	}

	millis, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		t.Fatalf("consumer_timeout 不是合法数字: %q", m[1])
	}
	got := time.Duration(millis) * time.Millisecond

	if got != AnalysisMaxRuntime {
		t.Fatalf("consumer_timeout(%s) 与 AnalysisMaxRuntime(%s) 不一致。"+
			"两边量化的是同一件事，必须一起改", got, AnalysisMaxRuntime)
	}
}
