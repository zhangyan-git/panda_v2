import {
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
} from '@ant-design/icons';
import { PageContainer, ProTable } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  listDeviceOptions,
  listDrinks,
  listManufacturers,
  updateDrinkStatus,
  type DeviceSummary,
  type Drink,
  type DrinkStatus,
  type DrinkType,
  type Manufacturer,
} from '../../../services/coffeeMachine';
import { formatYuan } from '../../../services/money';
import { toPageParams } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import DrinkFormModal from './DrinkFormModal';

const STATUS_TAG: Record<DrinkStatus, { color: string; label: string }> = {
  on_shelf: { color: 'green', label: '上架' },
  off_shelf: { color: 'default', label: '下架' },
};

const TYPE_TEXT: Record<DrinkType, string> = {
  milk_coffee: '奶咖',
  black_coffee: '黑咖',
  other: '其他',
};

const DrinksPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Drink | null>(null);
  const [manufacturers, setManufacturers] = useState<Manufacturer[]>([]);
  const [devices, setDevices] = useState<DeviceSummary[]>([]);

  // 厂商与设备都要在列表里显示成名称，搜索下拉也要全集，一次拉完建映射比逐行查接口划算。
  // 缺 coffee_machine:read 之外的权限时这个请求会失败，但这两个名字只是给人看的，
  // 不该因此让整页报错，所以吞掉异常退回显示 id。
  useEffect(() => {
    void (async () => {
      try {
        setManufacturers(await listManufacturers());
      } catch {
        setManufacturers([]);
      }
      try {
        setDevices(await listDeviceOptions());
      } catch {
        setDevices([]);
      }
    })();
  }, []);

  const manufacturerNames = Object.fromEntries(manufacturers.map((m) => [m.id, m.name]));

  /** 设备 id → 「设备名（序列号）」。取不到（超出下拉上限）时下面退回显示 id。 */
  const deviceNames = Object.fromEntries(
    devices.map((d) => [d.id, `${d.deviceName || '未命名设备'}（${d.serialUnique}）`]),
  );

  const columns: ProColumns<Drink>[] = [
    {
      title: '饮品名称',
      dataIndex: 'productName',
      width: 180,
      ellipsis: true,
      fieldProps: { placeholder: '名称' },
    },
    {
      title: '所属设备',
      dataIndex: 'deviceId',
      valueType: 'select',
      width: 200,
      ellipsis: true,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把「没挂设备」的历史行单独说出来。
      valueEnum: Object.fromEntries(
        devices.map((d) => [d.id, { text: `${d.deviceName || '未命名设备'}（${d.serialUnique}）` }]),
      ),
      render: (_, row) =>
        row.deviceId ? (
          (deviceNames[row.deviceId] ?? row.deviceId)
        ) : (
          // 迁移之前留下的行没有设备。它们不会出现在任何设备详情页上，只在这里能看到，
          // 所以给一句明说，别让人以为是设备名没加载出来。
          <Tag color="orange">未分配设备</Tag>
        ),
    },
    {
      title: '厂商',
      dataIndex: 'manufacturerId',
      valueType: 'select',
      width: 160,
      ellipsis: true,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把已停用/已删除厂商的 id 原样显示。
      valueEnum: Object.fromEntries(manufacturers.map((m) => [m.id, { text: m.name }])),
      render: (_, row) => manufacturerNames[row.manufacturerId] ?? row.manufacturerId,
    },
    {
      title: '商品编号',
      dataIndex: 'productNum',
      width: 120,
      search: false,
      render: (_, row) => row.productNum || '—',
    },
    {
      title: '类型',
      dataIndex: 'drinkType',
      width: 90,
      search: false,
      render: (_, row) => (row.drinkType ? TYPE_TEXT[row.drinkType] ?? row.drinkType : '不限'),
    },
    // 接口给的是「分」，直接铺出来就是「1800」；列表按元展示成「18.00」。
    {
      title: '原价（元）',
      dataIndex: 'price',
      width: 100,
      search: false,
      render: (_, row) => formatYuan(row.price),
    },
    {
      title: '会员价（元）',
      dataIndex: 'vipPrice',
      width: 110,
      search: false,
      render: (_, row) => (row.vipPrice ? formatYuan(row.vipPrice) : '—'),
    },
    {
      title: '提货码价（元）',
      dataIndex: 'pickupCodePrice',
      width: 120,
      search: false,
      render: (_, row) => (row.pickupCodePrice ? formatYuan(row.pickupCodePrice) : '—'),
    },
    { title: '排序', dataIndex: 'sort', width: 70, search: false },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      width: 80,
      valueEnum: { on_shelf: { text: '上架' }, off_shelf: { text: '下架' } },
      render: (_, row) => {
        const tag = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
        return <Tag color={tag.color}>{tag.label}</Tag>;
      },
    },
    {
      title: '操作',
      valueType: 'option',
      width: 150,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteCoffeeMachines && (
            <Button
              type="link"
              size="small"
              onClick={() => {
                setEditing(row);
                setFormOpen(true);
              }}
            >
              编辑
            </Button>
          )}
          {access.canWriteCoffeeMachines && row.status === 'on_shelf' && (
            <Popconfirm
              title="下架后该饮品在设备上不可售，确认下架？"
              onConfirm={async () => {
                try {
                  await updateDrinkStatus(row.id, 'off_shelf');
                  message.success('已下架');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '下架失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                下架
              </Button>
            </Popconfirm>
          )}
          {access.canWriteCoffeeMachines && row.status === 'off_shelf' && (
            <Popconfirm
              title="确认上架该饮品？"
              onConfirm={async () => {
                try {
                  await updateDrinkStatus(row.id, 'on_shelf');
                  message.success('已上架');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '上架失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                上架
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="饮品管理">
      <ProTable<Drink>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1300 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const result = await listDrinks(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        toolBarRender={() => [
          access.canWriteCoffeeMachines && (
            <Button
              key="add"
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => {
                setEditing(null);
                setFormOpen(true);
              }}
            >
              新建饮品
            </Button>
          ),
        ]}
      />

      <DrinkFormModal
        open={formOpen}
        onOpenChange={setFormOpen}
        editing={editing}
        onSaved={() => actionRef.current?.reload()}
      />
    </PageContainer>
  );
};

export default DrinksPage;
