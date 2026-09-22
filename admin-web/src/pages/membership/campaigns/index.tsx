import {
  ModalForm,
  PageContainer,
  ProFormDateTimePicker,
  ProFormDependency,
  ProFormDigit,
  ProFormSelect,
  ProFormText,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Alert, Button, Popconfirm, Tag, message } from 'antd';
import { useRef, useState } from 'react';
import { listCouponTemplates } from '../../../services/coupon';
import { formatDateTime, toRFC3339 } from '../../../services/datetime';
import { enumMeta, searchOptions } from '../../../services/labels';
import { listMembershipPlans } from '../../../services/membership';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { listStores, type Store } from '../../../services/store';
import {
  createCampaign,
  listCampaigns,
  listCampaignClaims,
  setCampaignStatus,
  updateCampaign,
  type Campaign,
  type CampaignClaim,
  type CampaignPayload,
  type CampaignQuery,
  type CampaignStatus,
} from '../../../services/campaign';

/**
 * 店铺码会员活动：门店里那张码扫进来送多少天会员。
 *
 * 三件今天没有的东西，页面上要看得见（都写在这儿，免得下一个人当成 bug）：
 *
 * 两件今天没有的东西，页面上要看得见（都写在这儿，免得下一个人当成 bug）：
 *
 * - **没有小程序码**。生成码要走微信的 wxacode.getUnlimited，而 appid/secret 与 token 缓存
 *   都在 user-service，会员服务没有微信配置；码的唯一用途又是被扫，扫码入口在小程序端。
 *   两件事一起做才有意义，所以一起等——活动表上的 qr_code_url 今天恒为空。
 * - **领取记录一定是空的**，同上：没人扫得了码。
 *
 * 活动的门槛是「必须挑连续包月（auto_renew）的套餐」——送出去的是会员天数，而券模式的套餐
 * 靠券给会员价、根本没有连续包月这一层，挑它做活动等于承诺了一个落不了地的东西。
 *
 * 赠券那一栏**是可选的**：不填就是只送会员天数。填了的话，券由券服务在收到
 * `membership.campaign.claimed` 之后发（会员服务只承诺、不执行），所以这里的列与抽屉上的
 * 张数说的是**承诺**，不是「券已经在用户账上了」。
 */

/** 活动状态。draft 是「还没配完」，只有 enabled 且落在起止窗口里的活动能被扫到。 */
const CAMPAIGN_STATUS: Record<string, { text: string; color: string }> = {
  draft: { text: '草稿', color: 'default' },
  enabled: { text: '已启用', color: 'success' },
  disabled: { text: '已停用', color: 'warning' },
};

/** 接口给的空值一律显示成「—」：留白与「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/**
 * 时间戳给人看：接口回的是带时区的完整时刻，表格里挤不下。
 *
 * 用 services/datetime.ts 的那个共享实现而不是引 dayjs：这个仓库没有直接依赖 dayjs（见那份
 * 文件头的说明），为一个格式化装一个包，装得上装不上全看提升结果。
 */
const shortTime = (value?: string | null) => formatDateTime(value) || '—';

/**
 * 新建时预填的活动码参数。
 *
 * **为什么要预填**：这串东西本身没有含义，它只是「扫进来时用来找回活动」的一个键（将来会印进
 * 小程序码的 scene）。让运营手打，除了逼人现编一个还会撞车之外没有别的作用——这一栏从前端到
 * 后端都要求它存在（`^smc_[A-Za-z0-9_-]+$`，32 字节上限），那就由页面生成一个。
 *
 * 12 位 base36（≈62 bit）够撞不上：真撞了后端回 409「这个活动码参数已经被别的活动用了」，
 * 页面上换个词重存即可。**用的是 Math.random 不是 crypto**——它不是凭据、不参与任何校验，
 * 只是个人可读可改的名字；拿真随机数生成器反而会让人以为它需要保密。
 *
 * 生成之后**仍可编辑**，也**只在新建时生成**：编辑时带的是库里那一条，绝不能重新生成——
 * 启用后的那串已经进了码，换掉它等于让手上的码静默失效（后端也会拒，见 409 那一段）。
 */
const newScene = () =>
  `smc_${Math.random().toString(36).slice(2, 10)}${Date.now().toString(36).slice(-4)}`;

