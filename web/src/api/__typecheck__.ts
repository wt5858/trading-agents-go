/**
 * 契约对账：把各 api 模块手写的返回类型和 swagger 推导出来的比一遍。
 *
 * 这个文件没有运行时代码，存在的唯一目的是让 `tsc` 在两者不一致时报错。
 * 它不被任何模块 import，但 tsconfig 的 include 覆盖了 src/，照样参与类型检查
 * （Vite 构建时因为没有副作用会被摇掉，不进产物）。
 *
 * # 为什么需要它
 *
 * request<T> 是泛型的，api 模块里手写 `Promise<SanitizedProvider[]>` 时
 * TypeScript 不会去核对后端到底返回什么。实际踩到过三次，全都编译通过：
 *
 *   - /config/providers 是分页信封，按裸数组写 → 表格永远空着
 *   - /notifications/unread-count 的字段叫 unread 不叫 count → 徽标永远是 0
 *   - /watchlist/groups/{id}/items 返回 {group, items} 不是裸数组 → 判空永远不成立
 *
 * # 为什么比的是键集而不是类型本身
 *
 * 严格相等在这里会全线误报：手写的 Page<T> 是 `items: T[]`（必填），
 * 而生成的类型是 `items?: T[]`（可选，swag 不标 required）。这个差异是故意的——
 * 页面里不想每个字段都写 `?.`。所以比的是**顶层键集**：
 * 数组 vs 对象、字段名写错、分页信封 vs 裸数组，这三类都会被抓住，
 * 而可选性差异不影响 keyof。
 *
 * 新增接口时在下面补一行。`make web-types` 之后这里报错，说明后端契约变了，
 * 要改的是 api 模块而不是这个文件。
 */
import type { Res } from './typed'
import type * as analysis from './analysis'
import type * as config from './config'
import type * as notification from './notification'
import type * as paper from './paper'
import type * as report from './report'
import type * as screening from './screening'
import type * as scheduling from './scheduling'
import type * as stock from './stock'
import type * as user from './user'
import type * as watchlist from './watchlist'

/**
 * 断言载体：T 不是 true 时这一行编译失败。
 *
 * 不能写成「求值成 never 就算失败」——类型别名求值成 never 不会报错，
 * 它只是安静地变成 never。必须让不匹配的情况产生一个**违反约束**的类型，
 * 约束检查才会真的失败。
 */
type AssertTrue<T extends true> = T

/** 顶层键集是否一致。不匹配时求值成 false（而不是 never），交给 AssertTrue 报错。 */
type KeysMatch<T, U> = [keyof T] extends [keyof U]
  ? [keyof U] extends [keyof T]
    ? true
    : false
  : false

type Returned<F> = F extends (...args: never[]) => Promise<infer R> ? R : never

// 每一项都必须把 AssertTrue 直接写出来，不能抽成 Check<F,P,M> 那样的泛型别名。
//
// 抽成泛型的话，TS 会在别名**定义处**、类型参数还没解析时就去检查约束，
// 证明不了就报错——于是这个检查永远是红的，和永远是绿的一样没用。
// 内联之后每一项都是具体类型，约束检查才发生在真正比对的那一刻。
export type ContractChecks = [
  AssertTrue<KeysMatch<Returned<typeof stock.listStocks>, Res<'/stocks', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof stock.getKlines>, Res<'/stocks/{code}/klines', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof stock.getNews>, Res<'/stocks/{code}/news', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof stock.getQuote>, Res<'/stocks/{code}/quote', 'get'>>>,

  AssertTrue<KeysMatch<Returned<typeof analysis.listTasks>, Res<'/analysis/tasks', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof analysis.getTask>, Res<'/analysis/tasks/{id}', 'get'>>>,
  AssertTrue<
    KeysMatch<
      Returned<typeof analysis.getDecisionChain>,
      Res<'/analysis/tasks/{id}/decision-chain', 'get'>
    >
  >,

  AssertTrue<KeysMatch<Returned<typeof report.listReports>, Res<'/reports', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof report.getReport>, Res<'/reports/{id}', 'get'>>>,

  AssertTrue<KeysMatch<Returned<typeof watchlist.listGroups>, Res<'/watchlist/groups', 'get'>>>,
  AssertTrue<
    KeysMatch<Returned<typeof watchlist.listItems>, Res<'/watchlist/groups/{id}/items', 'get'>>
  >,

  AssertTrue<KeysMatch<Returned<typeof scheduling.listJobs>, Res<'/scheduling/jobs', 'get'>>>,
  AssertTrue<
    KeysMatch<
      Returned<typeof scheduling.listExecutions>,
      Res<'/scheduling/jobs/{id}/executions', 'get'>
    >
  >,

  AssertTrue<KeysMatch<Returned<typeof screening.getFields>, Res<'/screening/fields', 'get'>>>,
  AssertTrue<
    KeysMatch<Returned<typeof screening.listTemplates>, Res<'/screening/templates', 'get'>>
  >,
  AssertTrue<KeysMatch<Returned<typeof screening.runScreening>, Res<'/screening/run', 'post'>>>,

  AssertTrue<KeysMatch<Returned<typeof paper.listAccounts>, Res<'/paper/accounts', 'get'>>>,
  AssertTrue<
    KeysMatch<Returned<typeof paper.getPortfolio>, Res<'/paper/accounts/{id}/portfolio', 'get'>>
  >,
  AssertTrue<
    KeysMatch<Returned<typeof paper.listTrades>, Res<'/paper/accounts/{id}/trades', 'get'>>
  >,

  AssertTrue<
    KeysMatch<Returned<typeof notification.listNotifications>, Res<'/notifications', 'get'>>
  >,
  AssertTrue<
    KeysMatch<Returned<typeof notification.unreadCount>, Res<'/notifications/unread-count', 'get'>>
  >,

  AssertTrue<KeysMatch<Returned<typeof config.listProviders>, Res<'/config/providers', 'get'>>>,
  AssertTrue<KeysMatch<Returned<typeof config.listSettings>, Res<'/config/settings', 'get'>>>,

  AssertTrue<KeysMatch<Returned<typeof user.listUsers>, Res<'/users', 'get'>>>,
]
