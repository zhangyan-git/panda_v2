package controller

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
)

// 这个文件是**开放接口**那一棵树的接入层。它只被 ingress.Guard 包着的那几条路由挂上去（见
// routes/openapi.go），所以在这里能拿到身份（ingress.CallerFrom）就意味着验签、时间窗、
// nonce、启停、过期、白名单、限流七道全过了——包括**设备刷卡回执**那一条写路径（见
// CreateDeviceOrder）：它能走到这里，就说明这条报文是验过签的。
//
// # 答复是英文的
//
// 与后台树相反，这里的中文会给错人：读者是外部系统的开发者，他手上是一份英文（或中英对照）
// 的对接文档，而**网关那一层的拒绝响应本来就是英文的**（见 ingress 的 unauthorizedBody）——
// 同一个接口上先出现一句英文 401、再出现一句中文 400，对接方会以为撞上了两个不同的系统。
//
// # 一句话都不能多说
//
// 这里的每一句话都会到达一个**没有经过我们认证**的调用方（他有一把有效密钥，但那是合作方
// 的机器，不是我们的人）。所以：
//
//   - 不复述下游的错误（会员服务说「这个 user 不存在」我们不转述，只回我们自己的说法）；
//   - 不区分「这个人不是会员」与「会员服务没答上来」之外的任何细分（前者的答案是
//     active=false，根本不该走到错误分支）；
//   - 500 那一支对外的句子永远是 internal error：真正的原因进日志（slog），因为一个把内部
//     错误串透出去的接口是**枚举**的入口——「哪个参数先生效」这类问题只能靠错误串的差别回答。
//
// 这也是为什么拒绝路径（没带凭据、签名错、限流）全都答同一句 unauthorized：见 ingress 的
// 包注释，那是本项目里最重要的一条对外约定。

// 这一棵树上今天三条路径。三个常量放在一起，是因为它们的读者是同一批人（谁在改开放接口就要
// 同时看到「这棵树上还有两条会写数据的路」）。
//
// 与网关那条路由（internal/proxy/proxy.go 的 /v1/openapi）**不是同一个串**：网关按前缀转发，
// 这里按完整路径匹配。路径模板写在 routes 里只是为了注册与指标里的标签好看，真正被解析的是
// r.URL.Path。
const (
	// openAPIEntitlementPath 是查会员权益那一条的路径（GET，只读）。
	openAPIEntitlementPath = "/v1/openapi/member-price-entitlement"
	// openAPIDeviceOrderPath 是设备刷卡回执那一条的路径（POST，写：钱已经在机器上收过了）。
	//
	// 这个串必须与网关那边的注释、compose 里的说明、以及对接文档逐字一致
	// （/v1/openapi/device/sync-order）。网关**认的是前缀**（/v1/openapi），所以这里改名网关
	// 照样转得过来——错法因此是「网关转得过来、这里 404」，而不是任何一处报错。真正卡住这件事
	// 的只有对接方手上那份文档。
	openAPIDeviceOrderPath = "/v1/openapi/device/sync-order"
	// openAPIDevicePickupPath 是取货码那一条的路径（POST，写：**我们从设备余额里扣钱**）。
	//
	// 逐字一致的要求与上一条相同（/v1/openapi/device/pickup）。它与上一条**不是同一条路**，
	// 尽管名字像：sync-order 记的是「机器上已经收过钱的既成事实」，这一条是「让机器上那个人
	// 敲一次码、钱从我们自己账上扣」。两条路的报文形状不同（这一条没有金额、多一个取货码），
	// 合并成一条会让两个含义在一个 handler 里打架。
	openAPIDevicePickupPath = "/v1/openapi/device/pickup"
)

