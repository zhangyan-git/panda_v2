import { PlusOutlined } from '@ant-design/icons';
import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDependency,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
} from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import {
  Button,
  Card,
  Col,
  Empty,
  Form,
  Input,
  message,
  Pagination,
  Row,
  Select,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  createDevice,
  getDevice,
  listDevices,
  listManufacturers,
  updateDevice,
  type DeviceDetail,
  type DeviceInput,
  type DeviceStatus,
  type DeviceSummary,
  type Manufacturer,
  type QrcodeType,
  type RegularQrcodePaymentMethod,
} from '../../../services/coffeeMachine';
import { formatDateTime, toRFC3339 } from '../../../services/datetime';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { listStores, type Store } from '../../../services/store';
import { scrollableModalBody } from '../../../components/common/modalProps';

const STATUS_TAG: Record<DeviceStatus, { color: string; label: string }> = {
  active: { color: 'green', label: '在架' },
  disabled: { color: 'default', label: '停用' },
};

/**
 * 厂商侧在线状态的三态。null 是「从未同步过」——把它显示成「离线」会让一台还没接上的
 * 设备看起来像掉线了，排查方向就从「同步没做」歪到「网络故障」。
 */
const VENDOR_ONLINE: Record<'online' | 'offline' | 'unknown', { color: string; label: string }> = {
  online: { color: '#52c41a', label: '在线' },
  offline: { color: '#ff4d4f', label: '离线' },
  unknown: { color: '#d9d9d9', label: '未同步' },
};

const vendorOnlineState = (value: DeviceSummary['vendorOnline']): keyof typeof VENDOR_ONLINE => {
  if (value === null || value === undefined) return 'unknown';
  return value ? 'online' : 'offline';
};

/** 一页 12 张：三列四行，翻页不至于碎。 */
const PAGE_SIZE = 12;

type FilterValues = {
  status?: DeviceStatus;
  brandId?: string;
  storeIds?: string[];
  manufacturerId?: string;
  keyword?: string;
};

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

/**
 * 品牌 → 门店 id。
 *
 * 品牌**不是**咖啡机库的列（设备身上只有 store_id），所以这一栏只能在前端换算成门店：
 * 后端不认识品牌，为了一个筛选项让它反向去调商户服务不划算。换算用的是门店下拉本来
 * 就加载好的那份全集。
 *
 * 返回 null 表示「门店这个维度上没有约束」；返回**空数组**表示「按这些条件不可能有
 * 结果」。两者必须分开：空数组当作「不筛」传给后端的话，「选了品牌 X，但 X 名下没有
 * 门店」会把整张表原样返回，看起来跟没筛一样——而且没有任何报错。
 */
function resolveStoreIds(values: FilterValues, stores: Store[]): string[] | null {
  const picked = values.storeIds ?? [];
  if (!values.brandId) return picked.length ? picked : null;
  const inBrand = stores.filter((store) => store.brandId === values.brandId).map((store) => store.id);
  // 品牌与门店是「与」：两个都选了就取交集，不是并集。
  return picked.length ? picked.filter((id) => inBrand.includes(id)) : inBrand;
}

/** 卡片里的一行「标签：值」。标签宽度固定，四行的值才能对齐。 */
function CardField({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', gap: 8, lineHeight: '24px' }}>
      <span style={{ flex: '0 0 64px', color: '#8c8c8c' }}>{label}</span>
      <span
        style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
        title={typeof children === 'string' ? children : undefined}
      >
        {children}
      </span>
    </div>
  );
}

