package wechatpay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// Client 是微信委托代扣 APIv2 的 HTTP 客户端。
//
// 它比别的协议族多一个出网客户端：**要商户证书的那两条路**（代扣、解约）与其余两条走不同的
// transport（见包注释）。两个客户端都持在它身上、在装配处建一次，之后只读。
type Client struct {
	protocol *Protocol
	// plain 是不带证书的那个，查签约与预扣费通知走它。两条路都不带证书是**微信侧的约定**，
	// 不是我们的选择。
	plain *httpx.Client
	// certs 按路径懒加载商户证书，加载出来的客户端也在里面缓存（见 certClients）。
	certs *certClients
}

// NewClient 构造客户端。protocol 不能为 nil。
func NewClient(protocol *Protocol) *Client {
	return &Client{
		protocol: protocol,
		plain:    httpx.New(httpx.DefaultTimeout),
		certs:    &certClients{},
	}
}

// Protocol 暴露解析好的协议声明（适配器要读 appID / mchID 组织回调应答）。
func (c *Client) Protocol() *Protocol { return c.protocol }

// certClients 是按证书路径缓存的 mTLS 客户端。
//
// 为什么缓存：一次 tls.LoadX509KeyPair 是两次读盘 + 一次解析，而每次代扣都做一遍纯属浪费；
// 更要紧的是 httpx.NewMutualTLS 每次都新建一个 Transport，那等于每次调用一个新连接池——
// 一条续费链路上每个用户都重开一次 TLS 握手。
//
// **只缓存成功**：加载失败时每次都重试。失败的原因是运维刚把证书拷进去、或者路径写错了，
// 而这两种都应当在修好之后立刻生效，不该被一次启动时的失败钉死。
//
// 键是 (certFile, keyFile)：今天只有一条微信渠道、一份证书，但 key 里的两个路径都是配置来的，
// 用路径做键就不必假定「只有一个」。
type certClients struct {
	mu      sync.Mutex
	clients map[string]*httpx.Client
}

// get 取出（必要时先加载）这一份配置对应的 mTLS 客户端。
//
// 证书没配时返回 provider.ErrSecretNotConfigured：它是**部署没配齐**这一类，与「渠道拒了」
// 分得开，也与「报文算错了」分得开。拒绝而不是降级，见包注释。
func (c *certClients) get(protocol *Protocol) (*httpx.Client, error) {
	if !protocol.hasCert() {
		return nil, fmt.Errorf("%w: merchant certificate is not configured for wechat pay "+
			"(config.certFile / config.keyFile)", provider.ErrSecretNotConfigured)
	}
	key := protocol.certFile + "\x00" + protocol.keyFile

	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.clients[key]; ok {
		return client, nil
	}
	certificate, err := tls.LoadX509KeyPair(protocol.certFile, protocol.keyFile)
	if err != nil {
		// **不把路径写进错误串**：这份错误会进 payment_provider_calls.error，而路径里有
		// 主机上的目录结构。要定位问题，下面那句 tls 的错误本身已经够了。
		return nil, fmt.Errorf("load merchant certificate for wechat pay: %w", err)
	}
	client := httpx.NewMutualTLS(httpx.DefaultTimeout, certificate)
	if c.clients == nil {
		c.clients = make(map[string]*httpx.Client)
	}
	c.clients[key] = client
	return client, nil
}

// CallResult 是一次出网调用的**传输层事实**。
//
// 它与业务结论分开：业务结论（签约成没成、扣款成没成）由各方法的返回值或 BusinessError 表达，
// 而这里装的是写 payment_provider_calls 要的那三样——HTTP 状态、实际尝试次数、解析出来的应答
// 报文。分开的理由与 ums 那边逐字相同：一次「200 加一个失败码」与一次「502」是完全不同的两件
// 事，而它们都会让调用方看到一个 error。
type CallResult struct {
	// Response 是解析出来的应答报文。**不要整份进摘要**：它可能含渠道回显的敏感字段，
	// 摘要只挑能定位这一笔的那几个键（见 adapter 的 responseSummary）。
	Response map[string]string
	// HTTPStatus 是渠道应答的 HTTP 状态码，0 表示没拿到应答。
	HTTPStatus int
	// Attempts 是实际发出去的次数，0 当 1。
	Attempts int
}

