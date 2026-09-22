import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 开放平台（合作方接入）的接口。字段名与 partner-service internal/dto/partner.go 的 json tag
 * 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处，否则 TypeScript 不报错、
 * 只是整列空白。
 *
 * # 全仓库唯一一处明文密钥，就在这个文件的 issueAPIKey 上
 *
 * `POST /partners/{id}/keys` 的响应（APIKeyIssued）里带 `apiKey` 与 `secret` 两个明文，别的
 * 接口一个都没有：列表与详情只回掩码，库里存的也只有掩码列加一个密文信封。**刷新页面之后
 * 就拿不到了，这是设计不是缺陷**——要换一把就重新签发，然后停用旧的那把。
 *
 * 所以这个函数的返回值**不许**并进任何列表 state 或缓存（那会让它在下一次 load() 之后
 * 仍然活着，看起来像「随时都能查」，而它其实只出现过一次）。调用方拿到之后要做的是当场
 * 展示、然后丢掉，见 pages/partners/IssuedKeyModal.tsx 顶部那段说明。
 *
 * # 两种写语义对应两组函数
 *
 * PUT 是**整份覆盖**（没带的字段就是清空，表单是全字段提交的），PATCH 只改 status——启停与
 * 「改个联系电话」不是同一种提交。合作方尤其明显：停用一家会让它名下**所有**密钥当场失效，
 * 那件事不该被塞进一次顺手保存里（见 routes/admin.go 上那句注释）。
 */

// ——— 枚举 ———
// 取值来自 partner-service 的 model 常量（也就是 migrations/partner 各表的 CHECK 约束），
// 不是接口给的——接口回的就是库里那些英文码。文案表在 services/partnerLabels.ts。

/** partner_accounts.status：这家合作方还能不能调。 */
export type PartnerStatus = 'enabled' | 'disabled';

/** partner_api_keys.status。与合作方那两个值同值，但**不是同一个概念**（见 partnerLabels）。 */
export type APIKeyStatus = 'enabled' | 'disabled';

// ——— 合作方 ———

/**
 * 一行合作方。**列表与详情是同一个形状**（后端也只有一个 dto），所以这里的类型两者共用。
 *
 * `expiresAt` 是 null 表示不过期，页面上显示成「长期有效」而不是一个空日期——那两件事在
 * 排查时不一样：一个是「签到了某天」，一个是「一直有效」。
 *
 * `keyCount` **含已停用的密钥**，而且只在列表那条查询上算得出来（写接口的响应里恒为 0，
 * 见 dto 的注释）。含已停用是有意的：停一把之后总数不变，才知道那把还在、能重新启用。
 */
export type Partner = {
  id: string;
  code: string;
  name: string;
  contactName: string;
  contactPhone: string;
  contactEmail: string;
  description: string;
  status: PartnerStatus;
  expiresAt: string | null;
  keyCount: number;
  createdAt: string;
  updatedAt: string;
};

/**
 * 新增 / 修改合作方的请求体。**新增与修改共用这一份**（后端也是同一个 struct）。
 *
 * `code` 只在**新增**时被采纳：修改时它只用来核对「你要改的是不是同一行」，传回来的与库里
 * 不一致会 400（编码不可修改）。它是合作方的稳定标识，接口签名与对账里都用它。
 *
 * `expiresAt` 传 null（或空串）就是**清空有效期 = 不过期**。PUT 是整份覆盖，所以表单里把
 * 日期清掉再保存，是「改成永久有效」而不是「这次不改这一项」。
 */
export type PartnerInput = {
  code: string;
  name: string;
  contactName: string;
  contactPhone: string;
  contactEmail: string;
  description: string;
  expiresAt: string | null;
};

// ——— 密钥 ———

/**
 * 一把密钥。**只有掩码，没有明文**——后端读路径上一次解密都不做（库里根本没有明文可算）。
 *
 * `ipWhitelist` 空数组表示**不限制来源**（不是全拒），见 migrations/partner/001 上那一列的
 * 注释。后端回显的是运营自己填的原串（不做归一化），所以这里也原样显示。
 */
export type PartnerAPIKey = {
  id: string;
  partnerId: string;
  name: string;
  /** 「前 4 + … + 后 4」，X-API-Key 那一半。 */
  apiKeyMask: string;
  /** 同形状，签名密钥那一半。 */
  secretMask: string;
  status: APIKeyStatus;
  expiresAt: string | null;
  ipWhitelist: string[];
  /** 每分钟额度。库里 CHECK (> 0)，正常值不会是 0。 */
  rateLimitPerMinute: number;
  lastUsedAt: string | null;
  /** 累计调用次数（含被拒的）。 */
  callCount: number;
  createdAt: string;
  updatedAt: string;
};

