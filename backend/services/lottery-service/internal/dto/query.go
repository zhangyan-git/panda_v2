// Package dto 是抽奖服务的对外数据形状：HTTP 的请求 / 响应，以及发到 MQ 上的事件体。
//
// 分成一个包而不是散在 controller 里，理由与 payment-service 的 dto 相同：这些结构上
// 的 json tag **不是我们自己说了算的**——admin-web 的 services/lottery.ts 与 ProTable 的
// dataIndex 是逐字照抄它们写的，改一个 tag 要三处同批改（tag、前端字段、前端标签表）。
// 放在一处，是为了让那份契约有一个可以被指着的东西。
package dto

// MaxPageSize 是列表接口允许的最大 pageSize，与 platform/api.MaxPageSize 取值一致。
//
// admin-web 的分页组件按 FULL_PAGE_PARAMS 一次要 200 条（下拉、导出、跨页全选用它），
// 接口的取值比它小的话，前端会拿到一页被悄悄截断的数据——看起来像「就是这么多」。
// coffee-machine-service 就吃过这个亏（那里原来是 100）。这里**不是**「顺手写大一点」，
// 是与全仓那一个数对齐。
const MaxPageSize = 200

// defaultPageSize 是没给 pageSize 时的每页条数，与 account-service 一致。
const DefaultPageSize = 20

// ActivationQuery 是开通列表的筛选条件。
type ActivationQuery struct {
	// 门店 ID，精确匹配。
	LocationID string
	// enabled / disabled，空表示不筛。
	Status string
	// 门店名的模糊匹配（运营按店名找）。刻意不做成「模糊搜 location_id」——UUID 前缀
	// 匹配没有意义，想按 id 找就精确贴 id。
	Name string
	Page int
	// PageSize 见 MaxPageSize。
	PageSize int
}

// CampaignQuery 是活动列表的筛选条件。
type CampaignQuery struct {
	// 开通记录 / 门店 ID。按门店看这个店有哪些活动是后台最常走的一条路。
	ActivationID string
	LocationID   string
	// 咖啡机 ID。筛设备级活动用它。
	MachineID string
	// draft / enabled / paused / ended，空表示不筛。
	Status   string
	Name     string
	Page     int
	PageSize int
}

// RoundQuery 是期次列表的筛选条件。
type RoundQuery struct {
	CampaignID string
	// open / closed / drawn / cancelled，空表示不筛。
	Status   string
	Page     int
	PageSize int
}

// ParticipationQuery 是参与记录的筛选条件。
//
// 后台按 roundId / campaignId / userId 查，小程序那条路只允许按自己的 userId 查
// ——归属由令牌决定，不接受查询串里的 userId（见 controller 的 requireConsumer）。
type ParticipationQuery struct {
	RoundID    string
	CampaignID string
	UserID     string
	// pending / confirmed / failed / reversed，空表示不筛。
	Status   string
	Page     int
	PageSize int
}

// WinQuery 是中奖记录的筛选条件。
type WinQuery struct {
	RoundID    string
	CampaignID string
	UserID     string
	// pending / claimed / ... 空表示不筛。本轮只有 pending 会出现。
	Status string
	// 凭证号精确匹配。门店端将来的核销入口按它查，本轮只有后台用它。
	ClaimNo  string
	Page     int
	PageSize int
}
