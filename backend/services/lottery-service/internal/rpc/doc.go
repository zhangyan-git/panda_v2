// Package rpc 是 lottery-service 的 gRPC 面。
//
// # 这个包本轮是空的，而且不是「还没写」
//
// 它是**唯一一个只出向、不入向的服务**：没有任何内部服务需要同步问抽奖域的事实。
//
//   - 谁该参与抽奖？由用户自己在订单完成后点。没有「订单完成了所以服务端替他参与」这条
//     路——方案 §3.1 明确舍弃了按个人累计进度的自动加入，原型也只有用户点确认弹窗之后才
//     扣卡（见 PANDA_V2_REFACTOR_PLAN.md §7.3 的回写）。
//   - 退款要不要追回？那是**事件**驱动的，而本轮不接退款：不配 ConsumerHandler / RABBITMQ_QUEUE
//     （见 cmd/main.go 与 migrations 那边关于「已开奖期次是终局」的规则）。将来要接的时候，
//     进来的是一条 MQ 消息，仍然不是一次 gRPC 调用。
//   - 中奖记录给谁看？给后台与小程序，都是 HTTP。
//
// # 它出向的那一条
//
// 参与要扣福卡，那是 account-service 的 DeductFortuneCards（见 internal/client/account.go）。
// **lottery-service 是 fortune_card.proto 的第一个真实调用方**：那三个 RPC 在此之前只有
// 实现与集成测试，全仓没有一处调用。
//
// # 将来什么时候会有入向 gRPC
//
// 商户端要核销中奖（本轮整块延后）时，会有两种可能的形状：走 HTTP（`/v1/merchant/lottery/...`，
// 与 order / coupon 的商户入口一致），或者由 fulfillment 在出杯时同步问一次「这一杯是不是
// 用中奖凭证换的」。后者才会需要这个包。在那之前，`contracts/proto/lottery/v1/lottery.proto`
// 里的 `service LotteryService {}` 保持空壳——它动了就得重跑 buf generate 并提交产物，
// 而一个没有调用方的契约没有理由动。
package rpc
