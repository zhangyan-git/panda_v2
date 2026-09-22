# 银联商务全民付 H5 支付对接要点

> **来源是外部规范，不是我们的设计。** 原文在 `panda_dev/开放平台H5支付/`：
> `全民付移动支付 H5支付 v20221020.pdf`（41 页）与同目录的 `认证流程.pdf`（3 页）。
> 本篇只做提炼与差距标注。老系统 `panda_serve` 只读对照，一个字都不改。
>
> 同目录还有 `H5测试参数.txt`（appId / appKey / **md5 密钥**的明文）。**那两把密钥不进本仓库**，
> 也不进任何配置文件——本文所有示例里的密钥值都是文档原文里的演示值。
>
> **这条渠道已经接完了**（见 §8.1），并且拿沙箱测试凭据实测过一轮（见 §10：定稿了什么、
> 还差什么）。所以本文今天的读法是「协议要点 + 我们的落地位置 + 还没证实的那几条」，
> 不再是「待接清单」。

## 0. 一句话结论

H5 这条路与我们已经实现的**小程序**那条（`internal/provider/ums`）**共用同一把密钥、同一套签名算法，
但下单接口的协议形状完全不同**：小程序是 `POST + JSON 体 + OPEN-BODY-SIG 请求头`，H5 是
`GET + 参数全拼在 URL 上 + OPEN-FORM-PARAM`，而且**没有应答报文**（浏览器直接 302）。

所以它不是「给 ums 加一个 createPath」，是**同一渠道下的第二种协议**。这条协议已经落地
（第 8 节），四条下单路径实测都回 302（第 10 节）。

## 1. 认证：三套，别混

| 方式 | 用在哪些接口 | 认证内容 |
| --- | --- | --- |
| `OPEN-ACCESS-TOKEN` | POST + JSON 的那批 | 先换 token，再放 `Open-Access-Token accessToken="…"` |
| `OPEN-BODY-SIG` | POST + JSON 的那批 | `AppId="…", Timestamp="…", Nonce="…", Signature="…"`，签名时 SHA256 覆盖**报文体** |
| `OPEN-FORM-PARAM` | **GET 的接口——H5 下单专用** | 认证与业务参数全部拼进 URL query |

**`OPEN-BODY-SIG` 与我们现在的实现逐字一致**（`ums.go:200` 的 `authorizationHeader`）：
`Signature = Base64(HmacSHA256(appId + timestamp + nonce + SHA256_hex(报文体), AppKey))`，
时间戳格式 `yyyyMMddHHmmss`。这条不用改。

**`OPEN-FORM-PARAM` 是新的**（认证流程 §4）：

```
url?authorization=OPEN-FORM-PARAM
   &appId={appId}
   &timestamp={yyyyMMddHHmmss}
   &nonce={随机数，≤128}
   &content={业务报文 JSON，URL 编码}
   &signature={URL 编码后的签名}
```

- 待签串与 `OPEN-BODY-SIG` **同形**：`appId + timestamp + nonce + SHA256_HEX(content)`，
  用 AppKey 做 HMAC-SHA256，再 base64——**但整条签名还要再 URLEncode 一遍**（因为它要进 URL）。
- `content` 本身也要 URLEncode。**签名用的是 content 的原始值（未编码）**，编码只发生在拼 URL 时。
  这正是正文第 3 页那句「只对上送报文的字段值进行加密，生成签名时的待签字串仍用原始字符」。
- 把拼好的 URL 交给浏览器跳转即可。

`OPEN-ACCESS-TOKEN` 我们没用到：token 有效期 1 小时、同 appId 最多 10 个，文档要求
「不要每次接口调用都先获取」并建议**中控服务器**统一换/刷。真要接它，得先有那台中控，
而不是在支付服务里每次现取——那会先把 10 个配额用光。

## 2. 下单：H5 与小程序的差别全在这里

**请求**：`HTTP GET`，认证按上节的 `OPEN-FORM-PARAM`。

**四个路径**（同一份报文，四条路）：

| 渠道 | 路径 |
| --- | --- |
| 支付宝 H5 | `/v1/netpay/trade/h5-pay` |
| 微信 H5 | `/v1/netpay/wxpay/h5-pay` |
| 银联云闪付 | `/v1/netpay/uac/order` |
| 微信 H5 转小程序 | `/v1/netpay/wxpay/h5-to-minipay` |

