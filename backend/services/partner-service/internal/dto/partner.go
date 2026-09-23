// Package dto 是 partner-service 的对外形状：HTTP 请求体与响应体。
//
// 每个服务各自一份（json tag 就是对外契约），所以它不抽到 platform——那边不该认识某个服务
// 的字段名。命名一律 camelCase，与后台其它模块一致：admin-web 的表单与表格直接吃这些字段。
//
// ⚠️ 响应形状里**没有任何一个字段叫 secret 的明文**。APIKeyIssued.Secret 是唯一的例外，
// 而它只出现在「新建密钥」这一个响应上（见那一处的说明）。
package dto

import "time"

// 分页参数用 platform/api 的默认值与上限（20 / 200），不在这里另起一套：后台所有列表的
// 分页语义必须完全一致，多一处默认值就多一处「这一页怎么只显示 10 条」的解释。

// PartnerQuery 是合作方列表的筛选条件。
type PartnerQuery struct {
	// Keyword 同时匹配编码与名称（模糊、不区分大小写）。一个框搜两个字段是有意的：
	// 运营手上要么是编码（对接方报过来的），要么是名字（他自己起的），让他先选搜哪个字段
	// 只是多一步。
	Keyword  string
	Status   string
	Page     int
	PageSize int
}

// PartnerInput 是新建 / 修改合作方的请求体。
//
// PUT 是整份覆盖（与后台其它模块同一种语义）：没带的字段就是清空。所以这里的每个字段都
// 必须能被前端表单填出来，否则覆盖语义会静默丢掉它。
type PartnerInput struct {
	// Code 是合作方的稳定标识，建好之后不可修改（UPDATE 语句里没有这一列）。
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	ContactEmail string `json:"contactEmail"`
	Description  string `json:"description"`
	// ExpiresAt 是 RFC3339 字符串，空表示不过期。用字符串而不是 time.Time：前端 picker 清空
	// 时发的是 ""，而 time.Time 收到 "" 会直接解析失败。
	ExpiresAt *string `json:"expiresAt"`
}

// APIKeyInput 是新建 / 修改密钥的请求体。
//
// 这里**没有 secret 字段**，而且不是因为「读接口不回显」那条规矩：签名密钥从来不由调用方
// 提供，它由本服务生成（见 partnerkey.Issue）。让调用方传一个密钥进来的后果是它自己能挑
// 一个弱密钥，而我们没有任何办法看出那是一个弱密钥。
type APIKeyInput struct {
	Name string `json:"name"`
	// ExpiresAt 同上，RFC3339 或空。
	ExpiresAt *string `json:"expiresAt"`
	// IPWhitelist 是来源白名单，接受 CIDR 与裸地址。**空数组表示不限制**（不是全拒），
	// 见 migrations/partner 上那一列的注释。
	IPWhitelist []string `json:"ipWhitelist"`
	// RateLimitPerMinute 为 0（没带）时用 ingress 的默认值。库上有 CHECK (> 0)，所以负数是
	// 服务的校验错误，不是「不限流」。
	RateLimitPerMinute int `json:"rateLimitPerMinute"`
}

// StatusInput 是启停请求体，三处（合作方、密钥）共用。
type StatusInput struct {
	Status string `json:"status"`
}

// CallLogQuery 是调用日志列表的筛选条件。
type CallLogQuery struct {
	PartnerID  string
	APIKeyID   string
	ErrorCode  string
	StatusCode int
	// From / To 是 created_at 的闭区间两端，nil 表示不筛这一端。
	From     *time.Time
	To       *time.Time
	Page     int
	PageSize int
}

// PartnerItem 是列表与详情共用的那一行。
type PartnerItem struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	ContactEmail string `json:"contactEmail"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	// ExpiresAt 为 null 表示不过期。前端据此显示「长期有效」而不是一个空日期。
	ExpiresAt *time.Time `json:"expiresAt"`
	// KeyCount 是名下密钥条数（含已停用的）。
	//
	// 列表页要显示它，是因为「这家合作方一把钥匙都没有」与「他有三把」在排查时是完全不同的
	// 两个处境，而它们只能靠这个数分开。含已停用的是有意的：停用一把之后总数不变，运营才
	// 知道那一把还在（能重新启用），而不是「不见了」。
	KeyCount  int       `json:"keyCount"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// APIKeyItem 是密钥列表与详情的那一行，**只有掩码，没有明文**。
