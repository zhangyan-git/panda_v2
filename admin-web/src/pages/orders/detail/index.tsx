import { PageContainer } from '@ant-design/pro-components';
import { history, useAccess, useParams } from '@umijs/max';
import { Button, Card, Empty, message, Popconfirm } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { completeOrder, getOrder, type OrderDetail } from '../../../services/order';
import { requestErrorMessage } from '../../../services/requestError';
import AfterSaleTab from './AfterSaleTab';
import BasicTab from './BasicTab';
import FortuneTab from './FortuneTab';
import LinesTab from './LinesTab';
import PaymentTab from './PaymentTab';
import TransitionTab from './TransitionTab';

/**
 * 订单详情。老系统这一屏是「订单号 + 一排 tab」，这里保留那个形状。
 *
 * 五个 tab 的数据全在同一个详情响应里（订单主表 + 行明细 + 出资分摊 + 状态流水 +
 * 售后记录），所以**只在外面取一次**，各 tab 拿它渲染。每个 tab 各自去取一次会让打开一次
 * 详情发五个请求，而接口本来就是一次给全的。
 *
 * 福卡 tab 是**第六个、也是唯一一个例外**：福卡的余额与流水在账户库（account-service），
 * 订单详情那个响应里没有它们，所以那一个 tab 自己去取，且要另一档权限（account:read）。
 *
 * 其余 tab 一个都不按权限裁：那五份数据同属 order:read，能进这个页面（路由上的 access
 * 就是它）就都能看。售后 tab 里的审核按钮才是另一档权限，那个在 tab 内部按
 * canReviewAfterSales 裁。
 *
 * 摆的全是库里真有的东西：老系统那屏里的「设备指令日志」在本服务里没有对应的表（属于
 * 厂商侧与 fulfillment），摆一个空 tab 只会让人以为「做了但没数据」。
 */

type TabKey = 'basic' | 'lines' | 'payment' | 'fortune' | 'transitions' | 'after-sales';

export default function OrderDetailPage() {
  const { id } = useParams<{ id: string }>();
  const access = useAccess();
  const [order, setOrder] = useState<OrderDetail>();
  const [loading, setLoading] = useState(true);
  // 取不到订单时的原因。id 是手敲的（或不是 uuid，后端会回 500）就是这个分支。
  const [error, setError] = useState<string>();
  const [tab, setTab] = useState<TabKey>('basic');
  const [completing, setCompleting] = useState(false);

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

  const tabList = [
    { key: 'basic', tab: '基本信息' },
    { key: 'lines', tab: `订单行${order ? `（${order.lines.length}）` : ''}` },
    { key: 'payment', tab: `出资分摊${order ? `（${order.paymentLines.length}）` : ''}` },
    // 福卡 tab 只在有 account:read 时出现，且**承诺过福卡才出现**：没承诺的单进去只会是
    // 一张空表加一句「本单承诺赠送 0 张」。计数不摆在这里——流水条数要取一次才知道，
    // 而这个 tab 列表在详情响应回来时就得画出来。
    ...(access.canViewFortuneCards && (order?.fortuneCardsExpected ?? 0) > 0
      ? [{ key: 'fortune', tab: '福卡' }]
      : []),
    { key: 'transitions', tab: `状态流水${order ? `（${order.transitions.length}）` : ''}` },
    { key: 'after-sales', tab: `售后记录${order ? `（${order.afterSales.length}）` : ''}` },
  ];

  /**
   * 标记完成。这一下会让福卡真的发出去（订单完成事件 → account-service 发放），
   * 所以先让人确认一次，而不是点一下就生效。
   */
  const complete = async () => {
    if (!order) return;
    setCompleting(true);
    try {
      await completeOrder(order.id);
      message.success('已标记完成');
      // 重取一次：状态变成 completed，福卡 tab 靠这个新状态去拉刚发出来的流水。
      await load();
    } catch (err) {
      message.error(requestErrorMessage(err, '标记完成失败，请稍后重试'));
    } finally {
      setCompleting(false);
    }
  };

  const renderTab = () => {
    if (!order) return null;
    switch (tab) {
      case 'lines':
        return <LinesTab order={order} />;
      case 'payment':
        return <PaymentTab order={order} />;
      case 'fortune':
        return <FortuneTab order={order} />;
      case 'transitions':
        return <TransitionTab order={order} />;
      case 'after-sales':
        // 审核之后这一屏要能自己刷新：订单详情是外面取的，重取一遍才拿得到新的售后状态。
        return <AfterSaleTab order={order} onReviewed={load} />;
      default:
        return <BasicTab order={order} />;
    }
  };

  return (
    <PageContainer
      loading={loading}
      title={order?.orderNo || '订单详情'}
      // 副标题放门店：客服拿着单号进来，第二件想知道的就是「哪家店的单」。
      subTitle={order?.storeName || undefined}
      onBack={() => history.push('/orders')}
      extra={
        // 只有 paid 能完成：后端的状态机只认 paid → completed，别的状态点了必然回 409，
        // 所以先把按钮藏掉，免得点出一个注定失败的确认框。与列表里「取消订单」同一个写法。
        access.canCompleteOrders && order?.status === 'paid' ? (
          <Popconfirm
            title="标记这一单为已完成？"
            description={
              // 会发福卡这件事必须写在确认里：这是这一下唯一的、不可逆的副作用。
              order.fortuneCardsExpected > 0
                ? `这一单承诺赠送 ${order.fortuneCardsExpected} 张福卡，确认后会立刻发到用户账上，且不可撤销。`
                : '完成后订单进入终态，不能改回已支付。'
            }
            okText="标记完成"
            cancelText="取消"
            onConfirm={complete}
          >
            <Button type="primary" loading={completing}>
              标记完成
            </Button>
          </Popconfirm>
        ) : undefined
      }
      tabList={tabList}
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
}
