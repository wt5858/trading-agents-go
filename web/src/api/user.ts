import { api, type Page } from './client'
import type { UserView } from '../types/api'

export interface ListUsersParams {
  /** 用户名关键字，模糊匹配。 */
  keyword?: string
  page?: number
  pageSize?: number
}

export function listUsers(params: ListUsersParams, signal?: AbortSignal): Promise<Page<UserView>> {
  return api.get<Page<UserView>>('/users', { query: { ...params }, signal })
}

export function getUser(id: number, signal?: AbortSignal): Promise<UserView> {
  return api.get<UserView>(`/users/${id}`, { signal })
}

export function createUser(body: unknown): Promise<UserView> {
  return api.post<UserView>('/users', body)
}

export function deactivateUser(id: number): Promise<unknown> {
  return api.post(`/users/${id}/deactivate`)
}

export function resetUserPassword(id: number): Promise<unknown> {
  return api.post(`/users/${id}/password/reset`)
}

/** 改自己的密码。这是登录态下的操作，不是管理员重置。 */
export function changeOwnPassword(oldPassword: string, newPassword: string): Promise<unknown> {
  return api.post('/users/password', { oldPassword, newPassword })
}

export function updateOwnProfile(body: unknown): Promise<UserView> {
  return api.put<UserView>('/users/profile', body)
}
