package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
)

// 这个文件只验路由表本身：哪个方法落在哪个处理器上、带的是哪个权限码。处理器不会被真正
// 调用——授了权的那个中间件在进处理器之前就短路了——所以控制器可以带 nil 服务，整个用例
// 不碰数据库、不发一条 SQL。
//
// 它挡的四件事：
//
//  1. **路径注册顺序**。路由是往表里追加而不是按路径合并，同一条路径注册两次的话先注册的
//     那条会吃掉所有方法——表现出来是 GET 好用、别的静默 404。这里每条路径都得各自命中
//     （418 而不是 404）。
//
//  2. **支付单被顺手加上写入口**。那是钱的既成事实，关单 / 改状态 / 重放回调都要先有退款
//     与对账的语义。给支付单加一个「关单」按钮时，最容易的写法就是在 routeFor 的表里加一行
//     ——那一刻下面 TestAdminRoutesUnlistedMethodIsNotFound 会红。
//
//  3. **支付方式与渠道那两页配置面又被加回来**。它们随着「方式收成代码里的常量」一起没了，
//     而「加回一个能改支付方式的后台页」在今天是一条没人需要的路：方式不再是数据，加回来
//     就得先有数据。下面那组路径全部 404 就是这条的证据。
//
//  4. **忘了装配授权中间件**变成失败开放。这是四条里唯一会造成真实损失的：后台支付单里有
//     用户 id、金额、渠道交易号。
//
// 支付单号不是 uuid（PAY+时间+随机后缀），所以这里用真实形状的单号。
const aPaymentNo = "PAY20260916140744000053049"

// aSettlementID 是分账那几条路径上的 id 形状：规则、账户、任务的主键都是 uuid。
//
// 它必须是**合法 uuid**：控制器在读路径参数时先 parse（见 controller.parsePathUUID），一个
// 形状不对的 id 会回 400 而不是走到处理器——那样这条用例验的就成了「参数校验」而不是
// 「哪个方法挂哪枚码」。
const aSettlementID = "6f5c0b7a-2d31-4a8e-9b44-1c7de2a5f903"

// probeAuthorizer 返回一个假的 authenticate：它记下每个请求被要求持有的权限码，
// 然后回 418 并**不调用**处理器。418 是「路由命中了、权限码是对的」的信号，
// 404 是「没匹配上」，两者都不依赖服务层的实现。
func probeAuthorizer(seen *string) func(...string) func(http.Handler) http.Handler {
	return func(permissions ...string) func(http.Handler) http.Handler {
		permission := ""
		if len(permissions) > 0 {
			permission = permissions[0]
		}
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*seen = permission
				w.WriteHeader(http.StatusTeapot)
			})
		}
	}
}

// probeServer 建一个挂好探针的服务端，返回它和「上一个请求要的权限码」那个出参。
func probeServer(seen *string) *khttp.Server {
	server := khttp.NewServer()
	RegisterAdmin(runtime.NewHTTPRouter(server),
		controller.NewAdminPaymentController(nil, nil),
		controller.NewAdminSettlementController(nil),
		probeAuthorizer(seen))
	return server
}

