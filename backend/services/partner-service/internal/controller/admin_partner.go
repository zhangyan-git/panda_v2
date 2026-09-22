package controller

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
)

// 后台开放平台域的**写与读**接口：合作方的增改启停、密钥的签发与启停、调用日志的查询。
//
// # 为什么读与写在一个控制器里
//
// payment-service 把只读（支付单）与可配（支付方式/渠道）分成两个控制器，因为那是两枚权限码、
// 两批接口。本域的两枚码（partner:read / partner:manage）落**在方法上**而不是在资源上：同一个
// `/v1/admin/partners/{id}` 上 GET 要 read、PUT 要 manage（见 routes/admin.go 的 methodRoute）。
// 于是「读的控制器」与「写的控制器」在这里没有对应的边界，只有一堆共用同一个路径前缀的 handler。
//
// # 这一层只做三件事
//
// 解析请求（路径参数、请求体）、调 service、把错误翻成给用户看的中文。**一条业务判断都不做**
// ——编码形状、白名单解析、额度归一都在 service / repository 里（见 service 的包注释）。
//
// # 明文密钥只从这一个文件出去一次
//
// issueKey 把 service.IssuedAPIKey.Secret 放进响应体，这是全仓库唯一一处。它旁边那句注释
// 不是装饰：改这个 handler 的人要先想一遍「这个值还要不要出现在别的地方」。
const (
	adminPartnersPath = "/v1/admin/partners"
	// adminCallLogsPath 是调用日志那一条（它是一个平铺的集合，不嵌在合作方下面：运营排查时
	// 手上往往只有一个 api_key 的掩码或一个时间点，让他先确定是哪个合作方再查日志是反过来的）。
	adminCallLogsPath = "/v1/admin/partner-call-logs"
	// contentSuffix / statusSuffix 是密钥路径上的两截尾巴：
	// `/partners/{id}/keys`、`/partners/{id}/keys/{keyId}`、`/partners/{id}/keys/{keyId}/status`。
	keysSegment    = "keys"
	statusSuffix   = "/status"
	maxPathDepth   = 4 // {id}/keys/{keyId}/status 拆开是四段，再多一律 404
	partnerSegment = 0
)

// AdminPartnerController 是后台开放平台域的控制器。
type AdminPartnerController struct {
	partners *service.AdminService
}

func NewAdminPartnerController(partners *service.AdminService) *AdminPartnerController {
	return &AdminPartnerController{partners: partners}
}

// Partners 分发 /v1/admin/partners 这一棵子树。
//
// 路径参数是**手写裁剪**出来的（与 payment-service 的 configPathID 同款）：路径模板写在 routes
// 里只是为了注册与指标里的标签好看，真正被解析的是 r.URL.Path。
//
// 形状不对（多一段、少一段、把 status 写成 statuses）一律 404，**不把它当成 id 去查库**：那
// 只会让「有人拼错了 URL」看起来像「这条记录查无此条」，而这两个结论的排查方向完全相反。
func (c *AdminPartnerController) Partners(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminPartnersPath)
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			c.listPartners(w, r)
		case http.MethodPost:
			c.createPartner(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) > maxPathDepth {
		http.NotFound(w, r)
		return
	}
	partnerID, ok := c.partnerID(w, parts[partnerSegment])
	if !ok {
		return
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			c.getPartner(w, r, partnerID)
		case http.MethodPut:
			c.updatePartner(w, r, partnerID)
		default:
			http.NotFound(w, r)
		}
	case len(parts) == 2 && parts[1] == keysSegment:
		switch r.Method {
		case http.MethodGet:
			c.listKeys(w, r, partnerID)
		case http.MethodPost:
			c.issueKey(w, r, partnerID)
		default:
			http.NotFound(w, r)
		}
	case len(parts) == 3 && parts[1] == keysSegment:
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		keyID, ok := c.keyID(w, parts[2])
		if !ok {
			return
		}
		c.updateKey(w, r, partnerID, keyID)
	case len(parts) == 4 && parts[1] == keysSegment && parts[3] == "status":
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		keyID, ok := c.keyID(w, parts[2])
		if !ok {
			return
		}
		c.setKeyStatus(w, r, partnerID, keyID)
	case len(parts) == 2 && parts[1] == "status":
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		c.setPartnerStatus(w, r, partnerID)
	default:
		http.NotFound(w, r)
	}
}

