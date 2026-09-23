package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
)

// pathVar 读取路径变量。kratos 的 Server.HandleFunc 走 gorilla/mux 路由，
// 路径变量存在 mux.Vars 中，Go 1.22 ServeMux 的 r.PathValue 取不到值。
func pathVar(r *http.Request, key string) string {
	return mux.Vars(r)[key]
}

// decodeJSON 将请求体反序列化为 v，body 过大或格式错误时返回 error
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20)) // 限制 1 MiB
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		return err
	}
	return nil
}

// requireConsumer 取出 C 端身份，失败时已经写好了响应。
//
// 判定必须落在 Realm 上，不能只看 subject == user_id：平台管理员和商户账号
// 的令牌同样满足那两条，只查它们等于把小程序的接口对管理端敞开。缺失和域不对
// 分成 401 与 403：前者是没登录，后者是拿错了身份的令牌，客户端要做的处理不同。
func requireConsumer(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return auth.Identity{}, false
	}
	if identity.Realm != auth.RealmConsumer {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非小程序用户")
		return auth.Identity{}, false
	}
	return identity, true
}

// loginIP 是写进登录审计的客户端地址。
//
// 用 ratelimit.ClientIP 而不是 r.RemoteAddr：后者带端口，同一台机器每次连接
// 都是不同的值，记进审计表就没法按地址聚合。
//
// 它刻意不读 X-Forwarded-For（理由见 platform/ratelimit：那是客户端自己写的头，
// 信它等于把审计字段交给被审计的人填）。代价是经网关过来的请求在这里一律记成
// 网关的地址——这张表现在能回答「什么时候、用什么方式、成没成功」，还回答不了
// 「从哪来」。补上后一半要先定网关与各服务之间的信任边界、由网关盖章转发，
// 那是一次独立的改动，不该顺手做在登录链路里。
func loginIP(r *http.Request) string {
	return ratelimit.ClientIP(r)
}

// nonNilStrings 把 nil 切片换成空切片再交给 JSON。
//
// nil 序列化成 null，空切片序列化成 []，而这一层下面每条路径都已经保证「没有范围」是
// 空切片而不是 nil。若在这里把那个区别漏出去，前端就要为一个不存在的状态写分支，
// 而多写的那条分支迟早会被当成真状态去用。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
