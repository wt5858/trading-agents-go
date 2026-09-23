import { api, type Page } from './client'
import type { components } from '../types/api.generated'

type S = components['schemas']
export type JobView = S['scheduling.JobView']
export type ExecutionView = S['scheduling.ExecutionView']
export type CreateJobRequest = S['scheduling.CreateJobRequest']
export type UpdateJobRequest = S['scheduling.UpdateJobRequest']
export type CronPreviewView = S['scheduling.CronPreviewView']

export interface ListJobsParams {
  page?: number
  pageSize?: number
}

export function listJobs(params: ListJobsParams, signal?: AbortSignal): Promise<Page<JobView>> {
  return api.get<Page<JobView>>('/scheduling/jobs', { query: { ...params }, signal })
}

export function getJob(id: string, signal?: AbortSignal): Promise<JobView> {
  return api.get<JobView>(`/scheduling/jobs/${encodeURIComponent(id)}`, { signal })
}

export function createJob(body: CreateJobRequest): Promise<JobView> {
  return api.post<JobView>('/scheduling/jobs', body)
}

export function updateJob(id: string, body: UpdateJobRequest): Promise<JobView> {
  return api.put<JobView>(`/scheduling/jobs/${encodeURIComponent(id)}`, body)
}

export function deleteJob(id: string): Promise<unknown> {
  return api.del(`/scheduling/jobs/${encodeURIComponent(id)}`)
}

export function pauseJob(id: string): Promise<unknown> {
  return api.post(`/scheduling/jobs/${encodeURIComponent(id)}/pause`)
}

export function resumeJob(id: string): Promise<unknown> {
  return api.post(`/scheduling/jobs/${encodeURIComponent(id)}/resume`)
}

export function triggerJob(id: string): Promise<unknown> {
  return api.post(`/scheduling/jobs/${encodeURIComponent(id)}/trigger`)
}

export function listExecutions(
  id: string,
  params: { page?: number; pageSize?: number },
  signal?: AbortSignal,
): Promise<Page<ExecutionView>> {
  return api.get<Page<ExecutionView>>(
    `/scheduling/jobs/${encodeURIComponent(id)}/executions`,
    { query: { ...params }, signal },
  )
}

/**
 * 校验 cron 并预览接下来几次触发时刻。
 *
 * 表单里在提交前调一次：cron 写错在保存时只会得到一句「表达式非法」，
 * 而看到「接下来三次是什么时候」才能发现「写对了但不是我想要的」那一类错误
 * ——比如把「每周一 9 点」写成了「每天 9 点」。
 */
export function previewCron(cron: string, signal?: AbortSignal): Promise<CronPreviewView> {
  return api.post<CronPreviewView>('/scheduling/jobs/preview-cron', { cron }, { signal })
}
