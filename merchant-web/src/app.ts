import { fetchCurrentUser, type CurrentUser } from './services/user';

export async function getInitialState(): Promise<{ currentUser?: CurrentUser }> {
  try {
    const currentUser = await fetchCurrentUser();
    return { currentUser };
  } catch {
    return {};
  }
}
