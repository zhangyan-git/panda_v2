import {
  ModalForm,
  PageContainer,
  ProFormDigit,
  ProFormDependency,
  ProFormSelect,
  ProFormSwitch,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, message, Popconfirm, Tag, Typography } from 'antd';
import { useRef, useState } from 'react';
import { MEMBERSHIP_PRICE_COUPON_TYPE_CODE, listCouponTemplates } from '../../../services/coupon';
import { enumMeta, searchOptions } from '../../../services/labels';
import {
  createMembershipPlan,
  listMembershipPlans,
  updateMembershipPlan,
  updateMembershipPlanStatus,
  type MembershipPlan,
  type PlanInput,
  type PlanPeriod,
  type PlanQuery,
} from '../../../services/membership';
import {
  MEMBER_PRICE_MODE,
  PLAN_PERIOD,
  PLAN_STATUS,
  memberPriceLabel,
  periodLabel,
} from '../../../services/membershipLabels';
import { fenToYuan, yuanToFen } from '../../../services/money';
import { FULL_PAGE_PARAMS } from '../../../services/pagination';
import { requestErrorMessage } from '../../../services/requestError';
import { scrollableModalBody } from '../../../components/common/modalProps';

/**
 * 会员套餐：卖了什么。
 *
 * 这一页是**套餐定义**，不是某个人的会员——这里的价格与时长是「现在买要多少钱、买多久」，
 * 已经买过的人的条款在会员行的成交快照上。所以改价、改时长、下架都不会动到已购会员，
 * 这也是本域唯一一处「改了也不必担心」的写入口（对照会员详情页那四个动作）。
 *
 * 两个容易踩的地方，都在表单里：
 *   1. 新建的套餐**一律是草稿**（后端的新建请求体里根本没有 status 字段），要卖必须再按一次
 *      「上架」。所以「建完怎么还是买不了」不是 bug。
 *   2. 条件字段是**成对**的（自动续费↔签约模板 ID、体验券模式↔券模板与每期张数），
 *      后端两边的配对都校验，**留着上一次的值会被 400 挡下**——只填不设，不等于清空。
 */

/** 接口给的空值一律显示成「—」：留白与「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

/**
 * 表单形状。价格按**元**填（人要按元想问题），提交前才换成后端要的分。
 *
 * 条件字段在这里不带 `?`：它们是同一张表单上的普通字段，靠 ProFormDependency 显隐，
 * 真正的「该不该有值」由提交那段按模式统一决定（见 buildPayload）。
 */
type PlanForm = {
  code: string;
  name: string;
  description?: string;
  benefits?: string;
  priceYuan: number;
  period: PlanPeriod;
  periodCount: number;
  autoRenew: boolean;
  wechatPlanId?: string;
  memberPriceMode: PlanInput['memberPriceMode'];
  memberPriceCouponTemplateId?: string;
  memberPriceCouponsPerPeriod?: number;
  sortOrder?: number;
};

/**
 * 表单值 → 请求体。**显式逐字段挑，不整个透传**：表单上有 code（修改接口不收）与 priceYuan
 * （接口要的是 priceCents），透传过去是两个后端不认识的键。
 *
 * 两处「配对字段的清空」在这里做，不在 UI 上做：ProFormDependency 只是让框看不见，
 * **看不见的值仍然会被提交**——关掉自动续费再保存，如果不清 wechatPlanId，
 * 后端那条「不自动续费就必须留空」的校验会 400，而用户眼里那个框早就没了。
 */
function buildPayload(values: PlanForm): PlanInput {
  const couponMode = values.memberPriceMode === 'coupon';
  return {
    name: values.name.trim(),
    description: values.description?.trim() || '',
    // 一行一条。空行丢掉：文本域里多敲一个回车就多一条空卖点，后端也只做 trim 不去重。
    benefits: (values.benefits ?? '')
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean),
    priceCents: yuanToFen(values.priceYuan),
    period: values.period,
    periodCount: values.periodCount,
    autoRenew: values.autoRenew,
    wechatPlanId: values.autoRenew ? (values.wechatPlanId ?? '').trim() : '',
    memberPriceMode: values.memberPriceMode,
    memberPriceCouponTemplateId: couponMode
      ? (values.memberPriceCouponTemplateId ?? '').trim()
      : '',
    memberPriceCouponsPerPeriod: couponMode ? (values.memberPriceCouponsPerPeriod ?? 0) : 0,
    sortOrder: values.sortOrder ?? 0,
  };
}

