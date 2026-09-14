import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 小程序（C 端）用户。
 *
 * 与 services/iam.ts 的 AdminUser 是两批人：那个是平台管理员（admin_users），
 * 这个是小程序顾客（users）。接口路径、权限码、状态取值都不一样，别混用。
 */

/** 状态取值与后端 users.status 的 CHECK 一致。deleted 是用户自己注销留下的终态。 */
export type MiniappUserStatus = 'active' | 'disabled' | 'deleted';

export type MiniappUser = {
  id: string;
  phone: string;
  nickname: string;
  avatarUrl: string;
  /** 取值与后端 users.gender 的 CHECK 一致；unknown 是没填过资料的默认值 */
  gender: 'unknown' | 'male' | 'female';
  /** YYYY-MM-DD；没填时为空串 */
  birthday: string;
  regionCode: string;
  regionName: string;
  status: MiniappUserStatus;
  /** 注册/首次登录来源：wechat_miniapp / wechat_phone / sms_code */
  registerSource: string;
  /** 为空串表示从未登录过 */
  lastLoginAt: string;
  lastLoginIp: string;
  loginCount: number;
  createdAt: string;
};

export type MiniappUserQuery = PageQuery & {
  status?: MiniappUserStatus;
  /** 手机号前缀或昵称片段 */
  keyword?: string;
};

export type MiniappWechatIdentity = {
  appType: string;
  openId: string;
  unionId: string;
  lastLoginAt: string;
  createdAt: string;
};

/** 一条登录尝试。成功和失败都有：失败记录是「有人在试这个号」的唯一线索。 */
export type MiniappLoginEvent = {
  loginType: string;
  /** openid 或脱敏手机号——后端写库前就已打码，这里拿到的不可能是完整号码。 */
  identifier: string;
  success: boolean;
  failReason: string;
  ip: string;
  userAgent: string;
  createdAt: string;
};

export type MiniappUserDetail = MiniappUser & {
  wechatIdentities: MiniappWechatIdentity[];
  /** 未撤销且未过期的会话数；禁用后归零 */
  activeSessions: number;
  recentLogins: MiniappLoginEvent[];
};

/** 用户列表（服务端分页） */
export async function listMiniappUsers(params?: MiniappUserQuery) {
  return request<PageResult<MiniappUser>>('/api/v1/admin/miniapp-users', { params });
}

/** 用户详情：账号 + 微信绑定 + 会话数 + 最近登录记录 */
export async function getMiniappUser(id: string) {
  return request<MiniappUserDetail>(`/api/v1/admin/miniapp-users/${id}`);
}

/**
 * 启用/禁用用户。
 *
 * 禁用会在服务端同一事务里撤销该用户全部有效会话，返回被撤销的会话数——
 * 界面据此提示「同时踢下线 N 个登录态」。已注销（deleted）的账号会被拒绝（409）。
 */
export async function updateMiniappUserStatus(
  id: string,
  status: 'active' | 'disabled',
) {
  return request<{ revokedSessions: number }>(
    `/api/v1/admin/miniapp-users/${id}/status`,
    { method: 'PATCH', data: { status } },
  );
}
