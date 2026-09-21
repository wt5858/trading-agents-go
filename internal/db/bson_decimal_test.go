package db

import (
	"testing"

	"github.com/shopspring/decimal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

type priceDoc struct {
	Close  decimal.Decimal  `bson:"close"`
	Volume decimal.Decimal  `bson:"volume"`
	PE     *decimal.Decimal `bson:"pe"`
}

// TestDecimalRoundTrip 钉死「写进去什么，读出来就是什么」。
// 这条如果不过，全仓库的行情数据都会被静默写坏。
func TestDecimalRoundTrip(t *testing.T) {
	reg := newDecimalRegistry()

	for _, raw := range []string{
		"12.3400",
		"0",
		"-5.25",
		"0.000001",
		// 20 位有效数字：float64 只有约 15.9 位十进制精度，
		// 如果编解码路径上任何一段退化成 double，这一条就会失真。
		"12345678901234.567890",
		"99999999999999999999",
	} {
		in := priceDoc{Close: decimal.RequireFromString(raw), Volume: decimal.Zero}
		data, err := bson.MarshalWithRegistry(reg, in)
		if err != nil {
			t.Fatalf("marshal %s: %v", raw, err)
		}
		var out priceDoc
		if err := bson.UnmarshalWithRegistry(reg, data, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if !out.Close.Equal(in.Close) {
			t.Errorf("往返 %s 得到 %s", raw, out.Close)
		}
	}
}

// TestDecimalEncodesAsDecimal128 确认落库类型是 Decimal128 而不是字符串。
// 字符串比较是字典序（"9" > "10"），选股的 $gte/$sort 会给出错误结果且不报错。
func TestDecimalEncodesAsDecimal128(t *testing.T) {
	reg := newDecimalRegistry()
	data, err := bson.MarshalWithRegistry(reg, priceDoc{Close: decimal.RequireFromString("12.34")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc bson.Raw = data
	v, err := doc.LookupErr("close")
	if err != nil {
		t.Fatalf("lookup close: %v", err)
	}
	if v.Type != bsontype.Decimal128 {
		t.Errorf("close 落库类型 = %s, want Decimal128", v.Type)
	}
}

// TestDecodeLegacyDouble 是存量数据的兼容保证：迁移之前落库的行情是 BSON double，
// 改完代码后必须仍然读得出来，否则历史 K 线会整片解码失败。
func TestDecodeLegacyDouble(t *testing.T) {
	reg := newDecimalRegistry()

	// 模拟旧文档：close 是 double，volume 是 int64（同步任务写整数成交量），
	// pe 缺省为 null。
	legacy, err := bson.Marshal(bson.M{
		"close":  12.34,
		"volume": int64(1000000),
		"pe":     nil,
	})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}

	var out priceDoc
	if err := bson.UnmarshalWithRegistry(reg, legacy, &out); err != nil {
		t.Fatalf("解码存量文档失败: %v", err)
	}
	if !out.Close.Equal(decimal.RequireFromString("12.34")) {
		t.Errorf("存量 double 12.34 解出 %s", out.Close)
	}
	if !out.Volume.Equal(decimal.NewFromInt(1000000)) {
		t.Errorf("存量 int64 解出 %s", out.Volume)
	}
}

// TestDecodeLegacyString 覆盖手工写入或外部导入的字符串数值。
func TestDecodeLegacyString(t *testing.T) {
	reg := newDecimalRegistry()
	legacy, err := bson.Marshal(bson.M{"close": "12.34", "volume": 0.0})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	var out priceDoc
	if err := bson.UnmarshalWithRegistry(reg, legacy, &out); err != nil {
		t.Fatalf("解码字符串数值失败: %v", err)
	}
	if !out.Close.Equal(decimal.RequireFromString("12.34")) {
		t.Errorf("字符串 12.34 解出 %s", out.Close)
	}
}

// TestDecimalPointerRoundTrip 确认可空字段走指针编解码器时也正常。
func TestDecimalPointerRoundTrip(t *testing.T) {
	reg := newDecimalRegistry()
	pe := decimal.RequireFromString("18.75")
	data, err := bson.MarshalWithRegistry(reg, priceDoc{PE: &pe})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out priceDoc
	if err := bson.UnmarshalWithRegistry(reg, data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PE == nil || !out.PE.Equal(pe) {
		t.Errorf("指针字段往返失败: %v", out.PE)
	}
}
