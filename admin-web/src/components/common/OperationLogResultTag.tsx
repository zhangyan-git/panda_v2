import { Tag } from 'antd';
import type { OperationLog } from '../../services/operationLog';

/**
 * 一条日志的结果标签：成功是「成功」，失败把错误原因挂出来。
 *
 * 单独抽出来是因为它现在有三个渲染处（日志页的表格列、详情抽屉、设备详情页的
 * 日志 tab），而失败时的取值逻辑不是一眼能看出的那种——`result` 不是 'success'
 * 就当作失败、原因取 errorMessage 退回「失败」两个字。抄三份的话，将来后端多一个
 * 「部分成功」之类的取值时改不全，界面上就会出现一种没人认识的状态。
 */
export default function OperationLogResultTag({ log }: { log: OperationLog }) {
  if (log.result === 'success') return <Tag color="success">成功</Tag>;
  // 一条只有「失败」的日志回答不了任何问题，所以原因必须露出来。
  return <Tag color="error">{log.errorMessage || '失败'}</Tag>;
}
