package dto

import "encoding/json"

// 抽奖域发出去的事件类型。今天只有一个。
//
// EventType 就是 RabbitMQ 的 routing key（交易所按它路由），所以这个字符串是**路由
// 契约**，不是普通的常量：改一个字母，事件就会投进一个没人绑定的路由，然后被静默丢掉
// ——Publish 对没有绑定的路由返回成功（见 [[publish-treats-unroutable-as-success]]）。
// 本轮**没有消费者**（退款追回不做，见 service 包注释），它发出去是为了让下一轮
// 「退款成功 → 追回未开奖期次的参与」有一条现成的线可以接。
const (
	EventRoundDrawn = "lottery.round.drawn"
)

// EventVersion 是事件信封上的版本号，不是负载里的字段。
//
// 它进 messaging.Envelope.EventVersion。负载本身**不套信封**：payload 是裸的
// RoundDrawnPayload JSON，多加一层会让将来的消费者解不出来。
const EventVersion = "v1"

// RoundDrawnPayload 是一次开奖的事件体。
//
// 现在没有消费者，所以它的形状**只由我们自己说了算**——但正因为如此，这里更要把话说
// 清楚：下游（将来的退款追回、数据看板）只能靠这些字段判断「这一期结束了」。
// 完整的名单不在事件里，在 lottery_wins：一个百人期的名单塞进消息体会让一条消息上百 KB，
// 而需要它的人拿着 drawId / roundId 一查就有。
type RoundDrawnPayload struct {
	DrawID     string `json:"drawId"`
	RoundID    string `json:"roundId"`
	RoundNo    string `json:"roundNo"`
	CampaignID string `json:"campaignId"`
	// auto / manual。
	Mode string `json:"mode"`
	// threshold / deadline / manual。
	Trigger          string `json:"trigger"`
	Seed             string `json:"seed"`
	Algorithm        string `json:"algorithm"`
	ParticipantCount int32  `json:"participantCount"`
	WinnerCount      int32  `json:"winnerCount"`
	// DrawnAtUnix 用开奖时刻的 Unix 秒。**不用消费者收到的时刻**：补投或重放一条旧事件
	// 时，NOW() 会把「昨晚开的奖」记成「刚才」。
	DrawnAtUnix int64 `json:"drawnAtUnix"`
	// OperatorID 是人工开奖的操作员，自动开奖为空。**必须是指针**：写成 string 会在
	// 自动开奖时发一个 ""，而对面收到的是一个「给了但为空」的值（与 payment 的
	// AccountEntryID 同一条理由）。
	OperatorID *string `json:"operatorId"`
}

// MarshalRoundDrawn 把事件体编码成 outbox 的 payload。
//
// 抽成一个函数是为了让「发出去的那份字节」有一个可以被测试指着的东西：仓储的
// appendOutbox 只负责落库，形状的错要在这一层就能被发现（round_event_test.go）。
func MarshalRoundDrawn(payload RoundDrawnPayload) ([]byte, error) {
	return json.Marshal(payload)
}
