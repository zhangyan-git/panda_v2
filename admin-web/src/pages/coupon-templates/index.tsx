import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDependency,
  ProFormDigit,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { Button, message, Popconfirm, Space, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns, ProFormInstance } from '@ant-design/pro-components';
import {
  auditCouponTemplate,
  createCouponTemplate,
  issueCoupons,
  listCouponTemplates,
  listCouponTypes,
  updateCouponTemplate,
  updateCouponTemplateStatus,
  type CouponTemplate,
  type TemplateInput,
} from '../../services/coupon';
import { listBrands } from '../../services/brand';
import { listStores, type Store } from '../../services/store';
import { listMerchants } from '../../services/merchant';
import { toRFC3339 } from '../../services/datetime';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { useAccess } from '@umijs/max';

// 表单里金额用 InputNumber **按元**编辑（运营的习惯），提交时换成分。
type TemplateFormValues = Omit<TemplateInput, 'faceValue' | 'minPurchaseAmount' | 'purchasePrice'> & {
  faceValue?: number;
  minPurchaseAmount?: number;
  purchasePrice?: number;
};

// 发券弹窗的表单值。templateName 只是把「发给哪个模板」显示出来，不进提交载荷：
// 模板由行内按钮决定，不允许手填 ID。
type IssueFormValues = {
  templateName?: string;
  userIds: string;
  quantityPerUser: number;
  reason?: string;
};

// 接口和库里金额一律是「分」的整数；页面按「元」录入和展示。换算只发生在这里，
// 别在别处再写一次 /100 —— 单位错位（12.50 存成 12）不会报错，只会静默算错钱。
//
// 用 Math.round 而不是直接截断：1.15 * 100 在 IEEE754 下是 114.99999999999999，
// 截断会悄悄少收一分钱。输入框另外用 precision={2} 限死两位小数。
const yuanToFen = (yuan?: number) => Math.round(Number(yuan ?? 0) * 100);
const fenToYuan = (fen?: number) => Number(fen ?? 0) / 100;
// 展示用，固定两位小数，与表单口径一致。不拼 ¥：这一列原本就没有币种前缀。
const formatYuan = (fen?: number) => fenToYuan(fen).toFixed(2);

// validFrom/validTo 是 Go 的 *time.Time，只认 RFC3339；dateFormatter 在这里不生效，
// 所以提交前显式转一次（见 services/datetime.ts 里的说明）。

// 逐字段挑，不用展开：模板响应比表单多出 auditStatus/status/createdAt 等，
// 展开会把它们混进提交载荷。
const toFormValues = (template: CouponTemplate): TemplateFormValues => ({
  couponTypeId: template.couponTypeId,
  merchantId: template.merchantId,
  name: template.name,
  shortTitle: template.shortTitle,
  description: template.description,
  totalQuantity: template.totalQuantity,
  validityMode: template.validityMode,
  validFrom: template.validFrom,
  validTo: template.validTo,
  validDays: template.validDays,
  claimLimitMode: template.claimLimitMode,
  redemptionType: template.redemptionType,
  visible: template.visible,
  faceValue: fenToYuan(template.faceValue),
  minPurchaseAmount: fenToYuan(template.minPurchaseAmount),
  purchasePrice: fenToYuan(template.purchasePrice),
  // 适用范围必须原样带进表单：PUT 是全量覆盖，漏了这两个字段就是静默清空范围。
  // 接口保证是数组（空数组 = 该层不限），这里只是防御 null。
  brandIds: template.brandIds ?? [],
  storeIds: template.storeIds ?? [],
});