测试域 `https://test-api-open.chinaums.com`，生产域 `https://api-mop.chinaums.com`。

> `H5Pay.java` 注释里的 `http://58.247.0.18:29015` 是**更老的地址**，正式文档已经不用它。
> 别照着那份示例代码填 baseURL。

**`instMid` 固定 `H5DEFAULT`**（小程序那条是 `MINIDEFAULT`）。

**响应**：**由浏览器直接跳转，不需要应答报文**（原文即此）。这一条决定了三件事：

1. 我们**拿不到** errCode，也就没有「渠道到底建单了没有」这个判据——`interpretCreate`
   那一整套「含糊码不能当失败」的逻辑（`create.go:288`）在这条路上**没有输入**。
2. 我们**拿不到** `miniPayRequest` 一类的东西；回给客户端的就是那条跳转 URL。
3. **绝不能跟随重定向**。`httpx` 用的是裸 `http.Client`（`httpx.go:105`，没有 `CheckRedirect`），
   Go 默认会跟最多 10 跳——直接照搬会把收银台页面的 **HTML** 当应答读回来，而且 HTTP 状态是 200。
   下拉单必须显式关掉重定向并从 `Location` 取值。

**报文关键字段**（完整表见原文 §1.1.3，这里只点与小程序不同的）：

| 字段 | 说明 |
| --- | --- |
| `merOrderId` | 商户订单号，**我们的支付单号**（回调靠它落单，这一条与小程序版一致） |
| `mid` / `tid` | 商户号（15）/ 终端号（8） |
| `instMid` | `H5DEFAULT` |
| `totalAmount` | **分**，JSON 数字 |
| `notifyUrl` | 支付结果通知地址 |
| `returnUrl` | 结果页回跳地址（**H5 独有**，见第 3 节） |
| `expireTime` | 订单过期时间，格式 `yyyy-MM-dd HH:mm:ss`。**小程序那份报文里老系统没发它，H5 这份有** |
| `sceneType` / `merAppName` / `merAppId` | **微信 H5 必填**。`sceneType` 取 `IOS_SDK`/`AND_SDK`/`IOS_WAP`/`AND_WAP` |
| `limitCreditCard` | 是否限制信用卡，`true`/`false`，默认 false |
| `secureTransaction` | 担保交易。**担保未完成的单不允许直接做反交易**，30 天不动自动按撤销处理 |
| `divisionFlag` / `asynDivisionFlag` / `platformAmount` / `subOrders` / `goods` | 分账（见第 5 节末尾） |
| `name` / `mobile` / `certNo` / `bankCardNo` | **敏感，一律 Base64 编码后再传** |

实名认证（**只支持支付宝与云闪付**）：必传 `name` + `certType` + `certNo`，且 `fixBuyer=T`。

**分账的硬约束**（做分账时必须满足，否则渠道会拒）：
- `divisionFlag=true` 时 `goods` 与 `subOrders` **不能同时为空**（建议只用 `subOrders`）；
- 且 `secureTransaction` 必须为 false 或不传（担保与分账互斥）；
- `totalAmount = Σ subOrders.totalAmount + platformAmount`，也必须等于 `Σ goods.subOrderAmount`。

## 3. 结果页返回（`returnUrl`）——H5 独有，且签名规则是另一套

支付完成后用户**先落到渠道的结果页**，点「完成」才跳回 `returnUrl`。跳回时银商在 URL 后面
**拼一长串支付结果参数**，其中 `sign` 是必返项。

**这套签名与我们所有其他签名都不同**（原文 §1.9.3）：

1. 参数（含名字与值）按 **ASCII 字典序**排序；
2. 用 `&` 连接成 `k=v&k=v`；
3. **末尾直接接上通讯密钥**（不是 HMAC，不是 base64）；
4. 对这个串算 MD5 **或** SHA256——**默认 SHA256**，实际用哪种看报文里的 `signType`；
5. 无值（含空串）的参数不参与；值里有特殊字符要 URLEncode，**但签名用原始值**。

注意它**不是** `OPEN-BODY-SIG`，也不是 `OPEN-FORM-PARAM` 的签名。要接就得单独实现一份。

**语义上它是展示用的，不是结算依据**：钱到没到只看支付结果通知与查单。因此这一路的处理应该是
「验签 → 展示结果」，**不能拿它去改支付状态**。

## 4. 支付结果通知

**投递方式：`POST`，格式是 `form 表单`，不是 JSON。**（原文 §1.10.2）

