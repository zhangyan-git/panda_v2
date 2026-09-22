import { CopyOutlined } from '@ant-design/icons';
import { Alert, Button, Input, Modal, Space, Typography, message } from 'antd';
import type { APIKeyIssued } from '../../services/partner';

/**
 * 签发结果：**全仓库唯一一次显示明文密钥的地方**。
 *
 * # 为什么它是独立一个组件、而且是「用完即弃」的
 *
 * 明文（issued.secret）是 props，而这个组件**只在签发成功的那一瞬间被条件挂载**——父组件
 * 写的是 `{issued && <IssuedKeyModal issued={issued} onClose={...} />}`，关掉它就把父组件那
 * 一个 state 槽清成 null，组件卸载、明文随之从内存里消失。
 *
 * 不能改成「塞进密钥列表那份 state 里，再拿一个 visible 开关控制弹窗」：那样明文会活得比
 * 弹窗久——它跟着列表一起被下一次 load() 重新渲染、被别处的任何一次提交带上，于是在「关掉
 * 了」之后它其实还在。那等于回显两次：用户以为过期了，其实随时能再看到。**这不是洁癖**：
 * 后端把「只回显一次」写成了契约（dto.APIKeyIssued 与 controller 的 issueKey 上都钉着），
 * 前端把它留在内存里就单方面破坏了那份契约，而页面上看不出来。
 *
 * # 为什么关闭要是一次明确的动作
 *
 * maskClosable / keyboard 都关掉、底部只有一个「我已保存」：点遮罩、按 ESC 都是误触，而
 * 误触的代价是这一把密钥作废（要停用再签一把）。这是一个不可撤销的「关掉就没了」。
 */
export default function IssuedKeyModal({
  issued,
  onClose,
}: {
  issued: APIKeyIssued;
  /** 关掉 = 清掉父组件那一个 state 槽，明文不再存在。 */
  onClose: () => void;
}) {
  const copy = async (value: string, label: string) => {
    try {
      await navigator.clipboard.writeText(value);
      message.success(`${label}已复制`);
    } catch {
      // 非安全上下文（http 的局域网地址）里 clipboard 不可用，这时只能手动选中复制——
      // 所以下面两个值都是**可选中的只读输入框**，而不是一段纯文本。
      message.error('复制失败，请手动选中这一格里的内容复制');
    }
  };

  /** 明文两个值各一格：值是只读输入框，右边一个按钮，两处行为完全一样。 */
  const field = (label: string, value: string, tooltip: string) => (
    <Space direction="vertical" size={4} style={{ width: '100%' }}>
      <Typography.Text strong>{label}</Typography.Text>
      <Space.Compact style={{ width: '100%' }}>
        <Input readOnly value={value} onFocus={(event) => event.target.select()} />
        <Button icon={<CopyOutlined />} onClick={() => void copy(value, label)}>
          复制
        </Button>
      </Space.Compact>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {tooltip}
      </Typography.Text>
    </Space>
  );

  return (
    <Modal
      title="密钥已签发 —— 请现在保存"
      open
      onCancel={onClose}
      // 误触遮罩或 ESC 都会把这把密钥关掉，而关掉就再也拿不到了，所以这两个出口都封死，
      // 只留底部那个按钮。
      maskClosable={false}
      keyboard={false}
      width={680}
      footer={
        <Button type="primary" danger onClick={onClose}>
          我已保存，关闭
        </Button>
      }
    >
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        message="关掉这个窗口就再也看不到明文，请现在保存"
        description={`签名密钥只在这一次签发响应里出现：库里存的是密文信封，之后无论刷新、看列表还是看详情，拿到的都只有掩码（${issued.secretMask}）。它丢了没有「再查一次」这条路，只能停用这一把、另签发一把。`}
      />
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        {field(
          'API Key（请求头 X-API-Key）',
          issued.apiKey,
          `公开标识，本来就在列表里能看到（${issued.apiKeyMask}）。这里给出来只是省得再去复制一次。`,
        )}
        {field(
          '签名密钥（Secret）',
          issued.secret,
          '用它算请求签名。只有这一刻能看到，请与 API Key 一起交给对接方并存在他自己的密钥管理里。',
        )}
      </Space>
    </Modal>
  );
}