// 给合作方看的句子。全英文，理由见文件头。
const (
	// msgOpenAPIInvalidUserID：userId 不是 uuid，或者根本没带。
	//
	// 这句话可以说得很具体：它回应的是一次**已经验签通过**的请求，说话的对象是那个拿着有效
	// 密钥的合作方，告诉他「你的参数写错了」不泄漏任何东西。
	msgOpenAPIInvalidUserID = "userId must be a UUID"
	// msgOpenAPIUnavailable：会员服务没答上来。可以重试（幂等查询），所以不说「失败」。
	msgOpenAPIUnavailable = "membership service is unavailable"
	// msgOpenAPIInternal：兜底。与所有兜底一样，真正的原因只在日志里。
	msgOpenAPIInternal = "internal error"

	// —— 下面几句是设备刷卡回执那一条（POST，见 CreateDeviceOrder）——
	//
	// msgOpenAPIMethodNotAllowed：这条路径上只有 POST。
	msgOpenAPIMethodNotAllowed = "this path only accepts POST"
	// msgOpenAPIBodyEmpty / msgOpenAPIBodyInvalid：报文没发 / 发错了。
	//
	// 两句分开与后台树同一条理由（见 admin_response.go 的 msgEmptyBody / msgInvalidBody）：
	// 一个是「你什么都没发」，一个是「你发的东西我读不了」，对接方要做的事不同。第二句也覆盖
	// 「字段名不在契约里」——解码不许未知字段（见 decodeOpenAPIBody），而那是拼错时最该被
	// 明确告知的一种。
	msgOpenAPIBodyEmpty   = "request body is empty"
	msgOpenAPIBodyInvalid = "request body is not valid JSON"
	// msgOpenAPIDeviceOrderInvalid：订单域不接受这份报文（字段缺失、饮品编号匹配不到）。
	//
	// 说得笼统是有意的：订单域把那两种情况归在同一个状态码里（InvalidArgument），分不出是哪
	// 一种。硬要猜一个会猜错，而猜错的句子会把对接方引到一个不存在的字段上去。
	msgOpenAPIDeviceOrderInvalid = "the device order request is invalid"
	// msgOpenAPIDeviceNotFound：设备序列号在咖啡机域里查不到。
	//
	// 这一句可以说得具体：它回应的是一次**已经验签通过**的请求，说话对象是拿着有效密钥的合作
	// 方，而设备是**他自己的**——告诉他「这台机器我们这儿没有」不泄漏任何东西（他本来就知道
	// 自己有哪些机器），而含糊其辞（「请求不合法」）会让他去改一份本来没错的报文。
	msgOpenAPIDeviceNotFound = "device is not registered"
	// msgOpenAPIOrderUnavailable：订单域没答上来。可以原样重投（幂等键在对方单号上），
	// 所以不说「失败」，与上面会员那一句同一条理由。
	msgOpenAPIOrderUnavailable = "order service is unavailable"

	// —— 下面几句是取货码那一条（POST，见 CreatePickupOrder）——
	//
	// 这条路上多出来两句话，而它们必须**说得不一样**：机器前面站着一个人，他接下来要么再敲
	// 一次码、要么去充值，而这两件事对他来说完全不同。合成一句「取货失败」等于让他猜。
	//
	// msgOpenAPIPickupInvalid：订单域不接受这份报文（字段缺失、饮品编号匹配不到、这一杯没有
	// 取货码价）。句子与刷卡机那条一样笼统，理由也相同（见 msgOpenAPIDeviceOrderInvalid）。
	// 「这一杯没定价」也落在这里：它是我们这边的配置缺失，但对合作方而言要做的事是同一件
	// （换一杯，或者来找我们）——而它**不能**回 503，那会让他一直重投一杯永远扣不动的饮品。
	// 已知代价：真正的原因只在服务端日志里（订单域那句「has no pickup code price」不会到
	// 这里）。这条路径上没人能区分「报文错」与「没定价」，值不值当是取舍，取的是不对外多说。
	msgOpenAPIPickupInvalid = "the pickup request is invalid"
	// msgOpenAPIPickupCodeRejected：取货码不对，或者这台机器根本没配过码。
	//
	// 咖啡机域有意不区分这两种（见它的扣减实现），这里也不分：合作方要做的都是让顾客重敲一次
	// 或者来找我们。403 而不是 400：报文没有写错任何东西——错的是**报文里那个人提供的一个值**，
	// 而 400 会让对接方去核对自己的拼写。
	msgOpenAPIPickupCodeRejected = "pickup code is not accepted"
	// msgOpenAPIPickupNotEnough：这台设备上的咖啡余额不够。
	//
	// 409 而不是 503：它是**确定的结论**，不是故障——咖啡机域在同一个事务里判的，拒了就一个
	// 字段都没写。回 503 会让合作方一直重投一次永远不可能成功的扣款，而机器前那个人等的是
	// 「余额不足，请充值」。这一档与上一档分开，理由也在这里：一个去充值、一个重新敲码。
	msgOpenAPIPickupNotEnough = "device balance is not enough"
	// msgOpenAPIPickupUnavailable：没问到（订单域或咖啡机域）。可以原样重投——同一笔取货重投
	// 不会扣两次（幂等键在对方单号上），所以不说「失败」。
	msgOpenAPIPickupUnavailable = "pickup service is unavailable"
)

// OpenAPIController 是开放接口的控制器。它只有一个依赖（服务层），而这个依赖会写订单域——
// 但**本控制器自己不碰任何存储**，它只做三件事：判动词、读报文、把错误翻成给合作方看的句子。
type OpenAPIController struct {
	openapi *service.OpenAPIService
}

