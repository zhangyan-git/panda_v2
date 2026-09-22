package ingress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
)

// ErrNonceUnavailable：去重判定本身做不了（Redis 不可达、没有 Redis、脚本报错）。
//
// 它是**拒绝**的理由，不是放行的理由——见 middleware.go 里它的用法。单独一个哨兵值是为了
// 让「去重坏了」与「这个 nonce 用过了」在调用日志里能分开写：两者的处置相同（都拒），但
// 排查的方向完全相反（一个是运维故障，一个是有人在重放）。
var ErrNonceUnavailable = errors.New("nonce store is unavailable")

// nonceScript 是「SET key 1 NX EX ttl」。
//
// 为什么要走 EVAL 而不是一个 SetNX 调用：platform/cache 的 Client 接口里没有 SetNX（它只
// 承诺 Ping / Close / Publish / Subscribe），唯一能下探到命令层的口子是 ScriptRunner.Eval。
// 这不是将就——SET NX EX 本身就是一条原子命令，包在 EVAL 里只是把它送出去的方式。
//
// 返回 1 = 这一次认领成功（新 nonce），0 = 已经被用过（重放）。
const nonceScript = `if redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[1]) then return 1 else return 0 end`

// NonceStore 认领一个 nonce。
//
// Claim 返回 (true, nil) 表示「这个 nonce 第一次出现」；(false, nil) 表示它已经被用过；
// 任何 error 都表示**判定没做成**，调用方必须拒绝请求（失败关闭），绝不能当成「通过」。
type NonceStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// NewNonceStore 建一个基于 Redis 的 nonce 去重器，并**在构造时**把两件事查清楚：
//
//  1. 传进来的客户端确实能跑脚本（cache.Noop 不能）。REDIS_ADDR 为空时
//     cache.NewFromEnv 返回的正是 Noop——这是本地 .env 里最容易踩的一格，也是这个构造函数
//     存在的全部理由：不在这里拦，它会在**第一次合作方调用**时变成一个「去重永远通不过」
//     （失败关闭会拒掉所有请求）或者更糟——一个悄悄跳过去重的实现。
//  2. Redis 真的应答（Ping）。地址写错、容器没起、密码不对，都要在启动时说话。
//
// 老系统没有这一层，方案 1.4 第 1 条要的就是它：Redis 连不上 → 跳过 nonce 检查是一个静默
// 的空操作，而一个静默失效的防重放比没有防重放更危险——它让人以为自己有。
func NewNonceStore(ctx context.Context, client cache.Client) (NonceStore, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: no cache client configured (set REDIS_ADDR)", ErrNonceUnavailable)
	}
	runner, ok := client.(cache.ScriptRunner)
	if !ok {
		// 这句话要能直接指导修复，所以它点名 REDIS_ADDR：Noop 只有一种来路。
		return nil, fmt.Errorf("%w: the cache client cannot run scripts (REDIS_ADDR is empty, so cache.Noop is in use); the open API needs a real Redis for nonce de-duplication", ErrNonceUnavailable)
	}
	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("%w: ping redis: %v", ErrNonceUnavailable, err)
	}
	return &redisNonceStore{runner: runner}, nil
}

type redisNonceStore struct{ runner cache.ScriptRunner }

// Claim 实现 NonceStore。
//
// key 由调用方拼好（见 middleware.go 的 nonceKey）：它必须包含 api_key_id，否则两个合作方
// 恰好用了同一个随机串时会互相把对方拒掉——那不是安全问题，是一次很难查的误伤。nonce 由
// 合作方生成，它随机到什么程度是对方的实现问题，不能拿来当我们键空间的一部分。
func (s *redisNonceStore) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	seconds := int(ttl.Seconds())
	if seconds < 1 {
		// 秒是最小粒度。TTL 小于一秒时 SET 的 EX 0 会被 Redis 直接拒绝（invalid expire
		// time），报出来是一句与真正原因无关的语法错。向上取整到 1 秒：短于窗口的 TTL
		// 本来就不该出现（见 Window 的注释），这里只是不让它变成一次语法错误。
		seconds = 1
	}
	raw, err := s.runner.Eval(ctx, nonceScript, []string{key}, seconds)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrNonceUnavailable, err)
	}
	// 脚本返回整数 1/0；go-redis 把它解成 int64。返回 nil 只可能是「键不存在」之类的
	// 空回复，那不是认领成功，一律按失败处理。
	claimed, ok := raw.(int64)
	if !ok {
		return false, fmt.Errorf("%w: unexpected reply %T", ErrNonceUnavailable, raw)
	}
	return claimed == 1, nil
}

// nonceKey 拼出 Redis 键。
//
// 前缀带版本（v1）：将来若把「一个 nonce」改成「一把密钥一个 nonce」之类的语义，旧键不会
// 与新键撞在一起——它们只是各自过期。
func nonceKey(apiKeyID, nonce string) string {
	return "partner:nonce:v1:" + strings.TrimSpace(apiKeyID) + ":" + strings.TrimSpace(nonce)
}