// BusinessError 是**渠道受理了报文、但判定这次调用失败**。
//
// 它与传输失败（httpx.Error）、与「我们根本没发出去」是三件不同的事，混成一个 error 的表现是
// payment_provider_calls.result 一律记成 failed——而「渠道说不行」与「我们没连上」在排查时
// 是完全不同的两条路。
//
// 报文里的 return_msg 与 err_code_des 进错误串：它们是渠道写给我们看的说明，不含我们的密钥
// （密钥是我们发出去的，不是它回过来的）。
type BusinessError struct {
	ReturnCode string
	ReturnMsg  string
	ErrCode    string
	ErrCodeDes string
}

func (e *BusinessError) Error() string {
	var builder strings.Builder
	builder.WriteString("wechat pay business failure")
	switch {
	case e.ErrCode != "":
		builder.WriteString(": ")
		builder.WriteString(e.ErrCode)
		if e.ErrCodeDes != "" {
			builder.WriteString(" - ")
			builder.WriteString(e.ErrCodeDes)
		}
	case e.ReturnCode != "" && e.ReturnCode != returnCodeSuccess:
		builder.WriteString(": return_code=")
		builder.WriteString(e.ReturnCode)
		if e.ReturnMsg != "" {
			builder.WriteString(" - ")
			builder.WriteString(e.ReturnMsg)
		}
	case e.ReturnMsg != "":
		builder.WriteString(": ")
		builder.WriteString(e.ReturnMsg)
	}
	return builder.String()
}

// FailureCode 是写进流水与 payments.failure_code 的那个码。
//
// 优先业务错误码：`return_code=FAIL` 那一层说的是「报文没被受理」（签名错、参数缺），而
// 排查的人要的首先是 err_code（比如 NOTENROLLED / CONTRACTNOTEXIST）。
func (e *BusinessError) FailureCode() string {
	if e.ErrCode != "" {
		return e.ErrCode
	}
	if e.ReturnCode != "" {
		return "RETURN_" + e.ReturnCode
	}
	return "BUSINESS_FAILURE"
}

// SignParams 算一次**纯签约**的签名参数。
//
// **它不发任何请求**，这正是「小程序纯签约」这条路的形状：服务端只把参数与签名算好交给客户端，
// 由客户端跳去微信官方的签约小程序（见 SignMiniProgramAppID），用户在那里点「同意并签约」。
// 微信不会因为我们「发起了签约」而回任何东西——回执只有两条：签约通知（change_type=ADD）与
// 客户端回来的主动确认（会员服务再调 QueryContract 收口）。
//
// # 参数集是 8 个，一个都不能多
//
// appid / mch_id / plan_id / contract_code / request_serial / contract_display_account /
// notify_url / timestamp。**不含 nonce_str，也不含 version**——老系统那行注释写着「含
// nonce_str 会 SIGN_ERROR」，而 version 是 entrustweb（网页版签约，已废弃）那一套才有的。
//
// ContractDisplayAccount 传的是**用户的 openid**（老系统 generateSignParams 就是这么传的）：
// 这个字段微信只用来展示，而 openid 是我们手上唯一稳定的用户标识。
func (c *Client) SignParams(req SignRequest, secret string) (SignResult, error) {
	timestamp := req.Timestamp
	if timestamp <= 0 {
		timestamp = time.Now().Unix()
	}
	serial := strings.TrimSpace(req.RequestSerial)
	if serial == "" {
		serial = requestSerial()
	}

	params := map[string]string{
		"appid":                    c.protocol.appID,
		"mch_id":                   c.protocol.mchID,
		"plan_id":                  strings.TrimSpace(req.PlanID),
		"contract_code":            strings.TrimSpace(req.ContractCode),
		"request_serial":           serial,
		"contract_display_account": strings.TrimSpace(req.ContractDisplayAccount),
		"notify_url":               strings.TrimSpace(req.NotifyURL),
		"timestamp":                strconv.FormatInt(timestamp, 10),
	}
	if params["plan_id"] == "" || params["contract_code"] == "" {
		// 签名算得出来、微信那边也会收，但这一份协议指向的套餐或用户是空的——那是一个
		// 我们自己的调用方漏填，不该变成一次「微信拒了」。
		return SignResult{}, fmt.Errorf("%w: wechat pay pure sign needs plan_id and contract_code",
			provider.ErrInvalidNotification)
	}
	if err := preSign(params, secret); err != nil {
		return SignResult{}, err
	}
	return SignResult{Params: params, RequestSerial: serial}, nil
}

