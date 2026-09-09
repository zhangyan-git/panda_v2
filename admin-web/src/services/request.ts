import { request } from '@umijs/max';
import type { CurrentUser } from './user';

export async function fetchCurrentUser(): Promise<CurrentUser> {
  return request<CurrentUser>('/v1/admin/users/me');
}