func NewOpenAPIController(openapi *service.OpenAPIService) *OpenAPIController {
	return &OpenAPIController{openapi: openapi}
}

// Entitlement 处理 GET /v1/openapi/member-price-entitlement?userId=…
//
// # 身份从上下文里来，不从请求里来
//
// 合作方**不能**在参数里声称自己是哪个合作方——那由验签决定（ingress.CallerFrom）。service
// 拿不到身份时返回 ErrCallerMissing，这里把它翻成 500：它意味着这个 handler 被挂到了
// Guard 之外（装配错误），而那种情况下「当成匿名放行」等于所有开放接口对任何人开放。
//
// # 只有 GET
//
// 写方法落在这里的 http.NotFound 上，不是 405：**这条路径上确实没有别的动词可用**（这一条是
// 查询）。回 405 会暗示「这个资源上还有别的动词可用」，而这里没有。
//
// 与 CreateDeviceOrder 那条的 405 不矛盾：那边的路径上**确实有** POST 这一个动词，两种回法
// 各自对着各自的路径成立，判据是「换一个方法打过来，是不是真的有一条路能通」。
func (c *OpenAPIController) Entitlement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if rest := restOf(r.URL.Path, openAPIEntitlementPath); rest != "" {
		http.NotFound(w, r)
		return
	}
	entitlement, partnerID, err := c.openapi.MemberPriceEntitlement(r.Context(), r.URL.Query().Get("userId"))
	if err != nil {
		writeOpenAPIError(w, r, err, "failed to read member price entitlement")
		return
	}
	api.Success(w, dto.EntitlementResponse{
		UserID:            strings.ToLower(strings.TrimSpace(r.URL.Query().Get("userId"))),
		PartnerID:         partnerID,
		Active:            entitlement.Active,
		GrantsMemberPrice: entitlement.GrantsMemberPrice,
		MemberPriceMode:   entitlement.MemberPriceMode,
		PlanCode:          entitlement.PlanCode,
		PlanName:          entitlement.PlanName,
		ExpireAtUnix:      entitlement.ExpireAtUnix,
	})
}

// CreateDeviceOrder 处理 POST /v1/openapi/device/sync-order（线下刷卡机，方案 §四）。
//
// # 这是这一棵树上唯一一条写路径
//
// 其余开放接口全是查询。这一条的性质与它们都不同：**它会让订单域落一行已支付的订单**，而且
// 钱已经在线下那台机器上收过了——报文进来之后没有二次确认的机会，也没有「先查一查再决定」
// 的余地。所以这里一件事都没省：没带凭据、签名不对、nonce 重放、限流，全部在 ingress.Guard
// 那一层就被挡回去（能走到这个函数，就说明那九道全过了，见 ingress.Caller）。
//
// # 身份来自凭据，不来自字段
//
// 报文里没有 partnerId 这一类字段（见 dto.DeviceOrderRequest 的说明）。所以这里不校验、也无
// 法校验「报文自称的合作方」与「验签出来的合作方」是否一致——那种字段之所以不该存在，正是
// 因为它们会不一致，而一旦不一致，写进订单域的每一行都要重新解释一遍。
//
// # 幂等命中不是错误
//
// 对方重投（网络重试、他自己点了两次、上一次的响应丢在路上了）会命中订单域的幂等键，拿回
// 既有那张单，created=false，HTTP 仍然是 200。把它翻成 4xx/5xx 会让对方一直重投下去，而这笔
// 钱早就收过了；created 如实回给对方，让他能区分「新建了」与「重投了一次」。
func (c *OpenAPIController) CreateDeviceOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 405，不是 Entitlement 那边的 404：**这条路径上确实有另一个动词可用**，而拿别的动词
		// 打过来的调用方几乎一定是照着错误的示例写的。告诉它「这个地址只在 POST 上存在」，比
		// 一句 404 少一轮排查（404 会让人去核对路径拼写，而路径是对的）。
		//
		// Allow 头是 405 的规范要求，也正好是对接方要的那句「该用哪个方法」。
		w.Header().Set("Allow", http.MethodPost)
		api.Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", msgOpenAPIMethodNotAllowed)
		return
	}
	if rest := restOf(r.URL.Path, openAPIDeviceOrderPath); rest != "" {
		http.NotFound(w, r)
		return
	}
	var request dto.DeviceOrderRequest
	if !decodeOpenAPIBody(w, r, &request) {
		return
	}
	order, err := c.openapi.CreateDeviceOrder(r.Context(), request)
	if err != nil {
		writeOpenAPIError(w, r, err, "failed to record device order")
		return
	}
	api.Success(w, order)
}

