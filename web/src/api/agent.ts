import { api } from './client'
import type { components } from '../types/api.generated'

export type CrewProfile = components['schemas']['domain_services.CrewProfile']
export type CrewView = components['schemas']['agent.CrewView']

export function getCrew(signal?: AbortSignal): Promise<CrewView> {
  return api.get<CrewView>('/agents/crew', { signal })
}

/**
 * 可选分析师列表。
 *
 * 阵容接口返回全部 14 个智能体，包含多空辩论、风控、交易员——提交分析时能勾选的
 * 只有分析师那 6 个，它们的 kind 正是 submitRequest.analysts 的取值
 * （market / fundamentals / news / sentiment / sector / index）。
 *
 * 判据用 step 的 `analyst:` 前缀，不要用 layer。layer 的实际取值是「分析层」
 * 「研究层」「风控层」「交易层」——一个给人看的中文展示串，随时可能因为
 * 措辞调整而变；step 则是 `analyst:market` 这种稳定的机器标识。
 * 这里最初按 layer === 'analyst' 写过，结果是选项永远为空且没有任何报错。
 */
const ANALYST_STEP_PREFIX = 'analyst:'

export function toAnalystOptions(crew: CrewView | null): { value: string; label: string }[] {
  return (crew?.members ?? [])
    .filter((m) => m.kind && m.step?.startsWith(ANALYST_STEP_PREFIX))
    .map((m) => ({ value: m.kind!, label: m.displayName || m.kind! }))
}
