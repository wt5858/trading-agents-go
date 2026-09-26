import {api} from './client'
import type {EvaluationListView} from '../types/api'

/**
 * 取最近的回测评估。
 *
 * 这条接口不要求登录：它是这个项目对外交代「这套东西准不准」的地方，
 * 藏在登录后面等于没有。
 */
export function listEvaluations(
  limit?: number,
  signal?: AbortSignal,
): Promise<EvaluationListView> {
  return api.get<EvaluationListView>('/agents/evaluations', {
    query: limit ? { limit } : undefined,
    signal,
  })
}
