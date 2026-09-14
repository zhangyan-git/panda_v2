import {
  PauseCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
  WalletOutlined,
} from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDependency,
  ProFormDigit,
  ProFormRadio,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Space, Tag, Tooltip } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import {
  adjustDeviceBalance,
  createDevice,
  getDevice,
  listDevices,
  listManufacturers,
  updateDevice,
  updateDeviceStatus,
  type DeviceDetail,
  type DeviceInput,
  type DeviceStatus,
  type DeviceSummary,
  type Manufacturer,
  type QrcodeType,
  type RegularQrcodePaymentMethod,
} from '../../../services/coffeeMachine';
import { toRFC3339 } from '../../../services/datetime';
import { FULL_PAGE_PARAMS, toPageParams } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { listStores, type Store } from '../../../services/store';

const STATUS_TAG: Record<DeviceStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '在架' },
  disabled: { color: 'default', label: '停用' },
};

const PAYMENT_METHOD_TEXT: Record<RegularQrcodePaymentMethod, string> = {
  fengxuan_wanlian: '丰选万联',
  youlian: '友联',
};

// 接口和库里金额一律是「分」的整数；页面按「元」录入和展示。换算只发生在这里，
// 别在别处再写一次 /100 —— 单位错位（12.50 存成 12）不会报错，只会静默算错钱。
//
// 用 Math.round 而不是直接截断：1.15 * 100 在 IEEE754 下是 114.99999999999999，
// 截断会悄悄少收一分钱。输入框另外用 precision={2} 限死两位小数。
const yuanToFen = (yuan?: number) => Math.round(Number(yuan ?? 0) * 100);
const fenToYuan = (fen?: number) => Number(fen ?? 0) / 100;
const formatYuan = (fen?: number) => fenToYuan(fen).toFixed(2);

/** axios 错误里的 HTTP 状态码，没有响应（断网、超时）时是 undefined。 */
const statusOf = (error: unknown) =>
  (error as { response?: { status?: number } } | null)?.response?.status;

type DeviceFormValues = {
  serialUnique: string;
  deviceName?: string;
  manufacturerId: string;
  storeId?: string | null;
  qrcodeType: QrcodeType;
  regularQrcodePaymentMethod?: RegularQrcodePaymentMethod;
  pickupPassword?: string;
  showVip?: boolean;
  enableCouponVerification?: boolean;
  /** dayjs 或接口带回来的字符串，统一交给 toRFC3339 */
  warrantyEndAt?: unknown;
};

type BalanceFormValues = {
  direction: 'increase' | 'decrease';
  amount?: number;
  remark?: string;
};

/**
 * 详情 → 表单值。
 *
 * **刻意不带 pickupPassword**：那一栏编辑时留空表示「不改」（后端按 nil 处理，
 * COALESCE 保住旧值）。回填了它，一次普通改名就会把店员正在用的码抹掉，而且要等到
 * 有人真去机器上提货才会发现。要看得见当前码，走字段上的 tooltip。
 */
const toFormValues = (device: DeviceDetail): DeviceFormValues => ({
  serialUnique: device.serialUnique,
  deviceName: device.deviceName,
  manufacturerId: device.manufacturerId,
  storeId: device.storeId,
  qrcodeType: device.qrcodeType,
  regularQrcodePaymentMethod: device.regularQrcodePaymentMethod ?? undefined,
  showVip: device.showVip,
  enableCouponVerification: device.enableCouponVerification,
  warrantyEndAt: device.warrantyEndAt ?? undefined,
});

