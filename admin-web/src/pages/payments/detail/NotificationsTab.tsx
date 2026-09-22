import { Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import type { PaymentDetail, PaymentNotification } from '../../../services/payment';
import { NOTIFICATION_STATUS } from '../../../services/paymentLabels';
import { dash, RawBlock, time } from './render';

/**
 * 支付单详情 → 回调通知：渠道打进来过什么。
 *
 * `body` 是渠道发来的**原始报文**，原样贴出来。有一点必须在页面上说清楚：它在库里是
 * bytea，后端已经把它转成字符串发出来（不转的话 JSON 出的是 base64，那才是真的乱码），
 * 但**非 UTF-8 的字节会被替换成 U+FFFD**（一个「�」）。所以正文里出现几个 � 是编码问题，
 * 不是解析错了——真要看原始字节得去查库。
 *
 * 签名那一列是「这条通知的签名验过没有」。它是收进来的那一刻判的，与「这条通知有没有被
 * 处理」是两件事：签名没过的通知照样会入库（要留证据），状态多半停在 failed。
 */

export default function NotificationsTab({ detail }: { detail: PaymentDetail }) {
  const columns: ColumnsType<PaymentNotification> = [
    {
      // 渠道给的通知 id，去重靠它。渠道那边查一笔通知也是拿它。
      title: '通知 ID',
      dataIndex: 'notificationId',
      width: 200,
      ellipsis: true,
    },
    { title: '事件类型', dataIndex: 'eventType', width: 160, ellipsis: true, render: (_, row) => dash(row.eventType) },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      render: (_, row) => {
        const meta = NOTIFICATION_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '验签',
      dataIndex: 'signatureVerified',
      width: 90,
      render: (_, row) => (
        <Tag color={row.signatureVerified ? 'success' : 'error'}>
          {row.signatureVerified ? '通过' : '未通过'}
        </Tag>
      ),
    },
    { title: '渠道实现', dataIndex: 'provider', width: 110 },
    {
      title: '失败原因',
      dataIndex: 'failureReason',
      width: 220,
      render: (_, row) => <span style={{ wordBreak: 'break-all' }}>{dash(row.failureReason)}</span>,
    },
    { title: '收到时间', dataIndex: 'receivedAt', width: 170, render: (_, row) => time(row.receivedAt) },
    {
      // 还没处理完就是 null（正在重试、或签名没过被搁下了）。
      title: '处理时间',
      dataIndex: 'processedAt',
      width: 170,
      render: (_, row) => time(row.processedAt),
    },
    {
      // 报文摘要：与库里那一列逐字节对得上，用来证明「我们看到的就是渠道发的」。
      title: '报文 SHA256',
      dataIndex: 'bodySha256',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.bodySha256),
    },
    {
      title: '请求头',
      dataIndex: 'headers',
      width: 260,
      render: (_, row) => <RawBlock value={row.headers} />,
    },
    {
      title: '报文正文',
      dataIndex: 'body',
      width: 360,
      render: (_, row) => <RawBlock value={row.body} maxHeight={240} />,
    },
  ];

  return (
    <Table<PaymentNotification>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={detail.notifications}
      pagination={false}
      // 2040 = 200+160+100+90+110+220+170+170+200+260+360，各列 width 之和。
      scroll={{ x: 2040 }}
      title={() => (
        <Typography.Text type="secondary">
          报文正文里的「�」是非 UTF-8 字节被替换后的样子，不是解析错误；要看原始字节请查库。
        </Typography.Text>
      )}
      locale={{
        emptyText: (
          <div style={{ padding: '32px 0', color: '#999' }}>
            这笔支付还没收到过渠道回调。手工渠道（curl 触发）时才写这里。
          </div>
        ),
      }}
    />
  );
}
