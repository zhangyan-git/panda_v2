// Command fakechannel 是一个**假装是收单渠道**的本地进程，用来端到端验证 payment-service
// 的出站与入站两条路。
//
// # 它为什么是独立的一份实现
//
// 这个文件**刻意不 import 仓库里任何东西**，尤其是 platform/signing。签名算法在这里是从
// 老系统 panda_serve 那几处实现（fengxuan/client.go:408、shouchuang/client.go:52）抄下来的
// 同一段骨架，自己写一遍。
//
// 用共享库来假装渠道当然更省事，但那会让这轮验证变成**自己签自己验**：库里那套拼串要是
// 歪了，假渠道跟着歪，两边照样对上，而真实渠道会拒。这里多写的二十行买到的正是这一点——
// 「payment-service 发出去的报文能不能被一个照老系统实现的对手方认下来」。
//
// # 它假装什么
//
//	POST /pay      收下单请求：验签 → 记下这一笔 → 回一个成功应答 → 稍后主动发回调
//	POST /query    收查单请求：按商户单号回「已经成功」
//	POST /callback 手动触发一次回调（?tamper=1 发一条**签名对不上**的）
//	GET  /received 回显最近收到的那份报文，给命令行断言用
//
// # 用法
//
//	FAKE_SECRET=... FAKE_NOTIFY_URL=http://127.0.0.1:18085/v1/payments/callback/fake_channel \
//	    go run ./tools/fakechannel
//
// 默认监听 127.0.0.1:19001。FAKE_NOTIFY_URL 就是我们那条渠道 config 里会被填进报文的
// notify_url，pay 之后它按 FAKE_CALLBACK_DELAY（默认 500ms）延后发送——延后是必须的，
// 让「下单的 HTTP 应答」先回到 payment-service，回调才是第二条独立的请求。
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	listen     string
	secret     string
	notifyURL  string
	keyName    string
	signField  string
	upper      bool
	callbackIn time.Duration
}