// CallLogs 处理 GET /v1/admin/partner-call-logs。
//
// 只有 GET：这张表是**只增**的（见 model.CallLog 的说明），本服务没有任何一条写它的路径。
// 写方法落在这里的 http.NotFound 上，不是 405、更不是 200。
func (c *AdminPartnerController) CallLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if rest := restOf(r.URL.Path, adminCallLogsPath); rest != "" {
		http.NotFound(w, r)
		return
	}
	page, pageSize, ok := pageParams(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	from, to, ok := timeRange(query.Get("from"), query.Get("to"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidDate)
		return
	}
	partnerID, ok := parseOptionalUUID(query.Get("partnerId"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidPathID)
		return
	}
	apiKeyID, ok := parseOptionalUUID(query.Get("apiKeyId"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidPathID)
		return
	}
	statusCode, ok := parseOptionalInt(query.Get("statusCode"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidStatusCode)
		return
	}
	// errorCode 的词表校验在 service 里（ingress.ErrorCodes()），这里只 trim：它是一条业务
	// 规则，而且校验失败时要返回一个能被对照表翻成中文的错误值。
	rows, total, err := c.partners.ListCallLogs(r.Context(), dto.CallLogQuery{
		PartnerID:  partnerID,
		APIKeyID:   apiKeyID,
		ErrorCode:  strings.TrimSpace(query.Get("errorCode")),
		StatusCode: statusCode,
		From:       from,
		To:         to,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to list partner call logs")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    callLogItems(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// ============================================================
// 合作方
// ============================================================

func (c *AdminPartnerController) listPartners(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	rows, total, err := c.partners.ListPartners(r.Context(), dto.PartnerQuery{
		Keyword:  query.Get("keyword"),
		Status:   query.Get("status"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to list partners")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    partnerItems(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminPartnerController) getPartner(w http.ResponseWriter, r *http.Request, id string) {
	partner, err := c.partners.GetPartner(r.Context(), id)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to read partner")
		return
	}
	api.Success(w, partnerItemFromModel(partner))
}

// createPartner 处理 POST /v1/admin/partners。
//
// 响应是 201（api.Created），与 payment 的配置写接口一致：新建这一类资源用 201 而不是 200，
// 前端据此区分「建好了」与「读到了」。
func (c *AdminPartnerController) createPartner(w http.ResponseWriter, r *http.Request) {
	operator, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var in dto.PartnerInput
	if !decodeJSON(w, r, &in) {
		return
	}
	created, err := c.partners.CreatePartner(r.Context(), in, operator)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to create partner")
		return
	}
	api.Created(w, partnerItemFromModel(created))
}

// updatePartner 处理 PUT /v1/admin/partners/{id}。PUT 是**整份覆盖**：没带的字段就是清空。
func (c *AdminPartnerController) updatePartner(w http.ResponseWriter, r *http.Request, id string) {
	var in dto.PartnerInput
	if !decodeJSON(w, r, &in) {
		return
	}
	updated, err := c.partners.UpdatePartner(r.Context(), id, in)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to update partner")
		return
	}
	api.Success(w, partnerItemFromModel(updated))
}

// setPartnerStatus 处理 PATCH /v1/admin/partners/{id}/status。
//
// PATCH 只改状态（另一种写语义）：停用会让这家合作方名下**所有**密钥当场失效，它不该与「顺手
// 改个联系电话」共用一条路径、共用一次提交。请求体里带别的字段会被 decodeJSON 挡下（字段名不
// 认识），这也正是这个 body 只有一个字段的原因。
func (c *AdminPartnerController) setPartnerStatus(w http.ResponseWriter, r *http.Request, id string) {
	in, ok := decodeStatus(w, r)
	if !ok {
		return
	}
	updated, err := c.partners.SetPartnerStatus(r.Context(), id, in.Status)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to update partner status")
		return
	}
	api.Success(w, partnerItemFromModel(updated))
}

// ============================================================
// 密钥
// ============================================================

// listKeys 读一个合作方名下的全部密钥。**只有掩码**，没有明文（库上也没有）。
//
// 不分页，与仓储那条查询一致：一把钥匙是发给一家公司的一个集成用的，现实里是几把。
func (c *AdminPartnerController) listKeys(w http.ResponseWriter, r *http.Request, partnerID string) {
	keys, err := c.partners.ListAPIKeys(r.Context(), partnerID)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to list partner api keys")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    apiKeyItems(keys),
		Total:    int64(len(keys)),
		Page:     1,
		PageSize: len(keys),
	})
}

