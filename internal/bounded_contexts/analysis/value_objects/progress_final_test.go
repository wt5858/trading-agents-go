package value_objects

import "testing"

// TestProgressFinalCoversAllTerminalPaths 钉住 Final 的语义。
//
// 这条测试守的是一个真实发生过的缺陷：SSE 处理器原先拿「步数是否跑满」当收流判据，
// 而失败与取消同样是终局、步骤却没跑完，于是流永不收口——连接、goroutine 与
// Redis 订阅一直挂着，前端状态永远停在「运行中」。
//
// 三条终局路径必须都置 Final，否则那个缺陷会以另一种形式回来。
func TestProgressFinalCoversAllTerminalPaths(t *testing.T) {
	base := NewProgress(Request{})

	t.Run("初始进度不是终局", func(t *testing.T) {
		if base.Final {
			t.Error("刚创建的进度不该是终局")
		}
	})

	t.Run("推进中不是终局", func(t *testing.T) {
		if got := base.WithMessage("分析中"); got.Final {
			t.Error("WithMessage 只是换文案，不该置终局")
		}
	})

	t.Run("成功收尾是终局", func(t *testing.T) {
		done := base.MarkDone()
		if !done.Final {
			t.Error("MarkDone 必须置 Final")
		}
	})

	t.Run("失败与取消是终局且不把步骤画成完成", func(t *testing.T) {
		for _, msg := range []string{"分析失败", "已取消"} {
			got := base.MarkFinal(msg)
			if !got.Final {
				t.Errorf("MarkFinal(%q) 必须置 Final", msg)
			}
			if got.Message != msg {
				t.Errorf("MarkFinal(%q) 的 Message = %q", msg, got.Message)
			}
			// 关键断言：终局不等于成功。把失败画成一排绿勾比不画更糟。
			for _, s := range got.Steps {
				if s.Done {
					t.Errorf("MarkFinal(%q) 不该把步骤 %q 置完成", msg, s.Key)
				}
			}
		}
	})
}