func main() {
	cfg := config{
		listen:     envOr("FAKE_LISTEN", "127.0.0.1:19001"),
		secret:     os.Getenv("FAKE_SECRET"),
		notifyURL:  os.Getenv("FAKE_NOTIFY_URL"),
		keyName:    envOr("FAKE_KEY_NAME", "Key"),
		signField:  envOr("FAKE_SIGN_FIELD", "Sign"),
		upper:      envOr("FAKE_UPPER", "true") == "true",
		callbackIn: 500 * time.Millisecond,
	}
	if raw := os.Getenv("FAKE_CALLBACK_DELAY"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			log.Fatalf("FAKE_CALLBACK_DELAY %q: %v", raw, err)
		}
		cfg.callbackIn = parsed
	}
	if cfg.secret == "" {
		log.Fatal("FAKE_SECRET 是必填的：没有它就没法验我们发出去的签名，也没法签我们发回去的回调")
	}
	if cfg.notifyURL == "" {
		log.Printf("警告：FAKE_NOTIFY_URL 没设，/pay 之后不会自动发回调（仍可用 POST /callback 手动发）")
	}

	server := &channel{
		cfg:       cfg,
		client:    &http.Client{Timeout: 10 * time.Second},
		trades:    map[string]string{},
		startedAt: time.Now(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/pay", server.pay)
	mux.HandleFunc("/query", server.query)
	mux.HandleFunc("/callback", server.callback)
	mux.HandleFunc("/received", server.received)

	log.Printf("假渠道监听 %s（Key=%s，签名字段=%s，大写=%v）", cfg.listen, cfg.keyName, cfg.signField, cfg.upper)
	if err := http.ListenAndServe(cfg.listen, mux); err != nil {
		log.Fatal(err)
	}
}

type channel struct {
	cfg    config
	client *http.Client

	mu        sync.Mutex
	last      map[string]string
	trades    map[string]string // 商户单号 → 下单时回给我们的交易号
	seq       int
	startedAt time.Time
}

// pay 收一次下单。
//
// 第一件事是**验签**，而不是先看报文内容：这正是真实渠道会做的事，也是这一轮验证里
// 最要紧的一条断言——payment-service 签出来的东西必须能被一个照老系统实现出来的对手方认下来。
func (c *channel) pay(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params, err := decodeBody(body)
	if err != nil {
		log.Printf("收不下这份报文：%v（原文 %s）", err, body)
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	provided := params[c.cfg.signField]
	if want := c.sign(params); provided != want {
		// 打全两边的串，这一条失败时最需要看到的就是「我们签的是什么、它期望的是什么」。
		log.Printf("签名对不上：收到 %q，期望 %q；报文 %v", provided, want, params)
		writeJSON(w, http.StatusOK, map[string]string{
			"RespCode": "9001", "RespMsg": "签名错误",
		})
		return
	}

	c.mu.Lock()
	c.last = params
	c.seq++
	// 交易号里带**进程启动时刻**：真实收单方的交易号是全表唯一的，而这个假渠道重启一次
	// 序号就从 1 重来。只写序号的话，第二轮的 FAKE-TRADE-1 会撞上第一轮那笔单的
	// payments.provider_transaction_id 唯一约束——症状是回调被拒、报文却完全正确，
	// 看起来像结算侧的 bug。
	tradeNo := fmt.Sprintf("FAKE-%d-%d", c.startedAt.Unix(), c.seq)
	c.trades[params["OrderNo"]] = tradeNo
	c.mu.Unlock()

	log.Printf("收到下单：商户单号=%s 金额=%s 设备=%s", params["OrderNo"], params["OrderAmount"], params["DeviceNo"])
	writeJSON(w, http.StatusOK, map[string]string{
		"RespCode":      "0000",
		"RespMsg":       "下单成功",
		"TransactionNo": tradeNo,
		"PayUrl":        "https://fake-channel.invalid/pay/" + tradeNo,
	})

	if c.cfg.notifyURL == "" {
		return
	}
	// 延后发回调：让上面那条下单应答先回到 payment-service。**这不是节奏偏好**——支付单
	// 在收到下单应答之后才会被置成 pending，一条抢在它前面的回调会撞上「找不到这一单」。
	paymentNo := params["OrderNo"]
	go func() {
		time.Sleep(c.cfg.callbackIn)
		if err := c.sendCallback(paymentNo, params["OrderAmount"], tradeNo, false); err != nil {
			log.Printf("发回调失败：%v", err)
		}
	}()
}

// query 收一次查单。它按商户单号回「成功」，用来验证超时之后那条兜底路径。
//
// 这一条**不是真语义**：真实渠道只有确实收到了钱才会这么答。这里一律答成功，是因为
// 假渠道没有真实的状态可查——所以靠它得出的结论只到「查单那条路通不通」，不到
// 「查询结论算不算数」（后者由单测覆盖）。
func (c *channel) query(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	params, err := decodeBody(body)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if provided, want := params[c.cfg.signField], c.sign(params); provided != want {
		log.Printf("查单签名对不上：收到 %q，期望 %q", provided, want)
		writeJSON(w, http.StatusOK, map[string]string{"RespCode": "9001", "RespMsg": "签名错误"})
		return
	}
	log.Printf("收到查单：商户单号=%s", params["OrderNo"])
	writeJSON(w, http.StatusOK, map[string]string{
		"RespCode": "0000", "RespMsg": "查询成功", "TransactionNo": "FAKE-TRADE-QUERY",
	})
}

// callback 手动触发一次回调。?tamper=1 时发一条**签完名之后又改了报文**的——那正是
// 「伪造回调不该改动任何字段」那条用例要的形状。
//
// 手动触发这条路是必要的：/pay 之后的自动回调与下单应答之间有竞态，而那让「验签失败的
// 回调一个字段都不改」这条断言没法稳定地做。
func (c *channel) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	paymentNo := query.Get("paymentNo")
	amount := query.Get("amount")

	c.mu.Lock()
	if paymentNo == "" {
		paymentNo = c.last["OrderNo"]
	}
	if amount == "" {
		amount = c.last["OrderAmount"]
	}
	// 交易号必须回**下单时我们自己给出去的那一个**：真实渠道不可能在一个回调里报出另一笔
	// 交易的号，而结算侧正是按这个字段对账的（对不上就拒）。早先这里写死 "FAKE-TRADE-MANUAL"，
	// 于是每条手动回调都被判「交易号不符」——那测的是结算侧的拒绝，不是回调本身。
	tradeNo := c.trades[paymentNo]
	c.mu.Unlock()
	if tradeNo == "" {
		tradeNo = "FAKE-TRADE-MANUAL"
	}

	if paymentNo == "" {
		http.Error(w, "还没收到过下单，得给个 ?paymentNo=", http.StatusBadRequest)
		return
	}
	if amount == "" {
		// 手填一个金额：测试里要用「金额与支付单不符」验结算侧的拒绝，那一条走的就是这里。
		http.Error(w, "没收到过金额，得给个 ?amount=", http.StatusBadRequest)
		return
	}

	tamper := query.Get("tamper") == "1"
	if err := c.sendCallback(paymentNo, amount, tradeNo, tamper); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": paymentNo, "amount": amount, "tampered": tamper})
}

// sendCallback 拼一条回调并 POST 到 payment-service。
func (c *channel) sendCallback(paymentNo, amount, tradeNo string, tamper bool) error {
	params := map[string]string{
		"OrderNo":       paymentNo,
		"OrderStatus":   "1",
		"OrderAmount":   amount,
		"TransactionNo": tradeNo,
		"NotifyId":      fmt.Sprintf("FAKE-NOTIFY-%d", time.Now().UnixNano()),
		"PayTime":       strconv.FormatInt(time.Now().Unix(), 10),
	}
	params[c.cfg.signField] = c.sign(params)

	if tamper {
		// **签完之后再改**：签名还是原来那份报文算出来的，于是验签必然不过。把金额改小是
		// 最像真实攻击的一种改法（把一笔大额说成小额）。
		params["OrderAmount"] = "1"
	}

	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, c.cfg.notifyURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("POST %s: %w", c.cfg.notifyURL, err)
	}
	defer response.Body.Close()
	echo, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	label := ""
	if tamper {
		label = "（签名对不上）"
	}
	log.Printf("发回调%s：支付单=%s 金额=%s → %d %s",
		label, paymentNo, params["OrderAmount"], response.StatusCode, echo)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("渠道回调被拒：%d %s", response.StatusCode, echo)
	}
	return nil
}