// CreatePickupOrder 处理 POST /v1/openapi/device/pickup（取货码，方案 §四）。
//
// # 这一条与刷卡机那一条像，但有一处不同，而那一处是整条路的重点
//
// 两条都是「验过签的报文进来，让订单域落一行已支付的订单」，所以下面的骨架逐行相同（405 +
// Allow、restOf 判尾、decodeOpenAPIBody、把错误交给 writeOpenAPIError）。差别在**钱从哪来**：
// 刷卡回执记的是已经发生的事，它只有「成不成」两种结局；这一条是**我们从一台设备的余额里扣
// 钱**，于是多了两条各自的结论——码不对、余额不够——而它们必须活着走到合作方那里（见下面
// writeOpenAPIError 里那两档）。把任一条并进 503 都会让机器前面那个人一直等一句永远不会来的
// 「可以了」。
//
// # 身份必须在
//
// 与刷卡机那条相同的理由，代价还要大：少了验签，任何人都能凭一句「给我做一杯」扣掉一台设备的
// 钱。所以这个 handler 与 CreateDeviceOrder 一样只挂在 ingress.Guard 之后（见 routes）。
func (c *OpenAPIController) CreatePickupOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 405 + Allow，与 CreateDeviceOrder 那一条逐字相同（理由见那边）。
		w.Header().Set("Allow", http.MethodPost)
		api.Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", msgOpenAPIMethodNotAllowed)
		return
	}
	if rest := restOf(r.URL.Path, openAPIDevicePickupPath); rest != "" {
		http.NotFound(w, r)
		return
	}
	var request dto.PickupRequest
	if !decodeOpenAPIBody(w, r, &request) {
		return
	}
	order, err := c.openapi.CreatePickupOrder(r.Context(), request)
	if err != nil {
		writeOpenAPIError(w, r, err, "failed to record pickup order")
		return
	}
	api.Success(w, order)
}

// decodeOpenAPIBody 把开放接口的请求体读进 v。读不动时已经把 400 写好了（返回 false）。
//
// # 没有自己的大小上限
//
// 报文在 ingress.Guard 那一步就已经整个读进内存并判过上限了（maxBodyBytes = 1 MiB，超了回
// 413），而且读完把 r.Body 换成了一份可重放的副本（见 middleware.go 的 readBody）——那是
// 为了让 handler 还能读到同一条报文。所以这里再套一层 LimitReader 只是把同一个上限抄第二遍，
// 而两个数迟早会不一致（一个改了另一个没改，症状是「网关说太大、服务说太短」）。
//
// 反过来说，这也意味着**输入上限保护在 Guard 那一层**：handler 单独挂出去时这一层没有上限。
// 那是同一个「忘了装 Guard」的错误，而它的表现已经是全树 401（见 routes/openapi.go）。
//
// # DisallowUnknownFields 在这一棵树上尤其重要
//
// 与后台树同一条理由（字段名拼错时静默忽略，会变成「我明明传了，怎么没生效」），但这里还多
// 一层：报文是外部的对接方照着自己的文档拼的，而**钱已经在机器上收过了**——静默丢掉一个
// brewFailed 的后果，比后台表单里丢掉一个备注严重得多。顺带它也把「报文里塞一个 partnerId
// 试试」变成一句明确的 400，而不是一个「传了但没人看」的谜题。
func decodeOpenAPIBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msgOpenAPIBodyEmpty)
			return false
		}
		// 原始错误（带着字段名）进日志，对外的句子是固定的——理由见文件头。
		slog.WarnContext(r.Context(), "partner open api body rejected", "error", err, "path", r.URL.Path)
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msgOpenAPIBodyInvalid)
		return false
	}
	return true
}

