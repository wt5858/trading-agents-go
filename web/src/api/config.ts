import { api, type Page } from './client'
import type { components } from '../types/api.generated'

type S = components['schemas']
export type SanitizedProvider = S['domain_services.SanitizedProvider']
export type SanitizedSetting = S['domain_services.SanitizedSetting']
export type ConfigSnapshot = S['domain_services.ConfigSnapshot']
export type ProbeResult = S['domain_services.ProbeResult']

// 注意：provider 的响应永远不含明文密钥，只有掩码（apiKey 字段形如 sk-…wxyz）
// 与 hasApiKey 布尔位。前端不要试图回显或缓存密钥，写入走单独的 /key 接口。

// 注意：providers 是**分页**的（response.PageData），而同一个上下文里的
// settings 返回的是裸数组。两者不一致是后端的现状，照 swagger 写，别想当然对齐。
export function listProviders(
  params: { page?: number; pageSize?: number } = {},
  signal?: AbortSignal,
): Promise<Page<SanitizedProvider>> {
  return api.get<Page<SanitizedProvider>>('/config/providers', { query: { ...params }, signal })
}

export function getProvider(id: number, signal?: AbortSignal): Promise<SanitizedProvider> {
  return api.get<SanitizedProvider>(`/config/providers/${id}`, { signal })
}

export function createProvider(body: unknown): Promise<SanitizedProvider> {
  return api.post<SanitizedProvider>('/config/providers', body)
}

export function updateProvider(id: number, body: unknown): Promise<SanitizedProvider> {
  return api.put<SanitizedProvider>(`/config/providers/${id}`, body)
}

export function deleteProvider(id: number): Promise<unknown> {
  return api.del(`/config/providers/${id}`)
}

export function enableProvider(id: number): Promise<unknown> {
  return api.post(`/config/providers/${id}/enable`)
}

export function disableProvider(id: number): Promise<unknown> {
  return api.post(`/config/providers/${id}/disable`)
}

/** 写入密钥。这是唯一会把明文送到后端的地方，写完不要在前端留存。 */
export function setProviderKey(id: number, apiKey: string): Promise<unknown> {
  return api.post(`/config/providers/${id}/key`, { apiKey })
}

export function testProvider(id: number): Promise<ProbeResult> {
  return api.post<ProbeResult>(`/config/providers/${id}/test`)
}

export function listSettings(signal?: AbortSignal): Promise<SanitizedSetting[]> {
  return api.get<SanitizedSetting[]>('/config/settings', { signal })
}

export function getSnapshot(signal?: AbortSignal): Promise<ConfigSnapshot> {
  return api.get<ConfigSnapshot>('/config/snapshot', { signal })
}

export function reloadConfig(): Promise<unknown> {
  return api.post('/config/reload')
}