func TestAdminRoutesMapEachMethodToItsPermission(t *testing.T) {
	var seen string
	server := probeServer(&seen)

	cases := []struct{ method, path, permission string }{
		// —— 支付单：只读 ——
		{http.MethodGet, "/v1/admin/payments", permRead},
		{http.MethodGet, "/v1/admin/payments/" + aPaymentNo, permRead},

		// —— 分账：读那一半 ——
		//
		// 任务与渠道是**纯只读**（下面还有一条用例断言它们的写方法全是 404），规则与账户
		// 的 GET 与写共用同一个 handler，差别只在 routeFor 那张表里挂的码——所以这四条
		// 必须各自出现在这里：它们是「GET 没有顺手挂成 manage」的唯一证据。
		{http.MethodGet, "/v1/admin/settlement/rules", permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/rules/" + aSettlementID, permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/accounts", permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/accounts/" + aSettlementID, permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/tasks", permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/tasks/" + aSettlementID, permSettlementRead},
		{http.MethodGet, "/v1/admin/settlement/channels", permSettlementRead},

		// —— 分账：写那一半 ——
		//
		// 六条**逐条**列出来，而不是抽两条当代表：「POST /rules 挂 manage」与
		// 「DELETE /rules/{id} 也挂 manage」是两次独立的声明，漏掉一条的表现是那个入口
		// 只要 read 就能删配置——而 read 是所有客服都有的。
		{http.MethodPost, "/v1/admin/settlement/rules", permSettlementManage},
		{http.MethodPut, "/v1/admin/settlement/rules/" + aSettlementID, permSettlementManage},
		{http.MethodDelete, "/v1/admin/settlement/rules/" + aSettlementID, permSettlementManage},
		{http.MethodPost, "/v1/admin/settlement/accounts", permSettlementManage},
		{http.MethodPut, "/v1/admin/settlement/accounts/" + aSettlementID, permSettlementManage},
		{http.MethodDelete, "/v1/admin/settlement/accounts/" + aSettlementID, permSettlementManage},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			seen = ""
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			if recorder.Code == http.StatusNotFound {
				t.Fatalf("%s %s 没有匹配到任何路由", tc.method, tc.path)
			}
			if recorder.Code != http.StatusTeapot {
				t.Fatalf("%s %s 回的是 %d，期望 418（授权中间件短路）", tc.method, tc.path, recorder.Code)
			}
			if seen != tc.permission {
				t.Fatalf("%s %s 要的是 %q，期望 %q", tc.method, tc.path, seen, tc.permission)
			}
		})
	}
}

// TestAdminRoutesWithoutAuthenticatorDenyEverything 挡的是「忘了装配」变成失败开放。
//
// 少了这条，RegisterAdmin 收到 nil 时整个后台支付面都是公开的——里面有用户 id、金额与
// 渠道交易号。
func TestAdminRoutesWithoutAuthenticatorDenyEverything(t *testing.T) {
	server := khttp.NewServer()
	RegisterAdmin(runtime.NewHTTPRouter(server), controller.NewAdminPaymentController(nil, nil),
		controller.NewAdminSettlementController(nil), nil)

	paths := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/payments"},
		{http.MethodGet, "/v1/admin/payments/" + aPaymentNo},
		// 分账那几条也在这里：写入口（POST/PUT/DELETE）与读入口一视同仁——忘了装配授权
		// 中间件时，连同「改一条规则把钱分给谁」一起公开，是这一棵树里代价最大的那种漏配。
		{http.MethodPost, "/v1/admin/settlement/rules"},
		{http.MethodPut, "/v1/admin/settlement/rules/" + aSettlementID},
		{http.MethodGet, "/v1/admin/settlement/tasks"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("没装配授权中间件时 %s %s 回的是 %d，期望 401", tc.method, tc.path, recorder.Code)
			}
		})
	}
}

