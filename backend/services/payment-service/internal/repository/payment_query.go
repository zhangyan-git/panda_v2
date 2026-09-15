package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// methodColumns / channelColumns 是两张配置表的读取列，列顺序与
// scanPaymentMethodWithChannel 的扫描顺序严格一一对应，两边必须一起改。
const methodColumns = `m.id::text, m.legacy_id, m.code, m.name, m.description, m.icon,
	m.channel_id::text, m.action, m.params, m.funding_type, m.status, m.sort_order,
	m.created_at, m.updated_at`

const channelColumns = `c.id::text, c.legacy_id, c.code, c.name, c.provider, c.mode, c.status,
	c.config, c.secret_ref, c.remark, c.created_at, c.updated_at`

// PaymentMethodWithChannel 是一条支付方式加上它的渠道（如果有）。
//
// 用 LEFT JOIN 一次查回来而不是分两次：发起支付的每一次调用都要这两个事实，分成两次
// 就意味着两次往返和一段「查到了方式、再去查渠道时失败」的半成品状态要处理。
//
// Channel 为 nil 是**合法**的：账户出资方式（咖啡豆）在 payment_methods.channel_id
// 上本来就是空的——它们走 account-service 扣余额，没有外部渠道。
type PaymentMethodWithChannel struct {
	Method  model.PaymentMethod
	Channel *model.PaymentChannel
	// MethodParams / ChannelConfig 是上面两行里那两个 JSONB 列解出来的**扁平字符串
	// 键值**，与 provider.Method 要的形状一致。
	//
	// 单独给出来是因为 model 层只做表镜像（那两列是 json.RawMessage），而「怎么起支付」
	// 要的是一个能直接读的 map。让每个调用点各自解一次，就会有三处对「数字该不该转成
	// 字符串」的不同答案。
	MethodParams  map[string]string
	ChannelConfig map[string]string
}

// FindPaymentMethod 按 id 读一条支付方式及其渠道。
//
// 不存在、或已停用时分别返回 ErrPaymentMethodNotFound / ErrPaymentMethodInactive：
// 前者是调用方传错了 id（配置问题），后者是运营有意关掉的（重试多少次都一样）。
// 混成一个错误会让排查时不知道该去看代码还是看后台。
func (r *PostgresRepository) FindPaymentMethod(ctx context.Context, methodID string) (*PaymentMethodWithChannel, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+methodColumns+`, `+channelColumns+`
		FROM payment_methods m
		LEFT JOIN payment_channels c ON c.id = m.channel_id
		WHERE m.id = $1`, methodID)
	found, err := scanPaymentMethodWithChannel(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentMethodNotFound
		}
		return nil, err
	}
	if found.Method.Status != model.MethodEnabled {
		return nil, ErrPaymentMethodInactive
	}
	return found, nil
}

func scanPaymentMethodWithChannel(row scanner) (*PaymentMethodWithChannel, error) {
	found := &PaymentMethodWithChannel{}
	// 渠道侧全部用指针：LEFT JOIN 没命中时它们整片是 NULL。用零值接会把「没有渠道」
	// 压成「渠道代码是空串」，而后者看上去像一条配坏了的渠道行。
	var (
		channelID        *string
		channelLegacyID  *string
		channelCode      *string
		channelName      *string
		channelProvider  *string
		channelMode      *string
		channelStatus    *string
		channelConfig    []byte
		channelSecretRef *string
		channelRemark    *string
		channelCreatedAt *time.Time
		channelUpdatedAt *time.Time
	)
	err := row.Scan(&found.Method.ID, &found.Method.LegacyID, &found.Method.Code,
		&found.Method.Name, &found.Method.Description, &found.Method.Icon,
		&found.Method.ChannelID, &found.Method.Action, &found.Method.Params,
		&found.Method.FundingType, &found.Method.Status, &found.Method.SortOrder,
		&found.Method.CreatedAt, &found.Method.UpdatedAt,
		&channelID, &channelLegacyID, &channelCode, &channelName, &channelProvider,
		&channelMode, &channelStatus, &channelConfig, &channelSecretRef, &channelRemark,
		&channelCreatedAt, &channelUpdatedAt)
	if err != nil {
		return nil, err
	}
	if channelID != nil {
		found.Channel = &model.PaymentChannel{
			ID:        *channelID,
			LegacyID:  channelLegacyID,
			Code:      deref(channelCode),
			Name:      deref(channelName),
			Provider:  deref(channelProvider),
			Mode:      deref(channelMode),
			Status:    deref(channelStatus),
			Config:    channelConfig,
			SecretRef: deref(channelSecretRef),
			Remark:    deref(channelRemark),
		}
		if channelCreatedAt != nil {
			found.Channel.CreatedAt = *channelCreatedAt
		}
		if channelUpdatedAt != nil {
			found.Channel.UpdatedAt = *channelUpdatedAt
		}
		found.ChannelConfig, err = jsonStrings(channelConfig)
		if err != nil {
			return nil, fmt.Errorf("channel %s config: %w", found.Channel.Code, err)
		}
	}
	found.MethodParams, err = jsonStrings(found.Method.Params)
	if err != nil {
		return nil, fmt.Errorf("payment method %s params: %w", found.Method.Code, err)
	}
	return found, nil
}

// ChannelRecord 是一行渠道配置加上它 config 列解出来的映射，形状同 PaymentMethodWithChannel。
type ChannelRecord struct {
	Channel *model.PaymentChannel
	Config  map[string]string
}

// FindChannelByCode 按渠道代码读一行渠道配置。渠道回调用它把 URL 里那段翻成 provider。
//
// **不过滤 status**：渠道被停用（disabled）或标成 legacy_readonly 都只影响「还能不能发起
// 新支付」，不影响已经发生的交易——那些单的款项该到还是要到，回调必须照收。按状态过滤会
// 让一笔已经付出去的钱的回调被当成「未知渠道」拒掉。
func (r *PostgresRepository) FindChannelByCode(ctx context.Context, code string) (*ChannelRecord, error) {
	var (
		id, channelCode, name, provider, mode, status string
		legacyID, secretRef, remark                   *string
		config                                        []byte
		createdAt, updatedAt                          time.Time
	)
	err := r.pool.QueryRow(ctx, `SELECT `+channelColumns+`
		FROM payment_channels c WHERE c.code = $1`, code).Scan(
		&id, &legacyID, &channelCode, &name, &provider, &mode, &status,
		&config, &secretRef, &remark, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}
	decoded, err := jsonStrings(config)
	if err != nil {
		return nil, fmt.Errorf("channel %s config: %w", channelCode, err)
	}
	return &ChannelRecord{
		Channel: &model.PaymentChannel{
			ID: id, LegacyID: legacyID, Code: channelCode, Name: name, Provider: provider,
			Mode: mode, Status: status, Config: config, SecretRef: deref(secretRef),
			Remark: deref(remark), CreatedAt: createdAt, UpdatedAt: updatedAt,
		},
		Config: decoded,
	}, nil
}

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

// jsonStrings 把 JSONB 列里的**标量**翻成字符串映射。
//
// 为什么不直接 Unmarshal 进 map[string]string：运营在后台填 `"timeout": 30` 时 JSONB
// 存的是数字，直接解会报「cannot unmarshal number into Go value of type string」，
// 而那个错误对填写的人毫无指导意义。这里把标量都转成字符串，只有嵌套对象与数组报错——
// 它们本来就不该出现在这两列里（两列都是「扁平字符串键值」，见 provider.Method 的注释）。
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
