package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// ErrReturnEmpty：这条回跳连查询串都没有。
//
// 与 ErrNotificationEmpty 分开而不是共用：一个是「POST 上来的报文是空的」，一个是「GET 上来
// 连一个参数都没带」。它们要修的东西不同——前者看渠道为什么发了空报文，后者看「谁在直接打
// 这个地址」，因为一个从收银台回来的浏览器不可能不带任何参数。
var ErrReturnEmpty = errors.New("result-page return carries no query parameters")

// ReturnRequest 是一次结果页回跳的输入。
//
// 它比 CallbackRequest **少一个 Body、多一个 Query**，这不是省事，是这一路的形状：回跳是
// 浏览器发起的 GET，渠道把支付结果拼在查询串里，**没有报文体**。把它塞进 Body 也能跑通，
// 但那会让「Body 是什么」变成一个按方法而异的事实（见 provider.NotificationRequest.Query）。
type ReturnRequest struct {
	// ChannelCode 是路径里那一段，用来查渠道配置。
	ChannelCode string
	// Query 是原样的查询串。**不做任何规范化**：验签盖的就是渠道拼出来的那串字符，重排一次
	// 参数顺序就会让签名对不上，而那种失败看起来像「渠道的签名算错了」。
	Query   url.Values
	Headers http.Header
	// RequestPath 是这条请求被投递到的路径。有的协议族把它签进待签串（见
	// provider.NotificationRequest.RequestPath），所以它与 Query 一样属于验签的输入。
	RequestPath string
}

// ReturnResult 是一次回跳的处置结果，控制器拿它决定回给浏览器什么。
type ReturnResult struct {
	// PaymentNo 是验签通过的报文里带的那一单。**它已经过验签**，但仍然只是一段被渠道
	// 写进 URL 的文本——所以它唯一的用途是把用户送去结果页，不参与任何查询或状态变更。
	PaymentNo string
	// RedirectURL 是验签通过之后要把浏览器送去的地址；**空串表示没配结果页**，控制器据此
	// 回 200 而不是 302（见 Options.ReturnPageURL）。
	RedirectURL string
}

// HandleReturn 处理一次结果页回跳。
//
// # 这条路径一行都不写库
//
// 这是它与 HandleNotification 最要紧的差别，也是它敢这么短的唯一原因。用户被渠道送回
// returnUrl 时，**钱到没到早已由支付结果通知或查单决定过了**（规范原文 §3 对这一路的描述
// 只有「验签 → 展示」）。所以这里：
//
//   - 不落 payment_notifications。那一列的语义是「渠道对这笔支付下过一个结论」，而回跳不带
//     任何新结论；记进去会让一条「用户点了完成」在库里长得像一条收款事实。验签失败时也不记
//     ——那条拒绝记录的形状（body_sha256、headers）都是为 POST 报文设计的，这里既没有体、
//     也没有 payment_no 可挂。
//   - 不改支付状态。不是靠某一行判据做到的，而是**结构上做不到**：这个函数里没有任何一个
//     写方法，连 repository 的写接口都没有出现在它的依赖里（PaymentService 上的 repository
//     字段是读接口与写接口的合集，但这条路一个都没调）。
//   - 不查支付单。查了也没用（不改状态），却会多出一次按用户可控输入去查库的动作。
//
// # 顺序与回调那边逐字相同
//
// 查渠道 → 验签 → 才读报文里的字段。channelCode 从路径来（渠道自己拼的回跳地址），
// paymentNo 从**验签之后**的报文来。在验签之前碰任何一个字段，等于让攻击者决定我们去看
// 哪一单。
//
// # 验签失败不写任何东西，只记日志
//
// 与回调那条路的差别要说清楚：回调验签失败会落一行 payment_notifications(status=failed)，
// 因为渠道会重投、运维需要看到「这条我们没认下来」。回跳不会重投——用户看一次就走了——
// 所以落库的收益只剩「有人拿刀戳这个地址时留下痕迹」，而那件事 access log 已经在做。
func (s *PaymentService) HandleReturn(ctx context.Context, in ReturnRequest) (*ReturnResult, error) {
	if strings.TrimSpace(in.ChannelCode) == "" {
		return nil, ErrChannelCodeRequired
	}
	if len(in.Query) == 0 {
		return nil, ErrReturnEmpty
	}

	channel, err := s.catalog.Channel(in.ChannelCode)
	if err != nil {
		return nil, err
	}
	if channel == nil {
		return nil, fmt.Errorf("%w: %s", ErrChannelNotFound, in.ChannelCode)
	}
	adapter, err := s.providers.Lookup(channel.Provider)
	if err != nil {
		return nil, err
	}

	method := methodFromChannel(channel)
	notification, verifyErr := adapter.Verify(ctx, provider.NotificationRequest{
		ChannelCode: channel.Code,
		// Body 留空，参数走 Query，见 provider.NotificationRequest.Query。
		Headers: in.Headers,
		Query:   in.Query,
		// 方法写死 GET 而不是从请求抄：这条路**就是** GET（控制器已经按方法分派过，
		// 见 ReturnController.Return）。适配器按这个字段决定用哪一套验签，从请求抄一遍
		// 只是把一次判断做两遍，而且两处将来可能不一致。
		HTTPMethod:  http.MethodGet,
		RequestPath: in.RequestPath,
		Method:      method,
		Secrets:     resolveSecrets(s.resolveSecret, channel, adapter, method),
	})
	if verifyErr != nil {
		slog.WarnContext(ctx, "refused a result-page return",
			"channel", channel.Code, "error", verifyErr)
		return nil, verifyErr
	}

	// 一道结构性的护栏：回跳**不可能**是一个结算事件。适配器若把这条 GET 归一化成了
	// succeeded / failed，那说明它把回跳接到了回调那条归一化路径上——那时这次跳转携带的
	// 就不是「展示」而是「结论」，而我们正要拿它去给用户显示结果页。
	//
	// 拒掉而不是照常展示：展示一个由结算事件驱动的结果页，等于在一个我们自己都说不清
	// 语义的输入上放行。这条分支今天不可达（ums 的 verifyReturn 只产出 "return"），
	// 留着是为了让下一个协议族接回跳时**接错会被当场打回**，而不是安静地多一条结算入口。
	if notification.EventType == provider.EventSucceeded || notification.EventType == provider.EventFailed {
		return nil, fmt.Errorf("%w: channel %q: a result-page return was normalized as a settlement event %q",
			provider.ErrInvalidNotification, channel.Code, notification.EventType)
	}

	return &ReturnResult{
		PaymentNo:   notification.PaymentNo,
		RedirectURL: s.returnPageURL(notification.PaymentNo),
	}, nil
}