// issueKey 处理 POST /v1/admin/partners/{id}/keys。
//
// # 这里的 Secret 是全仓库唯一一次明文出库
//
// service.IssuedAPIKey.Secret 是明文签名密钥，它只在这一个响应里出现。库上存的是密文信封，
// 任何一次读（列表、详情、审计）拿到的都只有掩码——所以运营刷新页面之后就拿不到了，这是设计。
// 改这个函数的人要先想一遍：还有没有别的地方要把这个值发出去。
//
// 响应用 201：一把新的凭据被创建了，而且它是**一次性**的——前端据此弹那个「只显示一次」的
// 对话框。
func (c *AdminPartnerController) issueKey(w http.ResponseWriter, r *http.Request, partnerID string) {
	operator, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var in dto.APIKeyInput
	if !decodeJSON(w, r, &in) {
		return
	}
	issued, err := c.partners.IssueAPIKey(r.Context(), partnerID, in, operator)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to issue partner api key")
		return
	}
	api.Created(w, dto.APIKeyIssued{
		APIKeyItem: apiKeyItem(issued.Key),
		APIKey:     issued.Key.APIKey,
		Secret:     issued.Secret,
	})
}

// updateKey 处理 PUT /v1/admin/partners/{id}/keys/{keyId}。
//
// 改的是可改的那几列（备注、有效期、白名单、额度），**改不了密钥本身**：请求体里根本没有
// secret 字段（见 dto.APIKeyInput），而仓库的 UPDATE 语句里也没有那两列。
func (c *AdminPartnerController) updateKey(w http.ResponseWriter, r *http.Request, partnerID, keyID string) {
	var in dto.APIKeyInput
	if !decodeJSON(w, r, &in) {
		return
	}
	updated, err := c.partners.UpdateAPIKey(r.Context(), partnerID, keyID, in)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to update partner api key")
		return
	}
	api.Success(w, apiKeyItem(updated))
}

// setKeyStatus 处理 PATCH /v1/admin/partners/{id}/keys/{keyId}/status。
//
// **下一个请求就生效**（验签那条路每个请求现查这一行）。怀疑泄露时，停用比「通知对接方改代码」
// 快得多，所以这条路径要能在后台一点就通。
func (c *AdminPartnerController) setKeyStatus(w http.ResponseWriter, r *http.Request, partnerID, keyID string) {
	in, ok := decodeStatus(w, r)
	if !ok {
		return
	}
	updated, err := c.partners.SetAPIKeyStatus(r.Context(), partnerID, keyID, in.Status)
	if err != nil {
		writeAdminPartnerError(w, r, err, "failed to update partner api key status")
		return
	}
	api.Success(w, apiKeyItem(updated))
}

// ============================================================
// 请求解析
// ============================================================

// partnerID / keyID 把路径里那一段转成 uuid。
//
// 在进 SQL 之前挡下来，理由同 msgInvalidPathID：不是 uuid 的串会让 PostgreSQL 解析参数时报
// 22P02，于是「复制粘贴少了一截的 id」会以「服务出错」的面目出现。
func (c *AdminPartnerController) partnerID(w http.ResponseWriter, segment string) (string, bool) {
	return parsePathUUID(w, segment, msgInvalidPartnerID)
}

func (c *AdminPartnerController) keyID(w http.ResponseWriter, segment string) (string, bool) {
	return parsePathUUID(w, segment, msgInvalidKeyID)
}

func parsePathUUID(w http.ResponseWriter, segment, message string) (string, bool) {
	parsed, err := uuid.Parse(segment)
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return "", false
	}
	return parsed.String(), true
}

// decodeStatus 读 PATCH 那个只有 status 的请求体。
func decodeStatus(w http.ResponseWriter, r *http.Request) (dto.StatusInput, bool) {
	var in dto.StatusInput
	if !decodeJSON(w, r, &in) {
		return in, false
	}
	return in, true
}

