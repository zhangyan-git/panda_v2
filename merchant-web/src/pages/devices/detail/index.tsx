import { PageContainer } from '@ant-design/pro-components';
import { history, useParams } from '@umijs/max';
import { Card, Descriptions, Empty, Tag } from 'antd';
import { useCallback, useEffect, useState } from 'react';
import { getDevice, type DeviceDetail } from '../../../services/coffeeMachine';
import { DEVICE_STATUS, QRCODE_TYPE, vendorOnlineMeta } from '../../../services/coffeeMachineLabels';
import { enumMeta } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import { requestErrorMessage } from '../../../services/session';
import { getStore } from '../../../services/store';

/** 空值统一显示「—」：留白看起来像加载失败，空串看起来像页面出错。 */
const dash = (value?: string | null) => (value?.trim() ? value : '—');

/**
 * 设备详情（只读）。
 *
 * 只有一屏，不挂 tab：后台那一屏的另外三个 tab（操作日志、饮品列表、账变记录）里，饮品
 * 商户端不做，另外两个是后台的运维视角。摆一排空 tab 只会让人以为「功能做了但这里没配数据」。
 *
 * 这里**没有**任何写入口。有一件事要记住：静态验证码在后台那份 DTO 里是给编辑表单用的
 * （留空即不改），这边只用来展示——运营在机器跟前核对那个码。
 */
const DeviceDetailPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const [device, setDevice] = useState<DeviceDetail>();
  const [storeName, setStoreName] = useState<string>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    try {
      const detail = await getDevice(id);
      setDevice(detail);
      setError(undefined);
      if (detail.storeId) {
        // 设备在范围内 ⇒ 它的门店也在范围内（过滤就是拿 store_id 去比授权点位集合的），
        // 所以这一次必定查得到。真查不到（门店刚被删掉）就退回显示 id，那不是错误。
        try {
          setStoreName((await getStore(detail.storeId)).name);
        } catch {
          setStoreName(detail.storeId);
        }
      } else {
        setStoreName(undefined);
      }
    } catch (err) {
      setError(requestErrorMessage(err, '加载设备失败'));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  const statusMeta = device ? enumMeta(DEVICE_STATUS, device.status) : undefined;
  const onlineMeta = device ? vendorOnlineMeta(device.vendorOnline) : undefined;
  const qrcodeMeta = device ? enumMeta(QRCODE_TYPE, device.qrcodeType) : undefined;

  return (
    <PageContainer
      loading={loading}
      title={device?.deviceName || device?.serialUnique || '设备详情'}
      subTitle={device ? `机器编码 ${device.serialUnique}` : undefined}
      onBack={() => history.push('/devices')}
    >
      {error ? (
        <Card>
          <Empty description={error} />
        </Card>
      ) : (
        <Card>
          <Descriptions column={2} bordered size="middle">
            <Descriptions.Item label="设备名称">{dash(device?.deviceName)}</Descriptions.Item>
            <Descriptions.Item label="机器编码">{dash(device?.serialUnique)}</Descriptions.Item>
            <Descriptions.Item label="所属门店">
              {device?.storeId ? (storeName ?? device.storeId) : '未分配门店'}
            </Descriptions.Item>
            <Descriptions.Item label="状态">
              {statusMeta ? <Tag color={statusMeta.color}>{statusMeta.text}</Tag> : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="厂商在线">
              {onlineMeta ? <Tag color={onlineMeta.color}>{onlineMeta.text}</Tag> : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="最近同步厂商">
              {dash(device?.lastSyncedAt)}
            </Descriptions.Item>
            <Descriptions.Item label="最近故障">
              {device?.lastFaultMessage
                ? `${device.lastFaultCode} ${device.lastFaultMessage}`
                : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="故障时间">{dash(device?.lastFaultAt)}</Descriptions.Item>
            <Descriptions.Item label="最近活跃">{dash(device?.lastActiveAt)}</Descriptions.Item>
            <Descriptions.Item label="静态验证码">
              {device ? dash(device.pickupPassword) : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="设备余额">
              {device ? `¥${formatYuan(device.coffeeBalance)}` : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="二维码类型">
              {qrcodeMeta ? <Tag color={qrcodeMeta.color}>{qrcodeMeta.text}</Tag> : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="普通码支付方式">
              {dash(device?.regularQrcodePaymentMethod)}
            </Descriptions.Item>
            <Descriptions.Item label="显示会员入口">
              {device ? (device.showVip ? '是' : '否') : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="启用券核销">
              {device ? (device.enableCouponVerification ? '是' : '否') : '—'}
            </Descriptions.Item>
            <Descriptions.Item label="保修截止">{dash(device?.warrantyEndAt)}</Descriptions.Item>
            <Descriptions.Item label="版本号">{dash(device?.versionNumber)}</Descriptions.Item>
            <Descriptions.Item label="Android 版本">
              {dash(device?.androidVersion)}
            </Descriptions.Item>
            <Descriptions.Item label="主板版本">{dash(device?.mainBoardVersion)}</Descriptions.Item>
            <Descriptions.Item label="创建时间">{dash(device?.createdAt)}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
    </PageContainer>
  );
};

export default DeviceDetailPage;
