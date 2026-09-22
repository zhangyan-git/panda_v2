package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// FindPaymentByNo 按支付单号读一张支付单。渠道回调与幂等回放都用它。
//
// 不加锁：这是一条读路径，真正决定「能不能改」的判断在 SettlePayment 的事务里、
// 用 FOR UPDATE 再读一次。在这里加锁只会让人误以为锁的有效范围跨到了调用方。
func (r *PostgresRepository) FindPaymentByNo(ctx context.Context, paymentNo string) (*model.Payment, error) {
	payment, err := scanPayment(r.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE payment_no = $1`, paymentNo))
	if err != nil {
		return nil, mapPGError(err)
	}
	return payment, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// jsonObject 把一个 JSONB 列解成一个**保留嵌套**的对象树。
//
// 它从前的唯一调用方是 payment_channels.config（协议声明：endpoints / sign / request /
// response / notify 五段）。那张表删掉之后，适配器要的那棵树由 catalog 在代码里拼（见
// catalog.umsChannel），这一族里还在读的 JSONB 列只剩 payments.attach——而 attach 恰恰
// 是一个**必须保留嵌套**的值（里面有 openid 与设备号）。
//
// 早先这里走过 jsonStrings（拍平成 map[string]string），那时它装的是商户号、appid 一类
// 的平铺参数。拍平会让「哪一段的哪个键」变成字符串拼接，而拼错一个点号取到的是空串——
// 那是一种不会报错的失败。见 provider.Config。
func jsonObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode json config: %w", err)
	}
	if object == nil {
		// JSON 里的 `null` 解出来是 nil map，写回去是 NULL 而不是 {}。这一列是 NOT NULL，
		// 而 nil 在后续任何一次读里都会 panic 或静默变成空——统一成空对象。
		return map[string]any{}, nil
	}
	return object, nil
}

// jsonStrings 把 JSONB 列里的**标量**翻成字符串映射。
//
// 支付方式与渠道的参数今天都由 catalog 在代码里给（map[string]string 直接构造），这一族
// 里还在读它的地方只剩历史行与快照。它仍然拒绝嵌套，而那是**特性不是限制**：一份平铺的
// 旋钮里出现嵌套对象，说明写它的人以为这里是另一列，报错比静默丢掉强。
//
// 为什么不直接 Unmarshal 进 map[string]string：运营在后台填 `"timeout": 30` 时 JSONB
// 存的是数字，直接解会报「cannot unmarshal number into Go value of type string」，
// 而那个错误对填写的人毫无指导意义。这里把标量都转成字符串，只有嵌套对象与数组报错。
func jsonStrings(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("decode json config: %w", err)
	}
	out := make(map[string]string, len(generic))
	for key, value := range generic {
		switch typed := value.(type) {
		case string:
			out[key] = typed
		case float64:
			// encoding/json 把 JSON 数字解成 float64。百分位金额已在这两列之外（金额走
			// payments.amount），这里的数字是超时秒数、序号一类的整数，用 %v 会打出
			// 1.2e+06 这种形状，所以走 strconv 的整数格式。
			out[key] = strconv.FormatInt(int64(typed), 10)
		case bool:
			out[key] = strconv.FormatBool(typed)
		case nil:
			out[key] = ""
		default:
			return nil, fmt.Errorf("config key %q holds a %T; only flat string values are supported", key, value)
		}
	}
	return out, nil
}
