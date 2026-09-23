// Package dto 是支付服务对外的数据形状：后台只读接口的请求与响应。
//
// 分成一个包而不是散在 controller 里，理由与 membership-service / lottery-service 的 dto 相同：
// 这些结构上的 json tag **不是我们自己说了算的**——admin-web 的 services/payment.ts 与
// ProTable 的 dataIndex 是逐字照抄它们写的，改一个 tag 要三处同批改（tag、前端字段、前端标签表）。
// 放在一处，是为了让那份契约有一个可以被指着的东西。
//
// 这个包里有两类东西：下面这几个 Query 是**读**的筛选条件（走 query string，由 controller
// 解析），write.go 里那几个 Input 是**写**的请求体（走 JSON body）。两边分文件，是因为它们的
// 变更理由不同：筛选条件只跟着页面上的筛选栏走，请求体跟着表单走，而表单里少一个字段
// （比如 status）是有意为之的——理由写在 write.go 的头上。
package dto

import "time"

// MaxPageSize 是列表接口允许的最大 pageSize，与 platform/api.MaxPageSize 取值一致。
//
// admin-web 的分页组件按 FULL_PAGE_PARAMS 一次要 200 条（下拉、导出、跨页全选用它），接口的
// 取值比它小的话，前端会拿到一页被悄悄截断的数据——看起来像「就是这么多」。
// coffee-machine-service 就吃过这个亏（那里原来是 100）。
const MaxPageSize = 200

// DefaultPageSize 是没给 pageSize 时的每页条数。
const DefaultPageSize = 20

// PaymentQuery 是支付单列表的筛选条件。
//
// 单号两个字段是**模糊匹配**（ILIKE），不是等值。这是有意的取舍，写在这里免得被当成疏忽：
// 运维手上多半只有单号的一截（从订单页、从客服转来的截图、从用户念出来的后六位），
// 精确匹配会把「明明有这单却搜不到」变成日常。代价是这两个条件都吃不到
// payments_order_idx 与 payment_no 唯一键，退化成筛选后排序——与 status 筛选同一条账。
type PaymentQuery struct {
	// PaymentNo / OrderNo 模糊匹配，空表示不筛。
	PaymentNo string
	OrderNo   string
	// UserID 精确匹配：用户 id 是 uuid，来自系统之间传递，不存在「只记得一截」的场景，
	// 而 uuid 上的 ILIKE 会让索引彻底失效。
	UserID string
	// Status / MethodCode 先过枚举白名单再进 SQL，空表示不筛。
	//
	// MethodCode 筛的是 `payments.payment_method`（catalog 的 code，如 `ums_h5_alipay`），
	// 不再是那套已经退场的「出资渠道」词表——两者从前后台同一条单上有两个可筛的值，
	// 现在只剩这一个。
	Status     string
	MethodCode string
	// CreatedFrom / CreatedTo 是创建时间区间，闭区间，nil 表示这一端不限。
	CreatedFrom *time.Time
	CreatedTo   *time.Time

	Page     int
	PageSize int
}