export default function MembershipPlansPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  // 「正在编辑的那一行」，undefined 表示新建。**开合必须单独一个 state**：只用它是做不到的
  // ——「新增」时 editing 就是 undefined，与「关掉了」长得一模一样，于是点「新增套餐」
  // 什么都不会发生。关窗时两个都要重置（见下面的 onOpenChange）。
  const [editing, setEditing] = useState<MembershipPlan>();
  const [open, setOpen] = useState(false);

  /**
   * 上下架。与「保存表单」分开：那个是把整份套餐换掉，这个只是开关——合在一起的话，
   * 改一次价格描述会把一个正在售的套餐顺手存成草稿，而那一刻可能正有人在下单页上。
   *
   * 只用通知刷新，不做乐观更新：列表那一行要显示的是后端算完的状态，本地猜一个再被
   * 下一帧覆盖掉，中间会闪一下错的状态。
   */
  const changeStatus = async (row: MembershipPlan, status: MembershipPlan['status']) => {
    try {
      await updateMembershipPlanStatus(row.id, status);
    } catch (error) {
      message.error(requestErrorMessage(error, status === 'active' ? '上架失败' : '下架失败'));
      return;
    }
    message.success(status === 'active' ? '已上架' : '已下架');
    actionRef.current?.reload();
  };

  const columns: ProColumns<MembershipPlan>[] = [
    {
      // 编码是给系统看的稳定标识，建了就不能改（库上有触发器）。它同时也是运营手里常有的
      // 那半截信息（「那个 monthly_auto」），所以下面那个关键词框按名称或编码一起搜。
      title: '编码',
      dataIndex: 'code',
      ellipsis: true,
      width: 140,
      search: false,
    },
    {
      // 名称与编码共用**一个**关键词框（后端 keyword 是 name ILIKE OR code ILIKE）。
      // 两列各给一个搜索框的话，两个都填时后端不认识第二个键，人会以为筛过两次。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '名称或编码片段' },
    },
    {
      title: '名称',
      dataIndex: 'name',
      ellipsis: true,
      search: false,
      width: 160,
    },
    {
      // 单位是元。**0 是合法取值**（免费领的会员），所以不用 `? :` 判真值。
      title: '价格（元）',
      dataIndex: 'priceCents',
      search: false,
      width: 120,
      render: (_, row) => fenToYuan(row.priceCents).toFixed(2),
    },
    {
      // 单位与个数一起读才对：「month/1」是连续包月，「year/1」是年度会员。两个字段在库里
      // 是分开的（对账时要用的就是它们），拼接归前端，见 periodLabel。
      title: '时长',
      dataIndex: 'period',
      search: false,
      width: 110,
      render: (_, row) => periodLabel(row.period, row.periodCount),
    },
    {
      // 这是**产品**属性（要不要签代扣），与会员行上那个「这个人现在开着没」不是一回事。
      title: '自动续费',
      dataIndex: 'autoRenew',
      search: false,
      width: 100,
      render: (_, row) =>
        row.autoRenew ? <Tag color="blue">支持</Tag> : <Tag>不支持</Tag>,
    },
    {
      // 会员价怎么来。coupon 模式下把每期张数带上——「会员价体验券」这句话答不了运营的
      // 下一个问题（一个月给几张），而那个数就在同一行上。
      title: '会员价方式',
      dataIndex: 'memberPriceMode',
      search: false,
      width: 190,
      render: (_, row) => (
        // 文案走 memberPriceLabel（coupon 模式要带上每期张数），颜色仍取自标签表——
        // 同一件事的颜色只有一处定义，加新模式时不用回来改这一列。
        <Tag color={enumMeta(MEMBER_PRICE_MODE, row.memberPriceMode).color}>
          {memberPriceLabel(row.memberPriceMode, row.memberPriceCouponsPerPeriod)}
        </Tag>
      ),
    },
    {
      title: '排序',
      dataIndex: 'sortOrder',
      search: false,
      width: 90,
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(PLAN_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(PLAN_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      valueType: 'dateTime',
      search: false,
      width: 170,
    },
    {
      title: '操作',
      valueType: 'option',
      // fixed 的列必须显式给宽度，且下面的 scroll.x 必须等于各列宽度之和（见下面的 1330）。
      // 160 是量出来的：「编辑」+「上架/下架」两个 link 按钮并排内容宽约 144，加左右各 8px
      // 内边距 = 160。**给窄了的后果是这一格的内容溢出，而钉右列的溢出会直接把表格的
      // scrollWidth 顶大**，与 scroll.x 声明的数对不上（这个仓库踩过，见 OrdersTable）。
      width: 160,
      fixed: 'right',
      render: (_, row) =>
        access.canManageMembership
          ? [
              <Button
                key="edit"
                type="link"
                size="small"
                onClick={() => {
                  // 直接拿列表那一行开表单：本域列表与详情**同形**（PlanResponse 是完整的，
                  // 没有嵌套结构），不像活动列表那样要先取一次详情。这是接口给的便利，
                  // 不是省了一步——真出现「列表缺字段」的那天，这里要改成先 GET 再开。
                  setEditing(row);
                  setOpen(true);
                }}
              >
                编辑
              </Button>,
              row.status === 'active' ? (
                <Popconfirm
                  key="disable"
                  title="下架这个套餐？"
                  // 纯文本节点，别在这里写 markdown 星号——它不会被渲染成加粗。
                  description="下架后买不到，但已经买过的人不受影响：他们的条款在成交快照上。"
                  okText="下架"
                  onConfirm={() => changeStatus(row, 'disabled')}
                >
                  <Button type="link" size="small" danger>
                    下架
                  </Button>
                </Popconfirm>
              ) : (
                <Popconfirm
                  key="activate"
                  title="上架这个套餐？"
                  description="上架后立刻可以在小程序里买到，包括草稿状态的套餐。"
                  okText="上架"
                  onConfirm={() => changeStatus(row, 'active')}
                >
                  <Button type="link" size="small">
                    上架
                  </Button>
                </Popconfirm>
              ),
            ]
          : [],
    },
  ];

  return (
    <PageContainer
      title="会员套餐"
      content="卖给用户的那几档会员。改价与改时长只影响接下来卖什么，已购会员拿到的是成交当时的条款。"
      extra={
        access.canManageMembership
          ? [
              <Button
                key="create"
                type="primary"
                onClick={() => {
                  setEditing(undefined);
                  setOpen(true);
                }}
              >
                新增套餐
              </Button>,
            ]
          : undefined
      }
    >
      <ProTable<MembershipPlan>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1330 = 140+160+120+110+100+190+90+90+170+160，各列 width 之和（搜索专用的几列
        // 不进表格，也就不进这个和）。改任何一列的宽度都要同批改这个数。
        scroll={{ x: 1330 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: PlanQuery = {
            page: params.current,
            pageSize: params.pageSize,
            keyword: exact(params.keyword),
            status: exact(params.status) as PlanQuery['status'],
          };
          const result = await listMembershipPlans(query);
          return { data: result.items, total: result.total, success: true };
        }}
      />

      {/*
        新增 / 编辑共用一张表单。**必须有 destroyOnClose 与 key**：没有它们，编完 A 再点 B，
        A 的值会留在复用同一个 Form 实例的表单里，看着像 B 的默认值——这个坑在这个仓库里
        踩过（见那一批 ModalForm 修复）。key 用 id 而不是固定值，是为了编辑不同行时强制换
        一个实例。

        编辑走 PUT 是**整份替换**（后端没有 PATCH 语义），所以 initialValues 必须把每一个
        字段都写上——漏掉的那个会被当成「清空了」。下面正好一个不漏。
      */}
      <ModalForm<PlanForm>
        key={editing?.id ?? 'new'}
        title={editing ? '编辑套餐' : '新增套餐'}
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          // 关窗必须**同时**清掉 editing（理由见上面那两行注释），提交成功后也走这里。
          if (!next) setEditing(undefined);
        }}
        modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
        initialValues={
          editing
            ? {
                code: editing.code,
                name: editing.name,
                description: editing.description,
                benefits: editing.benefits.join('\n'),
                priceYuan: fenToYuan(editing.priceCents),
                period: editing.period,
                periodCount: editing.periodCount,
                autoRenew: editing.autoRenew,
                wechatPlanId: editing.wechatPlanId,
                memberPriceMode: editing.memberPriceMode,
                memberPriceCouponTemplateId: editing.memberPriceCouponTemplateId,
                memberPriceCouponsPerPeriod: editing.memberPriceCouponsPerPeriod,
                sortOrder: editing.sortOrder,
              }
            : // 新建的默认值写在这里而不是靠 placeholder：默认值必须是表单里真的有的值，
              // 否则提交上去的是一串 undefined，而库上有 CHECK 的列会直接 400。
              // periodCount 给 1 而不是 0：库上要求 > 0，空着提交必被拒。
              {
                period: 'month',
                periodCount: 1,
                autoRenew: false,
                memberPriceMode: 'auto',
                sortOrder: 0,
              }
        }
        onFinish={async (values) => {
          const payload = buildPayload(values);
          try {
            if (editing) {
              await updateMembershipPlan(editing.id, payload);
            } else {
              await createMembershipPlan({ code: values.code.trim(), ...payload });
            }
          } catch (error) {
            message.error(
              requestErrorMessage(error, editing ? '保存失败，请稍后重试' : '新增失败，请稍后重试'),
            );
            // 留在原地：填过的东西不该让人重填一遍。
            return false;
          }
          message.success(editing ? '已保存' : '已新增，套餐是草稿状态，要卖请再上架一次');
          actionRef.current?.reload();
          return true;
        }}
      >
        {/*
          编码只在新建时出现。**不可修改**是库上的触发器钉着的，后端也没有第二个能改它的
          接口，所以编辑时连看都不必看——摆在表单里会让人以为能改。
        */}
        {!editing && (
          <ProFormText
            name="code"
            label="套餐编码"
            rules={[
              { required: true, message: '请填套餐编码' },
              {
                pattern: /^[a-z0-9_]+$/,
                message: '只能用小写字母、数字和下划线',
              },
            ]}
            fieldProps={{ maxLength: 64 }}
            tooltip="给系统看的稳定标识（如 monthly_auto）。**建成后不可修改**——它会被当成成交快照写进每一笔买了它的会员行。"
          />
        )}
        <ProFormText
          name="name"
          label="名称"
          rules={[{ required: true, message: '请填名称' }]}
          fieldProps={{ maxLength: 100 }}
        />
        <ProFormText
          name="description"
          label="描述"
          placeholder="可留空"
          fieldProps={{ maxLength: 500 }}
        />
        <ProFormTextArea
          name="benefits"
          label="权益"
          placeholder="一行一条，可留空"
          fieldProps={{ rows: 3 }}
          tooltip="展示给用户看的卖点，一行一条（如「每月 20 次咖啡享会员价」）。最多 20 行，每行 100 字以内——超了后端会拒。"
        />
        <ProFormDigit
          name="priceYuan"
          label="价格（元）"
          rules={[{ required: true, message: '请填价格' }]}
          min={0}
          // 后端上限是 100000000 分，也就是一百万元。
          max={1000000}
          fieldProps={{ precision: 2 }}
          tooltip="按元填，提交时换成后端要的分。**0 是合法取值**（不收费的会员）。"
        />
        <ProFormSelect
          name="period"
          label="时长单位"
          rules={[{ required: true, message: '请选择时长单位' }]}
          options={[
            { label: PLAN_PERIOD.month.text, value: 'month' },
            { label: PLAN_PERIOD.year.text, value: 'year' },
          ]}
          tooltip="与下面的个数一起读：「月 + 1」是连续包月，「年 + 1」是年度会员。"
        />
        <ProFormDigit
          name="periodCount"
          label="时长个数"
          rules={[{ required: true, message: '请填时长个数' }]}
          min={1}
          max={120}
          fieldProps={{ precision: 0 }}
          tooltip="买一次管几个「时长单位」。上限 120。"
        />
        <ProFormDigit
          name="sortOrder"
          label="排序"
          min={0}
          max={9999}
          fieldProps={{ precision: 0 }}
          tooltip="小程序套餐列表按它排（相同则按编码），数字小的在前。0 表示不特别排。"
        />
        {/*
          自动续费与签约模板 ID 是**一对**：打开必须给模板 ID，关掉必须留空。
          提交时 buildPayload 会按开关把 wechatPlanId 清掉——这里的显隐只是让人看得见，
          不是清空（看不见的值照样会被提交，那正是这一对最容易 400 的地方）。
        */}
        <ProFormSwitch
          name="autoRenew"
          label="支持自动续费"
          tooltip="打开表示这个套餐要签微信委托代扣、到期自动扣款。签约与代扣本身还没实现（只落了两张表），所以今天打开它只是先把位置留出来。"
        />
        <ProFormDependency name={['autoRenew']}>
          {({ autoRenew }) =>
            autoRenew ? (
              <ProFormText
                name="wechatPlanId"
                label="微信签约模板 ID"
                rules={[{ required: true, message: '支持自动续费时必须填签约模板 ID' }]}
                fieldProps={{ maxLength: 64 }}
                tooltip="微信商户平台上的模板 ID。后端在「支持自动续费」时必填、不支持时必须留空，两个方向都校验。"
              />
            ) : null
          }
        </ProFormDependency>
        {/*
          会员价方式与它下面两个字段也是一对，规则同上：coupon 必须都给，auto 必须都留空。
        */}
        <ProFormSelect
          name="memberPriceMode"
          label="会员价方式"
          rules={[{ required: true, message: '请选择会员价方式' }]}
          options={[
            { label: '会员自动享会员价', value: 'auto' },
            { label: '发会员价体验券', value: 'coupon' },
          ]}
          tooltip="auto 是会员本人自动享（年度会员那档），coupon 是靠每期发的会员价体验券（连续包月那档）。用户手机上看到的那句话就是照它说的。"
        />
        <ProFormDependency name={['memberPriceMode']}>
          {({ memberPriceMode }) =>
            memberPriceMode === 'coupon' ? (
              <>
                <ProFormSelect
                  name="memberPriceCouponTemplateId"
                  label="会员价券模板"
                  rules={[{ required: true, message: '发体验券时必须选券模板' }]}
                  // **只列会员价体验券那一类模板**，别的券（代金券之类）在这里没有意义，摆
                  // 出来只会被挑错。筛选交给服务端：要拿编码对应的 couponTypeId 得查
                  // coupon_types，而那个接口要 coupon:type:manage，前端自己过滤的话，
                  // 只有会员域权限（没有券类型管理权限）的人会看到一个空下拉。
                  //
                  // 券模板列表本身在优惠券域（coupon:read），只有会员域权限的人打开这里仍是
                  // 空的——后端的取值才是权威，这里不做额外的权限分支。
                  request={async () => {
                    const result = await listCouponTemplates({
                      ...FULL_PAGE_PARAMS,
                      couponTypeCode: MEMBERSHIP_PRICE_COUPON_TYPE_CODE,
                    });
                    return result.items.map((item) => ({
                      label: `${item.name}（${item.id.slice(0, 8)}）`,
                      value: item.id,
                    }));
                  }}
                  // 空下拉是这页最容易让人以为「坏了」的地方：dev 库里现在一张会员价体验券
                  // 模板都没有（只有代金券），所以先把话写在这儿——去建一张，不是重试。
                  fieldProps={{
                    notFoundContent: (
                      <span style={{ color: '#999' }}>
                        还没有「会员价体验券」类型的券模板。
                        {access.canViewCoupons ? (
                          // 用 history.push 而不是 <a href>：SPA 里 <a> 会整页重载。
                          <Typography.Link onClick={() => history.push('/coupons/templates')}>
                            去优惠券模板创建
                          </Typography.Link>
                        ) : (
                          '要去「优惠券模板」里建一张，类型选「会员价体验券」。'
                        )}
                      </span>
                    ),
                  }}
                  tooltip="每期按它发券：支付成功（开通或续费）时会员服务发出事件，由优惠券域照它发这一期的券。后台人工调整有效期不会发券。"
                />
                <ProFormDigit
                  name="memberPriceCouponsPerPeriod"
                  label="每期发几张"
                  rules={[{ required: true, message: '请填每期发几张' }]}
                  min={1}
                  max={1000}
                  fieldProps={{ precision: 0 }}
                  tooltip="一个会员期内发几张体验券。后端要求 coupon 模式下大于 0。"
                />
              </>
            ) : null
          }
        </ProFormDependency>
      </ModalForm>
    </PageContainer>
  );
}
