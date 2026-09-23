import { api } from './client'
import type { components } from '../types/api.generated'

type S = components['schemas']
export type GroupView = S['watchlist.GroupView']
export type ItemView = S['watchlist.ItemView']
export type GroupDetailView = S['watchlist.GroupDetailView']

export function listGroups(signal?: AbortSignal): Promise<GroupView[]> {
  return api.get<GroupView[]>('/watchlist/groups', { signal })
}

export function createGroup(name: string): Promise<GroupView> {
  return api.post<GroupView>('/watchlist/groups', { name })
}

export function renameGroup(id: number, name: string): Promise<unknown> {
  return api.put(`/watchlist/groups/${id}`, { name })
}

export function deleteGroup(id: number): Promise<unknown> {
  return api.del(`/watchlist/groups/${id}`)
}

/**
 * 取分组下的自选股。
 *
 * 返回的是 GroupDetailView（`{group, items}`），不是裸的 ItemView 数组——
 * 同一个上下文里 /watchlist/groups 返回的却是裸数组，两者不一致。
 * 写成数组不会有编译错误（request<T> 是泛型，手写什么就信什么），
 * 症状是表格拿到一个对象、判空永远不成立。
 */
export function listItems(groupId: number, signal?: AbortSignal): Promise<GroupDetailView> {
  return api.get<GroupDetailView>(`/watchlist/groups/${groupId}/items`, { signal })
}

export function addItem(groupId: number, code: string, note?: string): Promise<unknown> {
  return api.post(`/watchlist/groups/${groupId}/items`, { code, note })
}

export function removeItem(groupId: number, code: string): Promise<unknown> {
  return api.del(`/watchlist/groups/${groupId}/items/${encodeURIComponent(code)}`)
}

export function updateNote(groupId: number, code: string, note: string): Promise<unknown> {
  return api.put(`/watchlist/groups/${groupId}/items/${encodeURIComponent(code)}/note`, { note })
}

/** codes 是重排后的完整顺序，不是增量。后端按这个数组重写 sortOrder。 */
export function reorder(groupId: number, codes: string[]): Promise<unknown> {
  return api.post(`/watchlist/groups/${groupId}/items/reorder`, { codes })
}

export function moveItem(
  fromGroupId: number,
  toGroupId: number,
  code: string,
): Promise<unknown> {
  return api.post('/watchlist/items/move', { fromGroupId, toGroupId, code })
}
