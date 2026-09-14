import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 平台后台操作日志（admin_operation_logs）。
 *
 * 只读：这张表是审计证据，后端没有删除、清空之类的接口，页面也不该有按钮。
 * 写入方不是后台，而是各业务服务在事务里写进 outbox、经消息总线送过来的审计事件
 * （见 user-service 的审计消费者），所以新日志会在一两秒内自己出现。
 */

export type OperationLogResult = 'success' | 'failure';

export type OperationLog = {
  id: string;
  /** 操作人；管理员账号被删除后为空串 */
  adminUserId: string;
  /** 操作发生时的用户名/姓名快照，不随改名变化 */
  adminUsername: string;
  adminName: string;
  /** 业务模块，如 miniapp_users、merchants、roles */
  module: string;
  /** 标准动作，如 create、update_status、delete */
  action: string;
  /** 面向后台的操作描述，如「修改小程序用户状态」 */
  operation: string;
  targetType: string;
  targetId: string;
  targetName: string;
  /** 跨库软指针，为空串表示这次操作不针对某个商户 */
  merchantId: string;
  result: OperationLogResult | string;
  errorCode: string;
  errorMessage: string;
  /** 变更前后快照，写入时已脱敏；没有快照时是 null（不是空对象） */
  beforeData: Record<string, unknown> | null;
  afterData: Record<string, unknown> | null;
  /**
   * 落库时间，RFC3339。
   *
   * 注意它是这条审计事件被写进日志表的时间，不是操作发生的时刻：事件本身不带时间
   * 戳，relay 积压后补投时这一批日志的时间会整体晚于实际操作时间。
   */
  occurredAt: string;
};

export type OperationLogQuery = PageQuery & {
  /** 模块精确匹配；取值见 getOperationLogFacets */
  module?: string;
  /** 动作精确匹配；取值见 getOperationLogFacets */
  action?: string;
  result?: OperationLogResult;
  /** 操作人用户名或姓名的前缀 */
  operator?: string;
  /** 目标名称或操作描述的子串 */
  keyword?: string;
  /**
   * 目标类型精确筛选，如 device；与 targetId 是「与」（只给它就是「这类对象的全部操作」）。
   *
   * 设备详情页的「操作日志」那一屏靠这两个参数只看这一台设备：keyword 做不到这件事——
   * 目标名会重名，而且它匹配的是子串，「设备 1」会把「设备 12」一起带出来。
   */
  targetType?: string;
  /** 目标对象 id（UUID）。后端会校验格式，非 UUID 回 400 而不是一张空表。 */
  targetId?: string;
  /** 起始时间（含），RFC3339 */
  startTime?: string;
  /** 结束时间（含），RFC3339 */
  endTime?: string;
};

/**
 * 筛选下拉的候选值，取自库中实际出现过的取值。
 *
 * 从数据里取而不是前端写死一份：模块名由后端各服务的审计调用点决定，写死的那份
 * 会在下一个模块上线时静默少一项，而「筛选里没有我要找的模块」看起来就是日志
 * 没记上——排查方向完全错了。
 */
export type OperationLogFacets = {
  modules: string[];
  actions: string[];
};

/** 操作日志列表（服务端分页，按 occurredAt 倒序） */
export async function listOperationLogs(params?: OperationLogQuery) {
  return request<PageResult<OperationLog>>('/api/v1/admin/operation-logs', { params });
}

/** 模块与动作的候选值，供筛选下拉使用 */
export async function getOperationLogFacets() {
  return request<OperationLogFacets>('/api/v1/admin/operation-logs/facets');
}
