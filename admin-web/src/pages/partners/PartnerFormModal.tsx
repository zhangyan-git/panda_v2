import {
  ModalForm,
  ProFormDateTimePicker,
  ProFormText,
  ProFormTextArea,
} from '@ant-design/pro-components';
import { message } from 'antd';
import { toRFC3339 } from '../../services/datetime';
import {
  createPartner,
  updatePartner,
  type Partner,
  type PartnerInput,
} from '../../services/partner';
import { requestErrorMessage } from '../../services/requestError';
import { scrollableModalBody } from '../../components/common/modalProps';

/**
 * 新增 / 编辑一家合作方。
 *
 * # 为什么 status 不在这张表单里
 *
 * 启停走自己的 PATCH（列表那一列的操作）。**这不是洁癖**：停用一家合作方会让它名下所有
 * 密钥当场失效，那件事不该与「顺手改个联系电话」共用一次提交，也不该被一次保存顺手带过去
 * （后端为此把 status 挡在请求体之外，这里也就不可能填得进去）。
 *
 * # 为什么这些格子必须填全
 *
 * 后端 PUT 是**整份覆盖**：没带的字段就是清空。所以表单要么把每一格都提交（哪怕是用户没
 * 动过的），要么就会静默清掉它——这个组件走的是 ProForm 默认的全字段提交，改这一条时先
 * 想一遍「清掉谁」。
 *
 * # 编码只在新增时出现
 *
 * 编码是合作方的稳定标识（接口签名、对账、日志里都用它），建好之后不可修改——它没有挂在
 * 表单上，编辑时把原编码一起发回去只是为了满足后端那句「改的是不是同一行」的核对。
 */
type PartnerForm = {
  code: string;
  name: string;
  contactName?: string;
  contactPhone?: string;
  contactEmail?: string;
  description?: string;
  expiresAt?: string | null;
};

type Props = {
  open: boolean;
  /** undefined = 新增。 */
  editing?: Partner;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
};

export default function PartnerFormModal({ open, editing, onOpenChange, onSaved }: Props) {
  return (
    <ModalForm<PartnerForm>
      title={editing ? '编辑合作方' : '新增合作方'}
      open={open}
      onOpenChange={onOpenChange}
      // destroyOnClose + 调用处的 key：缺了它们，编完 A 再点 B 会把 A 的值带进 B 的表单里
      // （仓库里 10 个弹窗页都为此修过）。这里的后果具体是「把 A 的联系人电话写到 B 上」。
      modalProps={{ ...scrollableModalBody, destroyOnClose: true, width: 640 }}
      initialValues={
        editing
          ? {
              code: editing.code,
              name: editing.name,
              contactName: editing.contactName,
              contactPhone: editing.contactPhone,
              contactEmail: editing.contactEmail,
              description: editing.description,
              expiresAt: editing.expiresAt,
            }
          : undefined
      }
      onFinish={async (values) => {
        const payload: PartnerInput = {
          // 编辑时用**行上那个编码**而不是表单里的（编辑态那一格根本不渲染，值是空串）：
          // 编码不可修改，后端会拿它核对「你要改的是不是同一行」，对不上回 400。
          code: editing ? editing.code : values.code.trim(),
          name: values.name.trim(),
          contactName: (values.contactName ?? '').trim(),
          contactPhone: (values.contactPhone ?? '').trim(),
          contactEmail: (values.contactEmail ?? '').trim(),
          description: (values.description ?? '').trim(),
          // 清空日期 = 不过期（null 与空串在后端是同一个意思）。PUT 的覆盖语义下，这是一次
          // 「改成永久有效」，不是「这次不改这一项」。
          expiresAt: toRFC3339(values.expiresAt) ?? null,
        };
        try {
          if (editing) {
            await updatePartner(editing.id, payload);
          } else {
            await createPartner(payload);
          }
        } catch (error) {
          // 409 是编码撞车、400 是「编码不可修改」或某一格不合法、404 是这一行已经不在了。
          // 三句中文后端都写好了，这里原样显示，不再包一层自己的话。
          message.error(
            requestErrorMessage(error, editing ? '保存失败，请稍后重试' : '新增失败，请稍后重试'),
          );
          return false;
        }
        message.success(editing ? '已保存' : '已新增');
        onSaved();
        return true;
      }}
    >
      {!editing && (
        <ProFormText
          name="code"
          label="编码"
          rules={[
            { required: true, message: '请填编码' },
            {
              // 与 partner-service 的 service.ErrCodeInvalid 同一条规则（那边是
              // `^[a-z0-9][a-z0-9_]{1,63}$`）：在这里挡一次只是为了不让一次必输的提交
              // 白跑一趟，真正的判据仍在服务端。
              pattern: /^[a-z0-9][a-z0-9_]{1,63}$/,
              message: '只能用小写字母、数字与下划线，且以字母或数字开头，2-64 位',
            },
          ]}
          fieldProps={{ maxLength: 64 }}
          tooltip="建成后不可修改：对接方的签名头、对账与调用日志里都用它。一般用小写拼音或英文，如 fengxuan、shouchuang_01。"
        />
      )}
      <ProFormText
        name="name"
        label="名称"
        rules={[{ required: true, message: '请填名称' }]}
        fieldProps={{ maxLength: 100 }}
      />
      <ProFormText
        name="contactName"
        label="联系人"
        placeholder="可留空"
        fieldProps={{ maxLength: 50 }}
      />
      <ProFormText
        name="contactPhone"
        label="联系电话"
        placeholder="可留空"
        fieldProps={{ maxLength: 30 }}
      />
      <ProFormText
        name="contactEmail"
        label="联系邮箱"
        placeholder="可留空"
        fieldProps={{ maxLength: 100 }}
      />
      <ProFormDateTimePicker
        name="expiresAt"
        label="有效期"
        width="md"
        fieldProps={{ format: 'YYYY-MM-DD HH:mm:ss' }}
        tooltip="留空 = 长期有效。到期之后这家合作方的所有调用都会被拒（后端每请求现查），不用手动停用。"
      />
      <ProFormTextArea
        name="description"
        label="说明"
        placeholder="可留空：这家合作方是谁、接了哪几个接口"
        fieldProps={{ rows: 3, maxLength: 500 }}
      />
    </ModalForm>
  );
}