type APIKeyItem struct {
	ID string `json:"id"`
	// PartnerID 用于列表页上的归属列（调用日志那一页要按合作方筛）。
	PartnerID string `json:"partnerId"`
	Name      string `json:"name"`
	// APIKeyMask / SecretMask 是「前 4 + … + 后 4」。库里有这两列而不是在这里现算，是为了
	// 让读路径**一次解密都不做**——它手上根本没有明文可算（见 partnerkey 的包注释）。
	APIKeyMask string     `json:"apiKeyMask"`
	SecretMask string     `json:"secretMask"`
	Status     string     `json:"status"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	// IPWhitelist 空数组表示不限制来源。发给前端的是原样的字符串（那个 CIDR 是运营自己填的，
	// 回显成归一化后的形状会让「我填的和他显示的不一样」变成一次多余的工单）。
	IPWhitelist        []string   `json:"ipWhitelist"`
	RateLimitPerMinute int        `json:"rateLimitPerMinute"`
	LastUsedAt         *time.Time `json:"lastUsedAt"`
	CallCount          int64      `json:"callCount"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// APIKeyIssued 是**签发**响应，全仓库唯一一处会把明文密钥发出去的地方。
//
// 它内嵌 APIKeyItem 而不是另起一套字段：列表里那一行与这里的这一行必须是同一份事实的两面
// （掩码、状态、白名单都一样），分开写迟早会出现「新建之后响应里的掩码与列表里的对不上」。
//
// APIKey 与 Secret 是明文的两个值，而 Secret **只在这一刻存在**：库里存的是密文信封，读接口
// 一律只回掩码。运营刷新页面之后就拿不到了——这是设计，不是缺陷：要换一把就重新签发。
type APIKeyIssued struct {
	APIKeyItem
	// APIKey 是 X-API-Key 的值。它本来是公开标识（明文入库、明文可读），这里给出来只是
	// 省掉运营去列表里复制一次。
	APIKey string `json:"apiKey"`
	// Secret 是签名密钥。**只在这一次响应里出现**，之后任何接口都只回 SecretMask。
	Secret string `json:"secret"`
}

// CallLogItem 是一次调用的记录。
//
// 报文（requestBody / responseBody）直接给后台看：这是这张表存在的全部理由——合作方说
// 「我发了但你们没收到」时，能对上的只有这一行。两段都已被截断（8 KiB，见 ingress 的
// maxLoggedBodyBytes），截断处带显式标记。
type CallLogItem struct {
	ID int64 `json:"id"`
	// PartnerID 与 PartnerName 一起给：id 是外键，名字要 JOIN 出来。认不出调用方的那几条
	// （密钥查不到、头都没带）partnerId 是全零 UUID、partnerName 是空串——它们仍然要留在
	// 列表里，因为那正是「有人在试」的痕迹。
	PartnerID    string `json:"partnerId"`
	PartnerName  string `json:"partnerName"`
	APIKeyID     string `json:"apiKeyId"`
	APIKeyMask   string `json:"apiKeyMask"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Query        string `json:"query"`
	RequestIP    string `json:"requestIp"`
	RequestBody  string `json:"requestBody"`
	ResponseBody string `json:"responseBody"`
	StatusCode   int    `json:"statusCode"`
	DurationMs   int64  `json:"durationMs"`
	// ErrorCode 是我们内部的失败原因（SIGNATURE_MISMATCH / NONCE_REPLAYED / …）。它在这里
	// 给运营看，**不出现在调用方的任何响应里**（防枚举，见 ingress 的包注释）。
	ErrorCode string    `json:"errorCode"`
	CreatedAt time.Time `json:"createdAt"`
}