const DevicesPage: React.FC = () => {
  const access = useAccess();
  const [form] = Form.useForm<FilterValues>();

  // 已提交的筛选条件。表单里正在编辑的那一份不算，按「搜索」才生效。
  const [query, setQuery] = useState<FilterValues>({});
  const [page, setPage] = useState(1);
  const [rows, setRows] = useState<DeviceSummary[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);

  const [manufacturers, setManufacturers] = useState<Manufacturer[]>([]);
  const [stores, setStores] = useState<Store[]>([]);

  // 换算品牌用的门店全集。走 ref 而不是让 load 依赖 stores：那份全集是异步到位的，
  // 依赖它会让 load 的引用在它到位时变一次，页面刚打开就白拉一遍列表。
  const storesRef = useRef<Store[]>([]);

  // 新建/编辑
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<DeviceDetail | null>(null);
  // 详情要先取回来才敢开弹窗，取的过程中把这一张卡片的按钮转起来。
  const [loadingID, setLoadingID] = useState<string>();

  // 列表要把 id 显示成名称，筛选下拉也要全集，一次拉完建映射比逐行查接口划算。
  // 缺 admin:stores:view 时门店那个请求会失败，但点位名只是给人看的，不该因此让
  // 整页报错，所以门店单独吞异常，退回显示 id。
  //
  // ⚠️ 上限是 FULL_PAGE_PARAMS 的 200 条，品牌换算也吃这个上限（门店超过 200 家时，
  // 第 200 家之后的门店既不会出现在下拉里，也不会被品牌带出来）。这个上限在改成本
  // 页之前就存在——门店下拉本来就只加载 200 条——这里没有新增问题。真要支持更多门店，
  // 该做的是「远程搜索的门店下拉」，那是另一件事。
  useEffect(() => {
    void (async () => {
      try {
        setManufacturers(await listManufacturers());
      } catch {
        setManufacturers([]);
      }
      try {
        const { items } = await listStores(FULL_PAGE_PARAMS);
        storesRef.current = items;
        setStores(items);
      } catch {
        storesRef.current = [];
        setStores([]);
      }
    })();
  }, []);

  const manufacturerNames = useMemo(
    () => Object.fromEntries(manufacturers.map((m) => [m.id, m.name])),
    [manufacturers],
  );
  const storeById = useMemo(
    () => new Map(stores.map((store) => [store.id, store])),
    [stores],
  );

  /**
   * 品牌下拉的选项从门店全集里派生。
   *
   * 没有挂门店的品牌不会出现在这里——选中这样一项一定筛出空列表，摆出来只会让人以为
   * 是系统坏了。这也让「品牌选项」和「品牌能带出哪些门店」用的是同一份数据，两者不会
   * 对不上。
   */
  const brandOptions = useMemo(() => {
    const seen = new Map<string, string>();
    stores.forEach((store) => {
      if (store.brandId && !seen.has(store.brandId)) {
        seen.set(store.brandId, store.brandName || store.brandId);
      }
    });
    return [...seen].map(([value, label]) => ({ value, label }));
  }, [stores]);

  const storeOptions = useMemo(
    () =>
      stores.map((store) => ({
        label: store.brandName ? `${store.brandName} / ${store.name}` : store.name,
        value: store.id,
      })),
    [stores],
  );

  const manufacturerOptions = useMemo(
    () => manufacturers.map((m) => ({ label: m.name, value: m.id })),
    [manufacturers],
  );

  const load = useCallback(async (values: FilterValues, current: number) => {
    const storeIds = resolveStoreIds(values, storesRef.current);
    if (storeIds && storeIds.length === 0) {
      // 选了品牌，但该品牌名下没有门店（或与所选门店没有交集）：这一页注定是空的，
      // 不必发请求——空数组发出去会被后端当作「不筛门店」，把全量拿回来。
      setRows([]);
      setTotal(0);
      return;
    }
    setLoading(true);
    try {
      const result = await listDevices({
        page: current,
        pageSize: PAGE_SIZE,
        status: values.status,
        manufacturerId: values.manufacturerId,
        keyword: values.keyword?.trim() || undefined,
        storeIds: storeIds ?? undefined,
      });
      setRows(result.items);
      setTotal(result.total);
    } catch (error) {
      message.error(requestErrorMessage(error, '加载设备列表失败'));
      setRows([]);
      setTotal(0);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(query, page);
  }, [load, query, page]);

  const onSearch = () => {
    // 换条件要回到第 1 页：停在第 3 页搜一个只有一页结果的条件，会看到一张空列表，
    // 而「没有匹配」和「页码越界」在界面上长得一模一样。
    setPage(1);
    setQuery({ ...form.getFieldsValue() });
  };

  const onReset = () => {
    form.resetFields();
    setPage(1);
    setQuery({});
  };

  /**
   * 打开编辑弹窗之前先取一次详情。
   *
   * 列表那条 DeviceSummary 刻意不带二维码配置、开关和保修期（见后端 dto.DeviceDetail），
   * 所以编辑表单只能用详情接口的数据。取回来再开弹窗，而不是先开再填：否则表单会先用
   * 空值挂载一次，值回来时再改，用户看得见闪。
   */
  const openEditor = async (row: DeviceSummary) => {
    setLoadingID(row.id);
    try {
      setEditing(await getDevice(row.id));
      setFormOpen(true);
    } catch (error) {
      message.error(requestErrorMessage(error, '读取设备详情失败'));
    } finally {
      setLoadingID(undefined);
    }
  };

  return (
    <PageContainer
      title="设备管理"
      extra={
        access.canWriteCoffeeMachines && (
          <Button
            type="primary"
            icon={<PlusOutlined />}
            onClick={() => {
              setEditing(null);
              setFormOpen(true);
            }}
          >
            新建设备
          </Button>
        )
      }
    >
      <Card style={{ marginBottom: 16 }}>
        <Form form={form} layout="inline" style={{ rowGap: 12 }}>
          <Form.Item name="status" label="设备状态">
            <Select
              allowClear
              placeholder="全部"
              style={{ width: 120 }}
              options={[
                { label: '在架', value: 'active' },
                { label: '停用', value: 'disabled' },
              ]}
            />
          </Form.Item>
          <Form.Item name="brandId" label="品牌">
            <Select
              allowClear
              showSearch
              optionFilterProp="label"
              placeholder="全部"
              style={{ width: 160 }}
              options={brandOptions}
            />
          </Form.Item>
          <Form.Item name="storeIds" label="门店">
            {/* 多选：筛「这几家店里的设备」是一次很常见的动作（同品牌下的几家、
                或一个片区），单选的话得来回搜好几趟。 */}
            <Select
              allowClear
              mode="multiple"
              showSearch
              optionFilterProp="label"
              placeholder="全部"
              style={{ width: 260 }}
              maxTagCount="responsive"
              options={storeOptions}
            />
          </Form.Item>
          <Form.Item name="manufacturerId" label="设备厂商">
            <Select
              allowClear
              showSearch
              optionFilterProp="label"
              placeholder="全部"
              style={{ width: 160 }}
              options={manufacturerOptions}
            />
          </Form.Item>
          <Form.Item name="keyword" label="设备标识">
            <Input allowClear placeholder="按设备序列号模糊匹配" style={{ width: 200 }} />
          </Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" onClick={onSearch}>
                搜索
              </Button>
              <Button onClick={onReset}>重置</Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>

      <Spin spinning={loading}>
        {rows.length === 0 && !loading ? (
          <Card>
            <Empty description="没有符合条件的设备" />
          </Card>
        ) : (
          <Row gutter={[16, 16]}>
            {rows.map((row) => {
              const store = row.storeId ? storeById.get(row.storeId) : undefined;
              const status = STATUS_TAG[row.status] ?? { color: 'default', label: row.status };
              const online = VENDOR_ONLINE[vendorOnlineState(row.vendorOnline)];
              // 卡片底部那行小字是表格时代「故障 / 最后同步 / 最后活跃」三列的替身：
              // 有值才显示，没值的项不留空位，免得每张卡都拖一行「— — —」。
              const meta = [
                row.lastFaultCode && `故障 ${row.lastFaultCode}`,
                row.lastSyncedAt && `最后同步 ${formatDateTime(row.lastSyncedAt)}`,
                row.lastActiveAt && `最后活跃 ${formatDateTime(row.lastActiveAt)}`,
              ].filter(Boolean) as string[];

              return (
                <Col key={row.id} xs={24} sm={12} lg={8} xxl={6}>
                  <Card
                    size="small"
                    // 整张卡片进详情（hoverable 同时给出可点的手感和指针形状）。
                    // 卡里的按钮必须自己 stopPropagation，否则「点编辑」会先冒泡上来
                    // 导航一次，弹窗开在详情页上。
                    hoverable
                    onClick={() => history.push(`/coffee-machines/devices/${row.id}`)}
                    title={store?.name ?? <span style={{ color: '#8c8c8c' }}>未分配门店</span>}
                    extra={
                      <Space size={8}>
                        <Tag color={status.color} style={{ marginInlineEnd: 0 }}>
                          {status.label}
                        </Tag>
                        <Tooltip title={`厂商侧：${online.label}`}>
                          <span
                            style={{
                              display: 'inline-block',
                              width: 8,
                              height: 8,
                              borderRadius: '50%',
                              background: online.color,
                            }}
                          />
                        </Tooltip>
                      </Space>
                    }
                    styles={{ body: { padding: '12px 16px' } }}
                    actions={[
                      access.canWriteCoffeeMachines ? (
                        <Button
                          key="edit"
                          type="link"
                          size="small"
                          loading={loadingID === row.id}
                          // 拦住冒泡：卡片本体也是「进详情」，不拦就会一边开弹窗一边导航。
                          onClick={(event) => {
                            event.stopPropagation();
                            void openEditor(row);
                          }}
                        >
                          编辑
                        </Button>
                      ) : (
                        <span key="edit" style={{ color: '#bfbfbf' }}>
                          无编辑权限
                        </span>
                      ),
                    ]}
                  >
                    <CardField label="门店名称">
                      {store?.name ?? <span style={{ color: '#8c8c8c' }}>未分配门店</span>}
                    </CardField>
                    <CardField label="设备名称">
                      {/* 整张卡片已经能进详情了，名字这里仍然做成链接：蓝色的名字是
                          「这里能进去」最直接的提示，而且键盘用户只有它走得了。
                          stopPropagation 是为了不推两条历史记录（卡片本体的 onClick
                          也会导航，不拦的话点一次名字要按两次返回）。 */}
                      <Typography.Link
                        onClick={(event) => {
                          event.stopPropagation();
                          history.push(`/coffee-machines/devices/${row.id}`);
                        }}
                      >
                        {row.deviceName || row.serialUnique}
                      </Typography.Link>
                    </CardField>
                    <CardField label="设备编码">{row.serialUnique}</CardField>
                    <CardField label="门店地址">
                      {store?.address || <span style={{ color: '#8c8c8c' }}>—</span>}
                    </CardField>
                    <CardField label="厂商">{manufacturerNames[row.manufacturerId] ?? row.manufacturerId}</CardField>
                    {meta.length > 0 && (
                      <div
                        style={{
                          marginTop: 8,
                          paddingTop: 8,
                          borderTop: '1px solid #f0f0f0',
                          color: '#8c8c8c',
                          fontSize: 12,
                          lineHeight: '18px',
                        }}
                        title={row.lastFaultMessage || undefined}
                      >
                        {meta.join(' · ')}
                      </div>
                    )}
                  </Card>
                </Col>
              );
            })}
          </Row>
        )}
      </Spin>

      {total > 0 && (
        <div style={{ marginTop: 16, textAlign: 'right' }}>
          <Pagination
            current={page}
            pageSize={PAGE_SIZE}
            total={total}
            showSizeChanger={false}
            showTotal={(count) => `共 ${count} 台设备`}
            onChange={setPage}
          />
        </div>
      )}

      {/* 新建/编辑设备 */}
      <ModalForm<DeviceFormValues>
        key={editing?.id ?? 'create'}
        title={editing ? `编辑设备「${editing.deviceName || editing.serialUnique}」` : '新建设备'}
        open={formOpen}
        onOpenChange={setFormOpen}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
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
          await load(query, page);
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
          label="设备厂商"
          options={manufacturerOptions}
          showSearch
          rules={[{ required: true, message: '请选择设备厂商' }]}
        />
        <ProFormSelect
          name="storeId"
          label="所属门店"
          options={storeOptions}
          allowClear
          showSearch
          placeholder="不选表示不挂门店"
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
                label="普通二维码所属渠道"
                options={[
                  { label: '丰选万联', value: 'fengxuan_wanlian' },
                  { label: '优联', value: 'youlian' },
                ]}
                rules={[{ required: true, message: '请选择普通二维码所属渠道' }]}
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
        <ProFormSwitch
          name="showVip"
          label="是否显示VIP功能"
          fieldProps={{ checkedChildren: '显示', unCheckedChildren: '隐藏' }}
        />
        <ProFormSwitch
          name="enableCouponVerification"
          label="核销会员价体验券"
          fieldProps={{ checkedChildren: '核销', unCheckedChildren: '不核销' }}
          tooltip="关闭后会员价体验券使用不扣减，可复用"
        />
        <ProFormDateTimePicker
          name="warrantyEndAt"
          label="保修截止日期"
          width="md"
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
        />
      </ModalForm>
    </PageContainer>
  );
};

export default DevicesPage;