// ============================================================
// 映射：模型 / 仓储行 → dto
// ============================================================

// **映射只做形状转换，不做判断**：凡是「如果 X 就填 Y」的写法都要停一下——那多半是业务规则，
// 它该在 service 里，而且该被测试。这里唯一的例外是 nil 保护（一行数据读不到时给一个空壳，
// 而不是 panic）。

func partnerItem(row *repository.PartnerListRow) dto.PartnerItem {
	if row == nil || row.Partner == nil {
		return dto.PartnerItem{}
	}
	item := partnerItemFromModel(row.Partner)
	item.KeyCount = int(row.KeyCount)
	return item
}

func partnerItems(rows []*repository.PartnerListRow) []dto.PartnerItem {
	items := make([]dto.PartnerItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, partnerItem(row))
	}
	return items
}

// partnerItemFromModel 是单条映射，抽出来给写接口用（POST / PUT / PATCH 的响应与列表里那一行
// 必须完全同形），列表那边也只是把同一个函数套进循环——两份实现一定会在某一次加字段时分开。
//
// KeyCount 在**写路径**上留 0：create/update/status 的返回体里没有 JOIN 出来的密钥条数（那是
// 列表那条查询才付的代价），而前端在两个写操作之后本来就会重取列表。不在这里补一次查询：为了
// 一个列表页才显示的字段去多查一次库，是拿写路径的延迟换一个不成立的对称。
func partnerItemFromModel(partner *model.PartnerAccount) dto.PartnerItem {
	if partner == nil {
		return dto.PartnerItem{}
	}
	return dto.PartnerItem{
		ID:           partner.ID,
		Code:         partner.Code,
		Name:         partner.Name,
		ContactName:  partner.ContactName,
		ContactPhone: partner.ContactPhone,
		ContactEmail: partner.ContactEmail,
		Description:  partner.Description,
		Status:       string(partner.Status),
		ExpiresAt:    partner.ExpiresAt,
		CreatedAt:    partner.CreatedAt,
		UpdatedAt:    partner.UpdatedAt,
	}
}

// apiKeyItem 是密钥行的**唯一**映射，签发响应内嵌的就是它。
//
// **它只读掩码列，一次解密都不做**——本服务的读路径上根本没有明文可算（见 partnerkey 的包
// 注释）。APIKey 那一列（公开标识）出库是有意的：它是 X-API-Key 的值，本来就是明文标识。
func apiKeyItem(key *model.APIKey) dto.APIKeyItem {
	if key == nil {
		return dto.APIKeyItem{}
	}
	return dto.APIKeyItem{
		ID:                 key.ID,
		PartnerID:          key.PartnerID,
		Name:               key.Name,
		APIKeyMask:         key.APIKeyMask,
		SecretMask:         key.SecretMask,
		Status:             string(key.Status),
		ExpiresAt:          key.ExpiresAt,
		IPWhitelist:        whitelist(key.IPWhitelist),
		RateLimitPerMinute: key.RateLimitPerMinute,
		LastUsedAt:         key.LastUsedAt,
		CallCount:          key.CallCount,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
		// 注意：这里**没有** APIKey / Secret 两个字段可填——它们是 dto.APIKeyIssued 自己的，
		// 只能由 issueKey 那一个函数赋值。
	}
}

func apiKeyItems(keys []*model.APIKey) []dto.APIKeyItem {
	items := make([]dto.APIKeyItem, 0, len(keys))
	for _, key := range keys {
		items = append(items, apiKeyItem(key))
	}
	return items
}

// whitelist 把库里的 NULL/空数组统一成 `[]`。
//
// 前端拿到 null 会渲染成「—」（= 没配这条规则），而空数组的语义是「不限制来源」——两者在这
// 一列上恰好是同一个意思（库里空数组就是不限制），所以统一成空数组，让页面只处理一种形状。
func whitelist(entries []string) []string {
	if entries == nil {
		return []string{}
	}
	return entries
}