幂等与应答：
- 商户必须应答 `SUCCESS`（收到）或 `FAILED`（失败）；
- 没收到 `SUCCESS` 会在 **24 小时内多次重投**；
- 重投的 `notifyId` **不变**，可以据此判重；也可以主动调查询接口以查询结果为准。

`notifyId` 之外还带一个**随机字段**（原文写作「随机 key」，值是随机串），它**参与签名**——
也就是说验签时不能只取固定字段名，得把收到的全部参数拿去排序。

状态词表与小程序那条共用：成功有 `TRADE_SUCCESS` / `SUCCESS` / `success` 三个写法，
我们已经在 `ums.go:110` 的那组常量里都认了。

## 5. 其余接口

全部是 `POST + JSON`，认证走 `OPEN-BODY-SIG` 或 `OPEN-ACCESS-TOKEN`（**不是** `OPEN-FORM-PARAM`）：

| 接口 | 路径 | 要点 |
| --- | --- | --- |
| 订单交易查询 | `/v1/netpay/query` | 响应字段最全，是「回调没来」时的唯一权威 |
| 退款 | `/v1/netpay/refund` | 多次退款时 `refundOrderId` **每次必须不同**；重复送同一对 `merOrderId+refundOrderId` 会返回已有退货单而不是再退一次 |
| 退款查询 | `/v1/netpay/refund-query` | `refundStatus` 为 `PROCESSING`/`UNKNOWN` 时要靠它确认 |
| 担保撤销 | `/v1/netpay/secure-cancel` | 已撤销会回 `HAS_CANCELLED` |
| 担保完成 | `/v1/netpay/secure-complete` | 完成金额 `completedAmount`；未完成部分原路退回 |
| 订单关闭 | `/v1/netpay/close` | 关未支付的单 |
| 异步分账确认 | `/v1/netpay/sub-orders-confirm` | 支付成功 N 天后调整子单；**已确认的子单不允许隔天再确认**，退货单不允许做子单操作 |

分账标识 `asynDivisionFlag`（异步分账）与 `divisionFlag`（同步分账）是两条路，
`/sub-orders-confirm` 对应前者的后续确认。

## 6. 取值表（速查）

- **`status`**：`NEW_ORDER` / `WAIT_BUYER_PAY` / `TRADE_SUCCESS` / `TRADE_REFUND` / `TRADE_CLOSED` / `UNKNOWN`
  （`TRADE_CLOSED` 的单不允许再做任何操作）
- **`refundStatus`**：`SUCCESS` / `FAIL` / `PROCESSING` / `UNKNOWN`（后两个要再查）
- **`secureStatus`**：`UNCOMPLETED` / `PARTLY_COMPLETED` / `ALL_COMPLETED` / `CANCELED`
- **`targetSys`**（16 个，节选常用的）：`Alipay 2.0`、`WXPay`、`UnionPay`（银联钱包）、
  `UAC`（银联全渠道）、`ACP`（银联全渠道立码付）、`QMF`、`QmfWebPay`、`NetPayGtwy`
- **`certType`**：`IDENTITY_CARD` / `PASSPORT` / `OFFICER_CARD` / `SOLDIER_CARD` / `HOKOU`(仅支付宝) /
  `HM_EXIT` / `TW_EXIT` / `POLICE_CERTIFICATE` / `OTHER`
- **错误码**（22 个，要盯的）：`BAD_SIGN`(签名错)、`DUP_ORDER`(单号重复)、`ORDER_PROCESSING`(渠道还在处理)、
  `NO_MERCHANT`(商户号不认)、`NO_ORDER`、`OPERATION_NOT_ALLOWED`(订单已关闭)、`DENIED_IP`(IP 白名单)、
  `ABNORMAL_REQUEST_TIME`(**要求系统时间准**)、`INACTIVE_MERCHANT`(商户被冻结)

## 7. 客户端改造（只有走云闪付且在自己 APP 的 webview 里才需要）

- **iOS**：plist 里加 `LSApplicationQueriesSchemes`，五项：
  `uppaysdk`、`uppaywallet`、`uppayx1`、`uppayx2`、`uppayx3`
- **Android**：webview 要放行 `upwrp://`（云闪付 APP 的 scheme）。文档给了
  `shouldOverrideUrlLoading` 的示例，**必须 try/catch**——手机上没装云闪付时
  `startActivity` 会直接崩
