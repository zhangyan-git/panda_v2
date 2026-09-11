import { request } from '@umijs/max';

/** 后端 `upload.Result`：url 是可直接渲染的绝对地址，key 是不含 host 的对象名。 */
export type UploadResult = {
  url: string;
  key: string;
  name: string;
  size: number;
  contentType: string;
  md5: string;
};

/**
 * 上传一张图片，返回它的绝对 URL。
 *
 * 走的是后端到 OSS：AccessKey 只在服务端，浏览器拿不到也不需要。这个函数是
 * `ImageUpload` 与 umi 之间的那一层——共享组件不认识 request，宿主把函数注入进去。
 *
 * request 会自动带上 Authorization 与 JSON 信封解包（见 `src/app.ts`），所以这里
 * 只关心 FormData 本身。**不要手写 Content-Type**：multipart 的 boundary 必须由
 * 浏览器加，手写会把 boundary 弄丢。
 */
export async function uploadImage(file: File): Promise<string> {
  const form = new FormData();
  form.append('file', file);
  const result = await request<UploadResult>('/api/v1/admin/uploads/images', {
    method: 'POST',
    data: form,
  });
  if (!result?.url) {
    throw new Error('上传成功但响应里没有图片地址');
  }
  return result.url;
}
