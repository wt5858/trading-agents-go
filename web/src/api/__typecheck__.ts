/**
 * 契约对账：把各 api 模块手写的返回类型和 swagger 推导出来的比一遍。
 *
 * 这个文件没有运行时代码，存在的唯一目的是让 tsc 在两者不一致时报错。
 * 加接口时在下面补一行。
 *
 * # 为什么需要它
 *
 * request<T> 是泛型的，手写 `Promise<SanitizedProvider[]>` 时 TypeScript 不会去核对
 * 后端到底返回什么。实际踩到过四次，全都编译通过、只在真实数据上才暴露：
 * providers 是分页信封按裸数组写、unread-count 的字段叫 unread 不叫 count、
 * watchlist items 返回 {group,items} 不是裸数组、公开模板同样是分页。
 *
 * # 为什么比的是键集而不是类型本身
 *
 * 严格相等会全线误报：手写的 Page<T> 是 `items: T[]`（必填），生成的是 `items?:`。
 * 这个差异是故意的——页面里不想每个字段都写 `?.`。比顶层键集刚好覆盖
 * 「数组 vs 对象」「字段名写错」「分页 vs 裸数组」，且不受可选性影响。
 */
import type {Res} from './typed'
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

/** T 不是 true 时这一行编译失败。求值成 never 不会报错，必须让它违反约束。 */
type AssertTrue<T extends true> = T

type KeysMatch<T, U> = [keyof T] extends [keyof U]
  ? [keyof U] extends [keyof T]
    ? true
    : false
  : false

type Returned<F> = F extends (...args: never[]) => Promise<infer R> ? R : never

// 每一项都得把 AssertTrue 直接写出来，不能抽成 Check<F,P,M> 那样的泛型别名——
// 那样 TS 会在别名定义处、类型参数还没解析时就检查约束，于是永远是红的。
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
  AssertTrue<
    KeysMatch<
      Returned<typeof screening.listPublicTemplates>,
      Res<'/screening/templates/public', 'get'>
    >
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
