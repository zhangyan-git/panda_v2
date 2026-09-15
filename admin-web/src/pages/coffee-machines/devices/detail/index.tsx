import { PageContainer } from '@ant-design/pro-components';
import { history, useAccess, useParams } from '@umijs/max';
import { Card, Empty } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { getDevice, type DeviceDetail } from '../../../../services/coffeeMachine';
import { requestErrorMessage } from '../../../../services/requestError';
import BasicTab from './BasicTab';
import DrinksTab from './DrinksTab';
import LedgerTab from './LedgerTab';
import LogsTab from './LogsTab';

/**
 * 设备详情。老系统这一屏是「设备名 + 一排操作按钮 + 一列 tab」，这里保留那个形状：
 * 标题、返回列表、tabs，选中的 tab 在下面渲染。
 *
 * tab 只摆**有数据来源**的那几个：
 *   基本信息（设备自身的字段）、操作日志（admin_operation_logs，按 targetId 筛）、
 *   饮品列表（drinks 里 device_id 指向这台设备的那些行）、账变记录（device_balance_ledger）。
 * 老系统还有销售记录 / 配额管理 / 支付方式配置三屏，这三个在本服务里连数据模型都还没有
 * （没有销售单、没有配额列、支付方式挂在二维码上而不是设备上），摆一排空 tab 只会让人
 * 以为「功能做了但这里没配数据」，所以一个都不摆。
 *
 * 本页只取一次设备：标题要用它，而首屏（基本信息）整屏就是它。其余三个 tab 各自去取
 * 自己那一份——把四份数据都在这里拉齐，等于打开详情页就把后面可能一眼都不看的两个
 * 服务端分页接口也打了。
 */

type TabKey = 'basic' | 'logs' | 'drinks' | 'ledger';

export default function DeviceDetailPage() {
  const access = useAccess();
  const { id } = useParams<{ id: string }>();

  const [device, setDevice] = useState<DeviceDetail>();
  const [loading, setLoading] = useState(true);
  // 取不到设备时的原因。设备被删掉（或 id 是手敲的）就是这个分支。
  const [error, setError] = useState<string>();
  const [tab, setTab] = useState<TabKey>('basic');

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setDevice(await getDevice(id));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载设备失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  // 账变记录那一屏的接口与后端的「调整余额」共用一个权限码（后端 GET/POST 挂在同一条
  // 路由表项上；那个 POST 只留了路由，本应用不提供入口）：没有这个码的人打开它只会收到
  // 一串 403，所以干脆不摆。其余三个 tab 跟页面的 canViewCoffeeMachines 同权限，能进
  // 这个页面就都能看。
  const tabList = [
    { key: 'basic', tab: '基本信息' },
    { key: 'logs', tab: '操作日志' },
    { key: 'drinks', tab: '饮品列表' },
    ...(access.canAdjustCoffeeBalance ? [{ key: 'ledger', tab: '账变记录' }] : []),
  ];

  const renderTab = () => {
    if (!device) return null;
    switch (tab) {
      case 'logs':
        return <LogsTab deviceId={device.id} />;
      case 'drinks':
        // 厂商一并传下去：那一屏的「添加饮品」用它预填，设备与厂商本来就是绑定的。
        return <DrinksTab deviceId={device.id} />;
      case 'ledger':
        return <LedgerTab deviceId={device.id} />;
      default:
        return <BasicTab device={device} />;
    }
  };

  return (
    <PageContainer
      loading={loading}
      title={device?.deviceName || device?.serialUnique || '设备详情'}
      subTitle={device ? `设备编码 ${device.serialUnique}` : undefined}
      onBack={() => history.push('/coffee-machines/devices')}
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