const toPayload = (values: TemplateFormValues): TemplateInput => {
  const payload: TemplateInput = {
    ...values,
    faceValue: yuanToFen(values.faceValue),
    minPurchaseAmount: yuanToFen(values.minPurchaseAmount),
    purchasePrice: yuanToFen(values.purchasePrice),
    claimLimitMode: values.claimLimitMode ?? 'once_ever',
    validFrom: toRFC3339(values.validFrom),
    validTo: toRFC3339(values.validTo),
    // 多选框清空后给的是 undefined，而后端要的是「空数组 = 不限」。不归一的话
    // JSON.stringify 会把 undefined 的 key 整个丢掉，PUT 全量覆盖时旧范围仍在
    // 库里没被删——界面上看着清空了，实际没清掉。
    brandIds: values.brandIds ?? [],
    storeIds: values.storeIds ?? [],
  };
  // 两个模式各自的有效期字段在库里是同一行的 valid_from/valid_to/valid_days，
  // 而 PUT 是全量覆盖（templateUpdateQuery 的 SET 列表里有这三列）。antd 表单
  // 卸载字段时默认 preserve，值还留在 store 里，所以切换到另一个模式后旧值会
  // 照原样发出去 —— valid_to 一旦有值，发券处 COALESCE(valid_to, NOW()+days)
  // 就会压过 valid_days，管理员刚填的天数被静默吃掉。这里按模式显式清干净。
  if (payload.validityMode === 'fixed') {
    delete payload.validDays;
  } else {
    delete payload.validFrom;
    delete payload.validTo;
  }
  return payload;
};

