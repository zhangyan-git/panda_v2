import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Empty } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { getOrder, type OrderDetail } from '../../../services/order';
import { requestErrorMessage } from '../../../services/requestError';
import AfterSaleTab from './AfterSaleTab';
import BasicTab from './BasicTab';
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
 * tab 一个都不按权限裁：五份数据同属 order:read，能进这个页面（路由上的 access 就是它）
 * 就都能看。售后 tab 里的审核按钮才是另一档权限，那个在 tab 内部按 canReviewAfterSales 裁。
 *
 * 摆的全是库里真有的东西：老系统那屏里的「支付流水」「设备指令日志」在本服务里没有对应的
 * 表（分别属于 payment-service 和厂商侧），摆一排空 tab 只会让人以为「做了但没数据」。
 */

type TabKey = 'basic' | 'lines' | 'payment' | 'transitions' | 'after-sales';

export default function OrderDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [order, setOrder] = useState<OrderDetail>();
  const [loading, setLoading] = useState(true);
  // 取不到订单时的原因。id 是手敲的（或不是 uuid，后端会回 500）就是这个分支。
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

  const tabList = [
    { key: 'basic', tab: '基本信息' },
    { key: 'lines', tab: `订单行${order ? `（${order.lines.length}）` : ''}` },
    { key: 'payment', tab: `出资分摊${order ? `（${order.paymentLines.length}）` : ''}` },
    { key: 'transitions', tab: `状态流水${order ? `（${order.transitions.length}）` : ''}` },
    { key: 'after-sales', tab: `售后记录${order ? `（${order.afterSales.length}）` : ''}` },
  ];

  const renderTab = () => {
    if (!order) return null;
    switch (tab) {
      case 'lines':
        return <LinesTab order={order} />;
      case 'payment':
        return <PaymentTab order={order} />;
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
