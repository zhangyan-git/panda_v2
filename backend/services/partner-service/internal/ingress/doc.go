// Package ingress 是开放平台**入向**那一层：头解析、验签、防重放、访问控制、限流、调用日志。
//
// 它是 partner-service 里唯一一段「每个请求都要走」的代码，所以那五件事合成一条
// middleware（middleware.go），而不是五个可选的中间件——一个能少挂一段的链路，迟早会在某次
// 装配改动里少挂一段，而少挂的那一段不会报错，只会让那件事静默失效。
//
// # 契约
//
//	X-API-Key    {合作方密钥，我们签发}
//	X-Timestamp  {Unix 秒，±5 分钟窗口}
//	X-Nonce      {随机串，服务端 Redis 去重}
//	X-Signature  {见 spec.go}
//
// body 是 JSON，**一层打平**后与 query 一起进待签串（spec.go 的 Params）。path 与 method
// **必须**在待签串里——这是方案 1.4 的第二条：少了 path，同一份签名能跨接口重放；少了
// method，一次 GET 的签名能拿去发一次 POST。
//
// ⚠️ **path 用规范化后的 /v1/... 签，不是 /api/v1/...**。网关会把入站的 `/api` 前缀剥掉
// 再转发（internal/proxy 的 normalizePath），所以本服务看到的永远是 `/v1/openapi/...`。
// 老系统签的是客户端原样发来的路径（`c.Request.URL.Path`），V2 里那条路走不通：同一个接口
// 从 `/api/v1/...` 与 `/v1/...` 进来会算出两个签名，而服务端只看得到其中一个。这条差异是
// 网关存在的结果，写进对接文档，别让合作方自己猜。
//
// # 比老系统多出来的三条（方案 1.4），以及一条比方案多出来的
//
//  1. X-Nonce + Redis SETNX 去重，TTL = 时间窗。老系统只有时间戳，同一秒内同一请求可重复
//     提交。
//  2. path 与 method 进待签串。老系统的设备回调那版（md5(device_id+timestamp+secret)）连
//     报文都没进串。
//  3. body 进待签串（这里是打平后进串，见 spec.go 的说明）。
//  4. **nonce 也进待签串**——方案 1.2 的参数清单里没有它，这是本实现多出来的一条。理由在
//     spec.go 的 Params：不进串的 nonce 挡不住任何东西，攻击者只要把抓到的请求里的
//     X-Nonce 换个值，签名照样成立、去重照样通过。加进去之后「改 nonce」与「改 body」是
//     同一类操作，都必须重签。
//
// # 失败一律不回内部原因
//
// 所有拒绝路径回的都是同一句话（unauthorizedBody），真实原因只进 partner_call_logs 的
// error_code。老系统把「API密钥无效」与「签名验证失败」原样发回去，等于送攻击者一台枚举机，
// 告诉他猜到了哪一步。这里是同一个信息，但只有我们看得到。
//
// # 一个已知缺口
//
// 调用日志只增不删，没有保留期清理（见 migrations/partner 文末）。本包负责写，不负责删。
package ingress
