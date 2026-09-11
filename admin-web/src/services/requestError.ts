/** 优先使用后端返回的 errorMessage，否则退回稳定的缺省文案。 */
export function requestErrorMessage(error: unknown, fallback: string): string {
  const response = (error as { response?: { data?: { errorMessage?: string } } } | null)?.response;
  return response?.data?.errorMessage || fallback;
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