// SignRequest 是一次纯签约要用的那几个事实。
type SignRequest struct {
	// PlanID 是微信侧的签约模板 id（membership_plans.wechat_plan_id，连续包月是 214488）。
	PlanID string
	// ContractCode 是**我们**这边的协议号，签约通知与查约都按它认这一份协议。
	ContractCode string
	// ContractDisplayAccount 是展示给用户看的账户标识，今天传用户的 openid。
	ContractDisplayAccount string
	// NotifyURL 是签约结果的通知地址（微信推 change_type=ADD 的那一个）。
	NotifyURL string
	// Timestamp 是秒级时间戳，0 表示用当前时间。可由调用方钉住以便测试。
	Timestamp int64
	// RequestSerial 留空则现场生成（纳秒串）。
	RequestSerial string
}

// SignResult 是算好的签约参数：8 个字段 + sign，键名就是微信的字段名。
//
// 调用方**原样**把它交给客户端（老系统是塞进 `wx.navigateToMiniProgram` 的 extraData），
// 不要改名、不要挑拣——微信那个签约小程序按这几个名字取值。
type SignResult struct {
	Params        map[string]string
	RequestSerial string
}

// ContractQuery 是一次查签约的输入。
type ContractQuery struct {
	// PlanID 与 ContractCode 是这一族的**关联键**。查约只认这一对（微信不按 contract_id 查
	// 未签约的协议），所以签约那条路上我们的 contract_code 必须与回调里的那个同源——
	// 老系统续费回调对不上账的根因就在这儿（见包注释）。
	PlanID       string
	ContractCode string
}

// ContractQueryResult 是微信给的签约状态。
type ContractQueryResult struct {
	// ProviderContractID 是微信侧的协议号（contract_id）。签约成功后它是扣款与解约的输入，
	// 所以它要落库（payment_agreements.contract_no）。
	ProviderContractID string
	State              ContractState
	// TerminationMode 是解约方式，只在已解约时有值。
	TerminationMode string
	ContractCode    string
	OpenID          string
}

