import {api} from './client'
import type {components} from '../types/api.generated'

export type CrewProfile = components['schemas']['domain_services.CrewProfile']
export type CrewView = components['schemas']['agent.CrewView']

export function getCrew(signal?: AbortSignal): Promise<CrewView> {
  return api.get<CrewView>('/agents/crew', { signal })
}

const ANALYST_STEP_PREFIX = 'analyst:'

/**
 * 提交分析时可勾选的分析师。阵容接口返回全部 14 个智能体，能选的只有那 6 个。
 *
 * 判据用 step 的 `analyst:` 前缀，不要用 layer——layer 的实际取值是「分析层」
 * 「研究层」这类中文展示串，按 'analyst' 匹配的结果是选项永远为空且不报错。
 */
export function toAnalystOptions(crew: CrewView | null): { value: string; label: string }[] {
  return (crew?.members ?? [])
    .filter((m) => m.kind && m.step?.startsWith(ANALYST_STEP_PREFIX))
    .map((m) => ({ value: m.kind!, label: m.displayName || m.kind! }))
}