- **两端都有 UserAgent 要求**：iOS 只能加字段不能减，Android 必须含大小写敏感的 `Android`。
  **UA 不对会跳到 PC 页面**——那是用户在手机上看一个 PC 收银台

纯 H5（用户在自己浏览器里打开）不需要这套改造。

## 8. 落到 panda_v2：已落地 / 仍未做

这一节原先写的是「接 H5 需要动哪里」的差距表（H5 与小程序是**同一渠道下的两种协议**，不是
「加一个 createPath」）。**那一刀已经落地**，所以改成两段：做完了什么、以及有意没做什么。
改动落在 `internal/provider/ums/`、`internal/provider/httpx/`、支付服务的回跳入口，以及
`internal/catalog` 里那四条 H5 路径。（**别再去找种子文件**：这件事当初落在
`deploy/dev-seed/payment_ums_h5_channel.sql` 上，但那个目录连同两份种子一起删了，
H5 的配置今天是环境变量，见下面第 9 条。）

### 8.1 已落地

| # | 项 | 落点 |
| --- | --- | --- |
| 1 | **H5 下单四路径**（支付宝 / 微信 / 云闪付 / 微信转小程序）：GET、参数全拼 URL、`OPEN-FORM-PARAM`、`instMid=H5DEFAULT`、无 `tradeType`、**不跟重定向** | `ums/create.go` `createH5`、`httpx.go` `NoFollowRedirect` |
| 2 | **3xx + `Location` = 成功**，产出 `PayParams{"h5Url":…}`（客户端按 `action` 认这个键，跳收银台）；2xx 无 `Location` 落「结果不明」，交给既有的查单探针 | `ums/create.go` `interpretH5Create` |
| 3 | **form 体回调**：按内容（而不是 `Content-Type`）认 JSON/form，再走既有解码路径 | `ums/notify.go` |
| 4 | **入站 key 拼接验签**（这套协议里入站与出站是两套）：参数按 ASCII 排序、`k=v` 以 `&` 连、**末尾直接接通讯密钥**、按 `signType` 取 MD5 或 SHA256、hex 大小写归一后比较 | `ums/keyappend.go` |
| 5 | **结果页回跳**：新 `GET /v1/payments/return/{channelCode}`，验签 → 302 到结果页，**这条路一行都不写 payments** | `service/return.go`、`controller/return.go`、`routes/payment.go` |
| 6 | **第二把凭据**（通讯密钥）走既有的槽机制；解不到就 `ErrSecretNotConfigured`（fail closed），**不降级成跳过验签** | `ums.go` `SecretSlots`、`ums/config.go` |
| 7 | **退款 / 退款查询 / 关单**：适配器层实现（`provider.Operator` 可选接口），**服务层不接线** | `ums/operator.go` |
| 8 | 查单在这条路线上免改，补测试钉住 | `ums/query.go` |
| 9 | **配置只走环境变量**（`PAYMENT_UMS_*` 九行 + 两把密钥，见 `deploy/config/.env.example`），四条 H5 路径写在 `catalog` 的常量表里。原先那份种子（一行渠道 + 四条 `action='h5'` 的支付方式）随 `payment_channels` / `payment_methods` 两张表一起删了——**今天没有种子、也没有后台页** | `deploy/config/.env.example`、`internal/catalog` |

第 3 与第 4 条合起来才是原来那份差距表里**最危险的一条**：回调入口原先只 `json.Unmarshal`，
而通知是 form 编码。装上 H5 之后它不是「少一个功能」，而是**所有 H5 回调都被判成「签名对但
报文不完整」而拒收**——日志里满是 `ErrInvalidNotification`，看着像渠道在乱发，实际是我们在用
JSON 解析器读表单；结果是钱收了、支付单停在 pending、最后被超时关单。

### 8.2 仍未做（都是有意不做的）

- **退款的业务闭环**（订单侧售后 → 退款单 → 渠道）：适配器能发退款请求，但**没有调用方**——
  `payment_refunds.after_sale_no` 是 `NOT NULL UNIQUE`，写入方只能是订单侧的售后单，
  而那条聚合还没建。没有聚合就先造一条「能发起退款」的服务层路径，是在没有聚合的地方造聚合。
- **担保撤销 / 担保完成**：我们从不发 `secureTransaction=true` 的单，这两个接口**没有输入可操作**；
  而且 `payment_provider_calls.operation` 的 CHECK 里没有对应的值，要接得先改迁移。