// QueryContract 查一份签约在微信那边的状态。**不需要商户证书**（与老系统一致）。
//
// # 三个「等于没签」的错误码
//
// 微信在协议不存在、未签约、查无此约时回的是**业务失败**（result_code=FAIL），而不是一份
// 「state=1」的成功应答。把它们当失败返回的后果很具体：后台的「同步」按钮点下去只得到一句
// 「查询失败」，而用户其实已经解约了——本地那条订阅会一直是 active，直到下一次扣款失败。
// 所以这三个码在这里被翻成 ContractTerminated，与微信明说 state=1 是同一种结论。
//
// 「-25 RESULT NULL」是另一种：支付中签约刚完成时查约接口短暂查不到，微信回 -25。那是
// **还没结论**（ContractPending），不是解约——把它翻成 state=1 会让刚签完的用户被当成解约，
// 而我们这边会把一条有效的协议标成 cancelled。
//
// 其余错误码原样返回 BusinessError：我们不知道它是什么意思，就不假装知道。
func (c *Client) QueryContract(ctx context.Context, secret string, req ContractQuery) (ContractQueryResult, CallResult, error) {
	params := map[string]string{
		"appid":         c.protocol.appID,
		"mch_id":        c.protocol.mchID,
		"plan_id":       strings.TrimSpace(req.PlanID),
		"contract_code": strings.TrimSpace(req.ContractCode),
		// version 只在这条路与解约那条路上有（老系统 querycontract 发 1.0）。
		"version": "1.0",
		// 这条**不含 nonce_str**：老系统那行注释「文档未列出该字段，含则 SIGN_ERROR」。
	}
	call, err := c.post(ctx, c.plain, pathQueryContract, params, secret)
	if err != nil {
		var business *BusinessError
		if errors.As(err, &business) {
			switch business.ErrCode {
			case "NOTENROLLED", "CONTRACT_NOT_EXIST", "SIGNNOT_FOUND":
				return ContractQueryResult{State: ContractTerminated}, call, nil
			case "-25":
				if strings.Contains(strings.ToUpper(business.ErrCodeDes), "RESULT NULL") {
					return ContractQueryResult{State: ContractPending}, call, nil
				}
			}
		}
		return ContractQueryResult{}, call, err
	}

	state := ContractState(strings.TrimSpace(call.Response["contract_state"]))
	if state == "" {
		// result_code=SUCCESS 却没给状态：这一份应答我们没读懂。宁可报「读不懂」也不要
		// 猜一个 state——猜错的方向是「把已解约的当成有效」或者反过来。
		return ContractQueryResult{}, call, fmt.Errorf("%w: querycontract response has no contract_state",
			provider.ErrInvalidNotification)
	}
	return ContractQueryResult{
		ProviderContractID: strings.TrimSpace(call.Response["contract_id"]),
		State:              state,
		TerminationMode:    strings.TrimSpace(call.Response["contract_termination_mode"]),
		ContractCode:       strings.TrimSpace(call.Response["contract_code"]),
		OpenID:             strings.TrimSpace(call.Response["openid"]),
	}, call, nil
}

// TerminateContract 解一份协议。**需要商户证书**。
type TerminateContract struct {
	// ProviderContractID 是微信侧的协议号（contract_id）。
	ProviderContractID string
	// Remark 进渠道的解约备注，给人在微信商户平台上排查用。
	Remark string
}

// TerminateContract 向微信发起解约。
//
// 参数集与老系统逐字一致：appid / mch_id / contract_id / contract_termination_remark /
// version=1.0 / nonce_str。备注为空时它**照旧进报文**（一个空的 CDATA 段），但不进待签串
// （空值被 SkipEmpty 剔掉）——这两件事互不影响，见 encodeXML 的注释。
//
// 解约是**幂等**的：微信对一份已经解掉的协议回 success，所以这里不区分「本来就解了」与
// 「刚刚解掉」。调用方要的结论只有一个——这份协议现在不再是活的了。
func (c *Client) TerminateContract(ctx context.Context, secret string, req TerminateContract) (CallResult, error) {
	client, err := c.certs.get(c.protocol)
	if err != nil {
		return CallResult{}, err
	}
	params := map[string]string{
		"appid":                       c.protocol.appID,
		"mch_id":                      c.protocol.mchID,
		"contract_id":                 strings.TrimSpace(req.ProviderContractID),
		"contract_termination_remark": strings.TrimSpace(req.Remark),
		"version":                     "1.0",
		"nonce_str":                   nonceStr(),
	}
	return c.post(ctx, client, pathDeleteContract, params, secret)
}

