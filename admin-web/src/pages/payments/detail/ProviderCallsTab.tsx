import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import type { PaymentDetail, PaymentProviderCall } from '../../../services/payment';
import { PROVIDER_OPERATION, PROVIDER_RESULT } from '../../../services/paymentLabels';
import { dash, RawBlock, time } from './render';

/**
 * 支付单详情 → 渠道调用：我们出网问了渠道哪几次、它回了什么。
 *
 * 请求 / 响应摘要是**原样贴出来**的（用户已拍板）。今天渠道只有 manual 那个合成实现，
 * 摘要里是我们自己写的形状；接了真实渠道之后，这里会出现第三方报文里的字段——那正是运维
 * 排「渠道到底回了什么」时唯一的材料。摘要在入库时就由 provider 层做过脱敏，所以「原样」
 * 指的是不再做二次裁剪，不是把密钥也发出来。
 *
 * result=unknown 最需要被看见：那是「超时了、没拿到明确答复」——这笔到底成没成，得靠后续
 * 查单或对账才知道，页面上不能把它显示得跟 failed 一样。
 */

export default function ProviderCallsTab({ detail }: { detail: PaymentDetail }) {
  const columns: ColumnsType<PaymentProviderCall> = [
    {
      title: '操作',
      dataIndex: 'operation',
      width: 110,
      render: (_, row) => {
        const meta = PROVIDER_OPERATION[row.operation] ?? {
          text: row.operation,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '结果',
      dataIndex: 'result',
      width: 90,
      render: (_, row) => {
        const meta = PROVIDER_RESULT[row.result] ?? { text: row.result, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 渠道回的 HTTP 码。超时/连不上时是 null——不是 0，也别显示成 0。
      title: 'HTTP 状态',
      dataIndex: 'httpStatus',
      width: 90,
      render: (_, row) => dash(row.httpStatus),
    },
    { title: '渠道实现', dataIndex: 'provider', width: 110 },
    {
      // 渠道自己的业务码。与 HTTP 码分开摆：很多渠道是 200 + 业务失败码。
      title: '渠道码',
      dataIndex: 'providerCode',
      width: 140,
      ellipsis: true,
      render: (_, row) => dash(row.providerCode),
    },
    {
      title: '渠道消息',
      dataIndex: 'providerMessage',
      width: 220,
      render: (_, row) => <span style={{ wordBreak: 'break-all' }}>{dash(row.providerMessage)}</span>,
    },
    {
      // 同一件事的第几次尝试。>1 = 重试过，排「为什么慢了」要看它。
      title: '尝试次序',
      dataIndex: 'attemptNo',
      width: 90,
    },
    {
      title: '耗时',
      dataIndex: 'durationMs',
      width: 90,
      render: (_, row) => (row.durationMs === null ? '—' : `${row.durationMs} ms`),
    },
    {
      title: '请求 ID',
      dataIndex: 'requestId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.requestId),
    },
    {
      // 渠道侧的链路号：拿着它才能找渠道查「这一笔你们怎么处理的」。
      title: '渠道链路号',
      dataIndex: 'traceId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.traceId),
    },
    { title: '时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
    {
      title: '请求摘要',
      dataIndex: 'requestSummary',
      width: 320,
      render: (_, row) => <RawBlock value={row.requestSummary} />,
    },
    {
      title: '响应摘要',
      dataIndex: 'responseSummary',
      width: 320,
      render: (_, row) => <RawBlock value={row.responseSummary} />,
    },
  ];

  return (
    <Table<PaymentProviderCall>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={detail.providerCalls}
      pagination={false}
      // 2150 = 110+90+90+110+140+220+90+90+200+200+170+320+320，各列 width 之和。
      scroll={{ x: 2150 }}
      locale={{
        emptyText: (
          <div style={{ padding: '32px 0', color: '#999' }}>
            这笔支付没有出网调用过。渠道回调先到、我们再去问渠道查单时才会写这里。
          </div>
        ),
      }}
    />
  );
}