/**
 * 签发响应：**全仓库唯一一处带明文密钥的形状**。
 *
 * 它内嵌 PartnerAPIKey 而不是另起一套字段（后端也是内嵌）：列表里那一行与这里的这一行必须
 * 是同一份事实的两面，否则迟早出现「新建之后响应里的掩码与列表里的对不上」。
 *
 * `apiKey` 是 X-API-Key 的值（公开标识，明文入库）；`secret` 是签名密钥，**只在这一刻存在**。
 */
export type APIKeyIssued = PartnerAPIKey & {
  apiKey: string;
  secret: string;
};

/**
 * 新增 / 修改密钥的请求体。
 *
 * 这里**没有 secret 字段**，而且不是「读接口不回显」那条规矩：签名密钥从来不由调用方提供，
 * 它由服务端生成。让调用方传一个进来，等于他能自己挑一个弱密钥而我们看不出来。
 *
 * `rateLimitPerMinute` 传 0 表示「没填」，服务端用默认额度（60/分钟）；负数才是错误。
 */
export type APIKeyInput = {
  name: string;
  expiresAt: string | null;
  ipWhitelist: string[];
  rateLimitPerMinute: number;
};

// ——— 调用日志 ———

/**
 * 一条调用记录。这是这一域唯一的**只增**表，也是本域唯一一条会随量增长的读接口。
 *
 * 报文（requestBody / responseBody）原样给后台：这张表存在的全部理由就是「合作方说他发了、
 * 我们说他没发」时能对上一行。两段都已被 ingress 截断（8 KiB，截断处带显式标记），前端不再
 * 二次裁剪——裁出来的截断点比实际存储的多一处，查起来更绕。
 *
 * 认不出调用方的那几条（密钥查不到、头都没带）`partnerId` 是全零 UUID、`partnerName` 是
 * 空串——它们仍然在列表里，因为那正是「有人在试」的痕迹，所以**不要把它们过滤掉**。
 *
 * `errorCode` 是我们内部的失败原因，**不出现在给调用方的任何响应里**（防枚举）：运营能看到
 * 「这次是 SIGNATURE_MISMATCH」，对方只能收到一句 401。
 */
export type PartnerCallLog = {
  id: number;
  partnerId: string;
  partnerName: string;
  apiKeyId: string;
  apiKeyMask: string;
  method: string;
  path: string;
  query: string;
  requestIp: string;
  requestBody: string;
  responseBody: string;
  statusCode: number;
  durationMs: number;
  errorCode: string;
  createdAt: string;
};

// ——— 查询参数 ———

/**
 * 合作方筛选。
 *
 * `keyword` **同时匹配编码与名称**（后端 ILIKE、不区分大小写，一个框搜两个字段）：运营手上
 * 要么是编码（对接方报过来的），要么是名字（他自己起的），让他先选搜哪个字段只是多一步。
 */
export type PartnerQuery = PageQuery & {
  keyword?: string;
  status?: PartnerStatus;
};

/**
 * 调用日志筛选。
 *
 * `from` / `to` 是 `created_at` 的**闭区间**两端，必须是带时区的 RFC3339（用 toRFC3339 转）。
 * 它们是分别解析的，不校验先后：start > end 只会返回空列表，那不是错误。
 *
 * `statusCode` 只判形状不判范围——「abc」回 400（形状），「999」回 400（不在 100..599）。
 */
export type CallLogQuery = PageQuery & {
  partnerId?: string;
  apiKeyId?: string;
  errorCode?: string;
  statusCode?: number;
  from?: string;
  to?: string;
};

// ——— 读 ———

/**
 * 合作方列表，**分页**。
 *
 * 后端默认每页 20、上限 200（api.MaxPageSize）。这一页用 ProTable 的服务端分页，所以 page 与
 * pageSize 由表格给，不传满页参数：合作方会随接入方增长，一次拉全量迟早会撞上那条上限，
 * 而撞上的表现是「列表里少了一些合作方」——不报错。
 */
export async function listPartners(params?: PartnerQuery) {
  return request<PageResult<Partner>>('/api/v1/admin/partners', { params });
}

