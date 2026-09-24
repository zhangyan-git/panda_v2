import {
  ModalForm,
  PageContainer,
  ProDescriptions,
  ProFormDateTimePicker,
  ProFormDependency,
  ProFormDigit,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import { ProFormImageUpload } from '@panda-v2/ui';
import { Button, Drawer, Form, Image, message, Popconfirm, Space, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { ActionType, ProColumns, ProFormInstance } from '@ant-design/pro-components';
import {
  auditCouponTemplate,
  createCouponTemplate,
  getCouponTemplate,
  issueCoupons,
  listCouponTemplates,
  listCouponTypes,
  updateCouponTemplate,
  updateCouponTemplateStatus,
  type CouponTemplate,
} from '../../services/coupon';
import {
  couponTypeUsesAmounts,
  fenToYuan,
  toFormValues,
  toPayload,
  type TemplateFormValues,
} from './templateForm';
import { listBrands } from '../../services/brand';
import { listStores, type Store } from '../../services/store';
import { listMerchants } from '../../services/merchant';
import {
  AUDIT_STATUS,
  CLAIM_LIMIT_MODE,
  REDEMPTION_TYPE,
  TEMPLATE_STATUS,
  VALIDITY_MODE,
} from '../../services/couponLabels';
import { enumMeta, searchOptions } from '../../services/labels';
import UserPicker, { type PickedUser } from '../../components/common/UserPicker';
import { formatDateTime } from '../../services/datetime';
import { requestErrorMessage } from '../../services/requestError';
import { uploadImage } from '../../services/upload';
import { FULL_PAGE_PARAMS, toPageParams } from '../../services/pagination';
import { useAccess } from '@umijs/max';
import { scrollableModalBody } from '../../components/common/modalProps';

// 发券弹窗的表单值。templateName 只是把「发给哪个模板」显示出来，不进提交载荷：
// 模板由行内按钮决定，不允许手填 ID。
//
// userIds 是选出来的对象数组而不是一串 ID 文本：界面上要显示成谁，提交时才取 .id。
// 以前是手填 ID 的文本框，运营得先去别处把 ID 抄过来，抄错了只能等接口报错。
type IssueFormValues = {
  templateName?: string;
  userIds: PickedUser[];
  quantityPerUser: number;
  reason?: string;
};

// 展示用，固定两位小数，与表单口径一致。不拼 ¥：这一列原本就没有币种前缀。
// 分↔元的换算与表单的拼装都在 ./templateForm 里（那边可以单测）。
const formatYuan = (fen?: number) => fenToYuan(fen).toFixed(2);

// 剩余 = 总量 - 已发行 - 已预留，与库里的约束 issued + reserved <= total 同一个口径。
// 减掉预留：那部分额度已经被订单占住、不会再发出去，算进剩余会让人以为还能发。
// 不减「已释放」——释放是把占住的额度还回池子，本来就还没发行。
const remainingOf = (template: CouponTemplate) =>
  template.totalQuantity - template.issuedQuantity - template.reservedQuantity;

// 有效期按模式拼一句人话。两个模式在库里有 CHECK 保证字段互斥（见
// coupon_templates 上的两条 CHECK），所以 relative 一定有 validDays、fixed 一定有起止。
// 用 formatDateTime 而不是 valueType: 'dateTime'：这一格是拼出来的字符串，不是
// 一个时间字段，ProTable 的 valueType 只对整格是时间的情况生效。
const validityText = (template: CouponTemplate) =>
  template.validityMode === 'relative'
    ? `${template.validDays ?? '—'} 天（发放后起算）`
    : `${formatDateTime(template.validFrom)} 至 ${formatDateTime(template.validTo)}`;

// 两个金额字段的 0 都有专门含义，铺成「0.00」等于把含义抹掉：最低消费 0 是
// 无门槛，售价 0 是免费领取（coupon_templates 的列注释就是这么写的）。
const thresholdText = (fen: number) => (fen === 0 ? '无门槛' : formatYuan(fen));
const priceText = (fen: number) => (fen === 0 ? '免费领取' : formatYuan(fen));

/**
 * 「适用范围」的展示。
 *
 * 接口里是三个 id 字段（merchantId + brandIds + storeIds），直接把 id 铺出来没人
 * 看得懂，这里换成名称。三者都空 = 不限，这是「没配」和「配了但界面上不显示」唯一
 * 能区分开的地方——以前这个功能整条链路都没落地，界面上完全看不出来。
 *
 * 列表那一列和详情抽屉共用这一份：各写一遍的话，将来加一层范围（比如「渠道」）一定
 * 会漏改一处，而漏掉的表现是一边显示「不限」、另一边把 id 原样吐出来。
 *
 * 名映射由调用方传入（页面里是三个 state），拿不到名字时退回显示原始 id：缺相应
 * 权限时那几个请求会失败，而这只影响能不能看懂，不该让整页报错。
 */
function ScopeTags({
  template,
  merchantNames,
  brandNames,
  storeNames,
}: {
  template: CouponTemplate;
  merchantNames: Record<string, string>;
  brandNames: Record<string, string>;
  storeNames: Record<string, string>;
}) {
  const merchant = template.merchantId
    ? (merchantNames[template.merchantId] ?? template.merchantId)
    : undefined;
  const brands = (template.brandIds ?? []).map((id) => ({ id, name: brandNames[id] ?? id }));
  const stores = (template.storeIds ?? []).map((id) => ({ id, name: storeNames[id] ?? id }));
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
}

export default function CouponTemplatesPage() {
  const access = useAccess();
  const ref = useRef<ActionType>();
  const formRef = useRef<ProFormInstance<TemplateFormValues>>();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<CouponTemplate>();
  const [issueOpen, setIssueOpen] = useState(false);
  const [issueTarget, setIssueTarget] = useState<CouponTemplate>();
  // 详情抽屉。存的是**单独取回来的**那一条，不是列表那一行——见 services/coupon.ts
  // 里 getCouponTemplate 的说明。
  const [detail, setDetail] = useState<CouponTemplate>();
  // 取详情的过程中把那一行的「详情」按钮转起来，取回来再开抽屉（照设备管理等详情的
  // 做法）。先开再填的话，抽屉会先用空值挂一次，值回来时整片重画，用户看得见闪。
  const [detailLoadingID, setDetailLoadingID] = useState<string>();
  const [typeNames, setTypeNames] = useState<Record<string, string>>({});
  // id → 券类型编码。只用来判「这个类型有没有面值」（见 couponTypeUsesAmounts）——
  // 那件事必须按编码判，id 是每套环境各生成一次的 uuid。和 typeNames 同一次请求填。
  const [typeCodes, setTypeCodes] = useState<Record<string, string>>({});
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
      // 券类型名单独一个请求，不并进上面那组：类型接口的权限是 coupon:type:manage，
      // 与列表的 coupon:read 不是一套（见 coupon-service internal/routes/admin.go）。
      // 并进同一个 try 的话，只差这一个权限就会让商户/品牌/门店三列名字一起退回显示
      // id——那三列跟类型权限毫无关系。
      try {
        const types = await listCouponTypes();
        setTypeNames(Object.fromEntries(types.map((item) => [item.id, item.name])));
        setTypeCodes(Object.fromEntries(types.map((item) => [item.id, item.code])));
      } catch {
        // 忽略：同上，退回显示原始 id
      }
    })();
  }, []);

  // 行内那些一步到位的写动作（审核通过/驳回、启用/停用、发券）共用这条路径：
  // 成功就提示 + 刷表，失败把后端的原话弹出来。
  //
  // 异常**必须**在这里吃掉：四个调用方都是 Popconfirm 的 onConfirm，抛出去会变成
  // unhandled rejection（dev 下直接糊一层错误浮层），而用户那边只看见弹窗关掉了、
  // 界面上什么都没变。返回值给发券弹窗用：它为 false 时 ModalForm 不关。
  const refresh = async (
    operation: Promise<unknown>,
    successMessage: string,
  ): Promise<boolean> => {
    try {
      await operation;
    } catch (error) {
      message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
      return false;
    }
    message.success(successMessage);
    ref.current?.reload();
    return true;
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

  // 这张券的类型有没有面值/最低消费这个概念。判定按 coupon_types.code 走
  // （见 couponTypeUsesAmounts），拿不到类型列表时返回 true——宁可多显示一格
  // 「0.00」，也不要把一张代金券的面值显示成「—」。
  const hasAmounts = (template: CouponTemplate) =>
    couponTypeUsesAmounts(typeCodes[template.couponTypeId]);

  const columns: ProColumns<CouponTemplate>[] = [
    {
      // 老系统那一列就摆在最前面（ID 之后）。用 Image 而不是 Avatar：封面上是券的
      // 整张视觉，方形缩略图裁得看不出原样，而这里是能点开看大图的（Image 自带
      // 预览）。没有封面时给「—」，不给占位图——分不清「没传」和「传了张灰图」。
      title: '封面图',
      dataIndex: 'coverImage',
      search: false,
      width: 80,
      render: (_, record) =>
        record.coverImage ? (
          <Image src={record.coverImage} width={48} height={48} style={{ objectFit: 'cover' }} />
        ) : (
          '—'
        ),
    },
    { title: '名称', dataIndex: 'name', width: 180, ellipsis: true },
    {
      // 列表以前完全看不出这是一张什么券（满减/折扣/代金），只能从名字猜。类型目录
      // 本来就要为表单拉一次，顺手建映射，不新增请求。
      title: '券类型',
      dataIndex: 'couponTypeId',
      search: false,
      width: 100,
      render: (_, record) => typeNames[record.couponTypeId] ?? record.couponTypeId,
    },
    // 接口给的是「分」，直接铺出来就是「1250」；列表按元展示成「12.50」。
    //
    // 兑换券/会员价体验券这格是「—」：它们的 faceValue 恒为 0（表单不收集、
    // toPayload 强制归零），铺成「0.00」会让读的人以为「这张券的面值是 0 元」，
    // 而实际是「这张券没有面值这个概念」。这两件事在运营那边是两句话。
    {
      title: '面值',
      dataIndex: 'faceValue',
      search: false,
      width: 100,
      render: (_, record) => (hasAmounts(record) ? formatYuan(record.faceValue) : '—'),
    },
    // 「满 50 减 10」以前只写在名字里，接口有 minPurchaseAmount 却不显示：名字是
    // 人随手起的，改个名字这张券的用法就从界面上消失了。
    {
      title: '最低消费',
      dataIndex: 'minPurchaseAmount',
      search: false,
      width: 100,
      render: (_, record) => (hasAmounts(record) ? thresholdText(record.minPurchaseAmount) : '—'),
    },
    {
      title: '售价',
      dataIndex: 'purchasePrice',
      search: false,
      width: 100,
      render: (_, record) => priceText(record.purchasePrice),
    },
    {
      // 发出去的券什么时候过期，取决于这一列。以前只有编辑弹窗里看得到。
      title: '有效期',
      search: false,
      width: 190,
      render: (_, record) => validityText(record),
    },
    {
      title: '核销方式',
      dataIndex: 'redemptionType',
      search: false,
      width: 110,
      render: (_, record) => enumMeta(REDEMPTION_TYPE, record.redemptionType).text,
    },
    // 不摆搜索框：按「发行总量等于多少」筛没有实际用法，而 ProTable 默认会为每个
    // 列生成一个等值输入框——摆着但其实用不上。要按范围筛得先定口径（已发行还是
    // 总量），是另一件事。
    { title: '总量', dataIndex: 'totalQuantity', search: false, width: 90 },
    {
      // 摆出来是为了让「剩余」能自己对上：剩余 = 总量 - 已发行 - 已预留。只给总量和
      // 剩余，两个数之间的差额是多少、被什么占着，界面上没有出处。
      title: '已发行',
      dataIndex: 'issuedQuantity',
      search: false,
      width: 100,
    },
    {
      // 发到 0 就再也发不出去了（发券接口会回库存不足），所以剩多少是这一列要回答的
      // 问题——总量旁边没有它，运营得自己记每批发了多少。
      //
      // 也没有搜索框，理由同总量：按「剩余等于多少」筛没有实际用法。
      title: '剩余',
      search: false,
      width: 90,
      render: (_, record) => remainingOf(record),
    },
    {
      // valueEnum 同时管两件事：搜索下拉的选项，以及「这一列是可筛的」。后端
      // ListTemplates 收 name/status/auditStatus 三个参数，改这里要跟它的
      // CouponTemplateQuery 对上。
      title: '审核',
      dataIndex: 'auditStatus',
      valueType: 'select',
      width: 100,
      valueEnum: searchOptions(AUDIT_STATUS),
      render: (_, record) => {
        const meta = enumMeta(AUDIT_STATUS, record.auditStatus);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      width: 100,
      valueEnum: searchOptions(TEMPLATE_STATUS),
      render: (_, record) => {
        const meta = enumMeta(TEMPLATE_STATUS, record.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '适用范围',
      search: false,
      width: 200,
      render: (_, record) => (
        <ScopeTags
          template={record}
          merchantNames={merchantNames}
          brandNames={brandNames}
          storeNames={storeNames}
        />
      ),
    },
    {
      title: '操作',
      valueType: 'option',
      // 列变多之后横向一定要滚，操作列钉在右边：不钉的话「详情/编辑/发放」会被滚出
      // 视野，而这几列正好是这一页唯一能点的地方。宽度要显式给，fixed 的列量不出来。
      //
      // 300 不是随手给的：最多的一行是「详情 发放 编辑 停用」（或「详情 编辑 通过
      // 驳回」）四个按钮，实测每个 60px、加上 8px 间距和两侧内边距要 280px。之前
      // 写 220，按钮放不下又不换行（Space 默认 nowrap），就从格子里溢出去、越过表格
      // 右边缘——钉在右边也照样被切掉。
      width: 300,
      fixed: 'right',
      render: (_, record) => (
        <Space>
          {/* 详情不限权限：接口与列表同一个 coupon:read，能看见这一行就能看见详情。
              按钮不做权限门，否则会出现「有这一行但点不开」的死角。 */}
          <Button
            type="link"
            loading={detailLoadingID === record.id}
            onClick={async () => {
              setDetailLoadingID(record.id);
              try {
                setDetail(await getCouponTemplate(record.id));
              } catch (error) {
                message.error(requestErrorMessage(error, '读取模板详情失败'));
              } finally {
                setDetailLoadingID(undefined);
              }
            }}
          >
            详情
          </Button>
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
        // 券类型/封面图/费用/有效期/数量这几列加上之后一屏放不下，给一个下限宽度
        // 让它横向滚动，而不是把每列挤成几个字。
        //
        // 这个数**必须等于各列 width 之和**（80+180+100+100+100+100+190+110+90+
        // 100+90+100+100+200+300）：小于实际内容宽度时，钉在右边的操作列会按
        // scroll.x 去算位置、而表体宽出那一截，结果是它跑到表格外面去。所以下面
        // 每一列都写死了 width，不留 auto——auto 是浏览器量出来的，这个和永远算不准。
        scroll={{ x: 1940 }}
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

      {/* 模板详情。列的就是模板自己那些字段，一行一屏看不全的（说明、使用规则、
          封面图、几处开关）都在这里。 */}
      <Drawer
        title="模板详情"
        width={720}
        open={!!detail}
        onClose={() => setDetail(undefined)}
        // 不加 destroyOnClose：这里没有需要重置的内部状态，而 ProDescriptions 每次
        // 都按 dataSource 重画。加了反而会在关闭动画里闪一下空表。
      >
        {detail && (
          <ProDescriptions<CouponTemplate>
            column={2}
            dataSource={detail}
            // ProDescriptions 的 render 签名是 (dom, entity, index, action, schema)，
            // 第二个参数是**整行数据**，不是这一格的字段值——在用户券详情那里就栽过：
            // 按字段值用会拿到 [object Object]，界面上直接显示这串字。
            columns={[
              { title: '模板 ID', dataIndex: 'id', copyable: true, span: 2 },
              { title: '模板名称', dataIndex: 'name' },
              { title: '短标题', dataIndex: 'shortTitle', render: (_, record) => record.shortTitle || '—' },
              {
                title: '券类型',
                dataIndex: 'couponTypeId',
                render: (_, record) => typeNames[record.couponTypeId] ?? record.couponTypeId,
              },
              {
                title: '状态',
                dataIndex: 'status',
                render: (_, record) => {
                  const meta = enumMeta(TEMPLATE_STATUS, record.status);
                  return <Tag color={meta.color}>{meta.text}</Tag>;
                },
              },
              {
                title: '审核',
                dataIndex: 'auditStatus',
                render: (_, record) => {
                  const meta = enumMeta(AUDIT_STATUS, record.auditStatus);
                  return <Tag color={meta.color}>{meta.text}</Tag>;
                },
              },
              { title: '审核备注', dataIndex: 'auditRemark', render: (_, record) => record.auditRemark || '—' },
              { title: '审核时间', dataIndex: 'auditedAt', valueType: 'dateTime' },
              // 审核人是个管理员 UUID，这个页面拿不到姓名映射，原样显示：报障时对的就是它。
              { title: '审核人', dataIndex: 'auditedBy', render: (_, record) => record.auditedBy || '—' },
              {
                title: '适用范围',
                span: 2,
                render: (_, record) => (
                  <ScopeTags
                    template={record}
                    merchantNames={merchantNames}
                    brandNames={brandNames}
                    storeNames={storeNames}
                  />
                ),
              },
              // 这两格与列表同一条口径：无面值的券类型显示「—」，理由见列表那边的注释。
              {
                title: '面值',
                dataIndex: 'faceValue',
                render: (_, record) => (hasAmounts(record) ? formatYuan(record.faceValue) : '—'),
              },
              {
                title: '最低消费',
                dataIndex: 'minPurchaseAmount',
                render: (_, record) => (hasAmounts(record) ? thresholdText(record.minPurchaseAmount) : '—'),
              },
              { title: '售价', dataIndex: 'purchasePrice', render: (_, record) => priceText(record.purchasePrice) },
              { title: '有效期', render: (_, record) => validityText(record) },
              { title: '有效期模式', render: (_, record) => enumMeta(VALIDITY_MODE, record.validityMode).text },
              { title: '总量', dataIndex: 'totalQuantity' },
              { title: '已发行', dataIndex: 'issuedQuantity' },
              { title: '已预留', dataIndex: 'reservedQuantity' },
              { title: '剩余', render: (_, record) => remainingOf(record) },
              {
                title: '领取限制',
                dataIndex: 'claimLimitMode',
                render: (_, record) => enumMeta(CLAIM_LIMIT_MODE, record.claimLimitMode).text,
              },
              {
                title: '核销方式',
                dataIndex: 'redemptionType',
                render: (_, record) => enumMeta(REDEMPTION_TYPE, record.redemptionType).text,
              },
              {
                title: '外部核销说明',
                dataIndex: 'externalUseMethod',
                render: (_, record) => record.externalUseMethod || '—',
              },
              {
                title: '前台可见',
                dataIndex: 'visible',
                render: (_, record) => (record.visible ? '是' : '否'),
              },
              { title: '热门', dataIndex: 'isHot', render: (_, record) => (record.isHot ? '是' : '否') },
              {
                title: '推荐',
                dataIndex: 'isRecommended',
                render: (_, record) => (record.isRecommended ? '是' : '否'),
              },
              { title: '排序', dataIndex: 'sortOrder' },
              { title: '创建人', dataIndex: 'createdBy', render: (_, record) => record.createdBy || '—' },
              { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
              { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
              {
                title: '封面图',
                dataIndex: 'coverImage',
                span: 2,
                render: (_, record) =>
                  record.coverImage ? <Image src={record.coverImage} width={160} /> : '—',
              },
              // 说明和使用规则是给人读的长文本，各占一整行。
              { title: '说明', dataIndex: 'description', span: 2, render: (_, record) => record.description || '—' },
              {
                title: '使用规则',
                dataIndex: 'useRuleDescription',
                span: 2,
                render: (_, record) => record.useRuleDescription || '—',
              },
            ]}
          />
        )}
      </Drawer>

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
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
        onFinish={async (values) => {
          // 表单里金额是元，后端要的是分的整数，toPayload 里统一换算；
          // claimLimitMode 是后端必填项，漏了会 400。
          //
          // 类型编码要一起传：兑换券/会员价体验券没有面值，toPayload 会把那两个字段
          // 强制归零。查不到编码（类型列表没加载出来）时它按「有面值」处理，见
          // couponTypeUsesAmounts 的注释。
          const payload = toPayload(values, typeCodes[values.couponTypeId]);
          try {
            await (editing ? updateCouponTemplate(editing.id, payload) : createCouponTemplate(payload));
          } catch (error) {
            // 校验规则（金额区间、日期先后、范围合法性）全在后端 validateTemplate 里；
            // 返回 false 让弹窗留着，二十来个字段不用重填。
            message.error(requestErrorMessage(error, '保存失败，请稍后重试'));
            return false;
          }
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
        {/* description / coverImage / useRuleDescription 三格都是**列表和详情里展示着的**，
            所以这里必须可编辑。以前表单不渲染它们，而 PUT 是全量覆盖 —— 打开模板点保存
            就会把它们清空，界面上表现为「刚看过的字变成 —」。文案字段没有必填约束，
            后端也都给了 DEFAULT ''，渲染出来就等于带上去了。 */}
        <ProFormTextArea name="description" label="券说明" fieldProps={{ maxLength: 500, showCount: true }} />
        <ProFormTextArea name="useRuleDescription" label="使用规则说明" fieldProps={{ maxLength: 500, showCount: true }} />
        <ProFormImageUpload name="coverImage" label="封面图" upload={uploadImage} />
        {/* 单位是元：填 12.5，提交时 ×100 成分。precision 限死两位小数，
            否则 12.505 这种值会被 Math.round 悄悄修成 12.51，用户看不出来。

            面值与最低消费只对代金券有意义，兑换券/会员价体验券不渲染这两格
            （判定见 couponTypeUsesAmounts）。**光靠不渲染是不够的**——antd 卸载
            字段时保留值，旧值会跟着提交载荷发出去，所以 toPayload 那边还要按类型
            把它强制归零，两处缺一不可。 */}
        <ProFormDependency name={['couponTypeId']}>
          {({ couponTypeId }) =>
            couponTypeUsesAmounts(typeCodes[couponTypeId]) ? (
              <>
                <ProFormDigit
                  name="faceValue"
                  label="面值（元）"
                  min={0}
                  fieldProps={{ precision: 2 }}
                  rules={[{ required: true }]}
                />
                <ProFormDigit
                  name="minPurchaseAmount"
                  label="最低消费（元）"
                  min={0}
                  fieldProps={{ precision: 2 }}
                />
              </>
            ) : null
          }
        </ProFormDependency>
        {/* 购买价格不跟着券类型走：三种券都可能被售卖，今天会员价体验券的售价是 0
            （列表里显示「免费领取」），不是「没有售价」这个概念。 */}
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
        {/* 周期领取的两个字段与上面这一栏是一对：库里那条 CHECK 要求 periodic 时
            两个都非空、其余两档两个都为空（coupon_templates 上那条 CHECK）。所以「按周期」
            以前是**选不了的**——填不上周期，保存必然撞约束报 500；切走之后旧值又还
            留在 store 里，同样撞约束。必填标记与 templateForm 里那两行 delete 是同
            一件事的两半，缺一半就复现。 */}
        <ProFormDependency name={['claimLimitMode']}>
          {({ claimLimitMode }) => {
            if (claimLimitMode !== 'periodic') {
              return null;
            }
            return (
              <>
                <ProFormSelect
                  name="claimPeriodUnit"
                  label="领取周期单位"
                  options={[
                    { label: '天', value: 'day' },
                    { label: '周', value: 'week' },
                    { label: '月', value: 'month' },
                    { label: '年', value: 'year' },
                  ]}
                  rules={[{ required: true, message: '请选择领取周期单位' }]}
                />
                <ProFormDigit
                  name="claimPeriodQuantity"
                  label="每周期领取张数"
                  min={1}
                  fieldProps={{ precision: 0 }}
                  rules={[{ required: true, message: '请输入每周期领取张数' }]}
                />
              </>
            );
          }}
        </ProFormDependency>
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
        {/* 热推/推荐/排序在列表与详情里都有格子，原先只在库里、表单不认；
            全量覆盖的 PUT 会把它们一起清零（「是」变「否」、「3」变「0」）。 */}
        <ProFormSwitch name="isHot" label="热门推荐" />
        <ProFormSwitch name="isRecommended" label="编辑推荐" />
        <ProFormDigit name="sortOrder" label="排序" min={0} fieldProps={{ precision: 0 }} />
      </ModalForm>
      <ModalForm<IssueFormValues>
        open={issueOpen}
        onOpenChange={setIssueOpen}
        title={`发放优惠券${issueTarget ? `「${issueTarget.name}」` : ''}`}
        // initialValues 只在首次挂载生效：不销毁重建的话，第二次点开另一个模板，
        // 表单里留着的还是上一次的模板名。
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
        onFinish={async (values) => {
          if (!issueTarget) return false;
          // 走 refresh：失败时它已经弹过后端给的理由，这里跟着返回 false，
          // 弹窗留在原地——刚选好的那批人和填好的原因不该因为一次失败全丢掉。
          return refresh(
            issueCoupons({
              // templateId 取自行内按钮，不让用户手填
              templateId: issueTarget.id,
              userIds: values.userIds.map((user) => user.id),
              quantityPerUser: Number(values.quantityPerUser),
              reason: values.reason,
            }),
            '发券成功',
          );
        }}
      >
        <ProFormText name="templateName" label="模板" initialValue={issueTarget?.name} disabled />
        {/* 用原生 Form.Item 而不是 ProFormXxx：这里是自定义控件（值是一组用户对象），
            不是任何一个 ProForm 字段类型。required 交给 validator 判长度——
            清空后值是空数组，`required` 对空数组不生效，只加 required 会放行一次
            空提交，接口那边才报「len(userIds) < 1」。 */}
        <Form.Item
          name="userIds"
          label="用户"
          rules={[
            {
              validator: (_, value: PickedUser[] | undefined) =>
                value && value.length > 0
                  ? Promise.resolve()
                  : Promise.reject(new Error('请选择用户')),
            },
          ]}
        >
          <UserPicker />
        </Form.Item>
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