/** 表单值 → 提交载荷，顺带把几处「表单里还留着、但不该发出去」的值清干净。 */
const toPayload = (values: DeviceFormValues): DeviceInput => ({
  serialUnique: values.serialUnique,
  deviceName: values.deviceName,
  manufacturerId: values.manufacturerId,
  storeId: values.storeId ?? null,
  qrcodeType: values.qrcodeType,
  // 切回 miniprogram 时把支付方式显式写成 null。后端 buildDevice 也会清掉它，但
  // 这里必须自己清：antd 卸载字段时默认 preserve，切回小程序后旧的支付方式还留在
  // 表单里，不清就会跟着一起发出去。
  regularQrcodePaymentMethod:
    values.qrcodeType === 'regular' ? values.regularQrcodePaymentMethod : null,
  // 留空（undefined/空串）都表示不改，后端按 nil 处理。它没有「清空」这条路。
  pickupPassword: values.pickupPassword,
  showVip: values.showVip,
  enableCouponVerification: values.enableCouponVerification,
  warrantyEndAt: toRFC3339(values.warrantyEndAt),
});

const DevicesPage: React.FC = () => {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  const [manufacturers, setManufacturers] = useState<Manufacturer[]>([]);
  const [stores, setStores] = useState<Store[]>([]);

  // 新建/编辑
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<DeviceDetail | null>(null);

  // 余额调整
  const [balanceOpen, setBalanceOpen] = useState(false);
  const [balanceTarget, setBalanceTarget] = useState<DeviceDetail | null>(null);
  // 幂等键，跟着「打开弹窗」走：同一个 requestId 重发只会记一次账，所以它必须在
  // 用户改金额时保持不动，只在这一轮调整结束时才换。
  const [balanceRequestId, setBalanceRequestId] = useState('');

  // 详情要先取回来才敢开弹窗，取的过程中把这一行的按钮转起来。
  const [loadingID, setLoadingID] = useState<string>();

  // 列表要把 id 显示成名称，搜索下拉也要全集，一次拉完建映射比逐行查接口划算。
  // 缺 admin:stores:view 时门店那个请求会失败，但点位名只是给人看的，不该因此让
  // 整页报错，所以门店单独吞异常，退回显示 id。
  useEffect(() => {
    void (async () => {
      try {
        setManufacturers(await listManufacturers());
      } catch {
        setManufacturers([]);
      }
      try {
        const { items } = await listStores(FULL_PAGE_PARAMS);
        setStores(items);
      } catch {
        setStores([]);
      }
    })();
  }, []);

  const manufacturerNames = Object.fromEntries(manufacturers.map((m) => [m.id, m.name]));
  const storeNames = Object.fromEntries(stores.map((s) => [s.id, s.name]));

  const manufacturerOptions = async () => {
    const items = await listManufacturers();
    return items.map((m) => ({ label: m.name, value: m.id }));
  };

  const storeOptions = async () => {
    const { items } = await listStores(FULL_PAGE_PARAMS);
    return items.map((s) => ({
      label: s.brandName ? `${s.brandName} / ${s.name}` : s.name,
      value: s.id,
    }));
  };

  /**
   * 打开任何依赖详情的弹窗之前先取一次详情。
   *
   * 列表那条 DeviceSummary 刻意不带二维码配置、开关、保修期和余额（见后端
   * dto.DeviceDetail），所以编辑表单和余额调整都只能用详情接口的数据。取回来再开
   * 弹窗，而不是先开再填：否则表单会先用空值挂载一次，值回来时再改，用户看得见闪。
   */
  const openWithDetail = async (row: DeviceSummary, open: (detail: DeviceDetail) => void) => {
    setLoadingID(row.id);
    try {
      open(await getDevice(row.id));
    } catch (error) {
      message.error(requestErrorMessage(error, '读取设备详情失败'));
    } finally {
      setLoadingID(undefined);
    }
  };

  /** 重新取一次当前设备的详情。409 之后要把真实余额摆出来，不能拿旧值糊弄。 */
  const refreshBalanceTarget = async (): Promise<DeviceDetail | null> => {
    if (!balanceTarget) return null;
    try {
      const latest = await getDevice(balanceTarget.id);
      setBalanceTarget(latest);
      actionRef.current?.reload();
      return latest;
    } catch {
      // 拉不到就保持原样：提示里已经让人去核对了，这里再弹一个错误只会盖住那句。
      return null;
    }
  };

  const columns: ProColumns<DeviceSummary>[] = [
    { title: '序列号', dataIndex: 'serialUnique', width: 160, ellipsis: true },
    {
      title: '设备名称',
      dataIndex: 'deviceName',
      width: 160,
      ellipsis: true,
      fieldProps: { placeholder: '名称' },
      render: (_, row) => row.deviceName || '—',
    },
    {
      title: '厂商',
      dataIndex: 'manufacturerId',
      valueType: 'select',
      width: 150,
      ellipsis: true,
      // valueEnum 只喂搜索下拉；表格里走下面的 render，好把已停用厂商的 id 原样显示。
      valueEnum: Object.fromEntries(manufacturers.map((m) => [m.id, { text: m.name }])),
      render: (_, row) => manufacturerNames[row.manufacturerId] ?? row.manufacturerId,
    },
    {
      title: '点位',
      dataIndex: 'storeId',
      valueType: 'select',
      width: 160,
      ellipsis: true,
      valueEnum: Object.fromEntries(stores.map((s) => [s.id, { text: s.name }])),
      render: (_, row) =>
        row.storeId ? storeNames[row.storeId] ?? row.storeId : <span style={{ color: '#999' }}>未挂点位</span>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      width: 80,
      valueEnum: { active: { text: '在架' }, disabled: { text: '停用' } },
      render: (_, row) => {
        const tag = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
        return <Tag color={tag.color}>{tag.label}</Tag>;
      },
    },
    {
      title: '厂商侧',
      dataIndex: 'vendorOnline',
      width: 90,
      search: false,
      // 三态：null 是「从未同步过」，把它显示成「离线」会让一台从没接上的设备
      // 看起来像掉线了，排查方向就错了。
      render: (_, row) => {
        if (row.vendorOnline === null || row.vendorOnline === undefined) {
          return <Tag>未同步</Tag>;
        }
        return row.vendorOnline ? <Tag color="green">在线</Tag> : <Tag color="red">离线</Tag>;
      },
    },
    {
      title: '故障',
      dataIndex: 'lastFaultCode',
      width: 100,
      search: false,
      render: (_, row) =>
        row.lastFaultCode ? (
          <Tooltip title={row.lastFaultMessage || undefined}>
            <Tag color="volcano">{row.lastFaultCode}</Tag>
          </Tooltip>
        ) : (
          '—'
        ),
    },
    {
      title: '最后同步',
      dataIndex: 'lastSyncedAt',
      valueType: 'dateTime',
      width: 160,
      search: false,
      render: (_, row) => (row.lastSyncedAt ? row.lastSyncedAt : '—'),
    },
    {
      title: '最后活跃',
      dataIndex: 'lastActiveAt',
      valueType: 'dateTime',
      width: 160,
      search: false,
      render: (_, row) => (row.lastActiveAt ? row.lastActiveAt : '—'),
    },
    {
      title: '操作',
      valueType: 'option',
      width: 200,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          {access.canWriteCoffeeMachines && (
            <Button
              type="link"
              size="small"
              loading={loadingID === row.id}
              onClick={() =>
                openWithDetail(row, (detail) => {
                  setEditing(detail);
                  setFormOpen(true);
                })
              }
            >
              编辑
            </Button>
          )}
          {access.canAdjustCoffeeBalance && (
            <Button
              type="link"
              size="small"
              icon={<WalletOutlined />}
              loading={loadingID === row.id}
              onClick={() =>
                openWithDetail(row, (detail) => {
                  setBalanceTarget(detail);
                  // 每开一次弹窗就是一次新的调整意图，配一枚新的幂等键。
                  setBalanceRequestId(crypto.randomUUID());
                  setBalanceOpen(true);
                })
              }
            >
              余额
            </Button>
          )}
          {access.canWriteCoffeeMachines && row.status === 'active' && (
            <Popconfirm
              title="停用后设备不可用，确认停用？"
              onConfirm={async () => {
                try {
                  await updateDeviceStatus(row.id, 'disabled');
                  message.success('已停用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '停用失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger icon={<PauseCircleOutlined />}>
                停用
              </Button>
            </Popconfirm>
          )}
          {access.canWriteCoffeeMachines && row.status === 'disabled' && (
            <Popconfirm
              title="确认启用在设备？"
              onConfirm={async () => {
                try {
                  await updateDeviceStatus(row.id, 'active');
                  message.success('已启用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '启用失败，请稍后重试'));
                }
              }}
            >
              <Button type="link" size="small" icon={<PlayCircleOutlined />}>
                启用
              </Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="设备管理">
      <ProTable<DeviceSummary>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1500 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const result = await listDevices(toPageParams(params));
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
              新建设备
            </Button>
          ),
        ]}
      />

      {/* 新建/编辑设备 */}
      <ModalForm<DeviceFormValues>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑设备「${editing.deviceName || editing.serialUnique}」` : '新建设备'}
        open={formOpen}
        onOpenChange={setFormOpen}
        modalProps={{ destroyOnClose: true }}
        initialValues={
          editing ? toFormValues(editing) : { qrcodeType: 'miniprogram', showVip: true }
        }
        onFinish={async (values) => {
          try {
            if (editing) {
              await updateDevice(editing.id, toPayload(values));
              message.success('已保存');
            } else {
              await createDevice(toPayload(values));
              message.success('已创建');
            }
          } catch (error) {
            // 序列号撞唯一键回 409，后端的中文说明比「保存失败」有用得多。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
          actionRef.current?.reload();
          return true;
        }}
      >
        <ProFormText
          name="serialUnique"
          label="设备序列号"
          rules={[{ required: true, message: '请输入设备序列号' }]}
        />
        <ProFormText name="deviceName" label="设备名称" />
        <ProFormSelect
          name="manufacturerId"
          label="所属厂商"
          request={manufacturerOptions}
          showSearch
          rules={[{ required: true, message: '请选择所属厂商' }]}
        />
        <ProFormSelect
          name="storeId"
          label="点位"
          request={storeOptions}
          allowClear
          showSearch
          placeholder="不选表示不挂点位"
        />
        <ProFormSelect
          name="qrcodeType"
          label="二维码类型"
          options={[
            { label: '小程序码', value: 'miniprogram' },
            { label: '普通二维码', value: 'regular' },
          ]}
          rules={[{ required: true, message: '请选择二维码类型' }]}
        />
        <ProFormDependency name={['qrcodeType']}>
          {({ qrcodeType }) =>
            qrcodeType === 'regular' ? (
              <ProFormSelect
                name="regularQrcodePaymentMethod"
                label="扫码支付方式"
                options={[
                  { label: '丰选万联', value: 'fengxuan_wanlian' },
                  { label: '友联', value: 'youlian' },
                ]}
                rules={[{ required: true, message: '二维码类型为普通二维码时必须指定支付方式' }]}
              />
            ) : null
          }
        </ProFormDependency>
        <ProFormText
          name="pickupPassword"
          label="提货码"
          fieldProps={{ placeholder: editing ? '留空表示不修改' : '留空表示不设置' }}
          tooltip={
            editing
              ? `店员在设备上敲的静态验证码。当前：${
                  editing.pickupPassword || '未设置'
                }。留空表示不修改；填了就换成新的（没有「清空」这条路）。`
              : '店员在设备上敲的静态验证码。留空表示不设置。'
          }
        />
        <ProFormSwitch name="showVip" label="显示会员价" />
        <ProFormSwitch name="enableCouponVerification" label="启用车机核销优惠券" />
        <ProFormDateTimePicker
          name="warrantyEndAt"
          label="保修截止"
          width="md"
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
        />
      </ModalForm>

      {/* 余额调整 */}
      <ModalForm<BalanceFormValues>
        key={balanceTarget?.id ?? 'balance'}
        title={`调整余额「${balanceTarget?.deviceName || balanceTarget?.serialUnique || ''}」`}
        open={balanceOpen}
        onOpenChange={setBalanceOpen}
        modalProps={{ destroyOnClose: true }}
        initialValues={{ direction: 'increase' }}
        onFinish={async (values) => {
          if (!balanceTarget) return false;
          const fen = yuanToFen(values.amount);
          if (fen <= 0) {
            message.error('调整金额必须大于 0');
            return false;
          }
          try {
            const result = await adjustDeviceBalance(balanceTarget.id, {
              amount: values.direction === 'decrease' ? -fen : fen,
              requestId: balanceRequestId,
              remark: values.remark,
            });
            message.success(`已调整，当前余额 ¥${formatYuan(result.coffeeBalance)}`);
            actionRef.current?.reload();
            return true;
          } catch (error) {
            if (statusOf(error) === 409) {
              // 后端把「同一个 requestId 又来了」回成 409 而不是装作成功：上一次
              // 多半已经记过账，只是响应没收到，但到底记没记只有流水说得清，接口
              // 不替调用方下结论。所以这里既不报成功也不报失败，把最新余额摆出来
              // 让人自己核。
              const latest = await refreshBalanceTarget();
              // 这枚 requestId 已经用掉了，留着它下次提交还是 409。换一枚，让「再调
              // 一次」真的是新的一次调整，而不是又撞上同一次的账。
              setBalanceRequestId(crypto.randomUUID());
              message.warning(
                latest
                  ? `这次调整的流水已经记过账（上一次多半已经成功），当前余额 ¥${formatYuan(
                      latest.coffeeBalance,
                    )}，请核对`
                  : '这次调整的流水已经记过账（上一次多半已经成功），请关闭后重新打开核对当前余额',
              );
              return false;
            }
            // 网络错误这类「不知道成没成」的情况：弹窗不关、requestId 不换，重发还是
            // 同一次调整，靠它幂等。
            message.error(requestErrorMessage(error, '调整失败，请稍后重试'));
            return false;
          }
        }}
      >
        <ProFormDependency name={['direction', 'amount']}>
          {({ direction, amount }) => {
            const current = balanceTarget?.coffeeBalance ?? 0;
            const fen = yuanToFen(amount);
            const after = direction === 'decrease' ? current - fen : current + fen;
            return (
              <div style={{ marginBottom: 16 }}>
                <Space size="large">
                  <span>
                    当前余额：<strong>¥{formatYuan(current)}</strong>
                  </span>
                  <span>
                    调整后：
                    <strong style={{ color: after < 0 ? '#cf1322' : undefined }}>
                      ¥{formatYuan(after)}
                    </strong>
                  </span>
                </Space>
                {after < 0 && (
                  <div style={{ color: '#cf1322', marginTop: 4 }}>余额不能为负，请调整金额</div>
                )}
              </div>
            );
          }}
        </ProFormDependency>
        <ProFormRadio.Group
          name="direction"
          label="方向"
          options={[
            { label: '增加', value: 'increase' },
            { label: '减少', value: 'decrease' },
          ]}
          rules={[{ required: true }]}
        />
        {/* 金额填正数，「减少」由方向决定正负号。让用户自己敲负号，很容易出现
            「想减少却填了个正数」，而这是记在流水里的加钱。 */}
        <ProFormDigit
          name="amount"
          label="金额（元）"
          min={0.01}
          fieldProps={{ precision: 2 }}
          rules={[{ required: true, message: '请输入调整金额' }]}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          tooltip="余额调整全程留痕，备注会跟着这次操作一起记进流水"
          placeholder="例如：设备故障补偿"
        />
      </ModalForm>
    </PageContainer>
  );
};

export default DevicesPage;
