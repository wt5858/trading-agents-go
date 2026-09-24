import type {paths} from '../types/api.generated'

type Json200<T> = T extends {
  responses: { 200: { content: { 'application/json': infer R } } }
}
  ? R
  : never

type DataOf<E> = E extends { data?: infer D } ? D : never

/**
 * 某个端点成功响应里 data 的类型，从 swagger 推导。
 *
 * 路径写 swagger 里的模板形式（带 {id}），不是运行时拼好的那个。
 * 用途见 __typecheck__.ts。
 */
export type Res<P extends keyof paths, M extends keyof paths[P]> = DataOf<Json200<paths[P][M]>>
