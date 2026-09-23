import {
  ModalForm,
  ProFormDateTimePicker,
  ProFormDigit,
  ProFormText,
  ProFormTextArea,
} from '@ant-design/pro-components';
import { Alert, message } from 'antd';
import { toRFC3339 } from '../../services/datetime';
import {
  issuePartnerKey,
  updatePartnerKey,
  type APIKeyInput,
  type APIKeyIssued,
  type PartnerAPIKey,
} from '../../services/partner';
import { requestErrorMessage } from '../../services/requestError';
import { DEFAULT_RATE_LIMIT_PER_MINUTE, formatIPWhitelist, parseIPWhitelist } from './keyAccess';
import { scrollableModalBody } from '../../components/common/modalProps';

/**
 * 签发 / 修改一把密钥。两条路共用一张表单，因为**可填的格子完全相同**（后端也是同一个
 * dto.APIKeyInput），差别只有提交去哪个接口、以及签发那条路会回明文。
 *
 * # 为什么一张表单而不是两张
 *
 * 密钥身上可改的东西只有四样：备注名、有效期、IP 白名单、每分钟额度。多写一个只差标题的
 * 弹窗，换来的是一处「新增时校验过了、修改时忘了」——而白名单写坏的表现是这家合作方从
 * 下一个请求起全部 401（中间件解析失败即整条作废）。所以校验与换算都只写一份。
 *
 * # 为什么没有 secret 这一格
 *
 * 签名密钥不由调用方提供（服务端生成），改不了也填不了。要换一把只有一条路：停用旧的、
 * 签发新的。这一格不是「省略了」，是**不存在**。
 *
 * # 签发的明文不经过这个组件
 *
 * 它只负责把响应交给 onIssued，由父组件决定怎么显示（见 IssuedKeyModal.tsx 上那段）——
 * 这个组件的生命周期比那一次回显长得多（它会一直挂到用户关掉抽屉），明文不能住在这里。
 */

/**
 * 表单形状。`ipWhitelist` 在这里是**文本**（一行一条），提交时才换算成数组；
 * `expiresAt` 的形态取决于它怎么来的（刚选的 dayjs 还是接口带回的字符串），
 * 一并交给 toRFC3339 统一（见 services/datetime.ts）。
 */
type KeyForm = {
  name?: string;
  expiresAt?: string | null;
  ipWhitelist?: string;
  rateLimitPerMinute?: number;
};

type Props = {
  open: boolean;
  partnerId: string;
  /** undefined = 签发新密钥。 */
  editing?: PartnerAPIKey;
  onOpenChange: (open: boolean) => void;
  /** 只在签发那条路上调，把带明文的响应交给一次性弹窗。 */
  onIssued: (issued: APIKeyIssued) => void;
  /** 改名 / 改白名单 / 改额度之后刷新密钥列表。 */
  onSaved: () => void;
};

export default function KeyFormModal({
  open,
  partnerId,
  editing,
  onOpenChange,
  onIssued,
  onSaved,
}: Props) {
  return (
    <ModalForm<KeyForm>
      title={editing ? '修改密钥' : '签发新密钥'}
      open={open}
      onOpenChange={onOpenChange}
      // destroyOnClose + 调用处的 key：缺了它们，改完 A 再点 B，A 的值会留在复用同一个 Form
      // 实例的表单里（仓库里 10 个弹窗页都踩过这个坑），而这里的后果具体是「把上一把的 IP
      // 白名单写到下一把上」——一条能直接把对方打出去的改动。
      modalProps={{ ...scrollableModalBody, destroyOnClose: true, width: 620 }}
      initialValues={
        editing
          ? {
              name: editing.name,
              expiresAt: editing.expiresAt,
              ipWhitelist: formatIPWhitelist(editing.ipWhitelist),
              rateLimitPerMinute: editing.rateLimitPerMinute,
            }
          : // 签发时**不给额度默认值**：那一格留空就是「用后端的默认额度」，替用户填一个
            // 60 会让页面上看不出「这一格他到底动没动过」（见 keyAccess.ts 的说明）。
            undefined
      }
      onFinish={async (values) => {
        const payload: APIKeyInput = {
          // 备注名可以为空（库里那一列允许空串）：它是给人看的备注，不是判据。后端只 trim
          // 两端，不 trim 中间的空格。
          name: (values.name ?? '').trim(),
          // 清空日期 = 不过期。空串与 null 在后端是同一个意思（都解析成 nil），传 null 少
          // 一次「空串算不算没填」的猜测。
          expiresAt: toRFC3339(values.expiresAt) ?? null,
          ipWhitelist: parseIPWhitelist(values.ipWhitelist),
          // 留空 = 0 = 后端用默认额度。负数**原样发出去**：后端把它当成校验错误回一句中文，
          // 在这里拦下来的话那句话就永远不会出现，而两边的规则迟早会分家。
          rateLimitPerMinute: Number(values.rateLimitPerMinute) || 0,
        };
        try {
          if (editing) {
            await updatePartnerKey(partnerId, editing.id, payload);
            message.success('已保存');
            onSaved();
          } else {
            const issued = await issuePartnerKey(partnerId, payload);
            // 注意这里**没有 message.success**：成功那条路的话由那个一次性弹窗说，两条
            // 提示会同时出现，而其中一条会被立刻读成「已经存好了」。
            onIssued(issued);
          }
        } catch (error) {
          message.error(
            requestErrorMessage(error, editing ? '保存失败，请稍后重试' : '签发失败，请稍后重试'),
          );
          return false;
        }
        return true;
      }}
    >
      {!editing && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="明文密钥只在签发之后显示一次"
          description="签名密钥由服务端生成，签完那一刻显示一次，之后列表里只有掩码。请当场复制给对接方；丢了只能停用这一把、再签一把。"
        />
      )}
      <ProFormText
        name="name"
        label="备注名"
        placeholder="可留空，如「生产环境 主用」"
        fieldProps={{ maxLength: 100 }}
        tooltip="给下一个人看的：这一把是给谁、做什么用的。它不是判据，可以留空。"
      />
      <ProFormDateTimePicker
        name="expiresAt"
        label="有效期"
        width="md"
        fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
        tooltip="留空 = 不过期。到期之后这把钥匙自动被拒（后端每请求现查），不用手动停用。"
      />
      <ProFormTextArea
        name="ipWhitelist"
        label="来源 IP 白名单"
        placeholder={'一行一条，可填 CIDR 或裸地址\n10.0.0.0/8\n192.168.1.1'}
        fieldProps={{ rows: 4 }}
        tooltip="留空 = 不限制来源（不是全拒）。填了之后不在名单里的请求一律被拒，所以从别处粘进来的空行要留意——空行会被丢掉，不会被当成一条规则。"
      />
      <ProFormDigit
        name="rateLimitPerMinute"
        label="每分钟调用额度"
        width="md"
        min={0}
        fieldProps={{ precision: 0 }}
        tooltip={`留空 = 用默认额度（${DEFAULT_RATE_LIMIT_PER_MINUTE} 次/分钟）。填 0 与留空是同一件事——后端把 0 读作「没填」；只有负数才是错误。`}
      />
    </ModalForm>
  );
}
