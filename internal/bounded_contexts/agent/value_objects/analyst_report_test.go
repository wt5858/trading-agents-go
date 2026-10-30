package value_objects

import "testing"

func TestAnalystReportContract(t *testing.T) {
	cases := []struct {
		raw string
		ok  bool
	}{
		{`{"status":"complete","direction":"bullish","findings":[{"claim":"价格上涨","evidence_ids":["known"]}]}`, true},
		{`{"status":"complete","direction":"bullish","findings":[{"claim":"价格上涨","evidence_ids":["invented"]}]}`, false},
		{`{"status":"insufficient","direction":"neutral","missing":["行情缺失"]}`, true},
		{`{"status":"insufficient","direction":"bullish","missing":["行情缺失"]}`, false},
		{`{"status":"complete","direction":"neutral"}`, false},
		{`{"status":"partial","direction":"neutral","findings":[{"claim":"价格上涨","evidence_ids":["known"]}]}`, false},
	}
	for _, c := range cases {
		_, err := ParseAnalystReport(c.raw, map[string]bool{"known": true})
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.raw, err)
		}
	}
}