- **异步分账与其后续确认**（`asynDivisionFlag` / `/v1/netpay/sub-orders-confirm`）：我们走的是
  **同步**那条（`divisionFlag`），随下单一次下发、发出去就是终局，没有子单要确认。同步那三个
  字段（`divisionFlag` / `platformAmount` / `subOrders`）**已经加在 `createBody` 与
  `h5CreateBody` 上了**（见 `create.go` 的 `divisionFields`），要补的是异步那一条。
- **分账结果怎么回来，本文档没有答案**：第 4 节讲支付结果通知时一个字都没提应答里的分账字段。
  今天的口径与老系统一致——**支付成功即视作分账成功**（`repository/callback.go` 的
  `succeedSettlementInTx`），不引入查分账。渠道实际分账失败而我们显示成功，是这条口径已知的代价。
- **`OPEN-ACCESS-TOKEN`**：token 1 小时、同 appId 最多 10 个、文档要求中控服务器；没有那台中控不接。
- **关单能力实现了但不接进过期 worker**：报文里带了 `expireTime`，渠道侧那张预支付单**自己会过期**，
  所以 `worker/expiry.go` 里「不调渠道关单」的理由在这条路上依然成立。

## 9. 文档自身的坑（读原文时会撞上）

1. **商户订单号的长度自相矛盾**：正文第 3 页说「总长度需大于 6 位，**小于 28 位**」，
   而字段表里 `merOrderId` 写的是 **`6..32`**。更麻烦的是它推荐的生成规则
   `{4位来源编号}{17位时间}{7位随机数}` 算出来**正好 28 位**——按推荐规则生成的号
   刚好越过正文那个上限。落地时以**字段表的 6..32** 为准，并且**别超过 32**。
2. **推荐规则里的时间格式是错的**：`yyyyMMddmmHHssSSS` 里 `mm`（月）出现了两次。
   应是 `yyyyMMddHHmmssSSS`（17 位）。
3. **错误码 `INVLID_MERCHANT_CONFIG` 是文档自己的拼写错误**（少一个 A）。抄进代码时
   如果按正确拼写 `INVALID_MERCHANT_CONFIG` 去比对，永远匹配不上原文。
4. **订单关闭的示例报文是从扫码那条抄来的**：请求里是 `qrCodeId`、`instMid=QRPAYDEFAULT`，
   与本文档其余部分的 `H5DEFAULT` 不一致。示例不可当依据。
5. **三个签名不是同一个东西**：`OPEN-BODY-SIG`（出站）、`OPEN-FORM-PARAM`（GET 出站）、
   结果页/通知的 **key 拼接签名**（入站）。接的时候一定要按「这个签名是谁算给谁验的」分开实现，
   合并成一个函数的后果是其中一条永远验不过。

## 10. 实测：定稿了什么、还剩什么

这一节原先列的是「拿真实凭据后必须先确认」的清单。**2026-09-20 用沙箱测试凭据打了一轮**
（凭据只以环境变量注入本机进程，没有进仓库、也没有进任何配置文件），下面是结果。

### 10.1 已定稿

1. **下单跳转是 302，不是 200 + HTML**。四条下单路径各打一遍，**四条都回 `302` + `Location`
   指向 `qr-test2.chinaums.com` 的收银台**，没有一条回 200 + HTML（位置里还带回了 `msgId` /
   `merOrderId` / `openAppId` / `msgSrc` / `msgType` 与渠道自己算的一个 `sign`）。
   所以 §8.1 第 2 条那条「3xx + Location = 成功」是主干；「200 + HTML」那一档留着当防线
   （渠道哪天改了，落「结果不明」而不是失败——判成失败会让用户去付第二遍）。

   顺带定稿的两个形状细节：
   - **认证参数里的 `timestamp` 是紧凑的 `yyyyMMddHHmmss`**，与报文 `content` 里的
     `requestTimestamp`（`yyyy-MM-dd HH:mm:ss`）**不是同一个格式**。写成带短横线的那种，
     渠道回 `errCode=1000 / errInfo=Timestamp解析失败`——两个时间在日志里看着都对，
     这是最容易在联调时耗掉半天的坑。
   - **`content` 必须按 `java.net.URLEncoder` 那一套编码**（空格 → `+`）。用 RFC 3986 的
     `%20` 会得到同一个「Timestamp解析失败」（`ums.go` 里选 `url.QueryEscape` 而不是
     `PathEscape` 的理由，实测成立）。

