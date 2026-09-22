import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Descriptions, Empty, Tag } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { enumMeta } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { getOrder, type OrderDetail } from '../../../services/order';
import {
  FULFILLMENT_STATUS,
  ORDER_LINE_TYPE,
  ORDER_SOURCE,
  ORDER_STATUS,
  orderComposition,
  paymentMethodLabel,
} from '../../../services/orderLabels';
import { requestErrorMessage } from '../../../services/session';
import { AfterSalesTable, LinesTable, PaymentLinesTable, TransitionsTable } from './tables';

/** 空值统一显示「—」：留白看起来像加载失败，空串看起来像页面出错。 */
const dash = (value?: string | null) => (value?.trim() ? value : '—');

type TabKey = 'basic' | 'lines' | 'payments' | 'transitions' | 'afterSales';

/**
 * 订单详情（只读）。
 *
 * 形状照 admin-web 那一屏（标题 + 返回 + 一排 tab），但少了后台那一屏里的三样东西：
 * 取消订单（那是人工干预，会写平台审计，只有后台能做）、福卡确认、以及任何审核入口。
 * 商户端这一屏回答的是「这一单现在是什么情况」，不是「我要对它做什么」。
 */
const OrderDetailPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const [order, setOrder] = useState<OrderDetail>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();
  const [tab, setTab] = useState<TabKey>('basic');

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setOrder(await getOrder(id));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载订单失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  const renderTab = () => {
    if (!order) return null;
    switch (tab) {
      case 'lines':
        return <LinesTable lines={order.lines} />;
      case 'payments':
        return <PaymentLinesTable lines={order.paymentLines} />;
      case 'transitions':
        return <TransitionsTable transitions={order.transitions} />;
      case 'afterSales':
        return <AfterSalesTable afterSales={order.afterSales} />;
      default:
        return <BasicTab order={order} />;
    }
  };

  return (
    <PageContainer
      loading={loading}
      title={order ? `订单 ${order.orderNo}` : '订单详情'}
      subTitle={order?.storeName || undefined}
      onBack={() => history.push('/orders')}
      tabList={[
        { key: 'basic', tab: '基本信息' },
        { key: 'lines', tab: '商品明细' },
        { key: 'payments', tab: '出资分摊' },
        { key: 'transitions', tab: '状态流水' },
        { key: 'afterSales', tab: '售后记录' },
      ]}
      tabActiveKey={tab}
      onTabChange={(key) => setTab(key as TabKey)}
    >
      {error ? (
        <Card>
          <Empty description={error} />
        </Card>
      ) : (
        renderTab()
      )}
    </PageContainer>
  );
};

function BasicTab({ order }: { order: OrderDetail }) {
  const statusMeta = enumMeta(ORDER_STATUS, order.status);
  const fulfillmentMeta = enumMeta(FULFILLMENT_STATUS, order.fulfillmentStatus);
  const sourceMeta = enumMeta(ORDER_SOURCE, order.source);
  const composition = orderComposition(order);

  return (
    <Card>
      <Descriptions column={2} bordered size="middle">
        <Descriptions.Item label="订单号">{dash(order.orderNo)}</Descriptions.Item>
        <Descriptions.Item label="取杯号">{dash(order.pickupCode)}</Descriptions.Item>
        <Descriptions.Item label="状态">
          <Tag color={statusMeta.color}>{statusMeta.text}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="履约">
          <Tag color={fulfillmentMeta.color}>{fulfillmentMeta.text}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="来源">
          <Tag color={sourceMeta.color}>{sourceMeta.text}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="构成">
          {composition.length
            ? composition.map((lineType) => {
                const meta = enumMeta(ORDER_LINE_TYPE, lineType);
                return (
                  <Tag key={lineType} color={meta.color}>
                    {meta.text}
                  </Tag>
                );
              })
            : '—'}
        </Descriptions.Item>
        <Descriptions.Item label="门店">{dash(order.storeName || order.storeId)}</Descriptions.Item>
        <Descriptions.Item label="设备">{dash(order.deviceNo || order.deviceId)}</Descriptions.Item>
        <Descriptions.Item label="用户 ID">{dash(order.userId)}</Descriptions.Item>
        <Descriptions.Item label="承诺福卡">
          {order.fortuneCardsExpected > 0 ? order.fortuneCardsExpected : '—'}
        </Descriptions.Item>
        <Descriptions.Item label="原价">¥{formatYuan(order.originalAmount)}</Descriptions.Item>
        <Descriptions.Item label="优惠">¥{formatYuan(order.discountAmount)}</Descriptions.Item>
        <Descriptions.Item label="应付">¥{formatYuan(order.payableAmount)}</Descriptions.Item>
        <Descriptions.Item label="实付">¥{formatYuan(order.paidAmount)}</Descriptions.Item>
        <Descriptions.Item label="已退">¥{formatYuan(order.refundedAmount)}</Descriptions.Item>
        {/* 支付方式的 code，翻成中文；认不出来的原样回显。 */}
        <Descriptions.Item label="支付方式">
          {paymentMethodLabel(order.paymentMethod)}
        </Descriptions.Item>
        <Descriptions.Item label="支付单号">{dash(order.paymentNo)}</Descriptions.Item>
        <Descriptions.Item label="支付时间">{dash(order.paidAt)}</Descriptions.Item>
        <Descriptions.Item label="完成时间">{dash(order.finishedAt)}</Descriptions.Item>
        <Descriptions.Item label="取消时间">{dash(order.cancelledAt)}</Descriptions.Item>
        <Descriptions.Item label="取消原因">{dash(order.cancellationReason)}</Descriptions.Item>
        <Descriptions.Item label="支付截止">{dash(order.expiresAt)}</Descriptions.Item>
        <Descriptions.Item label="下单时间">{dash(order.createdAt)}</Descriptions.Item>
        <Descriptions.Item label="备注" span={2}>
          {dash(order.remark)}
        </Descriptions.Item>
      </Descriptions>
    </Card>
  );
}

export default OrderDetailPage;
