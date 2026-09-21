package value_objects

import (
	"testing"
	"time"

	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// base 是所有推演的固定起点。用固定时刻而不是 time.Now()：
// cron 推演的正确性与「现在几点」无关，用当前时间会让测试在月末、周末、
// 闰年这些边界上随机失败，而那种失败最难复现。
var base = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // 2026-03-02 是周一

func TestNewCronExpression_NextFireTime(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want time.Time
	}{
		{
			name: "每五分钟",
			spec: "*/5 * * * *",
			want: time.Date(2026, 3, 2, 0, 5, 0, 0, time.UTC),
		},
		{
			name: "每天固定时刻",
			spec: "30 9 * * *",
			want: time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC),
		},
		{
			name: "工作日开盘前",
			spec: "15 9 * * 1-5",
			want: time.Date(2026, 3, 2, 9, 15, 0, 0, time.UTC),
		},
		{
			// 起点是周一，所以下一个周六要等到 3-07。
			// 这正是「不能用 now + 固定间隔近似」的理由。
			name: "每周六",
			spec: "0 3 * * 6",
			want: time.Date(2026, 3, 7, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "描述符 @daily",
			spec: "@daily",
			want: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := NewCronExpression(tc.spec)
			if err != nil {
				t.Fatalf("解析 %q 失败: %v", tc.spec, err)
			}
			got := expr.Next(base)
			if !got.Equal(tc.want) {
				t.Fatalf("下次触发时刻不符：got %s, want %s", got, tc.want)
			}
			if expr.String() != tc.spec {
				t.Fatalf("String() 应返回原始表达式：got %q, want %q", expr.String(), tc.spec)
			}
			if !expr.Valid() {
				t.Fatal("合法表达式的 Valid() 应为 true")
			}
		})
	}
}

// TestNextN 验证连续推演。每一步都以上一步的结果为起点，
// 这是 cron 的语义；用 base.Add(i*interval) 近似在非等距表达式上必错。
func TestNextN(t *testing.T) {
	expr, err := NewCronExpression("*/5 * * * *")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := expr.NextN(base, 3)
	want := []time.Time{
		time.Date(2026, 3, 2, 0, 5, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 10, 0, 0, time.UTC),
		time.Date(2026, 3, 2, 0, 15, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("推演条数不符：got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("第 %d 次触发不符：got %s, want %s", i+1, got[i], want[i])
		}
	}
}

// TestNextN_NonUniformInterval 用「每月 1 号」这类非等距表达式守住
// 「必须逐次推演」这条规则——等距近似在这里会给出完全错误的序列。
func TestNextN_NonUniformInterval(t *testing.T) {
	expr, err := NewCronExpression("0 0 1 * *")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := expr.NextN(base, 3)
	want := []time.Time{
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("第 %d 次触发不符：got %s, want %s", i+1, got[i], want[i])
		}
	}
}

// TestNextN_Capped 验证预览条数被封顶，防止「参数即放大器」型的资源耗尽。
func TestNextN_Capped(t *testing.T) {
	expr, err := NewCronExpression("* * * * *")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := len(expr.NextN(base, 10_000)); got != maxPreviewCount {
		t.Fatalf("预览条数应被封顶到 %d，实际 %d", maxPreviewCount, got)
	}
}

func TestNewCronExpression_Invalid(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"空串", ""},
		{"只有空白", "   "},
		{"字段数不足", "* * *"},
		{"字段数过多", "* * * * * * *"},
		{"分钟越界", "99 * * * *"},
		{"完全不是 cron", "每天早上九点"},
		{"未知描述符", "@fortnightly"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCronExpression(tc.spec)
			if err == nil {
				t.Fatalf("非法表达式 %q 必须被拒绝", tc.spec)
			}
			// 必须是 INVALID_ARGUMENT：接口层据此映射 400。
			// 降级成 INTERNAL 会让一次用户输入错误变成一次 500 告警。
			if code := custom_errors.CodeOf(err); code != custom_errors.CodeInvalidArgument {
				t.Fatalf("错误码应为 %s，实际 %s", custom_errors.CodeInvalidArgument, code)
			}
		})
	}
}

// TestRehydrateCronExpression 守住读路径的容错约定：
// 库里的行是既成事实，一条坏掉的表达式不该让整个任务列表接口挂掉。
func TestRehydrateCronExpression(t *testing.T) {
	t.Run("合法表达式照常可用", func(t *testing.T) {
		expr := RehydrateCronExpression("*/10 * * * *")
		if !expr.Valid() {
			t.Fatal("合法表达式重建后应当可用")
		}
		want := time.Date(2026, 3, 2, 0, 10, 0, 0, time.UTC)
		if got := expr.Next(base); !got.Equal(want) {
			t.Fatalf("下次触发时刻不符：got %s, want %s", got, want)
		}
	})

	t.Run("脏数据不报错而是降级", func(t *testing.T) {
		const dirty = "0 0 31 2 *  legacy"
		expr := RehydrateCronExpression(dirty)
		if expr.Valid() {
			t.Fatal("坏表达式的 Valid() 应为 false")
		}
		// 原文必须保留：运维要能在界面上看到它写错成了什么，才能改对。
		if expr.String() != dirty {
			t.Fatalf("原文应原样保留：got %q", expr.String())
		}
		// Next 返回零值而不是 panic：调用方据此把它变成一个显式的领域错误。
		if got := expr.Next(base); !got.IsZero() {
			t.Fatalf("不可用表达式的 Next 应返回零值，实际 %s", got)
		}
		if got := expr.NextN(base, 3); got != nil {
			t.Fatalf("不可用表达式的 NextN 应返回 nil，实际 %v", got)
		}
	})
}

func TestCronExpression_Equal(t *testing.T) {
	a, _ := NewCronExpression("*/5 * * * *")
	b, _ := NewCronExpression("*/5 * * * *")
	c, _ := NewCronExpression("*/10 * * * *")
	// 两次解析出的 Schedule 是不同的接口实例，因此相等性只能按原文判定。
	if !a.Equal(b) {
		t.Fatal("同一表达式的两次解析结果应当相等")
	}
	if a.Equal(c) {
		t.Fatal("不同表达式不应相等")
	}
}
