/** 优先使用后端返回的 errorMessage，否则退回稳定的缺省文案。 */
export function requestErrorMessage(error: unknown, fallback: string): string {
  const response = (error as { response?: { data?: { errorMessage?: string } } } | null)?.response;
  return response?.data?.errorMessage || fallback;
}

/**
 * 取后端的错误码（信封里的 errorCode）。
 *
 * 与 requestErrorMessage 分工不同：那个取的是**给人看的话**，这个取的是**给代码判的判据**。
 * 需要它是因为有些错不是「失败了」而是「换个做法再来一次」——比如订单售后审核回的
 * FORTUNE_CARD_CONFIRMATION_REQUIRED：那不是请求写错了，是这一单有个必须由人确认的前提。
 * 靠 message 的措辞去猜这些分支，后端改一个字前端就悄悄失效。
 *
 * 取不到就返回 undefined，调用方按「不是这个码」处理。
 */
export function requestErrorCode(error: unknown): string | undefined {
  const response = (error as { response?: { data?: { errorCode?: string } } } | null)?.response;
  return response?.data?.errorCode || undefined;
}

export function roleSaveErrorMessage(error: unknown): string {
  const response = (error as { response?: { status?: number; data?: { errorMessage?: string } } } | null)?.response;
  if (response?.data?.errorMessage) return response.data.errorMessage;
  if (response?.status === 409) return '角色代码已存在或与已有授权冲突，请更换角色代码';
  return '保存失败，请稍后重试';
}

export function deletionErrorMessage(error: unknown): string {
  const response = (error as { response?: { status?: number; data?: { errorMessage?: string } } } | null)?.response;
  if (response?.status === 503) return '当前暂不支持删除，请稍后重试';
  return response?.data?.errorMessage || '删除失败，请稍后重试';
}
