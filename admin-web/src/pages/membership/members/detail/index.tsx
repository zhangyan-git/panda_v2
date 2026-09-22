import { ModalForm, PageContainer, ProFormDateTimePicker, ProFormTextArea } from '@ant-design/pro-components';
import { history, useAccess, useParams } from '@umijs/max';
import { Button, Card, Empty, Input, message, Modal } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { formatDateTime, toRFC3339 } from '../../../../services/datetime';
import {
  adjustMembershipExpiry,
  freezeMembership,
  getMembership,
  revokeMembership,
  unfreezeMembership,
  type Membership,
} from '../../../../services/membership';
import { requestErrorMessage } from '../../../../services/requestError';
import BasicTab from './BasicTab';
import ChangesTab from './ChangesTab';

type TabKey = 'basic' | 'changes';

/** 三个「填个原因就完事」的动作。它们共用同一个弹窗，只有文案与后果不同。 */
type ReasonAction = 'freeze' | 'unfreeze' | 'revoke';

/**
 * 三个动作各自的说法。
 *
 * 把它们摆成一张表而不是散在三个 onOk 里：这三段话是**用户看得见的后果说明**，
 * 并排放着才能一眼看出「冻结可逆、撤销不可逆」这件事有没有说清楚。
 */
const REASON_DIALOG: Record<
  ReasonAction,
  { title: string; okText: string; successText: string; danger: boolean; description: string; placeholder: string }
> = {
  freeze: {
    title: '冻结这个会员？',
    okText: '冻结',
    successText: '已冻结',
    danger: false,
    description:
      '冻结期间会员价权益停用，但有效期照旧在走——解冻之后不会补时间。到期时间不变，自动续费也不变。',
    placeholder: '为什么冻结（必填）。比如「风控命中，待核实」',
  },
  unfreeze: {
    title: '解冻这个会员？',
    okText: '解冻',
    successText: '已解冻',
    danger: false,
    description: '解冻后状态回到「生效中」。上一次的冻结时间与原因会留在记录里，不会清掉。',
    placeholder: '为什么解冻（必填）。比如「核实无误，恢复」',
  },
  revoke: {
    title: '撤销这个会员？',
    okText: '撤销',
    successText: '已撤销',
    danger: true,
    description:
      '撤销不可逆——系统里没有「取消撤销」这条路，撤销之后这个会员不能再做任何调整（后端会一律拒绝），自动续费也会一并关掉。确认已经与用户沟通过再点。',
    placeholder: '为什么撤销（必填）。比如「重复购买，已另行处理」',
  },
};

/**
 * 会员详情：基本信息 + 变更流水，**四个调整动作都在这里**（列表页只负责把人送进来）。
 *
 * 为什么不放到列表里：这四个动作**直接改一个人已经在享的权益**，权限码也是更高的一档
 * （membership:adjust）。缩在列表的一行里点错代价太大——撤销是不可逆的，而列表上看不到
 * 这个人现在到底什么状态、上一次被冻是什么时候。
 *
 * 状态与能做的动作（**前端只是藏按钮，真正的判断在后端**——这里每一条都有对应的 409）：
 * - `active`：能冻结、能改有效期、能撤销。
 * - `frozen`：能解冻、能改有效期、能撤销。**不能再次冻结**（后端只允许从 active 来）。
 * - `expired`：能改有效期、能撤销。**没有「重新激活」这个动作**——续期是买的，不是后台给的；
 *   真要恢复权益就改有效期（会写一条 admin_adjust 流水），那正是这个接口存在的理由。
 * - `revoked`：什么都不能做。
 */