// writeOpenAPIError 把服务层的错误翻成响应。
//
// 每一支都是**固定句子**，没有一条会带上 err 的内容（除了日志）。理由见文件头：调用方是没有
// 经过我们认证的外部系统，任何一条内部错误串透出去都是一次枚举的机会。
//
// 每一支都落在「合作方接下来该做什么」上，而那个问题的答案只有几种：改报文（400）、先把这台
// 机器登记上（404）、换个码（403）、充点钱（409）、等一会儿原样重投（503）、我们的错（500）。
// **4xx 不能落到兜底那一支**：把「你的设备号我这儿没有」包成 500，会让合作方一直重投一条
// 永远不可能成功的请求，而那是这类接口最贵的一种错法。
//
// 403 与 409 那两支只可能出自取货码那一条（刷卡机那条没有「扣钱」这一步，也就没有这两种
// 结论）；反过来，「没配取货码价」这类订单域的错误走的是 400 那一支，不在这里。
func writeOpenAPIError(w http.ResponseWriter, r *http.Request, err error, op string) {
	switch {
	case errors.Is(err, service.ErrUserIDInvalid), errors.Is(err, client.ErrEntitlementUserInvalid):
		// 两种说法合并成同一句话是有意的：对合作方而言它们是同一件事（那个 userId 我们发不
		// 出去或对方不收），而区分它们只会告诉对方「我们的校验在哪一层」。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msgOpenAPIInvalidUserID)

	case errors.Is(err, client.ErrMembershipUnavailable):
		// 503 而不是 500：这是一次**可以重试**的失败（查询幂等），而 500 对客户端意味着
		// 「别再试了」。网关那一段的下游不可用回的就是这个码，两边一致。
		slog.ErrorContext(r.Context(), "partner open api request failed", "error", err, "op", op)
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, msgOpenAPIUnavailable)

	case errors.Is(err, client.ErrDeviceOrderInvalid):
		// 400：报文要改。这一档与 500 的分界是本函数最要紧的一处——订单域明说了「这份报文
		// 不成立」，把它包成 500 会让合作方一直重投一份不会被接受的报文。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msgOpenAPIDeviceOrderInvalid)

	case errors.Is(err, client.ErrDeviceOrderNotFound):
		// 404：设备号不认识。**不是 400**——报文本身没问题，缺的是「这台机器还没登记」这个
		// 前提，而合作方要去做的是另一件事（把这台机器同步进来）。
		api.Error(w, http.StatusNotFound, api.CodeNotFound, msgOpenAPIDeviceNotFound)

	case errors.Is(err, client.ErrDeviceOrderUnavailable):
		// 503：没结论，等一会儿原样重投。这一档里 500 与 503 的区别对合作方是有意义的：
		// 幂等键在对方单号上，重投不会多出一张单，所以「稍后重试」是他该做的事。
		slog.ErrorContext(r.Context(), "partner open api request failed", "error", err, "op", op)
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, msgOpenAPIOrderUnavailable)

	// —— 取货码那一条的五档。它们排在刷卡机那三档之后，因为这条路上「报文要改」「机器没登记」
	// 的说法与那边一模一样（复用了同样的句子与码），真正多出来的是下面三条 ——

	case errors.Is(err, client.ErrPickupInvalid):
		// 400：报文要改，与刷卡机那条同一条理由。
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msgOpenAPIPickupInvalid)

	case errors.Is(err, client.ErrPickupDeviceNotFound):
		// 404：设备号不认识。**复用刷卡机那条的句子**：对合作方来说这是同一件事（他名下有一台
		// 机器我们这儿没有），两句不同的话只会让他以为两条接口对设备的要求不一样。
		api.Error(w, http.StatusNotFound, api.CodeNotFound, msgOpenAPIDeviceNotFound)

	case errors.Is(err, client.ErrPickupCodeRejected):
		// 403：码不对。**不是 400**——这份报文一个字都没写错（见 msgOpenAPIPickupCodeRejected）。
		api.Error(w, http.StatusForbidden, api.CodeForbidden, msgOpenAPIPickupCodeRejected)

	case errors.Is(err, client.ErrPickupNotEnough):
		// 409：余额不够。这一档是本条路上最要紧的一处：它**绝不是 503**。咖啡机域在同一个事务
		// 里判的，拒了就一个字段都没写，回 503 会让合作方一直重投一次永远不可能成功的扣款。
		//
		// 不进日志（与 400 那几条一样）：它是业务流程里正常发生的一种结果，不是异常——真要看
		// 「这台机器今天被拒了几次」，那是指标该回答的问题，不是把每次拒付写进错误日志。
		api.Error(w, http.StatusConflict, api.CodeConflict, msgOpenAPIPickupNotEnough)

	case errors.Is(err, client.ErrPickupUnavailable):
		// 503：没结论，等一会儿原样重投。同一笔取货重投不会扣两次（幂等键在对方单号上），
		// 所以「稍后重试」是他该做的事——而**这正是它与上面那两档的分界**。
		slog.ErrorContext(r.Context(), "partner open api request failed", "error", err, "op", op)
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, msgOpenAPIPickupUnavailable)

	default:
		// 包含 service.ErrCallerMissing（装配错误）。对外的句子与一次普通内部错误完全一样：
		// 告诉调用方「你这条请求没有走验签」等于告诉他怎么绕过它。
		slog.ErrorContext(r.Context(), "partner open api request failed", "error", err, "op", op)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, msgOpenAPIInternal)
	}
}