// ChargeRequest 是一次代扣的输入。
type ChargeRequest struct {
	// ProviderContractID 是微信侧的协议号，扣款按它认用户。
	ProviderContractID string
	// OutTradeNo 是**我们的商户单号**，进微信账单，也是回调里的关联键。
	//
	// 我们的 payment_agreement_charges 用 (agreement_id, biz_period) 做幂等，而 out_trade_no
	// 由调用方从那一行派生——**两边必须同源**，否则就是老系统那个「回调查不到单」的坑
	// （见包注释）。
	OutTradeNo string
	// Body 是账单上显示的商品描述。
	Body string
	// TotalFee 是金额，单位**分**。
	TotalFee int64
	// ClientIP 是「终端 IP」。代扣没有用户在终端上操作，微信要的只是一个合法 IP——
	// 老系统写死 127.0.0.1，这里照抄。
	ClientIP string
	// NotifyURL 是扣款结果的通知地址。
	NotifyURL string
}

// ChargeResult 是一次代扣被受理之后的结论。
type ChargeResult struct {
	// ProviderTransactionID 是微信支付订单号（transaction_id）。它落
	// payment_provider_calls.provider_transaction_id，也是扣款回调里的关联键之一。
	ProviderTransactionID string
}

// Charge 向一份已签约的协议发起一次扣款。**需要商户证书**（这是四个接口里唯一动钱的那个）。
//
// # 受理不等于扣到钱
//
// 这个接口是**异步**的：它回 result_code=SUCCESS 只说明微信收下了这笔扣款请求，钱到没到要看
// notify_url 上的那条回调（或事后查单）。所以调用方拿到 ChargeResult 之后不能把这一期记成
// 「已扣款」——真正的账在 payment_agreement_charges 的状态上，由回调推进。
//
// 参数集与老系统逐字一致：appid / mch_id / contract_id / out_trade_no / body / total_fee /
// spbill_create_ip / trade_type=PAP / notify_url / nonce_str。
func (c *Client) Charge(ctx context.Context, secret string, req ChargeRequest) (ChargeResult, CallResult, error) {
	client, err := c.certs.get(c.protocol)
	if err != nil {
		return ChargeResult{}, CallResult{}, err
	}
	clientIP := strings.TrimSpace(req.ClientIP)
	if clientIP == "" {
		// 与老系统同一个兜底值：代扣没有用户在终端上操作，这个字段只是微信侧的必填项。
		clientIP = defaultClientIP
	}
	params := map[string]string{
		"appid":            c.protocol.appID,
		"mch_id":           c.protocol.mchID,
		"contract_id":      strings.TrimSpace(req.ProviderContractID),
		"out_trade_no":     strings.TrimSpace(req.OutTradeNo),
		"body":             strings.TrimSpace(req.Body),
		"total_fee":        strconv.FormatInt(req.TotalFee, 10),
		"spbill_create_ip": clientIP,
		// PAP 是委托代扣的交易类型，写死（老系统亦然）。换一个值就是另一种业务。
		"trade_type": "PAP",
		"notify_url": strings.TrimSpace(req.NotifyURL),
		"nonce_str":  nonceStr(),
	}
	call, err := c.post(ctx, client, pathPAPPayApply, params, secret)
	if err != nil {
		return ChargeResult{}, call, err
	}
	return ChargeResult{
		ProviderTransactionID: strings.TrimSpace(call.Response["transaction_id"]),
	}, call, nil
}

// defaultClientIP 见 ChargeRequest.ClientIP。
const defaultClientIP = "127.0.0.1"

// PreNotifyRequest 是一次预扣费通知的输入。
type PreNotifyRequest struct {
	ProviderContractID string
	OutTradeNo         string
	Body               string
	TotalFee           int64
	// DeductDate 是**预计扣款日**，格式 yyyyMMdd（微信的格式，不是我们的）。
	DeductDate string
}