export default function MembershipDetailPage() {
  const { id } = useParams<{ id: string }>();
  const access = useAccess();
  const [membership, setMembership] = useState<Membership>();
  const [loading, setLoading] = useState(true);
  // 取不到时的原因。id 是手敲的、或者不是 uuid，就落在这个分支。
  const [error, setError] = useState<string>();
  const [tab, setTab] = useState<TabKey>('basic');

  // **「弹窗开着」与「请求在飞」必须是两个 state**：合成一个的话，提交中想禁掉取消就得拿
  // 按钮的 disabled 去猜，而且请求一结束弹窗会自己关掉——失败时人也看不到原因。
  const [reasonAction, setReasonAction] = useState<ReasonAction>();
  const [reasonText, setReasonText] = useState('');
  const [reasonSubmitting, setReasonSubmitting] = useState(false);
  const [expireOpen, setExpireOpen] = useState(false);

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      setMembership(await getMembership(id));
      setError(undefined);
    } catch (err) {
      setError(requestErrorMessage(err, '加载会员详情失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  /** 关掉原因弹窗并把框里的字清掉——不清的话下次开窗会留着上一个人的原因。 */
  const closeReason = () => {
    setReasonAction(undefined);
    setReasonText('');
  };

  const submitReason = async () => {
    if (!membership || !reasonAction) return;
    const dialog = REASON_DIALOG[reasonAction];
    const reason = reasonText.trim();
    if (!reason) {
      message.error('请填原因');
      return;
    }
    setReasonSubmitting(true);
    try {
      if (reasonAction === 'freeze') await freezeMembership(membership.id, reason);
      else if (reasonAction === 'unfreeze') await unfreezeMembership(membership.id, reason);
      else await revokeMembership(membership.id, reason);
    } catch (err) {
      // **失败时弹窗不关**：原因留在框里，人可以改一改重试，而不是重打一遍。
      // 后端挡下来的那些（比如状态已经不是 active 了、或者别的客服刚撤销过）会在
      // 这里原样显示它那句中文，不做 errorCode 分支——409 内部怎么区分全靠那句话。
      message.error(requestErrorMessage(err, `${dialog.okText}失败，请稍后重试`));
      setReasonSubmitting(false);
      return;
    }
    setReasonSubmitting(false);
    message.success(dialog.successText);
    closeReason();
    // 重取整份实体，而不是本地改状态：流水会多一行，按钮该消失的也要消失，全跟着这次响应变。
    await load();
  };

  const tabList = [
    { key: 'basic', tab: '基本信息' },
    // 流水条数写进页签：这一页最常被问的就是「这个人身上到底发生过什么」，条数能一眼看出
    // 有没有（开通那条一定在，所以正常情况下不会是 0）。
    { key: 'changes', tab: `变更流水（${membership?.changes.length ?? 0}）` },
  ];

  const adjustEnabled = access.canAdjustMembership && !!membership;
  const status = membership?.status;

  return (
    <PageContainer
      loading={loading}
      title="会员详情"
      // 副标题放套餐快照：拿着一个人进来，第二件想知道的就是「他买的是哪个」。
      subTitle={membership ? `${membership.planName}（${membership.planCode}）` : undefined}
      onBack={() => history.push('/membership/members')}
      extra={
        !adjustEnabled
          ? undefined
          : [
              status === 'active' ? (
                <Button key="freeze" onClick={() => setReasonAction('freeze')}>
                  冻结
                </Button>
              ) : null,
              status === 'frozen' ? (
                <Button key="unfreeze" onClick={() => setReasonAction('unfreeze')}>
                  解冻
                </Button>
              ) : null,
              // 改有效期与撤销对**除已撤销外**的所有状态开放：已过期的会员要靠它恢复权益，
              // 而冻结中的会员也要能退钱、能撤销。已撤销的不给——后端一律 409。
              status !== 'revoked' ? (
                <Button key="expire" onClick={() => setExpireOpen(true)}>
                  调整有效期
                </Button>
              ) : null,
              status !== 'revoked' ? (
                <Button key="revoke" danger onClick={() => setReasonAction('revoke')}>
                  撤销
                </Button>
              ) : null,
            ]
      }
      tabList={tabList}
      tabActiveKey={tab}
      onTabChange={(key) => setTab(key as TabKey)}
    >
      {error ? (
        <Card>
          <Empty description={error} />
        </Card>
      ) : membership ? (
        tab === 'changes' ? (
          <ChangesTab changes={membership.changes} />
        ) : (
          <BasicTab membership={membership} />
        )
      ) : null}

      {/*
        冻结 / 解冻 / 撤销。**单独一个 Modal 而不是 Popconfirm**：这三个都要收一个必填的
        原因，而 Popconfirm 里塞输入框是个坑（确认按钮在拿到输入前就能点）。三个动作共用一个
        弹窗，文案从 REASON_DIALOG 里取——它们的表单是同一个（一个原因框），差别只在后果说明。
      */}
      <Modal
        title={reasonAction ? REASON_DIALOG[reasonAction].title : ''}
        open={!!reasonAction}
        // destroyOnClose + 上面的 closeReason 双保险：受控的输入框本来就跟着 state 清，
        // 但这个弹窗里再长出一个不受控的控件时，只靠 state 是收不干净上一轮输入的。
        destroyOnClose
        // 提交中不许关：关掉之后那个请求还在飞，而人已经看不到结果了（成功与失败都看不到）。
        onCancel={() => {
          if (reasonSubmitting) return;
          closeReason();
        }}
        okText={reasonAction ? REASON_DIALOG[reasonAction].okText : '确定'}
        cancelText="取消"
        // 原因必须填（后端会拒），所以框空着的时候按钮就是灰的——让人先点了再被打回来是白跑一趟。
        okButtonProps={{
          danger: reasonAction ? REASON_DIALOG[reasonAction].danger : false,
          disabled: !reasonText.trim(),
        }}
        confirmLoading={reasonSubmitting}
        onOk={submitReason}
        maskClosable={false}
      >
        {/* 纯文本，不加 markdown 记号——这一段没有过 Markdown 渲染。 */}
        <p>{reasonAction ? REASON_DIALOG[reasonAction].description : ''}</p>
        <Input.TextArea
          value={reasonText}
          onChange={(event) => setReasonText(event.target.value)}
          rows={3}
          maxLength={200}
          showCount
          placeholder={reasonAction ? REASON_DIALOG[reasonAction].placeholder : ''}
        />
      </Modal>

      {/*
        调整有效期。**这是本域破坏力最大的一个动作**：没有任何订单、支付或流水跟着发生，
        只是把到期时间挪一下（会写一条 admin_adjust 流水，那是事后唯一的痕迹）。

        传的是**绝对时刻**，不是「延长 N 天」——所以框里填的是「新的到期时间」而不是「加多久」，
        当前值写在上面那行说明里让人对着改。这里**不回填**日期控件：它的值是 dayjs 对象，
        而这个仓库不直接依赖 dayjs（见 services/datetime.ts 那段），从接口那串 RFC3339 变回去
        要靠引一个新包，而收益只是省一次手动选。
      */}
      <ModalForm<{ expireAt?: unknown; reason: string; remark?: string }>
        key={`expire-${membership?.id ?? 'none'}`}
        title="调整到期时间"
        open={expireOpen}
        onOpenChange={setExpireOpen}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        onFinish={async (values) => {
          if (!membership) return false;
          const expireAt = toRFC3339(values.expireAt);
          if (!expireAt) {
            message.error('请选择新的到期时间');
            return false;
          }
          try {
            await adjustMembershipExpiry(membership.id, {
              expireAt,
              reason: values.reason.trim(),
              // 备注是可选的，空串就整个不传——后端把它当「没有」处理，传空串进去只会
              // 在流水里留一个看起来像有别的话没说的空格。
              remark: values.remark?.trim() || undefined,
            });
          } catch (err) {
            // 新的到期时间早于开通时间、或者状态已经是 revoked，都从这里回来（409）。
            message.error(requestErrorMessage(err, '调整有效期失败，请稍后重试'));
            return false;
          }
          message.success('已调整到期时间');
          await load();
          return true;
        }}
      >
        <p>
          这会直接改写这个会员的到期时间，不产生订单、不产生支付，除了流水之外没有任何凭证。
          当前到期时间：{membership ? formatDateTime(membership.expireAt) : '—'}。
        </p>
        <ProFormDateTimePicker
          name="expireAt"
          label="新的到期时间"
          width="md"
          fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
          rules={[{ required: true, message: '请选择新的到期时间' }]}
        />
        <ProFormTextArea
          name="reason"
          label="原因"
          fieldProps={{ rows: 3, maxLength: 200, showCount: true, placeholder: '为什么调整（必填）' }}
          rules={[{ required: true, message: '请填原因' }]}
        />
        <ProFormTextArea
          name="remark"
          label="备注"
          fieldProps={{ rows: 2, maxLength: 500, showCount: true, placeholder: '补充说明（可选）' }}
        />
      </ModalForm>
    </PageContainer>
  );
}