2. **失败应答的说明字段是 `errInfo`，不是 `errMsg`**（H5 这条协议）。
   一条格式不对的请求回的是 `{"errCode":"1000","errInfo":"Timestamp解析失败"}`，而 POST+JSON
   那条回的是 `errMsg`（查单的 `NO_ORDER` 那一份就是）。适配器两个都认、`errMsg` 优先
   （`ums/create.go` 的 `providerMessage`）。只读 `errMsg` 不会崩，只会让运维在流水里
   看到「provider returned http 412」加一个数字错误码，而渠道明明把原因写给了我们。

3. **退款查询的路径定稿：`/v1/netpay/refund-query`**（这一版开放平台的路径），老系统那条
   `/v1/netpay/trade/refund/query` 在这台沙箱上**直接 404**。假单号打过去，前者得到的是
   业务层应答 `200 + errCode=NO_ORDER「无法找到指定的订单」`。所以这一条**不该被配成别的值**
   ——今天它写在适配器自己的默认值里（`defaultRefundQueryPath`）。`catalog` 拼出来的那棵树里
   根本没有 `endpoints` 段，所以这条路径只能走默认值（早先它由运营在渠道 config 里填）。

4. **退款 / 退款查询 / 关单三条的报文形状被收下了**。三条都用假单号打，得到的都是
   `200 + NO_ORDER`——不是「参数不合法」、也不是签名错。这证明路径、字段集与出站签名
   （`OPEN-BODY-SIG`）在这一版渠道上都对。**它不证明这几条操作在真实单据上的语义**
   （见 10.2）。

### 10.2 仍未确认（要有真实交易才能看）

1. **回调到底带不带签名**。老系统的 `ParseNotify` 是个空壳（注释写着「验签（可选）」，函数体只有
   一句 `json.Unmarshal`），真正在收回调的入口**连那个空壳都没调**。我们在 V2 里按 key 拼接做了
   实现（`ums/keyappend.go`），但那**是推断不是事实**——没真付一笔就看不到回调。今天的行为是
   **fail closed**：验不过就按失败拒收（`payment_notifications` 落一行 `failed`，支付单不动）。
   要确认的是：通知的签名是第 4 节那套 key 拼接，还是 `OPEN-BODY-SIG`，还是根本没有。
2. **回调的报文体真是 form 编码吗**。文档这么写，但老系统两种都收（按 Content-Type 分流），
   说明真实投递可能两种都出现过。适配器按**内容**认，两种都能收。
3. **成功回调是否必带 `totalAmount`**。`repository/callback.go` 会**无条件**拿金额与支付单比一遍，
   对不上就整条拒收——若真实回调不带金额，要么回调全被拒，要么得回来重新讨论
   `Notification.Amount` 的语义（而不是在适配器里糊一个 0）。
4. **下单应答里有没有 `targetOrderId`**。小程序那条路上老系统只校验 `miniPayRequest` 就返回了，
   所以我们建单时**不记渠道交易号**（`ums.go` 的已知缺口），后果是回调那一道
   「渠道交易号对不上就拒」的防线在这条路上是空转的。H5 那条路连应答报文都没有（只有 302），
   这一条对它更没戏——得靠查单。
5. **退款查询里多带的 `refundOrderId` 渠道认不认**。一笔支付退过两次时，那份应答说的是哪一次？
   假单号那一轮得到的是 `NO_ORDER`，说明报文被收下了，但那多半发生在参数校验之前，
   结不了这个分歧（见 `ums/operator.go` 里那段）。

### 10.3 联调时撞到的一个渠道侧现象（记下来，不是我们的 bug）

沙箱**偶发**回 `HTTP 412 + errCode=9200`：同一形状的下单请求，隔几秒重试就成功了
（一轮里 12 次下单出现 3 次）。适配器按「4xx = 协议层拒了、没建单」把它判成 `failed`
（`interpretH5Create` 的既定判据），所以**一次渠道侧的抖动会变成一笔明确的支付失败**。

这一条留在这里当观察记录，**没有改判据**：errCode `9200` 的语义没有文档可依，
把 4xx 一律改成「结果不明」会让真正的参数错误（配置写错时用户能立刻换一种方式付）
变成一次超时关单。要收口得先拿到 9200 的定义，或者看到它在生产上的频率。