// callLogItems 只做形状转换。
//
// 报文（requestBody / responseBody）原样给后台：这张表存在的全部理由就是「合作方说他发了、
// 我们说他没发」时能对上这一行。两段都已被 ingress 截断（8 KiB，截断处带显式标记），这里
// 不再截一次——那会让页面上看到的截断点比实际存储的多一处，查起来更绕。
func callLogItems(rows []*repository.CallLogRow) []dto.CallLogItem {
	items := make([]dto.CallLogItem, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Log == nil {
			continue
		}
		log := row.Log
		items = append(items, dto.CallLogItem{
			ID:           log.ID,
			PartnerID:    log.PartnerID,
			PartnerName:  row.PartnerName,
			APIKeyID:     log.APIKeyID,
			APIKeyMask:   log.APIKeyMask,
			Method:       log.Method,
			Path:         log.Path,
			Query:        log.Query,
			RequestIP:    log.RequestIP,
			RequestBody:  log.RequestBody,
			ResponseBody: log.ResponseBody,
			StatusCode:   log.StatusCode,
			DurationMs:   log.DurationMS,
			ErrorCode:    log.ErrorCode,
			CreatedAt:    log.CreatedAt,
		})
	}
	return items
}

// ============================================================
// 错误 → 响应
// ============================================================

// 给用户看的中文句子。与 admin_response.go 里那批是同一套做法（那里是被两棵树共用的几句）。
const (
	// msgInvalidPartnerID / msgInvalidKeyID：路径里那一段不是 uuid。
	//
	// 分开两句话而不是共用一句「id 不是合法的 UUID」：密钥那条路径上有两个 uuid（合作方与密钥），
	// 一句笼统的话让运营不知道该改哪一个。
	msgInvalidPartnerID = "合作方 id 不是合法的 UUID"
	msgInvalidKeyID     = "密钥 id 不是合法的 UUID"
	// msgInvalidStatusCode：调用日志筛选里的 statusCode 不是一个整数。
	//
	// 与 admin_response.go 的 msgInvalidFilter 分开：那一条是枚举词表，这一条是数字形状——
	// 「abc」与「999」要收到不一样的话（后者是词表外的值，由 service 判）。
	msgInvalidStatusCode = "状态码必须是整数"
	// msgPartnerNotFound：这条合作方不存在。与「字段都是空的」严格分开。
	msgPartnerNotFound = "合作方不存在"
	// msgKeyNotFound：这把密钥不存在，**或者不属于路径上那个合作方**。
	//
	// 第二种情况不单独说：告诉调用方「这把钥匙属于别人」等于确认了那个 id 存在，而路径参数
	// 写错的人需要知道的是「这两个对不上」。
	msgKeyNotFound = "密钥不存在"
	// msgPartnerCodeTaken / msgKeyTaken：撞唯一键（409）。
	//
	// 409 而不是 400：请求本身是合法的，只是此刻与库里已有的那一行冲突。前端据此把用户引到
	// 那一行去，而不是让他怀疑自己填错了格式。
	msgPartnerCodeTaken = "该编码已被其他合作方使用"
	// msgKeyTaken：api_key 撞了。它是 32 位随机串，撞上的概率可以忽略——真撞上时的正确动作
	// 是重新签发一次，所以这句话要说出「重试」这层意思。
	msgKeyTaken = "密钥标识已被占用，请重新签发"
	// msgCodeImmutable：请求里的编码与库里那行不一致（编码建好之后不可修改）。
	msgCodeImmutable = "编码不可修改"
	// msgSecretUnavailable：签名密钥封不起来（主密钥缺失或不对）。
	//
	// 500 而不是 400：它是**我们这边**的故障，不是这次输入的问题。文案里点名「主密钥」是有意
	// 的——看到这句话的人（我们自己人）应当直接去看 PARTNER_SECRET_KEY 的配置，而不是去查
	// 这次提交的内容。
	msgSecretUnavailable = "签名密钥不可用，请检查服务的主密钥配置"

	// 下面这些是校验错误，与 service 的校验错误值一一对应（顺序也一致，便于对照）。漏一条
	// 测试会红（见 admin_partner_test.go）。
	msgCodeInvalid        = "编码只能用 2-64 位小写字母、数字或下划线，且以字母或数字开头"
	msgNameRequired       = "名称不能为空"
	msgExpiresAtInvalid   = "有效期必须是 RFC3339 时间，或者留空表示不过期"
	msgStatusInvalid      = "状态只能是启用或停用"
	msgRateLimitInvalid   = "每分钟调用额度必须是正整数"
	msgIPWhitelistInvalid = "来源白名单里有既不是 CIDR 也不是 IP 地址的条目"
	msgErrorCodeInvalid   = "失败原因不在可选范围内"
	msgStatusCodeInvalid  = "状态码必须在 100 到 599 之间"
	msgOperatorRequired   = "无法确定操作人，请重新登录后再试"
)

