import { Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { formatDateTime } from '../../../../services/datetime';
import { enumMeta } from '../../../../services/labels';
import type { MembershipChange } from '../../../../services/membership';
import { CHANGE_OPERATOR, CHANGE_TYPE, MEMBERSHIP_STATUS } from '../../../../services/membershipLabels';

/**
 * 变更流水：这个人身上发生过的事，倒序。
 *
 * **用表而不是时间线**：每一行要带的东西有八样（状态前后、到期时间前后、操作人、原因、备注、
 * 订单号），时间线那种左轴右卡的排版塞不下这些，要么截断要么换行到看不清。表格的列宽能按
 * 内容分配，对账时还能逐列扫。
 *
 * 两个「前后」列的读法：状态与到期时间各有一对 from/to，**两对都可能半边为空**——
 * 开通那一条没有前一个状态（fromStatus 是空串），续费只动有效期（状态两个都是 active，
 * 看起来像没变化，那正是「这一条没有改状态」的意思）。
 *
 * 这一页只读：流水是 append-only 的账，没有任何接口能改它或删它。
 */
export default function ChangesTab({ changes }: { changes: MembershipChange[] }) {
  /** 空串表示「没有前一个状态」，显示成「无」——留白会被读成「这一格没数据」。 */
  const statusLabel = (status: string) => {
    if (!status) return <span style={{ color: '#bfbfbf' }}>无</span>;
    return <Tag color={enumMeta(MEMBERSHIP_STATUS, status).color}>{enumMeta(MEMBERSHIP_STATUS, status).text}</Tag>;
  };

  const columns: ColumnsType<MembershipChange> = [
    {
      title: '时间',
      dataIndex: 'occurredAt',
      width: 170,
      render: (value: string) => formatDateTime(value),
    },
    {
      title: '类型',
      dataIndex: 'changeType',
      width: 110,
      render: (value: string) => {
        const meta = enumMeta(CHANGE_TYPE, value);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 前后都摆出来而不是只显示新的：续费与冻结的区别全在这里（前者状态不变、后者只变状态）。
      // 190 是量出来的：两个 Tag（「生效中」「已冻结」这类三字标签各约 60）加中间那个箭头
      // 约 20，再加左右内边距 32，150 装不下会把这一格折成两行——同一张表里只有这一行高
      // 一倍，扫起来很跳。
      title: '状态变化',
      dataIndex: 'fromStatus',
      width: 190,
      render: (_, row) => (
        <>
          {statusLabel(row.fromStatus)}
          <span style={{ color: '#8c8c8c', margin: '0 4px' }}>→</span>
          {statusLabel(row.toStatus)}
        </>
      ),
    },
    {
      // 时间戳是真 null（不是空串），所以判空用值本身。两边都可能为空：开通那一条没有
      // 「之前的到期时间」，而后端今天写的每一条都有 toExpireAt。
      title: '到期变化',
      dataIndex: 'fromExpireAt',
      width: 300,
      render: (_, row) => (
        <>
          {row.fromExpireAt ? formatDateTime(row.fromExpireAt) : <span style={{ color: '#bfbfbf' }}>无</span>}
          <span style={{ color: '#8c8c8c', margin: '0 4px' }}>→</span>
          {row.toExpireAt ? formatDateTime(row.toExpireAt) : <span style={{ color: '#bfbfbf' }}>无</span>}
        </>
      ),
    },
    {
      // operatorId 可能是 null：到期扫描（system）与后台任务（worker）写的那几条没有操作人。
      // 截断成前 8 位是因为它是 uuid，整串会把这一列撑到与订单号一样宽，而这一列的作用只是
      // 「几条记录是不是同一个人做的」——完整值挂悬停里。
      title: '操作人',
      dataIndex: 'operatorType',
      width: 150,
      render: (_, row) => {
        const meta = enumMeta(CHANGE_OPERATOR, row.operatorType);
        return (
          <>
            <Tag color={meta.color}>{meta.text}</Tag>
            {row.operatorId ? (
              <span title={row.operatorId} style={{ color: '#8c8c8c' }}>
                {row.operatorId.slice(0, 8)}
              </span>
            ) : null}
          </>
        );
      },
    },
    {
      // 原因与备注都是自由文本，可能很长，所以只在悬停里给全——这一列的宽度撑不住一整段
      // 客服写的话，而把行高撑起来会让整张表没法扫。
      title: '原因',
      dataIndex: 'reason',
      width: 200,
      ellipsis: true,
      render: (value: string) => value || <span style={{ color: '#bfbfbf' }}>—</span>,
    },
    {
      title: '备注',
      dataIndex: 'remark',
      width: 200,
      ellipsis: true,
      render: (value: string) => value || <span style={{ color: '#bfbfbf' }}>—</span>,
    },
    {
      // 后台调整与到期扫描都没有订单，所以这是常态里的常态：空着说明「这一条不是钱买来的」。
      // 有订单号的那几条（开通 / 续费 / 退款调整）是能与订单、支付对上账的。
      //
      // 整串摆出来而不是像操作人那样截断：订单号是要拿去与订单、支付对账的，前 8 位既不唯一
      // 也没法用眼睛比。复制按钮给的是整串（`copyable` 是 ProTable 的列属性，antd 的
      // 普通 Table 没有，所以这里走 Typography）。
      title: '订单号',
      dataIndex: 'orderId',
      width: 320,
      ellipsis: true,
      render: (value: string | null) =>
        value ? (
          <Typography.Text copyable>{value}</Typography.Text>
        ) : (
          <span style={{ color: '#bfbfbf' }}>—</span>
        ),
    },
  ];

  return (
    <Table<MembershipChange>
      rowKey="id"
      columns={columns}
      dataSource={changes}
      // 流水是随详情一次性给全的（接口不分页），所以这里不分页——**但上限在后端**：
      // 今天它是整份返回的，将来条数多到需要翻页时要改的是接口，不是这里补一个前端分页。
      pagination={false}
      size="middle"
      // 1640 = 170+110+190+300+150+200+200+320，各列 width 之和。改任何一列的宽度都要
      // 同批改这个数，否则表格会在不该出现横向滚动条的时候出现一个（或者反过来）。
      scroll={{ x: 1640 }}
      locale={{ emptyText: '没有变更记录' }}
    />
  );
}
