import {
  ModalForm,
  PageContainer,
  ProFormSelect,
  ProFormText,
  ProFormTextArea,
  ProTable,
} from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { useAccess } from '@umijs/max';
import { Alert, Button, Popconfirm, Tag, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { enumMeta, searchOptions } from '../../../services/labels';
import { requestErrorMessage } from '../../../services/requestError';
import {
  createSettlementAccount,
  deleteSettlementAccount,
  listSettlementAccounts,
  listSettlementChannels,
  updateSettlementAccount,
  type SettlementAccount,
  type SettlementAccountInput,
  type SettlementAccountQuery,
  type SettlementChannel,
} from '../../../services/settlement';
import { PARTY_TYPE, RECEIVER_TYPE, RECORD_STATUS } from '../../../services/settlementLabels';

/**
 * 分账账户：**钱分到谁的哪个子商户号上**。
 *
 * # 这一页里最要紧的一列是「渠道接收方号」
 *
 * 它就是发起支付时随下单报文发出去的子单里的 `mid`（`subOrders[].mid`）——渠道按它找到收款方。
 * 也是**快照的源头**：任务建下来的那一刻这个值会被冻结进 `settlement_receivers`，账户后来改了
 * 名或换了号都不会回头影响历史明细。这正是账户改得、而明细不改的原因。
 *
 * 写错它的表现是**钱分给了别人**，而且本地一切正常（分账是随支付一次下发的，支付成功即分账
 * 成功）。所以它不是一栏可以随手填的输入框。
 *
 * # 这一行只有两个名字，两个都有去处
 *
 * **主体名**（必填）是给人认人的那一个：规则项的账户下拉、分账接收方的快照、这一页的列表、
 * 关键词搜索，用的都是它。**渠道接收方号**是给渠道认的那个（见上）。此外没有第三个。
 *
 * 原先还有「账户号」与「接收方名」两个标签，随 017 一并删了：账户号只是我们自己编的号、它那条
 * 唯一约束护的也只是「别重名」，而 008 写着「结算单上写它」的那个用途从来没有兑现（结算单那两
 * 张表至今是空的，上面也只有 account_id）；接收方名连渠道报文都不进，全仓没有一处读它。删掉
 * 账户号之后主体名成了**唯一**可读的名字，所以它从「可空、空着显示账户号」改成了必填。
 *
 * # 渠道下拉里只有一项，那不是页面坏了
 *
 * 「钱能分到哪条渠道上」由「哪条渠道的下单报文里能带子单」决定，那是发版的事，不是运营的输入
 * ——所以它来自 payment-service 代码里的目录（`catalog.Channel.Settlement`），不是一张表。
 * 今天只有银联商务一条。这个下拉会随渠道能力增长，不用改这一页。
 *
 * # 停用而不是删除
 *
 * 被规则项引用过、或被历史分账明细引用过的账户**删不掉**（两张表都是 ON DELETE RESTRICT），
 * 后端回 409。而一条账户只要分过一次钱就必然被引用——所以常规出口是**停用**：停用之后不再有
 * 新的分账分到它（执行期那条 LEFT JOIN 按 `status='enabled'` 过滤），历史明细原样保留。
 *
 * 停用还有一个连带效果值得知道：**引用了停用账户的规则会保存不了**（后端在保存时要确认每一项
 * 挂的账户现在可用）。这时候要么先把这个账户启用、要么把那条规则里的账户换掉。
 */

/** 接口给的空值一律显示成「—」：留白和「有个空字符串」在表里分不出来。 */
const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：发出去之前 trim，空的整个丢掉。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function SettlementAccountsPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();

  /** 正在编辑的那一条。null = 弹窗关着；没有 id = 新建。 */
  const [editing, setEditing] = useState<SettlementAccount | null | undefined>(undefined);
  // undefined 是「关着」、null 是「新建」，两者要分得开，所以这里用三态而不是布尔。

  /** 可分账的渠道。今天只有一条（银联商务）——见文件头的说明。 */
  const [channels, setChannels] = useState<SettlementChannel[]>([]);

  useEffect(() => {
    // 取不到不该让整页打不开：渠道下拉是空的，账户列表照常（列里退回显示 provider 码）。
    void listSettlementChannels()
      .then(setChannels)
      .catch(() => setChannels([]));
  }, []);

  /** provider 码 → 渠道名。列里优先显示名字，取不到退回码。 */
  const channelName = (provider: string) =>
    channels.find((channel) => channel.provider === provider)?.name || provider;

  const openEdit = (row: SettlementAccount) => setEditing(row);

  const submit = async (values: SettlementAccountInput) => {
    const payload: SettlementAccountInput = {
      partyName: values.partyName,
      partyType: values.partyType,
      provider: values.provider,
      // 空 = MERCHANT_ID：那是默认、也是今天唯一用得到的那一个（银联商务按子商户号直接分）。
      receiverType: values.receiverType ?? 'MERCHANT_ID',
      receiverId: values.receiverId,
      status: values.status,
      remark: values.remark ?? '',
    };
    try {
      if (editing) {
        await updateSettlementAccount(editing.id, payload);
        message.success('账户已保存');
      } else {
        await createSettlementAccount(payload);
        message.success('账户已创建');
      }
      setEditing(undefined);
      actionRef.current?.reload();
      return true;
    } catch (error) {
      // 失败时**不关弹窗**：填的东西留着，改一改再提交。话照搬后端那句——它为「渠道不是可分账
      // 的那几条」「主体名不能为空」各写了一句中文。
      message.error(requestErrorMessage(error, '保存失败：请检查渠道、主体名与渠道接收方号'));
      return false;
    }
  };

  const remove = async (row: SettlementAccount) => {
    try {
      await deleteSettlementAccount(row.id);
      message.success('账户已删除');
      actionRef.current?.reload();
    } catch (error) {
      // 多半是 409「已被规则或分账明细引用」——后端为这一种写了中文，照搬它。
      message.error(requestErrorMessage(error, '删除失败：可能已被规则或分账引用，请改为停用'));
    }
  };

  const columns: ProColumns<SettlementAccount>[] = [
    {
      // 搜索框只有「关键词」一个：它会同时搜主体名与子商户号两项（后端定的口径）。逐个字段各摆
      // 一个框，会让人把两个都试一遍——而他手上那串是什么号，他自己多半也说不清。
      title: '关键词',
      dataIndex: 'keyword',
      hideInTable: true,
      fieldProps: { placeholder: '主体名 / 子商户号' },
    },
    {
      // 主体名是给人认人的，也是这一行**唯一**的名字（账户号那列随 017 删了）。所以它必填：
      // 空着的话这一行在页面上只剩一个渠道接收方号，而那是给渠道看的，不是给人认的。
      title: '主体名',
      dataIndex: 'partyName',
      search: false,
      ellipsis: true,
      width: 160,
      render: (_, row) => dash(row.partyName),
    },
    {
      title: '主体类型',
      dataIndex: 'partyType',
      valueType: 'select',
      valueEnum: searchOptions(PARTY_TYPE),
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(PARTY_TYPE, row.partyType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 按渠道筛的取值也是那个目录（今天只有一项）。不给搜索框一个自由文本：写错的渠道名会
      // 静默返回空列表，看着像「这个渠道一个账户都没有」。
      title: '渠道',
      dataIndex: 'provider',
      valueType: 'select',
      valueEnum: Object.fromEntries(channels.map((c) => [c.provider, { text: c.name }])),
      width: 120,
      render: (_, row) => dash(channelName(row.provider)),
    },
    {
      // 这一列就是渠道报文里的子单 mid。写错等于把钱分给别人，所以标出来是「号」不是「名」。
      title: '渠道接收方号',
      dataIndex: 'receiverId',
      search: false,
      copyable: true,
      ellipsis: true,
      width: 190,
      render: (_, row) => dash(row.receiverId),
    },
    {
      title: '接收方类型',
      dataIndex: 'receiverType',
      search: false,
      width: 120,
      render: (_, row) => enumMeta(RECEIVER_TYPE, row.receiverType).text,
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(RECORD_STATUS),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(RECORD_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '创建时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '操作',
      valueType: 'option',
      width: 140,
      fixed: 'right',
      render: (_, row) => {
        // 只读的人进得来这一页（「钱分到了哪个号上」是客服要答的话），但看不到写入口。
        if (!access.canManageSettlement) return [];
        return [
          <Button key="edit" type="link" size="small" onClick={() => openEdit(row)}>
            编辑
          </Button>,
          <Popconfirm
            key="delete"
            title="删除这个账户？"
            description="只要它分过一次钱就删不掉——那属于正常，改成停用即可。"
            onConfirm={() => remove(row)}
          >
            <Button type="link" size="small" danger>
              删除
            </Button>
          </Popconfirm>,
        ];
      },
    },
  ];

  return (
    <PageContainer
      title="分账账户"
      content="钱分到谁的哪个号上。渠道接收方号就是下单报文里子单的 mid——写错的表现是钱分给了别人，而本地一切正常。被引用过的账户删不掉，把它停用即可。"
      extra={
        access.canManageSettlement
          ? [
              <Button key="create" type="primary" onClick={() => setEditing(null)}>
                新建账户
              </Button>,
            ]
          : undefined
      }
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="渠道下拉里今天只有一项"
        description="能分账的渠道由「哪条渠道的下单报文里能带子单」决定，那是发版的事——所以这份列表来自代码里的目录，今天只有银联商务。停用一个账户时请一并留意：引用了它的分账规则会因此保存不了，要先把规则里的账户换掉或者把它启用回来。"
      />

      <ProTable<SettlementAccount>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 1110 = 160+110+120+190+120+100+170+140，各列 width 之和。改任何一列的宽度都要同批改
        // 这个数（钉右列的不变式：fixed 列必须显式 width，scroll.x = 各列 width 之和）。
        scroll={{ x: 1110 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: SettlementAccountQuery = {
            page: params.current,
            pageSize: params.pageSize,
            keyword: exact(params.keyword),
            partyType: exact(params.partyType) as SettlementAccountQuery['partyType'],
            provider: exact(params.provider),
            status: exact(params.status) as SettlementAccountQuery['status'],
          };
          try {
            const result = await listSettlementAccounts(query);
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            message.error(requestErrorMessage(error, '加载分账账户失败'));
            return { data: [], total: 0, success: false };
          }
        }}
      />

      {/*
        新建 / 编辑共用这一个弹窗。**key 与 destroyOnClose 两个都要有**——缺了它们，跨行编辑时
        上一条的字段会落到下一条上（全仓踩过一次的坑）。key 里带上 id，切行即重建。
      */}
      <ModalForm<SettlementAccountInput>
        key={`account-${editing?.id ?? 'new'}`}
        title={editing ? '编辑账户' : '新建账户'}
        open={editing !== undefined}
        width={640}
        modalProps={{
          destroyOnClose: true,
          maskClosable: false,
          onCancel: () => setEditing(undefined),
        }}
        initialValues={
          editing
            ? {
                partyName: editing.partyName,
                partyType: editing.partyType,
                provider: editing.provider,
                receiverType: editing.receiverType,
                receiverId: editing.receiverId,
                status: editing.status,
                remark: editing.remark,
              }
            : { partyType: 'member_store', receiverType: 'MERCHANT_ID', status: 'enabled' }
        }
        onFinish={submit}
      >
        <ProFormText
          name="partyName"
          label="主体名"
          rules={[{ required: true, message: '请填写主体名' }]}
          // 60 = 后端的 MaxSettlementNameLength（规则名那一栏也是 60）。原先写 100，比后端宽——
          // 多打的那 40 个字要等到提交才被拒。
          fieldProps={{ maxLength: 60 }}
          extra="收款主体的名字。必填——规则项的账户下拉、分账接收方的快照、账户列表用的都是它。"
        />
        <ProFormSelect
          name="partyType"
          label="主体类型"
          rules={[{ required: true, message: '请选择主体类型' }]}
          valueEnum={searchOptions(PARTY_TYPE)}
          extra="平台不是这里的选项——平台拿的是剩下的那一份，它没有账户。"
        />
        <ProFormSelect
          name="provider"
          label="渠道"
          rules={[{ required: true, message: '请选择渠道' }]}
          options={channels.map((channel) => ({ label: channel.name, value: channel.provider }))}
          fieldProps={{
            notFoundContent: (
              <span style={{ color: '#999' }}>
                没有可分账的渠道。这份列表来自代码里的目录，取不到多半是接口没起来。
              </span>
            ),
          }}
          extra="只有下单报文里能带子单的渠道能分账。今天只有银联商务一条。"
        />
        <ProFormSelect
          name="receiverType"
          label="接收方类型"
          rules={[{ required: true, message: '请选择接收方类型' }]}
          valueEnum={searchOptions(RECEIVER_TYPE)}
          extra="银联商务按子商户号直接分，所以默认且通常就是「子商户号」。个人 OpenID 是微信服务商分账那一种。"
        />
        <ProFormText
          name="receiverId"
          label="渠道接收方号"
          rules={[{ required: true, message: '请填写渠道接收方号' }]}
          // 64 = 后端的 MaxSettlementReceiverIDLength。原先写 100，同样是比后端宽的那种。
          fieldProps={{ maxLength: 64 }}
          extra="这一栏会随支付原样发给渠道（子单的 mid）。填错的表现是钱分给了别人，而本地一切正常——请对着渠道给的号核一遍。"
        />
        {/*
          这里原先还有「关联门店 / 关联品牌 / 关联商户」三个下拉，已随 016 迁移一并删掉：账户
          挂在哪条渠道、分给哪个子商户号上才是要紧的事，主体归属由**规则**表达（规则项直接挂
          account_id），账户自己带一份主体引用从来没有读者，只会带来「同一主体在同一渠道上只能
          有一条启用账户」这条多余的约束。
        */}
        <ProFormSelect
          name="status"
          label="状态"
          rules={[{ required: true, message: '请选择状态' }]}
          valueEnum={{ enabled: { text: '已启用' }, disabled: { text: '已停用' } }}
          extra="停用之后不再有新的分账分到它，历史明细原样保留。注意引用了停用账户的规则会因此保存不了。"
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          fieldProps={{ maxLength: 200, rows: 2, showCount: true }}
        />
      </ModalForm>
    </PageContainer>
  );
}
