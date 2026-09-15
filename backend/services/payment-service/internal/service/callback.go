package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// ErrNotificationPreviouslyRejected：这条通知上一次投递时我们拒了，这次还拒。
//
// 与「第一次见到的坏签名」分开只是为了日志能说清是重投还是新攻击面；对渠道的应答是一样的
// （失败）。**不能因为它是重投就改成成功应答**：那会让渠道把一条我们没认下来的通知从它的
// 重投队列里划掉，而 payment_notifications 里那条 failed 行还挂着等人查。
var ErrNotificationPreviouslyRejected = errors.New("payment notification was rejected on a previous delivery")

// ErrNotificationInProgress：同一条通知的上一次投递还在处理中。
//
// 它是个**让渠道稍后再投**的答复，不是一个结论。两种来源：并发下第二次投递先到、或者上一次
// 的处理刚好在读那一行时还没提交。无论哪种，替一次还没落地的处理做「已经处理完了」的判断
// 都是错的。
//
// 它**只覆盖窗口内**的重投（见 notificationInFlightWindow）。超过窗口的 received 是孤儿行，
// 那种由这一次接手做完，不再回这条错误。
var ErrNotificationInProgress = errors.New("payment notification is still being processed")

// CallbackRequest 是一次渠道回调的原始输入。
//
// Body 与 Headers 都是**原样传进来的**：验签要签原文，任何在中间做的规范化（比如重新
// 序列化 JSON）都会让签名对不上，而那种失败看起来像「渠道的签名算错了」。
type CallbackRequest struct {
	// ChannelCode 是回调 URL 里的那段，用来查渠道配置。
	ChannelCode string
	Body        []byte
	Headers     http.Header
}

// CallbackResult 是一次回调处理的结果，控制器拿它决定回给渠道什么。
type CallbackResult struct {
	NotificationID string
	PaymentNo      string
	// EventType 是归一化后的事件类型，空串表示没认出来（已拒的回调）。
	EventType string
	// Duplicate 表示这是重投，且上一次已经处理完了。**没有副作用**。
	Duplicate bool
	// Settled 表示这次回调真的改了支付状态、发了事件。
	Settled bool
}

