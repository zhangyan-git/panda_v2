import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Empty } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { getPaymentDetail, type PaymentDetail } from '../../../services/payment';
import { requestErrorMessage } from '../../../services/requestError';
import BasicTab from './BasicTab';
import FundingsTab from './FundingsTab';
import NotificationsTab from './NotificationsTab';
import ProviderCallsTab from './ProviderCallsTab';
import TransactionsTab from './TransactionsTab';
import TransitionsTab from './TransitionsTab';

/**
 * 支付单详情。形状与订单详情一致（一排 tab + 每 tab 里一张表），因为它要回答的是同一类
 * 问题：这一笔钱到底发生了什么。
 *
 * **六个 tab 的数据全在同一个详情响应里**（支付单 + 出资行 + 记账流水 + 状态流转 + 渠道调用
 * + 回调通知），所以只在外面取一次。每个 tab 各自去取一次会让打开一次详情发六个请求、页签
 * 切一次卡一次，而且六个响应之间天然不一致——后端本来就是在同一个只读事务里读齐了再回的。
 *
 * 三个 tab 是**故意没有的**：退款单、代扣协议、对账批次。那三组表建好了但零行、也没有任何
 * 代码在写（见 docs/architecture.md 支付那一段），开出来就是三个永远空着的页签，而
 * 「还没有」与「这里是空的」在一张空表上分不出来。等退款/对账真做出来再加。
 *
 * 路径参数是**支付单号**不是 uuid：单号查无此单回 404（不像订单详情那样会因非 uuid 回
 * 500），所以这一页拿到参数直接查，不用先验形状。
 */

type TabKey = 'basic' | 'fundings' | 'transactions' | 'transitions' | 'provider-calls' | 'notifications';

export default function PaymentDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [detail, setDetail] = useState<PaymentDetail>();
  const [loading, setLoading] = useState(true);
  // 取不到支付单时的原因。单号是手敲的（或压根不存在）就是这个分支。
  const [error, setError] = useState<string>();
  const [tab, setTab] = useState<TabKey>('basic');

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setDetail(await getPaymentDetail(id));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载支付单失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  // 计数摆在页签上：运维进来想知道的第一件事是「这笔钱有没有回调、有没有出网调用」，
  // 那三个数不必点进去才知道。
  const tabList = [
    { key: 'basic', tab: '基本信息' },
    { key: 'fundings', tab: `出资行${detail ? `（${detail.fundings.length}）` : ''}` },
    { key: 'transactions', tab: `记账流水${detail ? `（${detail.transactions.length}）` : ''}` },
    { key: 'transitions', tab: `状态流转${detail ? `（${detail.transitions.length}）` : ''}` },
    { key: 'provider-calls', tab: `渠道调用${detail ? `（${detail.providerCalls.length}）` : ''}` },
    { key: 'notifications', tab: `回调通知${detail ? `（${detail.notifications.length}）` : ''}` },
  ];

  const renderTab = () => {
    if (!detail) return null;
    switch (tab) {
      case 'fundings':
        return <FundingsTab detail={detail} />;
      case 'transactions':
        return <TransactionsTab detail={detail} />;
      case 'transitions':
        return <TransitionsTab detail={detail} />;
      case 'provider-calls':
        return <ProviderCallsTab detail={detail} />;
      case 'notifications':
        return <NotificationsTab detail={detail} />;
      default:
        return <BasicTab payment={detail.payment} />;
    }
  };

  return (
    <PageContainer
      loading={loading}
      title={detail?.payment.paymentNo || '支付单详情'}
      // 副标题放订单号：从订单域跳过来的人手上拿的是它。
      subTitle={detail?.payment.orderNo || undefined}
      onBack={() => history.push('/payments')}
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