/**
 * 赠券那一栏可选的券模板。
 *
 * **三道筛选缺一不可**，而且都只能在客户端做（模板列表接口没有这三个条件，能筛的只有名称、
 * 状态、审核状态与券类型编码）：
 *
 * - `status === 'active'` 且 `auditStatus === 'approved'`：这是**发券那一刻的判据**——券服务
 *   查模板时要求这两个值，不满足就记一条日志跳过。摆出来就是给人挑一个发不出去的券。
 * - `redemptionType === 'platform'`：平台核销。活动券在后台/收银台上核销，扫码出示那两类
 *   （show_qr / external_code）在这里没有意义。
 *
 * 客户端筛选的代价是**只在这一页里找**（FULL_PAGE_PARAMS 那一页）：券模板多到超过一页时，
 * 末尾那些不会被列出来。与券批次、用户券那两页是同一个取舍，真要治得给模板列表接口加条件。
 */
async function couponTemplateOptions() {
  const { items } = await listCouponTemplates(FULL_PAGE_PARAMS);
  return items
    .filter(
      (item) =>
        item.status === 'active' &&
        item.auditStatus === 'approved' &&
        item.redemptionType === 'platform',
    )
    .map((item) => ({ label: `${item.name}（${item.id.slice(0, 8)}）`, value: item.id }));
}

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function MembershipCampaignsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  /** 正在编辑的那一场。undefined = 弹窗关着；id 为空 = 新建。 */
  const [editing, setEditing] = useState<Partial<Campaign> | null>(null);

  /** 正在看领取记录的那一场（tab 打开时才有值）。 */
  const [claimsFor, setClaimsFor] = useState<Campaign>();

  // 每次打开都**重新生成**一串（不是打开一次用到底）：上一次开了又关掉的那串已经作废，
  // 复用它只会在列表里留下一条「看起来一样的活动」的困惑。它同时是下面 ModalForm 的 key
  // 的一部分——key 变了弹窗才会重建，initialValues 才会重新按新种子填进去。
  const openCreate = () => setEditing({ scene: newScene() });

  const columns: ProColumns<Campaign>[] = [
    {
      title: '活动名称',
      dataIndex: 'name',
      ellipsis: true,
      width: 180,
      fieldProps: { placeholder: '名称或活动码参数' },
    },
    {
      // scene 是码上那串参数。运营手里那半截可能就是这个，所以它可搜（与名称共用一个搜索框，
      // 后端按 name 或 scene 模糊匹配）。
      title: '活动码参数',
      dataIndex: 'scene',
      search: false,
      copyable: true,
      ellipsis: true,
      width: 200,
    },
    {
      title: '活动门店',
      dataIndex: 'storeName',
      search: false,
      ellipsis: true,
      width: 160,
      // 门店名是后端现解的，解不出来时是空串（商户域抖一下不该让整页打不开）。空的时候退回
      // 显示 id——门店的身份是 store_id 那一列，它是本库的事实。
      render: (_, row) => dash(row.storeName || row.storeId),
    },
    {
      title: '赠送套餐',
      dataIndex: 'planName',
      search: false,
      ellipsis: true,
      width: 160,
    },
    {
      title: '赠送天数',
      dataIndex: 'giftDays',
      search: false,
      width: 90,
      render: (_, row) => `${row.giftDays} 天`,
    },
    {
      // 只显示**张数**，不显示模板名：模板在券库、本页拿不到它的名字（要另一张表 join 一次跨库
      // 数据，而这一列只是给人扫一眼「这场有没有送券」）。想核模板是哪一个，去表单里看下拉。
      title: '赠送券',
      dataIndex: 'couponCount',
      search: false,
      width: 100,
      render: (_, row) => (row.couponCount > 0 ? `${row.couponCount} 张` : '—'),
    },
    {
      title: '活动时间',
      dataIndex: 'startAt',
      search: false,
      width: 200,
      render: (_, row) => `${shortTime(row.startAt)} ~ ${shortTime(row.endAt)}`,
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(CAMPAIGN_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(CAMPAIGN_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 启用中改 scene / 门店会被后端拒（409）：scene 进了码，改它会让已经印出去的码静默
      // 失效；门店是归属门店，改了等于把已经领过的人的归属挪走。所以这两个动作先让人停用。
      title: '操作',
      valueType: 'option',
      width: 200,
      fixed: 'right',
      render: (_, row) => {
        if (!access.canManageMembership) {
          return [
            <Button key="claims" type="link" size="small" onClick={() => setClaimsFor(row)}>
              领取记录
            </Button>,
          ];
        }
        return [
          <Button key="edit" type="link" size="small" onClick={() => setEditing(row)}>
            编辑
          </Button>,
          <Button key="claims" type="link" size="small" onClick={() => setClaimsFor(row)}>
            领取记录
          </Button>,
          row.status === 'enabled' ? (
            <Popconfirm
              key="status"
              title="停用这场活动？"
              description="停用之后这张码就领不了了，已经在有效期里的人不受影响。"
              onConfirm={async () => {
                try {
                  await setCampaignStatus(row.id, 'disabled');
                  message.success('活动已停用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '停用失败，请刷新后重试'));
                }
              }}
            >
              <Button type="link" size="small" danger>
                停用
              </Button>
            </Popconfirm>
          ) : (
            // draft 与 disabled 都能启用：前者是「配好了」，后者是「重新开」。启用之后这张码
            // 才领得了——而窗口过了的活动即使启用也领不到（后端按起止时间判）。
            <Button
              key="status"
              type="link"
              size="small"
              onClick={async () => {
                try {
                  await setCampaignStatus(row.id, 'enabled');
                  message.success('活动已启用');
                  actionRef.current?.reload();
                } catch (error) {
                  message.error(requestErrorMessage(error, '启用失败，请刷新后重试'));
                }
              }}
            >
              启用
            </Button>
          ),
        ];
      },
    },
  ];

  const claimColumns: ProColumns<CampaignClaim>[] = [
    { title: '用户 ID', dataIndex: 'userId', copyable: true, ellipsis: true, width: 260 },
    {
      // 快照：活动后来改了门店或天数，这一次不受影响。
      title: '归属门店',
      dataIndex: 'storeId',
      ellipsis: true,
      width: 260,
      render: (_, row) => dash(row.storeId),
    },
    { title: '赠送天数', dataIndex: 'giftDays', width: 90, render: (_, row) => `${row.giftDays} 天` },
    {
      // 同样是**承诺**，且同样只是张数：实际发出去几张在券库（user_coupons.campaign_claim_id
      // 指着这条记录），这一页查不到。发券失败（模板被停用）时券服务只记日志，不会回头改这里
      // ——所以这一列显示 3 不等于用户账上真有 3 张。
      title: '赠送券',
      dataIndex: 'couponCount',
      width: 100,
      render: (_, row) => (row.couponCount > 0 ? `${row.couponCount} 张` : '—'),
    },
    {
      title: '领到哪天',
      dataIndex: 'membershipExpireAt',
      width: 150,
      render: (_, row) => shortTime(row.membershipExpireAt),
    },
    {
      title: '领取时间',
      dataIndex: 'createdAt',
      width: 150,
      render: (_, row) => shortTime(row.createdAt),
    },
  ];

  return (
    <PageContainer
      title="店铺码会员活动"
      content="门店里那张码，扫进来送一段会员。一场活动绑定一个门店、一款连续包月套餐和一段赠送天数；只有已启用、且落在起止时间里的活动能被扫到。"
      extra={
        access.canManageMembership
          ? [
              <Button key="create" type="primary" onClick={openCreate}>
                新建活动
              </Button>,
            ]
          : undefined
      }
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="领取记录会是空的"
        description="扫码领会员要用户在小程序里扫门店的那张码，而小程序端还没接进来；小程序码本身也还没有生成入口（生成要走微信，能力在用户服务那一侧）。所以现在这里建得出活动、但没人领得了——这不是页面坏了。"
      />

      <ProTable<Campaign>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1390 = 180+200+160+160+90+100+200+100+200，各列 width 之和。改任何一列的宽度都要同批
        // 改这个数（钉右列的不变式：fixed 列必须显式 width，scroll.x = 各列 width 之和）。
        scroll={{ x: 1390 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: CampaignQuery = {
            page: params.current,
            pageSize: params.pageSize,
            status: exact(params.status) as CampaignQuery['status'],
            keyword: exact(params.name),
          };
          const result = await listCampaigns(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        新建 / 编辑共用这一个弹窗。**key 与 destroyOnClose 两个都要有**——缺了它们，跨行编辑
        时上一场的字段会落到下一场上（全仓踩过一次的坑）。key 里带上 id 与初始值，切行即重建。
      */}
      <ModalForm<CampaignPayload>
        // key 带上 id 或那串新种子：编辑切行、以及「关掉再新建」都是不同的一次，弹窗要重建
        // 才会按当次的 initialValues 填。
        key={`campaign-${editing?.id ?? editing?.scene ?? 'new'}`}
        title={editing?.id ? '编辑活动' : '新建活动'}
        open={!!editing}
        width={520}
        modalProps={{
          destroyOnClose: true,
          maskClosable: false,
          onCancel: () => setEditing(null),
        }}
        initialValues={
          editing?.id
            ? {
                name: editing.name,
                scene: editing.scene,
                storeId: editing.storeId,
                planId: editing.planId,
                giftDays: editing.giftDays,
                startAt: editing.startAt,
                endAt: editing.endAt,
                // 空串 / 0 就是「这场不送券」，控件自己会显示成占位符。这里不做
                // 「有券才填」的判断：null 与空串在控件上都要变成同一个东西。
                couponTemplateId: editing.couponTemplateId,
                couponCount: editing.couponCount,
              }
            : // 新建：码参数预填一串生成的，其余留空。
              { scene: editing?.scene }
        }
        onFinish={async (values) => {
          const payload: CampaignPayload = {
            name: values.name,
            scene: values.scene,
            storeId: values.storeId,
            planId: values.planId,
            giftDays: values.giftDays,
            // 两个时间控件给回来的可能是 dayjs 对象（用户刚点的）也可能是原样的字符串（编辑时
            // 从接口带回来的），后端只认 RFC3339——统一交给 toRFC3339，不靠库去猜。
            startAt: toRFC3339(values.startAt) ?? '',
            endAt: toRFC3339(values.endAt) ?? '',
            // 券是**要么都填、要么都不填**。清掉模板却留着张数是最容易发生的一种半截组合
            // （张数那一栏跟着模板出现，清模板时它会被卸载、值却留在表单里），所以这里按
            // 模板在不在把它归零——空模板 + 0 才是后端认的那一份「不送券」。
            couponTemplateId: values.couponTemplateId ?? '',
            couponCount: values.couponTemplateId ? (values.couponCount ?? 0) : 0,
          };
          try {
            if (editing?.id) {
              await updateCampaign(editing.id, payload);
              message.success('活动已保存');
            } else {
              await createCampaign(payload);
              message.success('活动已创建，记得启用它');
            }
            setEditing(null);
            actionRef.current?.reload();
            return true;
          } catch (error) {
            // 失败时**不关弹窗**：多半是活动开着不让改那两列，或码参数撞车了。填的东西还留着，
            // 运营可以直接改一改再提交。
            //
            // 话**照搬后端那句**（requestErrorMessage）：后端为这几种情况各写了一句中文，
            // 前端再猜一遍只会猜错——曾经就出过「接口 404（本机跑的还是旧二进制）而弹窗
            // 说码参数被占用」这种事。缺省那句只在拿不到信封时才用得上。
            message.error(
              requestErrorMessage(
                error,
                '保存失败：可能是活动启用中（先停用再改码参数与门店），或码参数已被占用',
              ),
            );
            return false;
          }
        }}
      >
        <ProFormText
          name="name"
          label="活动名称"
          rules={[{ required: true, message: '请填写活动名称' }]}
          fieldProps={{ maxLength: 60 }}
          extra="给运营看的，不出现在码里。"
        />
        <ProFormText
          name="scene"
          label="活动码参数"
          rules={[{ required: true, message: '请填写活动码参数' }]}
          extra="新建时自动生成，可以改；以 smc_ 开头，只能用字母、数字、下划线和连字符，不超过 32 个字符。启用之后不能改——它已经进了码。"
        />
        <ProFormSelect
          name="storeId"
          label="活动门店"
          rules={[{ required: true, message: '请选择活动门店' }]}
          tooltip="领到的会员归到这家店。会员还没过期时再领别的活动不会改归属；到期之后重新领才会改"
          request={async () => {
            const page = await listStores(FULL_PAGE_PARAMS);
            return page.items.map((store: Store) => ({
              label: `${store.name}（${store.brandName || store.merchantName}）`,
              value: store.id,
            }));
          }}
          fieldProps={{ showSearch: true, optionFilterProp: 'label' }}
          extra="启用之后不能改：改了等于把已经领过的人的归属挪走。"
        />
        <ProFormSelect
          name="planId"
          label="赠送套餐"
          rules={[{ required: true, message: '请选择赠送套餐' }]}
          // 只列连续包月（auto_renew）的套餐：活动送的是会员天数，而券模式的套餐靠券给会员价，
          // 挑它做活动等于承诺了一个送不出去的东西。后端也会拒，这里先挑出来省一次往返。
          request={async () => {
            const result = await listMembershipPlans({ ...FULL_PAGE_PARAMS, status: 'active' });
            return result.items
              .filter((plan) => plan.autoRenew)
              .map((plan) => ({ label: plan.name, value: plan.id }));
          }}
          fieldProps={{ showSearch: true, optionFilterProp: 'label' }}
          extra="只有开了连续包月的套餐能选。"
        />
        <ProFormDigit
          name="giftDays"
          label="赠送天数"
          rules={[{ required: true, message: '请填写赠送天数' }]}
          min={1}
          max={3650}
          fieldProps={{ precision: 0 }}
          extra="送几天会员。与套餐自带的时长无关。"
        />
        <ProFormDateTimePicker
          name="startAt"
          label="开始时间"
          width="md"
          rules={[{ required: true, message: '请选择开始时间' }]}
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
        />
        <ProFormDateTimePicker
          name="endAt"
          label="结束时间"
          width="md"
          rules={[{ required: true, message: '请选择结束时间' }]}
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
          extra="结束那一刻起就领不了了。"
        />
        {/*
          赠券：**可选**，不选就是只送会员天数。张数那一栏跟着模板出现——两栏要么都填、要么
          都不填，后端对「只填一半」回 400（那等于「该发券」静默变成「什么都不发」）。
        */}
        <ProFormSelect
          name="couponTemplateId"
          label="赠送券模板"
          allowClear
          request={couponTemplateOptions}
          fieldProps={{
            showSearch: true,
            optionFilterProp: 'label',
            // 空下拉是这里最容易让人以为「坏了」的地方，所以把「为什么空」写清楚：条件三条
            // （上架 + 已审核 + 平台核销），不是「券模板不存在」。
            notFoundContent: (
              <span style={{ color: '#999' }}>
                没有可用的券模板：只列已上架、已审核、平台核销的模板。去「优惠券模板」里建一张
                并审核通过。
              </span>
            ),
          }}
          extra="不选就不送券。只列已上架、已审核、平台核销的券模板——别的类型发出去也核销不了。"
        />
        <ProFormDependency name={['couponTemplateId']}>
          {({ couponTemplateId }) =>
            couponTemplateId ? (
              <ProFormDigit
                name="couponCount"
                label="每次赠送张数"
                rules={[{ required: true, message: '请填写每次赠送几张' }]}
                min={1}
                // 后端的上限（MaxCampaignCouponCount）与券服务每单 100 张的上限是同一个数。
                max={100}
                fieldProps={{ precision: 0 }}
                extra="每领一次送几张。券只在活动门店能用。"
              />
            ) : null
          }
        </ProFormDependency>
      </ModalForm>

      {/*
        领取记录。单独一个抽屉而不是活动详情页：这一场活动要看的就这一张表，而活动本身的字段
        在列表里已经齐了。
      */}
      <ModalForm
        key={`claims-${claimsFor?.id ?? ''}`}
        title={`领取记录 · ${claimsFor?.name ?? ''}`}
        open={!!claimsFor}
        width={900}
        modalProps={{
          destroyOnClose: true,
          onCancel: () => setClaimsFor(undefined),
          footer: null,
        }}
        submitter={false}
      >
        {/*
          列表为空时只有一句「暂无数据」，而这里**必然是空的**——不写一句，下一个人会去查
          「为什么没人领」。理由与页头那条 Alert 是同一条，这里再写一次是因为抽屉盖住了它。
        */}
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="这里现在一定是空的"
          description="领取要用户在小程序里扫门店那张码，扫码入口还没接进来，所以还没有人领得了。"
        />
        <ProTable<CampaignClaim>
          rowKey="id"
          columns={claimColumns}
          search={false}
          options={false}
          // 1010 = 260+260+90+100+150+150，同上。
          scroll={{ x: 1010 }}
          pagination={{ pageSize: 10 }}
          request={async (params) => {
            if (!claimsFor) return { data: [], total: 0, success: true };
            const result = await listCampaignClaims(claimsFor.id, {
              page: params.current,
              pageSize: params.pageSize,
            });
            return { data: result.items, total: result.total, success: true };
          }}
        />
      </ModalForm>
    </PageContainer>
  );
}
