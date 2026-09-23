import type { paths } from '../types/api.generated'

// 从生成的 paths 类型里把某个端点的响应体推导出来。
//
// # 为什么需要这个
//
// api 模块里的返回类型是手写的（`Promise<SanitizedProvider[]>`），而 request<T>
// 是泛型——手写成什么，TypeScript 就信什么。生成的类型在这里一点忙都帮不上，
// 于是「接口返回分页信封、前端按裸数组写」这种错会一路编译通过，
// 到运行时表现为一张永远空着的表格。
//
// 这一组工具把手写的返回类型换成从 swagger 推出来的，写错就是编译错误。
// 新增接口时优先用 Res<>，只有响应结构确实表达不出来时才手写。

type Json200<T> = T extends {
  responses: { 200: { content: { 'application/json': infer R } } }
}
  ? R
  : never

/** 剥掉统一信封，取 data 部分——调用方拿到的就是 request<T> 返回的东西。 */
type DataOf<E> = E extends { data?: infer D } ? D : never

/**
 * 某个端点成功响应里 data 的类型。
 *
 * 用法：`function listProviders(): Promise<Res<'/config/providers', 'get'>>`
 * 路径要写 swagger 里的模板形式（带 {id}），不是运行时拼好的那个。
 */
export type Res<P extends keyof paths, M extends keyof paths[P]> = DataOf<Json200<paths[P][M]>>
