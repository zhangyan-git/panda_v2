import { ProDescriptions } from '@ant-design/pro-components';
import type { ProDescriptionsItemProps } from '@ant-design/pro-components';
import { Tag } from 'antd';
import { useEffect, useState } from 'react';
import {
  listManufacturers,
  type DeviceDetail,
  type DeviceStatus,
  type QrcodeType,
  type RegularQrcodePaymentMethod,
} from '../../../../services/coffeeMachine';
import { formatDateTime } from '../../../../services/datetime';
import { formatYuan } from '../../../../services/money';
import { FULL_PAGE_PARAMS } from '../../../../services/pagination';
import { listStores } from '../../../../services/store';

/**
 * 设备详情 → 基本信息。老系统这一屏是一张「设备信息」表，列的是设备自身的属性。
 *
 * 只摆**库里真有的**字段：老系统那张表里的 IP / MAC / 价格模板 / 型号在本服务里没有列，
 * 摆一排「—」会让人以为是同步没做，实际上是没有这个概念。
 */

const STATUS_TAG: Record<DeviceStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '在架' },
  disabled: { color: 'default', label: '停用' },
};

/**
 * 厂商侧在线状态的三态。null 是「从未同步过」——把它显示成「离线」会让一台还没接上的
 * 设备看起来像掉线了，排查方向就从「同步没做」歪到「网络故障」。
 */
const VENDOR_ONLINE: Record<'online' | 'offline' | 'unknown', { color: string; label: string }> = {
  online: { color: 'green', label: '在线' },
  offline: { color: 'red', label: '离线' },
  unknown: { color: 'default', label: '未同步' },
};

const QRCODE_TYPE: Record<QrcodeType, string> = {
  miniprogram: '小程序码',
  regular: '普通二维码',
};

const PAYMENT_METHOD: Record<RegularQrcodePaymentMethod, string> = {
  fengxuan_wanlian: '丰选万联',
  youlian: '优联',
};

const vendorOnlineState = (value: DeviceDetail['vendorOnline']): keyof typeof VENDOR_ONLINE => {
  if (value === null || value === undefined) return 'unknown';
  return value ? 'online' : 'offline';
};

export default function BasicTab({ device }: { device: DeviceDetail }) {
  // 厂商与门店在详情里都只有 id。这两份映射只为了显示得认得出人，取不到就退回 id——
  // 缺 admin:coffee-machines:read 之外的权限时接口会失败，那不该让整屏变成错误页。
  const [manufacturerNames, setManufacturerNames] = useState<Record<string, string>>({});
  const [storeNames, setStoreNames] = useState<Record<string, string>>({});

  useEffect(() => {
    void (async () => {
      try {
        const items = await listManufacturers();
        setManufacturerNames(Object.fromEntries(items.map((item) => [item.id, item.name])));
      } catch {
        // 忽略：退回显示厂商 id。
      }
      try {
        const { items } = await listStores(FULL_PAGE_PARAMS);
        setStoreNames(Object.fromEntries(items.map((item) => [item.id, item.name])));
      } catch {
        // 同上。
      }
    })();
  }, []);

  const status = STATUS_TAG[device.status] ?? { color: 'default', label: device.status };
  const online = VENDOR_ONLINE[vendorOnlineState(device.vendorOnline)];

  // 故障信息有才摆：一台好设备上面挂着三行「—」会让人以为是数据没同步。
  // 单独拼出来（而不是在数组里做条件展开）是为了让每一项都留在这个类型里——
  // 条件展开会把 items 的推断断成 any，render 的两个参数就都没类型了。
  const faultColumns: ProDescriptionsItemProps<DeviceDetail>[] =
    device.lastFaultCode || device.lastFaultMessage
      ? [
          { title: '故障码', dataIndex: 'lastFaultCode', render: (_, row) => row.lastFaultCode || '—' },
          {
            title: '故障描述',
            dataIndex: 'lastFaultMessage',
            render: (_, row) => row.lastFaultMessage || '—',
          },
          { title: '故障时间', dataIndex: 'lastFaultAt', valueType: 'dateTime' },
        ]
      : [];

  const columns: ProDescriptionsItemProps<DeviceDetail>[] = [
    { title: '设备名称', dataIndex: 'deviceName', render: (_, row) => row.deviceName || '—' },
        { title: '设备编码', dataIndex: 'serialUnique', copyable: true },
        {
          title: '设备厂商',
          dataIndex: 'manufacturerId',
          render: (_, row) => manufacturerNames[row.manufacturerId] ?? row.manufacturerId,
        },
        {
          title: '所属门店',
          dataIndex: 'storeId',
          render: (_, row) =>
            row.storeId ? (
              (storeNames[row.storeId] ?? row.storeId)
            ) : (
              <span style={{ color: '#8c8c8c' }}>未分配门店</span>
            ),
        },
        {
          title: '设备状态',
          dataIndex: 'status',
          render: () => <Tag color={status.color}>{status.label}</Tag>,
        },
        {
          title: '厂商侧',
          dataIndex: 'vendorOnline',
          render: () => <Tag color={online.color}>{online.label}</Tag>,
        },
        {
          title: '设备余额',
          dataIndex: 'coffeeBalance',
          render: (_, row) => `¥${formatYuan(row.coffeeBalance)}`,
        },
        {
          // 提货码是设备上的静态验证码，明文。这里只读展示：编辑弹窗里留空表示「不改」，
          // 详情页做一个可以改的地方，就会有人以为这里是唯一的入口（见编辑表单的 tooltip）。
          title: '提货码',
          dataIndex: 'pickupPassword',
          render: (_, row) => row.pickupPassword || <span style={{ color: '#8c8c8c' }}>未设置</span>,
        },
        {
          title: '二维码类型',
          dataIndex: 'qrcodeType',
          render: (_, row) => QRCODE_TYPE[row.qrcodeType] ?? row.qrcodeType,
        },
        {
          // 只有普通二维码才有渠道，小程序码时这格是空的——空着比摆一个「—」更准。
          title: '二维码渠道',
          dataIndex: 'regularQrcodePaymentMethod',
          render: (_, row) =>
            row.qrcodeType === 'regular' && row.regularQrcodePaymentMethod
              ? (PAYMENT_METHOD[row.regularQrcodePaymentMethod] ?? row.regularQrcodePaymentMethod)
              : '—',
        },
        {
          title: 'VIP功能',
          dataIndex: 'showVip',
          render: (_, row) => (row.showVip ? '显示' : '隐藏'),
        },
        {
          title: '核销会员价体验券',
          dataIndex: 'enableCouponVerification',
          render: (_, row) => (row.enableCouponVerification ? '核销' : '不核销'),
        },
        { title: '保修截止', dataIndex: 'warrantyEndAt', valueType: 'dateTime' },
        { title: '设备版本', dataIndex: 'versionNumber', render: (_, row) => row.versionNumber || '—' },
        { title: '安卓版本', dataIndex: 'androidVersion', render: (_, row) => row.androidVersion || '—' },
        { title: '主板版本', dataIndex: 'mainBoardVersion', render: (_, row) => row.mainBoardVersion || '—' },
        { title: '最后同步', dataIndex: 'lastSyncedAt', valueType: 'dateTime' },
        { title: '最后活跃', dataIndex: 'lastActiveAt', valueType: 'dateTime' },
    ...faultColumns,
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
    { title: '设备 ID', dataIndex: 'id', copyable: true },
  ];

  return (
    <ProDescriptions<DeviceDetail>
      column={2}
      dataSource={device}
      title="设备信息"
      columns={columns}
    />
  );
}
