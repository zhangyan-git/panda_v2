/**
 * 开放平台（合作方）那些枚举码的中文文案。
 *
 * 前两张表来自 partner-service 的 model 常量（也就是 migrations/partner 各表的 CHECK 约束），
 * 第三张来自 ingress 的内部错误码常量——三张都是**封闭词表**，界面上出现一个没登记的码就
 * 说明迁移或 ingress 加了码而这里没跟上（见 services/labels.ts 的 enumMeta：它退回显示原始码，
 * 不显示空白，就是为了让那件事看得见）。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts。
 */

import type { EnumMeta } from './labels';

/** partner_accounts.status：这家合作方还能不能调。 */
export const PARTNER_STATUS: Record<string, EnumMeta> = {
  enabled: { text: '启用', color: 'success' },
  // 停用是**连坐**的：名下所有密钥（含状态还是启用的那些）从下一个请求起全部被拒。所以
  // 这一格用红色——它不是「这一行不生效」，是「这家公司的整条链路断了」。
  disabled: { text: '停用', color: 'error' },
};

/**
 * partner_api_keys.status：**这一把**钥匙能不能用。
 *
 * 与合作方那两个值同值（都是 enabled / disabled），但刻意分成两张表而不是共用一张：
 * 「这家合作方停用」与「这一把密钥停用」在页面上要能一眼分开——前者是整条链路断了，后者
 * 通常是我们主动掐掉一把怀疑泄露的钥匙。共用一张表的那天，有人会把其中一句文案改得更贴切，
 * 于是另一个场景就错了。
 */
export const API_KEY_STATUS: Record<string, EnumMeta> = {
  enabled: { text: '启用', color: 'success' },
  disabled: { text: '停用', color: 'default' },
};

/**
 * partner_call_logs.error_code：这次调用**在我们这边**是被哪一条挡下的。
 *
 * 给运营看，不出现在调用方的任何响应里（防枚举：对方拿到 SIGNATURE_MISMATCH 就等于确认
 * 了密钥前缀是对的）。所以这张表存在的意义就是把「对方说的 401」还原成一句话。
 *
 * **取值必须与 partner-service internal/ingress 的 errorCodes 逐字一致**：它是一套只有那个
 * 包会写的封闭词表，筛错一个码不会报错，只会永远筛出空列表。后台没有「列出错误码」的接口
 * （那个清单只用于服务端校验），所以这里是它的第二份，partnerLabels.test.ts 钉着这一份的
 * 全集——那边加码，这里会红。
 */
export const PARTNER_ERROR_CODE: Record<string, EnumMeta> = {
  // 认证段：头都没带全、或者那把钥匙我们认不出来。
  MISSING_CREDENTIALS: { text: '没带齐凭据', color: 'default' },
  UNKNOWN_API_KEY: { text: '密钥不存在', color: 'default' },
  CREDENTIAL_LOOKUP_FAILED: { text: '凭据查询失败', color: 'error' },
  API_KEY_DISABLED: { text: '密钥已停用', color: 'warning' },
  API_KEY_EXPIRED: { text: '密钥已过期', color: 'warning' },
  // 合作方那一层：停用是连坐的，这两条是「整家公司被拒」。
  PARTNER_DISABLED: { text: '合作方已停用', color: 'warning' },
  PARTNER_EXPIRED: { text: '合作方已过期', color: 'warning' },
  // 签名与重放。
  TIMESTAMP_OUT_OF_RANGE: { text: '时间戳超窗', color: 'warning' },
  SIGNATURE_MISMATCH: { text: '签名不匹配', color: 'error' },
  NONCE_REPLAYED: { text: '重放（nonce 用过）', color: 'error' },
  NONCE_STORE_UNAVAILABLE: { text: 'nonce 存储不可用', color: 'error' },
  // 来源与额度。
  IP_NOT_ALLOWED: { text: '来源 IP 不在白名单', color: 'warning' },
  RATE_LIMITED: { text: '被限流', color: 'warning' },
  // 下面两条要分开看：RATE_LIMIT_DEGRADED 是**限流器没建起来、这次放行了**，它会出现在一条
  // 状态码 2xx 的记录上。共用 RATE_LIMITED 那句话会让人以为「这次被挡了」，而它其实成功了。
  RATE_LIMIT_DEGRADED: { text: '限流器降级（未拦截）', color: 'gold' },
  // 报文段。
  BODY_UNREADABLE: { text: '报文读不出来', color: 'warning' },
  BODY_TOO_LARGE: { text: '报文超过上限', color: 'warning' },
  SECRET_UNREADABLE: { text: '密钥解不开', color: 'error' },
};
