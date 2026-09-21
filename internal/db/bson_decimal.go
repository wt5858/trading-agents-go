package db

import (
	"fmt"
	"reflect"

	"github.com/shopspring/decimal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// decimalType 是注册编解码器时用的类型键。
var decimalType = reflect.TypeOf(decimal.Decimal{})

// newDecimalRegistry 构造带 decimal 编解码器的 BSON 注册表。
//
// # 为什么必须自己写
//
// shopspring/decimal 没有实现 bson.ValueMarshaler，驱动默认会把它当成普通结构体，
// 按未导出字段反射——结果是写进去一个空文档 {}。也就是说，不注册编解码器的话，
// 把 DTO 字段从 float64 改成 decimal.Decimal 不会编译报错，只会在运行时
// 静默写坏全部行情数据。这是整个迁移里最容易埋雷的一处。
//
// # 为什么落成 Decimal128 而不是字符串
//
// 选股要在 Mongo 侧对 pe、pb、换手率这些字段做 $gte/$lte 和 $sort
// （见 screening/repositories/screen_query_builder.go）。字符串比较是字典序：
// "9" > "10"，"9.5" > "12.3"，范围筛选会给出完全错误的结果集，而且不报错。
// Decimal128 是 BSON 原生的十进制类型，比较与排序都是数值语义，
// 且与 shopspring/decimal 一样是十进制而非二进制浮点，往返无损。
func newDecimalRegistry() *bsoncodec.Registry {
	reg := bson.NewRegistry()
	reg.RegisterTypeEncoder(decimalType, bsoncodec.ValueEncoderFunc(encodeDecimal))
	reg.RegisterTypeDecoder(decimalType, bsoncodec.ValueDecoderFunc(decodeDecimal))
	return reg
}

func encodeDecimal(_ bsoncodec.EncodeContext, vw bsonrw.ValueWriter, val reflect.Value) error {
	if !val.IsValid() || val.Type() != decimalType {
		return bsoncodec.ValueEncoderError{
			Name:     "encodeDecimal",
			Types:    []reflect.Type{decimalType},
			Received: val,
		}
	}
	d := val.Interface().(decimal.Decimal)
	// 经由字符串中转：decimal 与 Decimal128 都是「系数 + 十进制指数」，
	// 字符串是两者共有的无损表示，比手工搬运系数与指数少一整类边界错误。
	d128, err := primitive.ParseDecimal128(d.String())
	if err != nil {
		return fmt.Errorf("decimal %s 无法表示为 Decimal128: %w", d.String(), err)
	}
	return vw.WriteDecimal128(d128)
}

// decodeDecimal 刻意接受多种来源类型。
//
// Decimal128 是本仓库写入的新格式；Double 是这次迁移之前落库的存量行情
// （彼时字段是 float64）。容忍 Double 意味着存量文档无需 backfill 就能读出来，
// 迁移可以先上线、再按自己的节奏重新同步——否则改完代码的那一刻，
// 历史 K 线会整片解码失败。Int32/Int64 是同步任务偶尔写进整数值的情况
// （成交量这类），String 则是为手工写入或外部导入的数据留的后路。
func decodeDecimal(_ bsoncodec.DecodeContext, vr bsonrw.ValueReader, val reflect.Value) error {
	if !val.CanSet() || val.Type() != decimalType {
		return bsoncodec.ValueDecoderError{
			Name:     "decodeDecimal",
			Types:    []reflect.Type{decimalType},
			Received: val,
		}
	}

	var result decimal.Decimal
	switch vr.Type() {
	case bsontype.Decimal128:
		d128, err := vr.ReadDecimal128()
		if err != nil {
			return err
		}
		parsed, err := decimal.NewFromString(d128.String())
		if err != nil {
			return fmt.Errorf("Decimal128 %s 无法解析成 decimal: %w", d128.String(), err)
		}
		result = parsed
	case bsontype.Double:
		f, err := vr.ReadDouble()
		if err != nil {
			return err
		}
		// NewFromFloat 取的是能唯一还原该 double 的最短十进制表示，
		// 所以存量里的 12.34 读出来就是 12.34，而不是 12.339999999999999。
		result = decimal.NewFromFloat(f)
	case bsontype.Int32:
		i, err := vr.ReadInt32()
		if err != nil {
			return err
		}
		result = decimal.NewFromInt32(i)
	case bsontype.Int64:
		i, err := vr.ReadInt64()
		if err != nil {
			return err
		}
		result = decimal.NewFromInt(i)
	case bsontype.String:
		s, err := vr.ReadString()
		if err != nil {
			return err
		}
		parsed, err := decimal.NewFromString(s)
		if err != nil {
			return fmt.Errorf("字符串 %q 无法解析成 decimal: %w", s, err)
		}
		result = parsed
	case bsontype.Null:
		if err := vr.ReadNull(); err != nil {
			return err
		}
		result = decimal.Zero
	case bsontype.Undefined:
		if err := vr.ReadUndefined(); err != nil {
			return err
		}
		result = decimal.Zero
	default:
		return fmt.Errorf("无法把 BSON 类型 %s 解码成 decimal.Decimal", vr.Type())
	}

	val.Set(reflect.ValueOf(result))
	return nil
}