/**
 * 合作方详情。**密钥抽屉每次打开时调用它**，理由写在 KeysDrawer.tsx 里：抽屉里会改 keyCount
 * 与 status，而列表上那一行是打开抽屉那一刻的快照。
 */
export async function getPartner(id: string) {
  return request<Partner>(`/api/v1/admin/partners/${id}`);
}

/**
 * 调用日志列表，**分页**。这一域唯一一条会随量增长的读路径，所以筛选项都在服务端。
 *
 * 时间两端为空时后端不过滤那一端（不是回空列表）。
 */
export async function listPartnerCallLogs(params?: CallLogQuery) {
  return request<PageResult<PartnerCallLog>>('/api/v1/admin/partner-call-logs', { params });
}

// ——— 写：合作方 ———
//
// 三个错误码不在这一层处理，由页面把后端的 errorMessage 原样弹出来（requestErrorMessage）：
// 409 是编码撞车、400 是「编码不可修改」或某个字段不合法、404 是这一行已经不在了。它们各自
// 对应一句后端写好的中文，前端再翻译一遍只会得到第二套说法。

/** 新增一家合作方。编码撞车回 409。 */
export async function createPartner(data: PartnerInput) {
  return request<Partner>('/api/v1/admin/partners', { method: 'POST', data });
}

/**
 * 整份覆盖一家合作方。**没带的字段会被清空**——表单是全字段提交的，拿到的就是看到的那份。
 *
 * 请求体里没有 status：启停走下面那条独立的 PATCH。这不是洁癖——停用会让名下**所有**密钥
 * 当场失效，而那件事不该与「顺手改个联系电话」共用一次提交。
 */
export async function updatePartner(id: string, data: PartnerInput) {
  return request<Partner>(`/api/v1/admin/partners/${id}`, { method: 'PUT', data });
}

/**
 * 启用 / 停用一家合作方。
 *
 * 停用**立即生效，而且是连坐的**：后端验签那条路现查这一行，合作方停了，它名下每一把密钥
 * （含状态还是 enabled 的那些）从下一个请求起全部被拒。所以页面上这个按钮带二次确认，且
 * 确认文案要说出这个后果——恢复时还要逐把检查密钥状态，不是把合作方一开就全好。
 */
export async function updatePartnerStatus(id: string, status: PartnerStatus) {
  return request<Partner>(`/api/v1/admin/partners/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

// ——— 写：密钥 ———

/**
 * 一个合作方名下的全部密钥。**不分页**：后端那条查询也不分页（一把钥匙是发给一家公司的一个
 * 集成用的，现实里是几把），分页器在这里只会是一个永远只有一页的装饰。
 *
 * 只有掩码。明文只在 issueAPIKey 那一次。
 */
export async function listPartnerKeys(partnerId: string) {
  return request<PageResult<PartnerAPIKey>>(`/api/v1/admin/partners/${partnerId}/keys`);
}

/**
 * 签发一把新密钥。**响应里是明文**（见文件头那段），而且只有这一次。
 *
 * 后端回 201；主密钥配置缺失时回 500 且带一句点名 `PARTNER_SECRET_KEY` 的话——那句话是
 * 写给我们自己人看的，页面原样显示，不要再包一层。
 */
export async function issuePartnerKey(partnerId: string, data: APIKeyInput) {
  return request<APIKeyIssued>(`/api/v1/admin/partners/${partnerId}/keys`, {
    method: 'POST',
    data,
  });
}

/**
 * 改一把密钥的访问控制（备注、有效期、IP 白名单、限流）。
 *
 * **改不了密钥本身**：请求体里没有 secret 字段，后端那条 UPDATE 里也没有那两列。所以「怀疑
 * 泄露」的动作只有一个——停用这一把（然后另签发一把），见下面的 status。
 */
export async function updatePartnerKey(partnerId: string, keyId: string, data: APIKeyInput) {
  return request<PartnerAPIKey>(`/api/v1/admin/partners/${partnerId}/keys/${keyId}`, {
    method: 'PUT',
    data,
  });
}

/**
 * 启用 / 停用一把密钥。**下一个请求就生效**（验签那条路每请求现查这一行）。
 *
 * 这是怀疑泄露时唯一快的动作：不用等对接方改代码、也不用重新联调，一点就通。
 */
export async function updatePartnerKeyStatus(
  partnerId: string,
  keyId: string,
  status: APIKeyStatus,
) {
  return request<PartnerAPIKey>(`/api/v1/admin/partners/${partnerId}/keys/${keyId}/status`, {
    method: 'PATCH',
    data: { status },
  });
}
