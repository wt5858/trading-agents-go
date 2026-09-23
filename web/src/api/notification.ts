import { api, type Page } from './client'
import type { components } from '../types/api.generated'

type S = components['schemas']
export type NotificationView = S['notification.NotificationView']

/** 已读状态筛选。取值见 notification_handler.go 的 Enums(unread, read)，留空表示不限。 */
export type ReadStatus = 'unread' | 'read'

export interface ListNotificationsParams {
  page?: number
  pageSize?: number
  status?: ReadStatus | ''
}

export function listNotifications(
  params: ListNotificationsParams,
  signal?: AbortSignal,
): Promise<Page<NotificationView>> {
  return api.get<Page<NotificationView>>('/notifications', { query: { ...params }, signal })
}

export type UnreadCountView = S['notification.UnreadCountView']

// 字段名是 unread，不是 count。写错不会有任何报错——徽标安静地永远显示 0。
export function unreadCount(signal?: AbortSignal): Promise<UnreadCountView> {
  return api.get<UnreadCountView>('/notifications/unread-count', { signal })
}

// 通知 ID 是整数，不是字符串——与分析任务（task_2026...）、报告（rpt_2026...）
// 那种带前缀的字符串 ID 不是一回事。这个上下文用的是自增主键。
export function markRead(id: number): Promise<unknown> {
  return api.post(`/notifications/${id}/read`)
}

export function markAllRead(): Promise<unknown> {
  return api.post('/notifications/read-all')
}

export function deleteNotification(id: number): Promise<unknown> {
  return api.del(`/notifications/${id}`)
}
