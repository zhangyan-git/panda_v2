package routes

import (
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
)

// RegisterPayment 挂载支付服务面向渠道的 HTTP 路由。
//
// 四条路由，都是**渠道打进来的**，所以都不套任何中间件——这是本仓库里仅有的四条这样的入口，
// 理由见包注释：它们的凭据是签名，不是我们的令牌，验签在 service 里做。四条的差别在方向：
//
//   - `POST /callback/{channelCode}`（callback.Callback）—— 渠道的支付结果通知，**会改支付
//     状态**，应答形状由适配器决定（渠道要认它才停止重投）。
//   - `POST /agreement-notify/{channelCode}`（agreementNotify.Notify）—— 渠道的协议变更通知
//     （签约 / 解约），**会改协议状态**。它与上一条是并排的两条而不是一条：报文形状、判据、
//     落库的表都不同（见 service.HandleAgreementNotification 的文件头）。
//   - `POST /agreement-charge-notify/{channelCode}`（agreementChargeNotify.Notify）—— 渠道
//     的**扣款结果**通知（某一期扣到了没有），改的是 payment_agreement_charges 那一行。
//     它是「受理不等于扣到钱」那句话的落点：没有它，发起过的扣款永远停在 charging 上。
//   - `GET /return/{channelCode}`（returns.Return）—— 浏览器被渠道送回的结果页回跳，
//     **一行库都不写**（见 service.HandleReturn），应答是给人看的。
//
// 四条都挂在这里而不是拆成几个 Register：它们对「身份从哪来」的答案逐字相同（都答「签名」），
// 而 routes 包那两条注册入口的分界线正是这个答案（见包注释里与 RegisterAdmin 的对比）。
// 按「谁调用」再拆一层只会让那条分界线变模糊。
//
// 路径模板里的 {channelCode} 是给注册与指标用的（运行时中间件从路径模板读 operation 与
// path 标签，不读 r.URL.Path），真正被解析的是控制器的前缀裁剪。两处共用同一个常量，
// 改了前缀不会一边改一边忘。
func RegisterPayment(r *runtime.HTTPRouter, callback *controller.CallbackController,
	returns *controller.ReturnController, agreementNotify *controller.AgreementNotifyController,
	agreementChargeNotify *controller.AgreementChargeNotifyController) {
	r.HandleFunc(controller.CallbackPath+"/{channelCode}", callback.Callback)
	r.HandleFunc(controller.AgreementNotifyPath+"/{channelCode}", agreementNotify.Notify)
	r.HandleFunc(controller.AgreementChargeNotifyPath+"/{channelCode}", agreementChargeNotify.Notify)
	r.HandleFunc(controller.ReturnPath+"/{channelCode}", returns.Return)
}
