import { ModalForm, PageContainer, ProDescriptions, ProFormTextArea, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { Button, Drawer, message, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { getUserCoupon, listCouponBatches, listCouponTemplates, listCouponTypes, listUserCoupons, redeemUserCoupon, revokeUserCoupon, type UserCoupon, type UserCouponQuery } from '../../services/coupon';
import { CLAIM_TYPE, REDEMPTION_TYPE, USER_COUPON_STATUS } from '../../services/couponLabels';
import { enumMeta, searchOptions } from '../../services/labels';
import { getMiniappUser } from '../../services/miniappUser';
import { FULL_PAGE_PARAMS } from '../../services/pagination';
import { requestErrorMessage } from '../../services/requestError';
import { useAccess } from '@umijs/max';
import { scrollableModalBody } from '../../components/common/modalProps';

// 接口给的面值/最低消费是「分」的整数，列表和详情都按元展示。
// 与 pages/coupon-templates 的换算方向相反（那边是录入），但口径必须一致：
// 两边都除以 100，任何一处漏了都会让同一张券在两个页面上显示成不同的钱。
const formatYuan = (fen?: number) => (Number(fen ?? 0) / 100).toFixed(2);

// revoke 的语义是「作废未使用的券」（claimed/held -> invalidated），不是冲正已核销的券，
// 所以文案统一叫「作废」：叫「撤销」会让管理员以为能撤掉一笔已经核销的账。
type CouponAction = 'redeem' | 'revoke';
const actionTitle = (type: CouponAction) => (type === 'redeem' ? '核销用户券' : '作废用户券');

/**
 * user_coupons 这张表存的全是「别人家的 id」：user_id 是小程序用户、template_id 是模板、
 * batch_id 是批次、coupon_type_code 是券类型码。全铺出来就是四列 uuid/英文码，
 * 而看这一页的人（客服）认的是手机号、券名、批次号。
 *
 * 四个都一样处理：先查字典，查不到就退回显示原值 —— 退回比空白好，原值是能拿去搜的。
 * 字典取不到（权限不足、接口挂了）也只是不好看，不该让整页报错。
 */
export default function UserCouponsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [detail, setDetail] = useState<UserCoupon>();
  const [actionTarget, setActionTarget] = useState<{ coupon: UserCoupon; type: CouponAction }>();
  // 券类型码 → 名称。user_coupons.couponTypeCode 存的是 coupon_types.code 的副本
  // （发券时就写进去了），光看 COFFEE_CASH 认不出是哪张券。
  const [typeNames, setTypeNames] = useState<Record<string, string>>({});
  const [templateNames, setTemplateNames] = useState<Record<string, string>>({});
  const [batchNos, setBatchNos] = useState<Record<string, string>>({});
  const [userNames, setUserNames] = useState<Record<string, string>>({});
  // 已经查过（或正在查）的用户，避免翻页、刷新、开详情时重复发同一个人的请求。
  const askedUsers = useRef<Set<string>>(new Set());

  useEffect(() => {
    void (async () => {
      try {
        const types = await listCouponTypes();
        setTypeNames(Object.fromEntries(types.map((item) => [item.code, item.name])));
      } catch {
        // 忽略：取不到字典就退回显示原始编码。看用户券的权限（coupon:user-coupon:read）
        // 和看类型字典的权限（coupon:type:manage）是分开的，只有前者的人这里必定失败，
        // 不该因为一个只用来翻译的请求让整页报错——与模板列表那几列取名称的处理一致。
      }
      try {
        const { items } = await listCouponTemplates(FULL_PAGE_PARAMS);
        setTemplateNames(Object.fromEntries(items.map((item) => [item.id, item.name])));
      } catch {
        // 同上，取不到就用 id。模板/批次是「字典」性质的集合，走全集映射和
        // pages/coupon-templates 取商户/品牌/门店名是同一个路子。
      }
      try {
        const { items } = await listCouponBatches(FULL_PAGE_PARAMS);
        setBatchNos(Object.fromEntries(items.map((item) => [item.id, item.batchNo])));
      } catch {
        // 同上。
      }
    })();
  }, []);

  const typeName = (code?: string) => (code ? (typeNames[code] ?? code) : '—');
  const templateName = (id?: string) => (id ? (templateNames[id] ?? id) : '—');
  const batchNo = (id?: string) => (id ? (batchNos[id] ?? id) : '—');
  const userName = (id?: string) => (id ? (userNames[id] ?? id) : '—');

  /**
   * 用户不能像上面三张字典那样一次拉全集：小程序用户没有上限，后台用户列表接口
   * 也不支持按 id 批量取（只有 page/keyword），拉前 200 条再映射，第 201 个人就会
   * 显示成一串 uuid，还看不出是「查不到」还是「没这个人」。
   *
   * 所以按「本页出现过的用户」逐个查详情：一页 20 张券通常只涉及几个人（一个人
   * 可以持多张），查过的记在 askedUsers 里，翻页和刷新都不会重发。
   */
  const hydrateUsers = (items: UserCoupon[]) => {
    const missing = Array.from(new Set(items.map((item) => item.userId).filter(Boolean))).filter(
      (id) => !askedUsers.current.has(id),
    );
    if (missing.length === 0) return;
    missing.forEach((id) => askedUsers.current.add(id));
    void Promise.all(
      missing.map(async (id) => {
        try {
          const user = await getMiniappUser(id);
          const label = user.phone ? (user.nickname ? `${user.phone}（${user.nickname}）` : user.phone) : '';
          // 没手机号（比如已注销）时保留 id，不要显示成空串。
          if (label) setUserNames((prev) => ({ ...prev, [id]: label }));
        } catch {
          // 查不到就继续显示 id。注意这里**没有**把 id 从 askedUsers 里去掉：
          // 网络抖一次就重查一遍会让每次翻页都发同一批请求。
        }
      }),
    );
  };

  const columns: ProColumns<UserCoupon>[] = [
    {
      title: '用户',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 160,
      // 搜索仍然是按 id 精确匹配（后端 user_id::text = $n），下拉里给不出候选，
      // 所以这一格是输入框不是选择器：客服手里拿的就是顾客报的 id。
      render: (_, record) => userName(record.userId),
    },
    // 券号：客服拿顾客发来的这一串定位到具体哪一张，所以它自己就是主键式的列，
    // 不做翻译，只做复制和省略。
    { title: '用户券 ID', dataIndex: 'id', copyable: true, ellipsis: true, width: 200 },
    {
      title: '模板',
      dataIndex: 'templateId',
      ellipsis: true,
      width: 160,
      valueType: 'select',
      // valueEnum 只喂搜索下拉：给的是模板名，发出去的仍是 id（后端按 template_id 比）。
      valueEnum: Object.fromEntries(Object.entries(templateNames).map(([id, name]) => [id, { text: name }])),
      render: (_, record) => templateName(record.templateId),
    },
    {
      title: '类型',
      dataIndex: 'couponTypeCode',
      width: 110,
      valueType: 'select',
      // 搜索下拉给中文名，发出去的仍是编码——后端 coupon_type_code 比的是编码。
      valueEnum: Object.fromEntries(
        Object.entries(typeNames).map(([code, name]) => [code, { text: name }]),
      ),
      render: (_, record) => typeName(record.couponTypeCode),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      valueType: 'select',
      valueEnum: searchOptions(USER_COUPON_STATUS),
      render: (_, record) => {
        const meta = enumMeta(USER_COUPON_STATUS, record.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '面值', dataIndex: 'faceValue', search: false, width: 90, render: (_, record) => formatYuan(record.faceValue) },
    { title: '有效期', dataIndex: 'expiredAt', valueType: 'dateTime', search: false, width: 180 },
    {
      title: '操作',
      valueType: 'option',
      // 三个按钮（详情/核销/作废）实测要 204px：动作类的两个按钮是按状态出现的，
      // 已领取的那一行三个都在。这一列以前没写宽度、表格也没开 scroll.x，走的是浏览器
      // 自动分配（八列均分 152px），按钮直接溢到表格右边 52px。
      width: 220,
      fixed: 'right',
      render: (_, record) => [
        <Button key="detail" type="link" onClick={async () => { const one = await getUserCoupon(record.id); hydrateUsers([one]); setDetail(one); }}>详情</Button>,
        access.canRedeemCoupon && record.status === 'claimed' ? <Button key="redeem" type="link" onClick={() => setActionTarget({ coupon: record, type: 'redeem' })}>核销</Button> : null,
        access.canRevokeCoupon && ['claimed', 'held'].includes(record.status) ? <Button key="revoke" type="link" danger onClick={() => setActionTarget({ coupon: record, type: 'revoke' })}>作废</Button> : null,
      ].filter(Boolean),
    },
  ];

  return <PageContainer title="用户券">
    {/* scroll.x 必须等于各列 width 之和（160+200+160+110+90+90+180+220=1210）。
        不加这个，表格就没有固定布局，列宽由浏览器按内容分配，钉在右边的操作列会算错位置。 */}
    <ProTable<UserCoupon> rowKey="id" actionRef={actionRef} columns={columns} scroll={{ x: 1210 }} search={{ labelWidth: 'auto' }} request={async (params) => {
      const query: UserCouponQuery = { page: params.current, pageSize: params.pageSize, userId: params.userId, status: params.status, batchId: params.batchId, templateId: params.templateId, id: params.id, couponTypeCode: params.couponTypeCode };
      const result = await listUserCoupons(query);
      hydrateUsers(result.items);
      return { data: result.items, total: result.total, success: true };
    }} />
    <Drawer title="用户券详情" width={640} open={!!detail} onClose={() => setDetail(undefined)}>
      {detail && <ProDescriptions<UserCoupon>
        column={2}
        dataSource={detail}
        // ProDescriptions 的 render 签名是 (dom, entity, index, action, schema)，
        // 第二个参数是**整行数据**，不是这一格的字段值。原来的「状态」按字段值用
        // （String(value)），于是整行对象被转成了字符串 [object Object]，再拿去
        // 查状态表当然查不到，界面上就直接显示成这串字。列表那一列用的是 record，
        // 所以只有详情里是坏的。
        //
        // 下面每格都补一行「人看得懂的那个」：券类型、模板名、批次号、用户手机号，
        // id 一律保留 —— 报障时客服和开发对的就是那串 id。
        columns={[
          { title: '用户', dataIndex: 'userName', render: (_, record) => userName(record.userId) },
          { title: '用户 ID', dataIndex: 'userId' },
          { title: '用户券 ID', dataIndex: 'id' },
          { title: '券类型', dataIndex: 'couponTypeCode', render: (_, record) => typeName(record.couponTypeCode) },
          { title: '模板', dataIndex: 'templateName', render: (_, record) => templateName(record.templateId) },
          { title: '模板 ID', dataIndex: 'templateId' },
          { title: '批次号', dataIndex: 'batchNo', render: (_, record) => batchNo(record.batchId) },
          { title: '批次 ID', dataIndex: 'batchId' },
          { title: '领取方式', dataIndex: 'claimType', render: (_, record) => enumMeta(CLAIM_TYPE, record.claimType).text },
          { title: '状态', dataIndex: 'status', render: (_, record) => { const meta = enumMeta(USER_COUPON_STATUS, record.status); return <Tag color={meta.color}>{meta.text}</Tag>; } },
          { title: '面值', dataIndex: 'faceValue', render: (_, record) => formatYuan(record.faceValue) },
          { title: '最低消费', dataIndex: 'minPurchaseAmount', render: (_, record) => formatYuan(record.minPurchaseAmount) },
          { title: '核销方式', dataIndex: 'redemptionType', render: (_, record) => enumMeta(REDEMPTION_TYPE, record.redemptionType).text },
          { title: '生效时间', dataIndex: 'validFrom', valueType: 'dateTime' },
          { title: '过期时间', dataIndex: 'expiredAt', valueType: 'dateTime' },
          { title: '领取时间', dataIndex: 'claimedAt', valueType: 'dateTime' },
          { title: '核销时间', dataIndex: 'redeemedAt', valueType: 'dateTime' },
          { title: '作废时间', dataIndex: 'invalidatedAt', valueType: 'dateTime' },
          { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime' },
          { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
        ]}
      />}
    </Drawer>
    {/* 用 ModalForm 而不是 Modal.confirm：命令式弹窗的 onOk 一旦 reject，
        antd 的 ActionButton 会把异常原样抛出去变成 unhandled rejection，
        dev 下直接糊一层错误浮层；返回 false 又会关掉弹窗、丢掉已填的原因。
        必填校验交给 rules，弹窗自然留在原地并显示行内错误。 */}
    <ModalForm<{ reason?: string }>
      open={!!actionTarget}
      onOpenChange={(next) => { if (!next) setActionTarget(undefined); }}
      title={actionTarget ? actionTitle(actionTarget.type) : ''}
      // initialValues 只在首次挂载生效，上一个券填的原因会留到下一个券：
      // 没有 initialValues 也仍然要 destroyOnClose，否则输入框内容不重置。
      modalProps={{ ...scrollableModalBody, destroyOnClose: true }}
      onFinish={async (values) => {
        if (!actionTarget) return false;
        const { coupon, type } = actionTarget;
        const reason = values.reason?.trim();
        try {
          if (type === 'redeem') await redeemUserCoupon(coupon.id, { reason });
          else await revokeUserCoupon(coupon.id, { reason });
        } catch (error) {
          // 后端按「当下这一张是什么状态」判能不能核销/作废；客服手上那一行可能是
          // 几秒前拉的，这期间券已经被别处核销掉了。理由得让人看见，并且返回 false
          // 留着弹窗，填好的原因不丢。
          message.error(requestErrorMessage(error, '操作失败，请稍后重试'));
          return false;
        }
        message.success(`${actionTitle(type)}成功`);
        actionRef.current?.reload();
        if (detail?.id === coupon.id) {
          try {
            setDetail(await getUserCoupon(coupon.id));
          } catch (error) {
            // 写已经成功了，这一步只是把抽屉里那份刷新一遍；取不到就关掉它——
            // 留着的是改动前的状态，比空着更误导。
            setDetail(undefined);
            message.error(requestErrorMessage(error, '重新读取用户券详情失败'));
          }
        }
        return true;
      }}
    >
      <ProFormTextArea
        name="reason"
        label={actionTarget?.type === 'revoke' ? '作废原因' : '原因'}
        placeholder={actionTarget?.type === 'revoke' ? '请输入作废原因' : '请输入原因（可选）'}
        fieldProps={{ rows: 3 }}
        // 服务端不强制 revoke 要原因（UserCouponActionInput.reason 可选），
        // 这里要求必填是运营口径：作废是人工介入，得留下依据。
        rules={actionTarget?.type === 'revoke' ? [{ required: true, message: '请输入作废原因' }] : []}
      />
    </ModalForm>
  </PageContainer>;
}
