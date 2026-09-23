package dto

import (
	"encoding/json"
	"time"
)

// PaymentItem 是一张支付单在**列表与详情里的同一份形状**。
//
// 列表与详情不各留一份，是为了让前端两处读同一组字段名：列表页的 dataIndex 与详情页
// ProDescriptions 的 dataIndex 逐字相同，抄列定义时不会错位。代价是列表多传几个字段
// （attach、legacyId 之类），它们都很小，而支付单列表本来就按页取。
//
// 可空列一律用指针，让 JSON 出 null 而不是零值：closedAt / paidAt 的「没发生过」与
// 「发生在 0001-01-01」必须分得开，后台的 `—` 是照着 null 显示的。
type PaymentItem struct {
	// ID 一起发出去：这张页面是给运维排查用的，拿到 uuid 才能直接去库里
	// SELECT * FROM payment_fundings WHERE payment_id = '…'。
	ID        string  `json:"id"`
	PaymentNo string  `json:"paymentNo"`
	LegacyID  *string `json:"legacyId"`
	OrderNo   string  `json:"orderNo"`
	UserID    string  `json:"userId"`
	Amount    int64   `json:"amount"`
	Status    string  `json:"status"`
	Subject   string  `json:"subject"`
	// Attach 是渠道附加数据（小程序 openid、设备号等），**不放密钥**。
	Attach                json.RawMessage `json:"attach"`
	ProviderTransactionID string          `json:"providerTransactionId"`
	// FailureCode / FailureMessage 在成功单上都是空串（不是 null）——见 payments 那两列的列定义。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
	RequestID      string `json:"requestId"`
	// 渠道名与方式名。两个 code 直接来自支付单自己的两列（provider / payment_method），
	// 名字由服务层照**代码里的目录**补上（见 service.AdminQueryService.decorate）——
	// 渠道与支付方式已经不是两张表了，也没什么可 JOIN 的。
	//
	// 账户出资那条路上 ChannelCode 是空串（没有第三方），于是这一行两个字段都是空。
	// 认不出来的 code（目录变过、这张单是老的）同样留空：页面显示 `—`，而 code 本身照常在，
	// 那正是排查时要看的东西。
	ChannelCode string `json:"channelCode"`
	ChannelName string `json:"channelName"`
	MethodCode  string `json:"methodCode"`
	MethodName  string `json:"methodName"`
	// AccountEntryID 非空 = 这笔钱确实动过账户域（纯豆支付扣豆的那笔账变），
	// 在扣豆返回的那一刻就落了库、先于结算。见 model.Payment 与 payments.account_entry_id 的列注释。
	AccountEntryID  *string    `json:"accountEntryId"`
	AccountFundedAt *time.Time `json:"accountFundedAt"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	PaidAt          *time.Time `json:"paidAt"`
	ClosedAt        *time.Time `json:"closedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

// PaymentFundingItem 是一条出资行。
type PaymentFundingItem struct {
	ID                    string     `json:"id"`
	LineNo                int        `json:"lineNo"`
	LineType              string     `json:"lineType"`
	Amount                int64      `json:"amount"`
	Status                string     `json:"status"`
	ProviderTransactionID string     `json:"providerTransactionId"`
	FailureCode           string     `json:"failureCode"`
	AccountEntryID        *string    `json:"accountEntryId"`
	SucceededAt           *time.Time `json:"succeededAt"`
	ReversedAt            *time.Time `json:"reversedAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

// PaymentTransactionItem 是一条记账流水。
//
// 这张表是**只增的**：一笔出资成功写一条 in，将来退款写一条 out，冲正再写一条。所以同一条
// 出资行会有多行流水，靠 kind 与 direction 区分——页面上不合并，合并了就看不到冲正。
type PaymentTransactionItem struct {
	ID            string  `json:"id"`
	LegacyID      *string `json:"legacyId"`
	Kind          string  `json:"kind"`
	PaymentNo     string  `json:"paymentNo"`
	RefundNo      string  `json:"refundNo"`
	FundingLineNo int     `json:"fundingLineNo"`
	LineType      string  `json:"lineType"`
	Direction     string  `json:"direction"`
	Amount        int64   `json:"amount"`
	// Provider 是这条流水走的那条渠道（渠道名），账户出资的那条是空串。
	Provider              string    `json:"provider"`
	ProviderTransactionID string    `json:"providerTransactionId"`
	AccountEntryID        *string   `json:"accountEntryId"`
	OccurredAt            time.Time `json:"occurredAt"`
	CreatedAt             time.Time `json:"createdAt"`
}

// PaymentTransitionItem 是一条状态流转。
//
// Reason 与 RequestID **原样发出去、页面上也原样显示**：Reason 是中英混着的诊断串
// （`payment created` / `provider reported payment failed: 用户取消` /
// `咖啡豆余额不足，需要 3400 分`），Mapping 它只会映射错——发明这套串的人当时就在代码里
// 写中文，把三种来源的串硬翻成一张码表，等于把唯一的一手信息换成二手猜测。
type PaymentTransitionItem struct {
	// ID 发出去是因为这张表是排查时的入口：拿到它能在库里直接定位到那次流转。
	ID            string          `json:"id"`
	AggregateType string          `json:"aggregateType"`
	AggregateID   string          `json:"aggregateId"`
	FromStatus    string          `json:"fromStatus"`
	ToStatus      string          `json:"toStatus"`
	Reason        string          `json:"reason"`
	RequestID     string          `json:"requestId"`
	ActorType     string          `json:"actorType"`
	ActorID       *string         `json:"actorId"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// PaymentProviderCallItem 是一次出网调用。
//
// 请求 / 响应摘要是**原样发出去**的（用户已拍板）。今天渠道只有 manual 那个合成实现，
// 摘要里是我们自己写的形状；接了真实渠道之后，这里会出现第三方报文里的字段。摘要在入库时
// 就由 provider 层做过脱敏（见 provider 包），所以「原样」指的是不再做二次裁剪，不是
// 把密钥也发出来。
type PaymentProviderCallItem struct {
	ID              string          `json:"id"`
	Provider        string          `json:"provider"`
	Operation       string          `json:"operation"`
	PaymentNo       string          `json:"paymentNo"`
	RefundNo        string          `json:"refundNo"`
	AgreementNo     string          `json:"agreementNo"`
	RequestID       string          `json:"requestId"`
	TraceID         string          `json:"traceId"`
	AttemptNo       int             `json:"attemptNo"`
	RequestSummary  json.RawMessage `json:"requestSummary"`
	ResponseSummary json.RawMessage `json:"responseSummary"`
	HTTPStatus      *int            `json:"httpStatus"`
	ProviderCode    string          `json:"providerCode"`
	ProviderMessage string          `json:"providerMessage"`
	Result          string          `json:"result"`
	DurationMS      *int            `json:"durationMs"`
	CreatedAt       time.Time       `json:"createdAt"`
}

// PaymentNotificationItem 是一条回调通知。
//
// Body 在库里是 bytea（渠道发来的原始报文），这里**显式转成 string**：Go 把 []byte 编码成
// JSON 是 **base64**，直接发 []byte 的话前端拿到的会是一串 base64——页面上就是乱码中的乱码，
// 而它本来的用途恰恰是「渠道到底回了什么」。转成 string 之后非 UTF-8 的字节会被替换成
// U+FFFD，页面注释里写明了这件事。
type PaymentNotificationItem struct {
	ID                string          `json:"id"`
	Provider          string          `json:"provider"`
	NotificationID    string          `json:"notificationId"`
	EventType         string          `json:"eventType"`
	PaymentNo         string          `json:"paymentNo"`
	RefundNo          string          `json:"refundNo"`
	Body              string          `json:"body"`
	BodySHA256        string          `json:"bodySha256"`
	Headers           json.RawMessage `json:"headers"`
	SignatureVerified bool            `json:"signatureVerified"`
	Status            string          `json:"status"`
	FailureReason     string          `json:"failureReason"`
	ReceivedAt        time.Time       `json:"receivedAt"`
	ProcessedAt       *time.Time      `json:"processedAt"`
}

// PaymentDetail 是一次把详情页要的六块全给出去。
//
// 一次给全而不是每个页签各发一次请求：打开一次详情发六个请求，页签切一次卡一次，而且
// 六个响应之间天然不一致（第三次请求回来时第二块可能已经变了）。订单详情的五个页签走的是
// 同一条路。
//
// 六块都在**同一个只读事务**里读（见 repository.GetPaymentDetail）：否则一次渠道回调正好
// 落在六条查询中间时，页面会显示「支付单 succeeded 但出资行还停在 reserved」——一个不存在
// 的中间态，而这张页面的全部意义就是如实反映某一刻的状态。
type PaymentDetail struct {
	Payment       PaymentItem               `json:"payment"`
	Fundings      []PaymentFundingItem      `json:"fundings"`
	Transactions  []PaymentTransactionItem  `json:"transactions"`
	Transitions   []PaymentTransitionItem   `json:"transitions"`
	ProviderCalls []PaymentProviderCallItem `json:"providerCalls"`
	Notifications []PaymentNotificationItem `json:"notifications"`
}