export default function CouponTemplatesPage() {
  const access = useAccess();
  const ref = useRef<ActionType>();
  const formRef = useRef<ProFormInstance<TemplateFormValues>>();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<CouponTemplate>();
  const [issueOpen, setIssueOpen] = useState(false);
  const [issueTarget, setIssueTarget] = useState<CouponTemplate>();
  const [merchantNames, setMerchantNames] = useState<Record<string, string>>({});
  const [brandNames, setBrandNames] = useState<Record<string, string>>({});
  const [storeNames, setStoreNames] = useState<Record<string, string>>({});
  // 记录表单里当前的商户，用来判断「商户是不是真的换了」——见 onValuesChange。
  const lastMerchant = useRef<string | undefined>(undefined);
  // 门店目录：当前商户（不限商户时是全量）的门店列表，带 brandId。
  // 「品牌 → 门店」这一级的收窄在客户端做：listStores 只收单个 brandId，而品牌
  // 是多选，逐品牌发请求要么串行要么并发合并，前者慢、后者有竞态；这份列表本来
  // 就要为下拉框拉一次。这里存的是**未经品牌过滤**的全量，供 onValuesChange
  // 判断已选门店是不是还落在当前品牌集合里。
  const storeCatalog = useRef<Store[]>([]);

  // 「适用范围」列要把 id 显示成名称。商户/品牌/门店都是个位数量级，一次拉全集建
  // 映射比在 render 里逐行查接口划算。缺 admin:merchants/brands/stores:view 权限时
  // 请求会失败，这一列只是给人看的，不该因此让整页报错，所以吞掉异常退回显示 id。
  useEffect(() => {
    void (async () => {
      try {
        const [merchants, brands, stores] = await Promise.all([
          listMerchants(FULL_PAGE_PARAMS),
          listBrands(FULL_PAGE_PARAMS),
          listStores(FULL_PAGE_PARAMS),
        ]);
        setMerchantNames(Object.fromEntries(merchants.items.map((item) => [item.id, item.name])));
        setBrandNames(Object.fromEntries(brands.items.map((item) => [item.id, item.name])));
        setStoreNames(Object.fromEntries(stores.items.map((item) => [item.id, item.name])));
      } catch {
        // 忽略：拿不到名字时列里显示原始 id
      }
    })();
  }, []);

  const refresh = async (operation: Promise<unknown>, successMessage: string) => {
    await operation;
    message.success(successMessage);
    ref.current?.reload();
  };

  // 品牌变动后，已选门店里不再属于新品牌集合的那些要去掉：后端对品牌/门店只做
  // 形状校验（跨库，验不了真伪），留下的交叉组合会被全量覆盖的 PUT 原样写进
  // 范围，库里就多出一组界面上再也表达不出来的范围——和「换商户要清品牌/门店」
  // 是同一个理由，只是这一级能判断出哪些仍然合法，就不必整片清掉。
  //
  // 判断用的是未过滤的门店目录；目录为空说明还没成功取到（接口失败或权限不足），
  // 这时宁可不剪：剪错是静默丢数据，留着只是暂时不一致，保存前用户还看得见。
  const pruneStores = (brandIds: string[]) => {
    if (brandIds.length === 0) return; // 不限品牌 → 商户下门店都合法
    const current = (formRef.current?.getFieldValue('storeIds') ?? []) as string[];
    if (current.length === 0) return;
    const catalog = storeCatalog.current;
    if (catalog.length === 0) return;
    const valid = new Set(
      catalog.filter((item) => brandIds.includes(item.brandId)).map((item) => item.id),
    );
    const kept = current.filter((id) => valid.has(id));
    if (kept.length !== current.length) formRef.current?.setFieldsValue({ storeIds: kept });
  };

  const columns: ProColumns<CouponTemplate>[] = [
    { title: '名称', dataIndex: 'name' },
    // 接口给的是「分」，直接铺出来就是「1250」；列表按元展示成「12.50」。
    { title: '面值', dataIndex: 'faceValue', search: false, render: (_, record) => formatYuan(record.faceValue) },
    { title: '总量', dataIndex: 'totalQuantity' },
    {
      title: '审核',
      dataIndex: 'auditStatus',
      render: (_, record) => <Tag>{record.auditStatus}</Tag>,
    },
    { title: '状态', dataIndex: 'status' },
    {
      // 适用范围在接口里是三个 id 字段，直接把 id 铺在表格里没人看得懂，这里换成
      // 名称。三者都空 = 不限，这是「没配」和「配了但列表不显示」唯一能区分开的
      // 地方——以前这个功能整条链路都没落地，界面上完全看不出来。
      title: '适用范围',
      search: false,
      render: (_, record) => {
        const merchant = record.merchantId
          ? (merchantNames[record.merchantId] ?? record.merchantId)
          : undefined;
        const brands = (record.brandIds ?? []).map((id) => ({ id, name: brandNames[id] ?? id }));
        const stores = (record.storeIds ?? []).map((id) => ({ id, name: storeNames[id] ?? id }));
        if (!merchant && brands.length === 0 && stores.length === 0) {
          return <span style={{ color: '#999' }}>不限</span>;
        }
        return (
          <Space size={[4, 4]} wrap>
            {merchant && <Tag color="blue">商户：{merchant}</Tag>}
            {brands.map((item) => (
              <Tag key={`brand:${item.id}`}>品牌：{item.name}</Tag>
            ))}
            {stores.map((item) => (
              <Tag key={`store:${item.id}`}>门店：{item.name}</Tag>
            ))}
          </Space>
        );
      },
    },
    {
      title: '操作',
      valueType: 'option',
      render: (_, record) => (
        <Space>
          {/* 只有 active + approved 的模板才发得出去：后端取模板时带
              status='active' AND audit_status='approved'，放行了别的行
              用户只会拿到一个看不懂的服务端报错。 */}
          {access.canIssueCoupons && record.status === 'active' && record.auditStatus === 'approved' && (
            <Button
              type="link"
              onClick={() => {
                setIssueTarget(record);
                setIssueOpen(true);
              }}
            >
              发放
            </Button>
          )}
          {access.canWriteCouponTemplates && (
            <Button
              type="link"
              onClick={() => {
                setEditing(record);
                // 打开时先对齐基线，否则表单挂载把 merchantId 填成这一行的值时，
                // onValuesChange 会误判成「商户被换了」而清空品牌/门店。
                lastMerchant.current = record.merchantId;
                setOpen(true);
              }}
            >
              编辑
            </Button>
          )}
          {access.canAuditCouponTemplates && record.auditStatus === 'pending' && (
            <>
              <Popconfirm
                title="确认通过审核？"
                onConfirm={() => refresh(auditCouponTemplate(record.id, 'approved'), '审核已通过')}
              >
                <Button type="link">通过</Button>
              </Popconfirm>
              <Popconfirm
                title="确认驳回审核？"
                onConfirm={() => refresh(auditCouponTemplate(record.id, 'rejected'), '审核已驳回')}
              >
                <Button type="link" danger>
                  驳回
                </Button>
              </Popconfirm>
            </>
          )}
          {access.canWriteCouponTemplates && record.status !== 'active' && record.auditStatus === 'approved' && (
            <Popconfirm
              title="确认启用模板？"
              onConfirm={() => refresh(updateCouponTemplateStatus(record.id, 'active'), '模板已启用')}
            >
              <Button type="link">启用</Button>
            </Popconfirm>
          )}
          {access.canWriteCouponTemplates && record.status === 'active' && (
            <Popconfirm
              title="确认停用模板？"
              onConfirm={() => refresh(updateCouponTemplateStatus(record.id, 'disabled'), '模板已停用')}
            >
              <Button type="link">停用</Button>
            </Popconfirm>
          )}
        </Space>
      ),
    },
  ];

  return (
    <PageContainer title="优惠券模板">
      <ProTable<CouponTemplate>
        rowKey="id"
        actionRef={ref}
        columns={columns}
        request={async (params) => {
          // ProTable 传的是 current，接口要的是 page，必须转一次：
          // 直接透传的话后端收不到 page，翻到第 2 页拿回来的还是第 1 页的数据。
          const result = await listCouponTemplates(toPageParams(params));
          return { data: result.items, total: result.total, success: true };
        }}
        toolBarRender={() =>
          access.canWriteCouponTemplates
            ? [
                <Button
                  key="add"
                  type="primary"
                  onClick={() => {
                    setEditing(undefined);
                    lastMerchant.current = undefined;
                    setOpen(true);
                  }}
                >
                  新建模板
                </Button>,
              ]
            : []
        }
      />
      <ModalForm<TemplateFormValues>
        formRef={formRef}
        open={open}
        onOpenChange={setOpen}
        title={editing ? '编辑模板' : '新建模板'}
        // 商户是品牌/门店的上层：换了商户，原来选的品牌/门店多半不属于新商户，
        // 留着会被全量覆盖的 PUT 原样写进范围（跨商户的范围就这么静默存下来了）。
        // 宁可让管理员重选，也不要在库里留下这种组合。写法与 pages/stores 换商户
        // 清品牌一致。
        //
        // 比对 lastMerchant 是必要的：表单挂载时如果也触发一次 onValuesChange
        // （把 merchantId 填成编辑行的值），无条件清空会立刻把刚回显的范围抹掉，
        // 之后一保存就是静默的数据丢失。
        onValuesChange={(changed) => {
          if ('merchantId' in changed) {
            const next = changed.merchantId as string | undefined;
            if (next !== lastMerchant.current) {
              lastMerchant.current = next;
              // 目录也跟着作废：否则紧接着选品牌时，会拿上一个商户的门店来判断
              // 哪些门店还合法。
              storeCatalog.current = [];
              formRef.current?.setFieldsValue({ brandIds: [], storeIds: [] });
            }
          }
          // 换品牌只剪掉失效的门店，不像换商户那样整片清空——品牌是多选，
          // 加一个品牌就把已选门店全清掉太粗暴。
          if ('brandIds' in changed) {
            pruneStores(Array.isArray(changed.brandIds) ? (changed.brandIds as string[]) : []);
          }
        }}
        initialValues={
          editing
            ? toFormValues(editing)
            : { claimLimitMode: 'once_ever', redemptionType: 'platform', visible: true }
        }
        // 表单只在首次挂载时读 initialValues，而 Modal 默认关闭时不卸载子节点。
        // 少了这两行，「编辑 A → 取消 → 编辑 B」表单里留着的还是 A 的字段值，
        // 点确定却按 editing.id 提交 —— 而后端 PUT 是全量覆盖，
        // 结果是把 A 的整份字段写到 B 行上。key 换行即换实例，destroyOnClose 兜底。
        key={editing?.id ?? 'new'}
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          // 表单里金额是元，后端要的是分的整数，toPayload 里统一换算；
          // claimLimitMode 是后端必填项，漏了会 400。
          const payload = toPayload(values);
          await (editing ? updateCouponTemplate(editing.id, payload) : createCouponTemplate(payload));
          message.success('已保存');
          ref.current?.reload();
          return true;
        }}
      >
        <ProFormSelect
          name="couponTypeId"
          label="优惠券类型"
          request={async () => (await listCouponTypes()).map((item) => ({ label: item.name, value: item.id }))}
          rules={[{ required: true }]}
        />
        {/* 适用范围。商户是 coupon_templates 上的一个列（单选，NULL = 平台券），
            品牌/门店是 coupon_template_scopes 里的多选行；后端怎么归一化见
            coupon-service/internal/service/template.go 的 normalizeScopes。
            三者都留空 = 不限，与库里没有 scope 行是同一个意思。

            这个 merchantId 以前是个 hidden 字段，只为了在全量覆盖的 PUT 里原样
            带回已有值；现在它是真的能改的，所以 onValuesChange 里要处理换商户
            导致的品牌/门店失效。 */}
        <ProFormSelect
          name="merchantId"
          label="发行商户"
          placeholder="不选表示为平台券"
          allowClear
          showSearch
          request={async () => {
            const { items } = await listMerchants(FULL_PAGE_PARAMS);
            return items.map((item) => ({ label: item.name, value: item.id }));
          }}
        />
        {/* 三级联动：商户 → 品牌 → 门店。
            两级的过滤条件都必须走 params，不能关在 request 闭包里——ProFormSelect
            只在挂载时取一次数，闭包里读到的永远是首次渲染那个 merchantId，
            换了商户下拉框还停在上一个商户的选项上（这就是之前看起来「没联动」的
            原因）。params 变了 pro 组件会重新取数，pages/stores 换商户刷品牌用的
            也是这个写法。

            依赖里要带上 brandIds：门店选项要随品牌收窄，不带就只在换商户时才重渲染。 */}
        <ProFormDependency name={['merchantId', 'brandIds']}>
          {({ merchantId, brandIds }) => (
            <>
              <ProFormSelect
                name="brandIds"
                label="适用品牌"
                mode="multiple"
                placeholder="不选表示不限品牌"
                allowClear
                showSearch
                params={{ merchantId }}
                request={async (params) => {
                  const { items } = await listBrands({
                    ...FULL_PAGE_PARAMS,
                    merchantId: params.merchantId,
                  });
                  return items.map((item) => ({ label: item.name, value: item.id }));
                }}
              />
              <ProFormSelect
                name="storeIds"
                label="适用门店"
                mode="multiple"
                placeholder="不选表示不限门店"
                allowClear
                showSearch
                // 品牌多选 → 门店取所选品牌门店的并集；不限品牌时是商户下全部门店。
                // brandKey 用字符串而不是数组：params 的变更检测比的是值，数组每次
                // 渲染都是新引用，会变成渲染一次重取一次。门店选项里带上品牌名，
                // 多选情况下看得清每个门店属于哪个品牌。
                params={{ merchantId, brandKey: (brandIds ?? []).join(',') }}
                request={async (params) => {
                  const { items } = await listStores({
                    ...FULL_PAGE_PARAMS,
                    merchantId: params.merchantId,
                  });
                  // 存全量再按品牌筛，剪门店（pruneStores）要用的是这份全量。
                  storeCatalog.current = items;
                  const picked = String(params.brandKey ?? '')
                    .split(',')
                    .filter(Boolean);
                  return items
                    .filter((item) => picked.length === 0 || picked.includes(item.brandId))
                    .map((item) => ({
                      label: item.brandName ? `${item.brandName} / ${item.name}` : item.name,
                      value: item.id,
                    }));
                }}
              />
            </>
          )}
        </ProFormDependency>
        <ProFormText name="name" label="模板名称" rules={[{ required: true }]} />
        <ProFormText name="shortTitle" label="短标题" />
        {/* 单位是元：填 12.5，提交时 ×100 成分。precision 限死两位小数，
            否则 12.505 这种值会被 Math.round 悄悄修成 12.51，用户看不出来。 */}
        <ProFormDigit name="faceValue" label="面值（元）" min={0} fieldProps={{ precision: 2 }} rules={[{ required: true }]} />
        <ProFormDigit name="minPurchaseAmount" label="最低消费（元）" min={0} fieldProps={{ precision: 2 }} />
        <ProFormDigit name="purchasePrice" label="购买价格（元）" min={0} fieldProps={{ precision: 2 }} />
        <ProFormDigit name="totalQuantity" label="发行总量" min={1} rules={[{ required: true }]} />
        <ProFormSelect
          name="validityMode"
          label="有效期"
          options={[{ label: '固定日期', value: 'fixed' }, { label: '领取后天数', value: 'relative' }]}
          rules={[{ required: true }]}
        />
        {/* 有效期按模式二选一，字段本身必须跟着走：原来这两个控件无条件同时渲染，
            「固定日期」根本没有日期可填，发出去的券只能靠后端 COALESCE 兜底成
            NOW()..+30 天 —— 30 是这个兜底里的硬编码，管理员在界面上看不到。
            没选模式前两个都不显示，避免出现一个对当前选择没有意义的必填项。 */}
        <ProFormDependency name={['validityMode']}>
          {({ validityMode }) => {
            if (validityMode === 'fixed') {
              return (
                <>
                  <ProFormDateTimePicker
                    name="validFrom"
                    label="生效时间"
                    width="md"
                    fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
                    rules={[{ required: true, message: '请选择生效时间' }]}
                  />
                  <ProFormDateTimePicker
                    name="validTo"
                    label="失效时间"
                    width="md"
                    fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
                    rules={[{ required: true, message: '请选择失效时间' }]}
                  />
                </>
              );
            }
            if (validityMode === 'relative') {
              return (
                <ProFormDigit
                  name="validDays"
                  label="有效天数"
                  min={1}
                  rules={[{ required: true, message: '请输入有效天数' }]}
                />
              );
            }
            return null;
          }}
        </ProFormDependency>
        <ProFormSelect
          name="claimLimitMode"
          label="领取限制"
          options={[
            { label: '仅一次', value: 'once_ever' },
            { label: '使用后可再领', value: 'unlimited_after_use' },
            { label: '按周期', value: 'periodic' },
          ]}
          rules={[{ required: true }]}
        />
        {/* 后端 validateTemplate 把 redemptionType 当必填枚举校验，不选会直接
            400 invalid coupon template。以前这里没有 required，用户不碰这一栏
            就会拿到一个讲不清的服务端报错。 */}
        <ProFormSelect
          name="redemptionType"
          label="核销方式"
          options={[{ label: '平台核销', value: 'platform' }, { label: '展示二维码', value: 'show_qr' }]}
          rules={[{ required: true, message: '请选择核销方式' }]}
        />
        <ProFormSwitch name="visible" label="前台可见" />
      </ModalForm>
      <ModalForm<IssueFormValues>
        open={issueOpen}
        onOpenChange={setIssueOpen}
        title={`发放优惠券${issueTarget ? `「${issueTarget.name}」` : ''}`}
        // initialValues 只在首次挂载生效：不销毁重建的话，第二次点开另一个模板，
        // 表单里留着的还是上一次的模板名。
        modalProps={{ destroyOnClose: true }}
        onFinish={async (values) => {
          if (!issueTarget) return false;
          await refresh(
            issueCoupons({
              // templateId 取自行内按钮，不让用户手填
              templateId: issueTarget.id,
              userIds: String(values.userIds)
                .split(/[,\n\s]+/)
                .filter(Boolean),
              quantityPerUser: Number(values.quantityPerUser),
              reason: values.reason,
            }),
            '发券成功',
          );
          return true;
        }}
      >
        <ProFormText name="templateName" label="模板" initialValue={issueTarget?.name} disabled />
        <ProFormTextArea
          name="userIds"
          label="用户 ID"
          fieldProps={{ rows: 6 }}
          placeholder="逗号、空格或换行分隔"
          rules={[{ required: true }]}
        />
        <ProFormDigit
          name="quantityPerUser"
          label="每人数量"
          min={1}
          max={100}
          initialValue={1}
          rules={[{ required: true }]}
        />
        <ProFormText name="reason" label="发券原因" fieldProps={{ maxLength: 200 }} />
      </ModalForm>
    </PageContainer>
  );
}
