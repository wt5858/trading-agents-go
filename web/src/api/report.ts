import { api, type Page } from './client'
import type { ReportSummaryView, ReportView, SectionView } from '../types/api'

export interface ListReportsParams {
  page?: number
  pageSize?: number
}

export function listReports(
  params: ListReportsParams,
  signal?: AbortSignal,
): Promise<Page<ReportSummaryView>> {
  return api.get<Page<ReportSummaryView>>('/reports', { query: { ...params }, signal })
}

export function getReport(id: string, signal?: AbortSignal): Promise<ReportView> {
  return api.get<ReportView>(`/reports/${encodeURIComponent(id)}`, { signal })
}

export function getSection(
  id: string,
  key: string,
  signal?: AbortSignal,
): Promise<SectionView> {
  return api.get<SectionView>(
    `/reports/${encodeURIComponent(id)}/sections/${encodeURIComponent(key)}`,
    { signal },
  )
}

export function deleteReport(id: string): Promise<unknown> {
  return api.del(`/reports/${encodeURIComponent(id)}`)
}
