import { PageContainer, ProDescriptions, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { Drawer, Empty, message, Table, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { toRFC3339 } from '../../services/datetime';
import {
  getOperationLogFacets,
  listOperationLogs,
  type OperationLog,
  type OperationLogFacets,
} from '../../services/operationLog';
import { toPageParams } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';

/**
 * 模块名 → 中文。取值由后端各服务的审计调用点决定，这里只覆盖已知的那些，
 * 没登记的按原名显示 —— 新模块上线时看到的是 `coupons`，总好过看到一个「未知」。
 */
const moduleText: Record<string, string> = {
  users: '管理员用户',
  merchant_users: '商户账号',
  merchants: '商户',
  brands: '品牌',
  stores: '门店',
  roles: '角色',
  permissions: '权限',
  menus: '菜单',
  miniapp_users: '小程序用户',
  coupons: '优惠券',
};

const actionText: Record<string, string> = {
  create: '新增',
  update: '修改',
  delete: '删除',
  update_status: '修改状态',
  update_scope: '修改数据范围',
  audit: '审核',
  assign_permissions: '分配权限',
  remove_permission: '移除权限',
  assign_roles: '分配角色',
  remove_role: '移除角色',
  assign_menus: '分配菜单',
};

/** 操作人的展示形式：姓名（用户名），只有一样时显示那样，都没有时是「—」。 */
function adminUserText(row: OperationLog): string {
  if (!row.adminUsername) return row.adminName || '—';
  return row.adminName ? `${row.adminName}（${row.adminUsername}）` : row.adminUsername;
}

/** 快照里的一个值。空值显示成「空」而不是留白：留白和「字段不存在」分不出来。 */
function snapshotValue(value: unknown): string {
  if (value === null || value === undefined) return '—';
  if (typeof value === 'object') return JSON.stringify(value);
  if (value === '') return '（空）';
  return String(value);
}

type SnapshotRow = {
  key: string;
  field: string;
  before: unknown;
  after: unknown;
  changed: boolean;
  /** 字段是否只存在于其中一侧：新增没有「前」，删除没有「后」。 */
  existsBefore: boolean;
  existsAfter: boolean;
};

/**
 * 把 before/after 两个快照并成一张对照表。
 *
 * 两段 JSON 并排贴出来看着累，而管理员打开这条日志想问的就是「到底改了什么」——
 * 把两边同名字段对齐、标出差异，答案就是一列「已变更」。
 *
 * 字段名取两边的并集而不是各自的键：新增和删除时只有一侧有键，只看交集会把这次
 * 操作的全部内容都漏掉。
 */
function toSnapshotRows(before: unknown, after: unknown): SnapshotRow[] {
  const beforeObj = (before ?? {}) as Record<string, unknown>;
  const afterObj = (after ?? {}) as Record<string, unknown>;
  const fields = Array.from(new Set([...Object.keys(beforeObj), ...Object.keys(afterObj)]));
  return fields.map((field) => {
    const existsBefore = Object.prototype.hasOwnProperty.call(beforeObj, field);
    const existsAfter = Object.prototype.hasOwnProperty.call(afterObj, field);
    const beforeValue = beforeObj[field];
    const afterValue = afterObj[field];
    return {
      key: field,
      field,
      before: beforeValue,
      after: afterValue,
      changed: snapshotValue(beforeValue) !== snapshotValue(afterValue),
      existsBefore,
      existsAfter,
    };
  });
}

const snapshotColumns = [
  { title: '字段', dataIndex: 'field', width: 160 },
  {
    title: '变更前',
    dataIndex: 'before',
    render: (_: unknown, row: SnapshotRow) => (row.existsBefore ? snapshotValue(row.before) : '—'),
  },
  {
    title: '变更后',
    dataIndex: 'after',
    render: (_: unknown, row: SnapshotRow) => (row.existsAfter ? snapshotValue(row.after) : '—'),
  },
  {
    title: '差异',
    dataIndex: 'changed',
    width: 90,
    render: (_: unknown, row: SnapshotRow) =>
      row.changed ? <Tag color="processing">已变更</Tag> : <span>—</span>,
  },
];

export default function OperationLogsPage() {
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<OperationLog>();
  // 候选值只在进页面时取一次：它来自历史数据，一次会话里不会变，而每翻一页都去
  // 取一遍等于给这张表加一份没必要的全表聚合。
  const [facets, setFacets] = useState<OperationLogFacets>({ modules: [], actions: [] });

  useEffect(() => {
    getOperationLogFacets()
      .then(setFacets)
      // 取不到只影响两个下拉的候选值，列表本身还能用（关键词、操作人、时间照样筛），
      // 所以只提示不阻断。
      .catch((error) => message.error(requestErrorMessage(error, '加载筛选选项失败')));
  }, []);

  const columns: ProColumns<OperationLog>[] = [
    {
      // 仅搜索用的时间范围。用独立的 dataIndex，不和下面那列 occurredAt 共用：
      // 同名的两列会在搜索表单里争同一个键，范围数组覆盖掉字符串之后，表格里的
      // 时间列就空了。transform 把它换成后端要的 startTime/endTime 两个参数。
      title: '时间范围',
      dataIndex: 'timeRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { startTime: toRFC3339(range[0]), endTime: toRFC3339(range[1]) };
        },
      },
    },
    {
      // 仅搜索用：后端按「操作人用户名或姓名」的前缀匹配。
      title: '操作人',
      dataIndex: 'operator',
      hideInTable: true,
      fieldProps: { placeholder: '用户名或姓名前缀' },
    },
    {
      // 仅搜索用：匹配目标名称或操作描述。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '目标名称或操作描述' },
    },
    {
      title: '模块',
      dataIndex: 'module',
      valueType: 'select',
      width: 120,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把没登记过的模块名原样显示。
      valueEnum: Object.fromEntries(
        facets.modules.map((value) => [value, { text: moduleText[value] ?? value }]),
      ),
      render: (_, row) => moduleText[row.module] ?? row.module,
    },
    {
      title: '动作',
      dataIndex: 'action',
      valueType: 'select',
      width: 120,
      valueEnum: Object.fromEntries(
        facets.actions.map((value) => [value, { text: actionText[value] ?? value }]),
      ),
      render: (_, row) => actionText[row.action] ?? row.action,
    },
    {
      title: '操作描述',
      dataIndex: 'operation',
      search: false,
      ellipsis: true,
      render: (_, row) => row.operation || '—',
    },
    {
      title: '操作人',
      dataIndex: 'adminUsername',
      search: false,
      width: 150,
      render: (_, row) => adminUserText(row),
    },
    {
      title: '目标',
      dataIndex: 'targetName',
      search: false,
      ellipsis: true,
      render: (_, row) => row.targetName || '—',
    },
    {
      title: '结果',
      dataIndex: 'result',
      valueType: 'select',
      width: 110,
      valueEnum: {
        success: { text: '成功' },
        failure: { text: '失败' },
      },
      render: (_, row) =>
        row.result === 'success' ? (
          <Tag color="success">成功</Tag>
        ) : (
          // 失败时把原因挂出来：一条只有「失败」的日志回答不了任何问题。
          <Tag color="error">{row.errorMessage || '失败'}</Tag>
        ),
    },
    {
      title: '时间',
      dataIndex: 'occurredAt',
      // 交给 ProTable 按 dateTime 渲染（本地时区）。本页别自己拼时间字符串：
      // 同一个表里两种格式看着像两个系统拼出来的。
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      fixed: 'right',
      width: 80,
      render: (_, row) => [
        <a key="detail" onClick={() => setDetail(row)}>
          详情
        </a>,
      ],
    },
  ];

  return (
    <PageContainer title="操作日志">
      <ProTable<OperationLog>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1300 }}
        // 后端按 occurredAt 倒序（最近的在前），翻页也从近往远走。
        request={async (params) => {
          const result = await listOperationLogs(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        search={{ labelWidth: 'auto' }}
      />

      <Drawer
        title="操作详情"
        width={760}
        open={!!detail}
        onClose={() => setDetail(undefined)}
      >
        {detail && (
          <>
            <ProDescriptions<OperationLog>
              column={2}
              dataSource={detail}
              columns={[
                { title: '日志 ID', dataIndex: 'id', copyable: true },
                { title: '时间', dataIndex: 'occurredAt', valueType: 'dateTime' },
                {
                  title: '操作人',
                  dataIndex: 'adminUsername',
                  render: (_, row) => adminUserText(row),
                },
                {
                  title: '结果',
                  dataIndex: 'result',
                  render: (_, row) =>
                    row.result === 'success' ? (
                      <Tag color="success">成功</Tag>
                    ) : (
                      <Tag color="error">失败</Tag>
                    ),
                },
                { title: '模块', dataIndex: 'module', render: (_, row) => moduleText[row.module] ?? row.module },
                { title: '动作', dataIndex: 'action', render: (_, row) => actionText[row.action] ?? row.action },
                { title: '操作描述', dataIndex: 'operation', render: (_, row) => row.operation || '—' },
                {
                  title: '目标',
                  dataIndex: 'targetName',
                  render: (_, row) => row.targetName || '—',
                },
                { title: '目标类型', dataIndex: 'targetType', render: (_, row) => row.targetType || '—' },
                { title: '目标 ID', dataIndex: 'targetId', copyable: true, render: (_, row) => row.targetId || '—' },
                { title: '商户 ID', dataIndex: 'merchantId', render: (_, row) => row.merchantId || '—' },
                { title: '错误码', dataIndex: 'errorCode', render: (_, row) => row.errorCode || '—' },
                { title: '错误信息', dataIndex: 'errorMessage', render: (_, row) => row.errorMessage || '—' },
              ]}
            />

            <div style={{ marginTop: 24 }}>
              <h4>变更内容</h4>
              {/* 没有快照是正常情况（不是每个操作都记 before/after，比如纯查询类），
                  这时明确说「没有」，而不是摆一张空表让人以为数据丢了。 */}
              {!detail.beforeData && !detail.afterData ? (
                <Empty description="这次操作没有记录变更快照" />
              ) : (
                <Table<SnapshotRow>
                  rowKey="key"
                  size="small"
                  columns={snapshotColumns}
                  dataSource={toSnapshotRows(detail.beforeData, detail.afterData)}
                  pagination={false}
                />
              )}
            </div>
          </>
        )}
      </Drawer>
    </PageContainer>
  );
}