// PreNotify 发一条预扣费通知。**不需要商户证书**。
//
// 微信要求它在实际扣款**前 24 小时内**发出（老系统那行注释），是「让用户先知道要被扣钱」的
// 那一步。它不影响钱：通知发失败不该阻断扣款（老系统的处置是记一条 warn 继续），所以这里的
// 失败由调用方决定要不要吞掉——本方法只负责把失败如实报出来。
//
// 参数集与老系统逐字一致，**没有 notify_url、也没有 spbill_create_ip**：这条通知没有结果要
// 回给我们（它是单向的告知），而 ip 那一项文档没列。
func (c *Client) PreNotify(ctx context.Context, secret string, req PreNotifyRequest) (CallResult, error) {
	params := map[string]string{
		"appid":        c.protocol.appID,
		"mch_id":       c.protocol.mchID,
		"contract_id":  strings.TrimSpace(req.ProviderContractID),
		"out_trade_no": strings.TrimSpace(req.OutTradeNo),
		"body":         strings.TrimSpace(req.Body),
		"total_fee":    strconv.FormatInt(req.TotalFee, 10),
		"deduct_date":  strings.TrimSpace(req.DeductDate),
		"nonce_str":    nonceStr(),
	}
	return c.post(ctx, c.plain, pathPreDeduction, params, secret)
}

// post 是四条出站路径共用的那一段：签名 → 拼报文 → 发出去 → 解析 → 判两层。
//
// 抽出来的理由与 ums 的 authorize 逐字相同：分开写四遍的写法必然会让其中一条悄悄少签一段，
// 而那种错只在联调时暴露。**参数集仍然各写各的**（见包注释）——这里统一的只是「怎么发」。
func (c *Client) post(ctx context.Context, client *httpx.Client, path string, params map[string]string, secret string) (CallResult, error) {
	if err := preSign(params, secret); err != nil {
		return CallResult{}, err
	}
	body := encodeXML(params)

	response, err := client.Do(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    c.protocol.url(path),
		Header: map[string]string{"Content-Type": "application/xml"},
		Body:   body,
	})
	if err != nil {
		call := CallResult{}
		var httpErr *httpx.Error
		if errors.As(err, &httpErr) {
			call.Attempts = httpErr.Attempts
		}
		// 错误串原样往外传：httpx 已经做过 URL 脱敏，而这一族的密钥在**报文体**里
		// （sign 字段），不在 URL 上，所以它不会跟着出来。
		return call, fmt.Errorf("wechat pay %s: %w", path, err)
	}

	call := CallResult{
		HTTPStatus: response.StatusCode,
		Attempts:   response.Attempts,
	}
	decoded, decodeErr := decodeXML(response.Body)
	call.Response = decoded
	if decodeErr != nil {
		// 报文没读懂。渠道回了非 200 时它更可能是「对面挂了」的回执，而不是协议报文——
		// 两种都归到「没结论」，由调用方按 ResultUnknown 处置。
		return call, fmt.Errorf("wechat pay %s returned HTTP %d: %w", path, response.StatusCode, decodeErr)
	}
	if err := verdict(decoded); err != nil {
		return call, err
	}
	return call, nil
}

// verdict 检查微信的两层判据：`return_code`（报文受理了吗）与 `result_code`（业务成功了吗）。
//
// 两层都要看，顺序也不能反：只有 return_code=SUCCESS 时 result_code 才有意义（老系统那两个
// 方法就是先看前者再看后者）。
func verdict(response map[string]string) error {
	returnCode := strings.TrimSpace(response["return_code"])
	if returnCode != returnCodeSuccess {
		return &BusinessError{
			ReturnCode: returnCode,
			ReturnMsg:  strings.TrimSpace(response["return_msg"]),
		}
	}
	resultCode := strings.TrimSpace(response["result_code"])
	if resultCode != resultCodeSuccess {
		return &BusinessError{
			ReturnCode: returnCode,
			ReturnMsg:  strings.TrimSpace(response["return_msg"]),
			ErrCode:    strings.TrimSpace(response["err_code"]),
			ErrCodeDes: strings.TrimSpace(response["err_code_des"]),
		}
	}
	return nil
}
