import { ProDescriptions } from '@ant-design/pro-components';
import { Drawer, Empty, Table, Tag } from 'antd';
import type { OperationLog } from '../../services/operationLog';
import OperationLogResultTag from './OperationLogResultTag';
import { actionText, adminUserText, moduleText, targetText } from './operationLogText';

/**
 * 一条操作日志的详情抽屉：左边是这条事件的字段，下面是变更前后的对照表。
 *
 * 列表与详情共用同一个接口、同一份结构，所以这个抽屉在两个地方都用得上——平台操作日志
 * 页（点「详情」）和设备详情页的「操作日志」tab。快照对照那部分逻辑不少（两侧取并集、
 * 标记差异、区分「字段不存在」和「值为空」），抄第二份必然抄歪。
 */

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

type Props = {
  /** 为 undefined 时抽屉关闭。传进来就是打开的那一条。 */
  log?: OperationLog;
  onClose: () => void;
  /**
   * 商户 id → 名称。日志里只有跨库软指针，界面要认人。
   *
   * 由调用方取好传进来而不是抽屉自己去拉：那个映射是一份全集字典，一页日志会开很多次
   * 抽屉，每次重拉一遍没意义。取不到就不用传，那一格会退回显示 id。
   */
  merchantNames?: Record<string, string>;
};

export default function OperationLogDetailDrawer({ log, onClose, merchantNames = {} }: Props) {
  return (
    <Drawer title="操作详情" width={760} open={!!log} onClose={onClose}>
      {log && (
        <>
          <ProDescriptions<OperationLog>
            column={2}
            dataSource={log}
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
                render: (_, row) => <OperationLogResultTag log={row} />,
              },
              { title: '模块', dataIndex: 'module', render: (_, row) => moduleText[row.module] ?? row.module },
              { title: '动作', dataIndex: 'action', render: (_, row) => actionText[row.action] ?? row.action },
              { title: '操作描述', dataIndex: 'operation', render: (_, row) => row.operation || '—' },
              {
                title: '目标',
                dataIndex: 'targetName',
                render: (_, row) => row.targetName || '—',
              },
              { title: '目标类型', dataIndex: 'targetType', render: (_, row) => (row.targetType ? (targetText[row.targetType] ?? row.targetType) : '—') },
              { title: '目标 ID', dataIndex: 'targetId', copyable: true, render: (_, row) => row.targetId || '—' },
              // dataIndex 不重复：ProDescriptions 拿它当 key，两行都写 merchantId
              // 会撞 key，而这一格的取值全靠 render 从整行里取，叫什么名字都行。
              { title: '商户', dataIndex: 'merchantName', render: (_, row) => (row.merchantId ? (merchantNames[row.merchantId] ?? row.merchantId) : '—') },
              { title: '商户 ID', dataIndex: 'merchantId', copyable: true, render: (_, row) => row.merchantId || '—' },
              { title: '错误码', dataIndex: 'errorCode', render: (_, row) => row.errorCode || '—' },
              { title: '错误信息', dataIndex: 'errorMessage', render: (_, row) => row.errorMessage || '—' },
            ]}
          />

          <div style={{ marginTop: 24 }}>
            <h4>变更内容</h4>
            {/* 没有快照是正常情况（不是每个操作都记 before/after，比如纯查询类），
                这时明确说「没有」，而不是摆一张空表让人以为数据丢了。 */}
            {!log.beforeData && !log.afterData ? (
              <Empty description="这次操作没有记录变更快照" />
            ) : (
              <Table<SnapshotRow>
                rowKey="key"
                size="small"
                columns={snapshotColumns}
                dataSource={toSnapshotRows(log.beforeData, log.afterData)}
                pagination={false}
              />
            )}
          </div>
        </>
      )}
    </Drawer>
  );
}