// adminPartnerMessages 是开放平台域写接口的错误对照表：错误值 → (状态码, 错误码, 中文句子)。
//
// 表驱动而不是 switch：这里二十来条错误，散成 case 之后「新加一条校验忘了配文案」的表现是它
// 掉进兜底，用户看到「服务暂时不可用」——一个看起来像故障的 500，而真相只是名字没填。表在这
// 里，缺一条测试就红。
//
// **顺序有关系**：命中第一条就返回。各条错误值之间不许有 errors.Is 上的包含关系（测试盯着
// 这一条——它们是各自独立的哨兵错误，没有包着谁的）。
//
// WriteAPIKey 那条路上 ErrPartnerNotFound 也会命中（INSERT 撞了外键）：路径上那个合作方不
// 存在时返回 404 是准确的——要操作的那个对象不在。
var adminPartnerMessages = []struct {
	err    error
	status int
	code   string
	msg    string
}{
	// —— 校验（400）——
	{service.ErrCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgCodeInvalid},
	{service.ErrNameRequired, http.StatusBadRequest, "INVALID_ARGUMENT", msgNameRequired},
	{service.ErrExpiresAtInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgExpiresAtInvalid},
	{service.ErrStatusInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusInvalid},
	{service.ErrRateLimitInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgRateLimitInvalid},
	{service.ErrIPWhitelistInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgIPWhitelistInvalid},
	{service.ErrErrorCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgErrorCodeInvalid},
	{service.ErrStatusCodeInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusCodeInvalid},

	// —— 入参指向的东西不在（404）——
	{repository.ErrPartnerNotFound, http.StatusNotFound, api.CodeNotFound, msgPartnerNotFound},
	{repository.ErrAPIKeyNotFound, http.StatusNotFound, api.CodeNotFound, msgKeyNotFound},

	// —— 撞唯一键（409）——
	{repository.ErrPartnerCodeTaken, http.StatusConflict, api.CodeConflict, msgPartnerCodeTaken},
	{repository.ErrAPIKeyTaken, http.StatusConflict, api.CodeConflict, msgKeyTaken},

	// —— 不可修改的列（400）——
	{repository.ErrPartnerCodeImmutable, http.StatusBadRequest, "INVALID_ARGUMENT", msgCodeImmutable},

	// —— 我们这边的故障（500）——
	//
	// ErrOperatorRequired 走到这里意味着**路由少挂了一层**（身份没进上下文），是一个装配错误：
	// 对用户说一句「重新登录」，日志里留下 operator is required。
	{service.ErrSecretUnavailable, http.StatusInternalServerError, api.CodeInternal, msgSecretUnavailable},
	{service.ErrOperatorRequired, http.StatusInternalServerError, api.CodeInternal, msgOperatorRequired},
}

// writeAdminPartnerError 按对照表把服务层的错误翻成响应。
//
// 兜底那一支**必须记日志**：它对用户说「服务暂时不可用」，只有日志里才有真正的原因。一个不记
// 日志的兜底会让「密钥签不出来」变成一条没有任何线索的报障。
func writeAdminPartnerError(w http.ResponseWriter, r *http.Request, err error, op string) {
	for _, entry := range adminPartnerMessages {
		if errors.Is(err, entry.err) {
			if entry.status >= http.StatusInternalServerError {
				// 5xx 那两条也要进日志：它们不是调用方的问题，只有日志里能看出是哪一次、
				// 哪一条路径上发生的。
				slog.ErrorContext(r.Context(), "partner admin request failed", "error", err, "op", op)
			}
			api.Error(w, entry.status, entry.code, entry.msg)
			return
		}
	}
	slog.ErrorContext(r.Context(), "partner admin request failed", "error", err, "op", op)
	api.Error(w, http.StatusInternalServerError, api.CodeInternal, msgGeneric)
}
