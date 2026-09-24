import {api, type Page} from './client'
import type {components} from '../types/api.generated'

type S = components['schemas']
export type FieldDictView = S['screening.FieldDictView']
export type FieldView = S['screening.FieldView']
export type OperatorView = S['screening.OperatorView']
export type ResultSetView = S['screening.ResultSetView']
export type TemplateView = S['screening.TemplateView']
export type RunRequest = S['screening.RunRequest']
export type TemplateRequest = S['screening.TemplateRequest']

/** 可筛选字段字典。字段与算子都由后端定义，前端不要硬编码任何一个。 */
export function getFields(signal?: AbortSignal): Promise<FieldDictView> {
  return api.get<FieldDictView>('/screening/fields', { signal })
}

export function runScreening(body: RunRequest, signal?: AbortSignal): Promise<ResultSetView> {
  return api.post<ResultSetView>('/screening/run', body, { signal })
}

export function listTemplates(signal?: AbortSignal): Promise<TemplateView[]> {
  return api.get<TemplateView[]>('/screening/templates', { signal })
}

// 注意：公开模板是**分页**的，而同上下文的 /screening/templates 返回裸数组。
export function listPublicTemplates(
  params: { page?: number; pageSize?: number } = {},
  signal?: AbortSignal,
): Promise<Page<TemplateView>> {
  return api.get<Page<TemplateView>>('/screening/templates/public', {
    query: { ...params },
    signal,
  })
}

export function createTemplate(body: TemplateRequest): Promise<TemplateView> {
  return api.post<TemplateView>('/screening/templates', body)
}

// 模板 ID 是整数（自增主键），不是字符串。
export function updateTemplate(id: number, body: TemplateRequest): Promise<TemplateView> {
  return api.put<TemplateView>(`/screening/templates/${id}`, body)
}

export function deleteTemplate(id: number): Promise<unknown> {
  return api.del(`/screening/templates/${id}`)
}

export function runTemplate(id: number, signal?: AbortSignal): Promise<ResultSetView> {
  return api.post<ResultSetView>(`/screening/templates/${id}/run`, undefined, { signal })
}