func (c *channel) received(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		http.Error(w, `{"received":false}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, c.last)
}

// sign 是 panda_serve 那几处签名实现的复刻：过滤空值与签名字段 → 按键升序 → "k=v" 用 "&"
// 连接 → 追加 "&{keyName}={secret}" → MD5 hex。
//
// **密钥前那个 "&" 是无条件写的**（老系统三处都如此），参数为空时会产出 "&Key=xxx" 这种
// 形状——照抄，不"修正"：这一类偏差正是迁移时对不上账的根源。
func (c *channel) sign(params map[string]string) string {
	filtered := make(map[string]string, len(params))
	for key, value := range params {
		if value != "" && key != c.cfg.signField {
			filtered[key] = value
		}
	}
	keys := make([]string, 0, len(filtered))
	for key := range filtered {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for index, key := range keys {
		if index > 0 {
			builder.WriteString("&")
		}
		builder.WriteString(key + "=" + filtered[key])
	}
	builder.WriteString("&" + c.cfg.keyName + "=" + c.cfg.secret)

	sum := md5.Sum([]byte(builder.String()))
	digest := hex.EncodeToString(sum[:])
	if c.cfg.upper {
		return strings.ToUpper(digest)
	}
	return digest
}

// decodeBody 按**内容**认报文的形状，不按 Content-Type：真实渠道把 JSON 标成
// text/plain 是常事，假渠道要是按头部认，验的就是一个真实渠道不会发的形状。
func decodeBody(body []byte) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(body))
	out := map[string]string{}
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]any
		if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
			return nil, err
		}
		for key, value := range object {
			out[key] = fmt.Sprint(value)
		}
		return out, nil
	}
	values, err := url.ParseQuery(trimmed)
	if err != nil {
		return nil, err
	}
	for key, list := range values {
		if len(list) > 0 {
			out[key] = list[0]
		}
	}

	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