// HandleNotification 处理一次渠道回调。
//
// 顺序是有讲究的，每一步都在把「我们还不知道这条报文可不可信」的窗口缩小：
//
//  1. 查渠道配置（认 URL 里那段，不认报文里说的任何东西）
//  2. 验签 —— **在碰报文里的任何字段之前**。报文里的 payment_no 是不可信输入，
//     拿它去查支付单等于让攻击者决定我们去看哪一行
//  3. 落 payment_notifications，靠 UNIQUE (provider, notification_id) 防重放
//  4. 一个事务里改支付状态 + 写流水 + 写 outbox
//
// 验签失败、金额对不上、状态冲突这三条路径上**支付状态一个字段都不动**，只是把这条回调
// 记成 failed。这是「伪造回调不会把订单变成已支付」的全部依据。
func (s *PaymentService) HandleNotification(ctx context.Context, in CallbackRequest) (*CallbackResult, error) {
	if strings.TrimSpace(in.ChannelCode) == "" {
		return nil, ErrChannelCodeRequired
	}
	if len(in.Body) == 0 {
		return nil, ErrNotificationEmpty
	}

	record, err := s.repository.FindChannelByCode(ctx, in.ChannelCode)
	if err != nil {
		return nil, err
	}
	adapter, err := s.providers.Lookup(record.Channel.Provider)
	if err != nil {
		return nil, err
	}

	method := methodFromChannel(record)
	notification, verifyErr := adapter.Verify(ctx, provider.NotificationRequest{
		ChannelCode: record.Channel.Code,
		Body:        in.Body,
		Headers:     in.Headers,
		Method:      method,
		Secret:      s.resolveSecret(record.Channel),
	})
	if verifyErr != nil {
		s.recordRejectedNotification(ctx, record, in, verifyErr)
		return nil, verifyErr
	}

	inserted, err := s.repository.InsertNotification(ctx, repository.NotificationParams{
		Provider:       method.Provider,
		ChannelID:      record.Channel.ID,
		NotificationID: notification.NotificationID,
		EventType:      string(notification.EventType),
		PaymentNo:      notification.PaymentNo,
		Body:           in.Body,
		BodySHA256:     bodySHA256(in.Body),
		Headers:        recordableHeaders(),
		// 走到这里签名一定验过了：Verify 失败时上面已经返回，绝不会带着错误往下走。
		SignatureVerified: true,
		Status:            model.NotificationReceived,
	})
	if err != nil {
		return nil, err
	}
	if !inserted.Inserted {
		// handled 为 false 表示上一次投递死在半路、这一次要接手把它做完（见
		// handleRedelivery），那就当成一条新回调继续往下走。
		if result, handled, err := s.handleRedelivery(ctx, inserted, notification); handled {
			return result, err
		}
	}

	if notification.EventType != provider.EventSucceeded && notification.EventType != provider.EventFailed {
		// 渠道会推各种与收款无关的通知（签约变更、对账文件就绪）。标 ignored 回成功应答——
		// 回失败会让渠道一直重投一条我们永远处理不了的通知，而把它当事故标 failed 又会让
		// 人工巡查队列被淹没。它确实不需要任何动作。
		if err := s.repository.MarkNotification(ctx, inserted.ID, model.NotificationIgnored,
			"notification event type is not a payment result: "+string(notification.EventType)); err != nil {
			return nil, err
		}
		slog.InfoContext(ctx, "ignored payment notification with an unhandled event type",
			"payment_no", notification.PaymentNo, "event_type", string(notification.EventType))
		return &CallbackResult{
			NotificationID: notification.NotificationID,
			PaymentNo:      notification.PaymentNo,
			EventType:      string(notification.EventType),
		}, nil
	}

	settlement, err := s.repository.SettlePayment(ctx, repository.SettleNotificationParams{
		NotificationID:        inserted.ID,
		PaymentNo:             notification.PaymentNo,
		Succeeded:             notification.EventType == provider.EventSucceeded,
		Amount:                notification.Amount,
		ProviderTransactionID: notification.ProviderTransactionID,
		PaidAt:                notification.PaidAt,
		FailureCode:           notification.FailureCode,
		FailureMessage:        notification.FailureMessage,
		TraceID:               audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		// 业务事务已经回滚了，所以这条回调记录还是 received。用**独立连接**把它标成
		// failed：那次回滚不该把「我们拒了这条回调」的留痕一起带走。
		if markErr := s.repository.MarkNotification(ctx, inserted.ID, model.NotificationFailed, err.Error()); markErr != nil {
			slog.ErrorContext(ctx, "failed to record a rejected payment notification",
				"notification_id", inserted.ID, "error", markErr)
		}
		slog.WarnContext(ctx, "refused a payment notification",
			"payment_no", notification.PaymentNo, "event_type", string(notification.EventType), "error", err)
		return nil, err
	}

	return &CallbackResult{
		NotificationID: notification.NotificationID,
		PaymentNo:      settlement.Payment.PaymentNo,
		EventType:      string(notification.EventType),
		// AlreadySettled 是「状态早就到了、这次什么都没改」：重复回调，或者一条迟到的
		// 失败通知打在已经成功的单上。回给渠道的是成功应答——它确实不需要再投了。
		Settled: !settlement.AlreadySettled,
	}, nil
}

// AckFor 给出这个渠道对这次回调的应答形状。
//
// 控制器只在这一处问「怎么答渠道」，而不是自己按渠道码写 if——应答形状与验签一样是渠道的
// 私有协议（见 provider.Ack 的注释）。代价是每次回调多一条按 code 的索引查询（渠道表只有
// 几行），买到的是「怎么答渠道」只有一条路径决定，而不是「成功走结果、失败再查一次」两套
// 规则各写一遍。控制器在拿到 HandleNotification 的结果之后才知道要答什么，所以这次查询
// 没有更好的时机。
//
// 查不到渠道、或者渠道指向一个没注册的适配器时回一句通用拒绝：这时我们连这是谁的回调都
// 不知道，任何模仿某家格式的应答都是猜，而猜错的方向如果是「成功」，就等于替一个不认识的
// 渠道确认了一条我们根本没处理的回调。**不在这里打日志**——`channelCode` 是攻击者能随便
// 填的，每次请求一条 Warn 就是一个日志放大面；真正的失败原因由调用方带着错误一起记。
func (s *PaymentService) AckFor(ctx context.Context, channelCode string, accepted bool) provider.Ack {
	record, err := s.repository.FindChannelByCode(ctx, channelCode)
	if err != nil {
		return unresolvedAck()
	}
	adapter, err := s.providers.Lookup(record.Channel.Provider)
	if err != nil {
		return unresolvedAck()
	}
	return adapter.Ack(accepted)
}

// unresolvedAck 是「不知道这是哪个渠道的回调」时的应答。
//
// 非 2xx：我们没收下这条通知。体里不放任何渠道特征，理由同 AckFor 的注释。
func unresolvedAck() provider.Ack {
	return provider.Ack{
		Status:      http.StatusBadRequest,
		ContentType: "application/json",
		Body:        []byte(`{"code":"FAIL","message":"unknown channel"}`),
	}
}

// notificationInFlightWindow 是「上一次投递还在处理中」的最长可信时间。
//
// 上一次投递要做的全部工作是 InsertNotification 与 SettlePayment 之间那一个数据库事务，
// 毫秒级；30 秒比它宽出好几个数量级。反过来，渠道的重投间隔最短也在十秒以上、通常几分钟，
// 所以窗口取这个量级不会把一次正常的并发重投误判成孤儿。
//
// 窗口内不接手是**有意的**：并发下第二次投递先到、而上一次的处理刚好还没提交，这时替一次
// 还没落地的处理做「已经处理完了」的判断是错的。
const notificationInFlightWindow = 30 * time.Second

// handleRedelivery 决定一条已经落过库的通知该怎么答。
//
// 不能见到重复就一律回成功（见 repository.NotificationRecord.ExistingStatus 的注释）：
// 上次被拒掉的那条，这次也得拒。
//
// 第二个返回值 handled 为 false 表示「这一次要接手把上次没做完的做完」，调用方接着走正常
// 的结算流程。只有一种情况会走到那里：上一次投递留下了 status='received' 的孤儿行。
func (s *PaymentService) handleRedelivery(ctx context.Context, record *repository.NotificationRecord, notification provider.Notification) (*CallbackResult, bool, error) {
	switch record.ExistingStatus {
	case model.NotificationProcessed, model.NotificationIgnored:
		// 上次确实处理完了。回成功应答，**一点状态都不碰**——防重放靠的就是这条。
		return &CallbackResult{
			NotificationID: notification.NotificationID,
			PaymentNo:      notification.PaymentNo,
			EventType:      string(notification.EventType),
			Duplicate:      true,
		}, true, nil
	case model.NotificationFailed:
		return nil, true, fmt.Errorf("%w: notification %s", ErrNotificationPreviouslyRejected, notification.NotificationID)
	case model.NotificationReceived:
		// received 有两种含义，必须分开：还在飞，还是死在半路。
		//
		// 死在半路是进程被 kill、机器掉电留下的孤儿行（落库与结算之间的那个事务还没提交）。
		// **没有任何东西会去动它**——这个服务里只有 SettlePayment 与 MarkNotification 两个
		// 写入点，都不会回头捡它。一直按「稍后再投」答，渠道投满重试次数就放弃了：钱在渠道
		// 那边已经收了，而我们这边的支付单停在 created，订单永远变不成 paid，也没有任何一条
		// 线索指向这次回调。
		//
		// 所以超过窗口就由这一次接手。重复结算是安全的：settleInTx 先把支付单 FOR UPDATE
		// 锁住，看到状态已是目标值就只把通知标 processed 后返回 AlreadySettled，不改状态、
		// 不发事件。
		if time.Duration(record.ExistingAgeSeconds)*time.Second <= notificationInFlightWindow {
			slog.InfoContext(ctx, "payment notification redelivered while the previous delivery is in flight",
				"notification_id", notification.NotificationID, "age_seconds", record.ExistingAgeSeconds)
			return nil, true, fmt.Errorf("%w: notification %s", ErrNotificationInProgress, notification.NotificationID)
		}
		slog.WarnContext(ctx, "taking over a payment notification abandoned by an earlier delivery",
			"notification_id", notification.NotificationID,
			"payment_no", notification.PaymentNo, "age_seconds", record.ExistingAgeSeconds)
		return nil, false, nil
	default:
		// 冲突了但读不到那一行（空串），只可能是另一条投递还在事务里没提交。让渠道稍后再投。
		slog.InfoContext(ctx, "payment notification redelivered while a concurrent delivery is uncommitted",
			"notification_id", notification.NotificationID)
		return nil, true, fmt.Errorf("%w: notification %s", ErrNotificationInProgress, notification.NotificationID)
	}
}

// recordRejectedNotification 把一条验签失败的回调落库留痕。
//
// 这条路径上我们**没有**渠道给的 notification_id——报文还没通过验证，里面的任何字段都还
// 不可信，包括它自称的通知号。所以用报文自身的 sha256 合成一个：同一个被篡改的报文体重投
// 时会撞上 UNIQUE (provider, notification_id)，不会一秒钟刷出一堆行。
//
// 落库失败只记日志、不改变对渠道的答复：拒绝的理由是验签不过，而不是我们写不进库。
func (s *PaymentService) recordRejectedNotification(ctx context.Context, record *repository.ChannelRecord, in CallbackRequest, verifyErr error) {
	_, err := s.repository.InsertNotification(ctx, repository.NotificationParams{
		Provider:       record.Channel.Provider,
		ChannelID:      record.Channel.ID,
		NotificationID: rejectedNotificationID(in.Body),
		// EventType / PaymentNo 一律留空：报文没通过验证，里面的值不可信，落进去只会
		// 让后来查这条记录的人以为我们知道它说的是哪一单。
		EventType:         "",
		PaymentNo:         "",
		Body:              in.Body,
		BodySHA256:        bodySHA256(in.Body),
		Headers:           recordableHeaders(),
		SignatureVerified: signatureVerified(verifyErr),
		Status:            model.NotificationFailed,
		FailureReason:     verifyErr.Error(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to record a rejected payment notification",
			"channel", record.Channel.Code, "body_sha256", bodySHA256(in.Body), "error", err)
	}
	slog.WarnContext(ctx, "refused a payment notification",
		"channel", record.Channel.Code, "body_sha256", bodySHA256(in.Body), "error", verifyErr)
}

// signatureVerified 判断一个验签错误发生时签名到底过没过。
//
// 只有 ErrInvalidNotification 表示「签名是对的，报文内容不合法」——那是适配器验完签之后
// 才可能报的错。其余（签名不对、密钥读不到、适配器自己的故障）一律记 false：
// **不知道算不算过，就不能记成过了**，这一列的全部意义就是它能被人拿来做判断。
func signatureVerified(verifyErr error) bool {
	return errors.Is(verifyErr, provider.ErrInvalidNotification)
}

// rejectedNotificationID 给一条验签失败的报文体合成一个通知号。
//
// 前缀让它一眼可辨：查库时看到 `rejected:` 开头就知道这一行的来源是「我们没认下来」，
// 而不是渠道给的通知号。column 是 TEXT，没有长度限制。
func rejectedNotificationID(body []byte) string {
	return "rejected:" + bodySHA256(body)
}

// bodySHA256 是报文的十六进制摘要。日志里只允许出现它，不许出现报文本身（方案 11.5）。
func bodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// recordableHeaders 是可以进 payment_notifications.headers 的请求头。
//
// 目前**一个都不留**，这是有意的：那一列的定义是「只留定位用的请求头（请求号、时间戳、
// 签名算法名）；签名原文与密钥不在这里」。manual 渠道的回调只有带签名的那个头，而签名本身
// 不能留——哪怕截断成前 8 位，它也是「这条报文是有效签名」的一份凭证，留着会让重放在签名
// 有效期内变得可行。真实渠道接进来时，它们的时间戳与请求号那类头在这里加。
func recordableHeaders() map[string]string {
	return map[string]string{}
}

// methodFromChannel 把渠道行翻成适配器要的 shape。
//
// **Params 与行动作是空的**，因为回调路径上我们不知道用户当时选的是哪一条支付方式——那要读
// payments.payment_method_id，而支付单号在报文里、报文还没验签。适配器的 Verify 只需要
// 渠道侧的事实（config 里的密钥引用、mode），所以这个缺口不影响验签，也正因为如此验签可以
// 在碰报文之前完成——这是这个顺序的全部价值。
func methodFromChannel(record *repository.ChannelRecord) provider.Method {
	return provider.Method{
		ChannelID:     record.Channel.ID,
		ChannelCode:   record.Channel.Code,
		Provider:      record.Channel.Provider,
		Mode:          record.Channel.Mode,
		ChannelConfig: record.Config,
		SecretRef:     record.Channel.SecretRef,
	}
}