// TestAdminRoutesUnlistedMethodIsNotFound 确认路径在、方法不对时是 404 而不是落到某个别的
// 处理器上。
//
// 这一组同时是三件事在路由层的证据：
//
//   - **支付单仍然是只读的**：POST / PUT / PATCH / DELETE 一条都不通。它是钱的既成事实，
//     关单 / 改状态 / 重放回调都要先有退款与对账的语义，而它们本轮没做。验收时会拿 404
//     （而不是 405、更不是 200）当作这条契约的证据。
//   - **配置面已经不在了**：payment-methods / payment-channels 下面每一条路径、每一个方法
//     都是 404——不是「只留了读」也不是「只留了写」，是整棵子树没有注册。支付方式与渠道
//     是代码里的常量，后台没有可改的东西。
//   - 单号里不会出现斜杠，多一段一律不匹配——否则「有人拼错了 URL」会看起来像
//     「这个单号查无此单」，而 404 与 404 在页面上分不开。
func TestAdminRoutesUnlistedMethodIsNotFound(t *testing.T) {
	var seen string
	server := probeServer(&seen)

	const gone = "6f5c0b7a-2d31-4a8e-9b44-1c7de2a5f903" // 一个形状合法的 uuid，早先那两页用它当 id

	cases := []struct{ method, path string }{
		// 支付单：四种写方法一条都不该通。
		{http.MethodPost, "/v1/admin/payments"},
		{http.MethodPut, "/v1/admin/payments"},
		{http.MethodPatch, "/v1/admin/payments"},
		{http.MethodDelete, "/v1/admin/payments"},
		{http.MethodPost, "/v1/admin/payments/" + aPaymentNo},
		{http.MethodPatch, "/v1/admin/payments/" + aPaymentNo},
		{http.MethodDelete, "/v1/admin/payments/" + aPaymentNo},

		// 支付方式与渠道：整棵子树都不在了，读写一起消失。
		{http.MethodGet, "/v1/admin/payment-methods"},
		{http.MethodPost, "/v1/admin/payment-methods"},
		{http.MethodPut, "/v1/admin/payment-methods/" + gone},
		{http.MethodPatch, "/v1/admin/payment-methods/" + gone + "/status"},
		{http.MethodDelete, "/v1/admin/payment-methods/" + gone},
		{http.MethodGet, "/v1/admin/payment-channels"},
		{http.MethodPost, "/v1/admin/payment-channels"},
		{http.MethodPut, "/v1/admin/payment-channels/" + gone},
		{http.MethodPatch, "/v1/admin/payment-channels/" + gone + "/status"},
		{http.MethodDelete, "/v1/admin/payment-channels/" + gone},
		// 协议族规格与试跑也一起没了：只剩一族适配器，没有「选协议族」可讲。
		{http.MethodGet, "/v1/admin/payment-channels/providers"},
		{http.MethodPost, "/v1/admin/payment-channels/dry-run"},

		// 分账任务与渠道：**只读**。任务是一条已经发生的分账，它的状态由回调推进（见
		// service/admin_settlement.go 的说明），没有一个「人工改状态」的入口；渠道更彻底
		// ——它连库都不查，取值来自代码里的目录。这两条的写方法 404 与上面支付单那组是
		// 同一条契约。
		{http.MethodPost, "/v1/admin/settlement/tasks"},
		{http.MethodPut, "/v1/admin/settlement/tasks/" + aSettlementID},
		{http.MethodDelete, "/v1/admin/settlement/tasks/" + aSettlementID},
		{http.MethodPost, "/v1/admin/settlement/channels"},
		{http.MethodDelete, "/v1/admin/settlement/channels/" + aSettlementID},

		// 分账规则项**没有自己的路径**：它们是规则的一部分，整体随规则提交（跨行约束，
		// 见 service/admin_settlement.go）。这一组 404 就是「没有子资源入口」的证据。
		{http.MethodGet, "/v1/admin/settlement/rules/" + aSettlementID + "/items"},
		{http.MethodPost, "/v1/admin/settlement/rules/" + aSettlementID + "/items"},
		{http.MethodPut, "/v1/admin/settlement/rules/" + aSettlementID + "/items/" + aSettlementID},
		{http.MethodDelete, "/v1/admin/settlement/rules/" + aSettlementID + "/items/" + aSettlementID},

		// 结算单与打款那一层本轮没做，`settlement:payout` 也没种：整棵子树没有注册。
		{http.MethodGet, "/v1/admin/settlement/statements"},
		{http.MethodPost, "/v1/admin/settlement/payouts"},

		// 单号里不会出现斜杠，多一段一律不匹配——否则「有人拼错了 URL」会看起来像
		// 「这个单号查无此单」，而 404 与 404 在页面上分不开。
		{http.MethodGet, "/v1/admin/payments/" + aPaymentNo + "/refund"},
		{http.MethodGet, "/v1/admin/settlement/rules/" + aSettlementID + "/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s 回的是 %d，期望 404", tc.method, tc.path, recorder.Code)
			}
		})
	}
}
