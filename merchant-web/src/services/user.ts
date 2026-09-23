import { request } from '@umijs/max';

export type CurrentUser = {
  id: string;
  username: string;
  name: string;
  email: string;
  merchantId: string;
  merchantName: string;
  /**
   * 数据范围：merchant（全部门店）/ brand / store。
   *
   * 这三列回显的就是**列表接口真正用来过滤的那个边界**，不是另算的一份描述——服务端在
   * 同一次请求里现取现算，所以界面上写着「门店「XX店」」时，列表拿到的必然也是那一家店。
   */
  scopeType: string;
  /** 范围目标：品牌档是品牌 id，门店档是门店 id，可以多个；商户档为空数组。 */
  scopeIds: string[];
  /** 范围目标的名称，与 scopeIds 同序等长。商户档为空数组（那是界面文案），目标被删除时该位置为空串。 */
  scopeNames: string[];
};

/** 获取当前登录商户用户信息（含所属商户与数据范围） */
export async function fetchCurrentUser(): Promise<CurrentUser> {
  return request<CurrentUser>('/api/v1/merchant/users/me');
}

/** 登录 */
export async function login(params: { username: string; password: string }) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/merchant/auth/login',
    {
      method: 'POST',
      data: params,
    },
  );
}

/**
 * 刷新 token。
 *
 * **这个函数没有任何调用点，服务端也不存在这条路由**：MerchantAuthService 只有 Login 与
 * Me，`/v1/merchant/auth/refresh` 从来没有实现过。留着它只有一个作用——将来真要做续期时
 * 有个明确的名字可以接。现在**不要**把它接到响应拦截器上：那会让一次 401 变成对一条不存
 * 在的路径的请求，失败之后仍然要登出，只是多绕一圈。
 *
 * 也就是说：access token 过期（24 小时）就等于要重新登录。这是已知缺口，不是遗漏。
 */
export async function refreshToken(token: string) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/merchant/auth/refresh',
    {
      method: 'POST',
      data: { refreshToken: token },
    },
  );
}
