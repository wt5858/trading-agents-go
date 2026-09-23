import { api } from './client'
import type { components } from '../types/api.generated'

type S = components['schemas']
export type AccountView = S['paper.AccountView']
export type PortfolioView = S['paper.PortfolioView']
export type PositionView = S['paper.PositionView']
export type TradeView = S['paper.TradeView']
export type AccountListResult = S['paper.AccountListResult']
export type PortfolioResult = S['paper.PortfolioResult']
export type TradeHistoryResult = S['paper.TradeHistoryResult']
export type PlaceOrderRequest = S['paper.PlaceOrderRequest']

// 每个含业绩数字的响应都带 disclaimer 字段，这是后端刻意的设计
// （见 paper_trading_handler.go 顶部那段注释）：模拟盘的收益率截图和真实业绩
// 截图长得一模一样，声明只要不在响应体里，就一定会在某次改版或某张截图里消失。
//
// 因此前端必须渲染响应里的这个字段，而不是自己抄一份常量——抄一份的话，
// 后端改了措辞前端不会跟，而「任何消费者都拿不到不带声明的版本」这条保证就破了。
// 所有 paper 相关的返回类型都保留完整 Result 结构，不要在这一层把 disclaimer 剥掉。

export type OrderSide = 'buy' | 'sell'

export function listAccounts(signal?: AbortSignal): Promise<AccountListResult> {
  return api.get<AccountListResult>('/paper/accounts', { signal })
}

// 金额与数量在请求体里全部是字符串，不是数字。
//
// 后端用定点小数处理资金（internal/helpers/decimalx），而 JSON 的 number 是
// IEEE754 双精度——1000000.10 这种值在往返一次之后就可能变成 1000000.0999999999。
// 传字符串是刻意的，前端不要「顺手」转成 number。
export function openAccount(name: string, initialCash: string): Promise<unknown> {
  return api.post('/paper/accounts', { name, initialCash })
}

export function getPortfolio(id: string, signal?: AbortSignal): Promise<PortfolioResult> {
  return api.get<PortfolioResult>(`/paper/accounts/${encodeURIComponent(id)}/portfolio`, {
    signal,
  })
}

export function listTrades(
  id: string,
  params: { page?: number; pageSize?: number },
  signal?: AbortSignal,
): Promise<TradeHistoryResult> {
  // 注意：这个接口的分页字段是平铺的（items/total/page/pageSize 直接在 data 上），
  // 不是其余列表接口那种 response.PageData 结构，套 Page<T> 会拿到 undefined。
  return api.get<TradeHistoryResult>(`/paper/accounts/${encodeURIComponent(id)}/trades`, {
    query: { ...params },
    signal,
  })
}

export function placeOrder(id: string, body: PlaceOrderRequest): Promise<unknown> {
  return api.post(`/paper/accounts/${encodeURIComponent(id)}/orders`, body)
}

export function resetAccount(id: string): Promise<unknown> {
  return api.post(`/paper/accounts/${encodeURIComponent(id)}/reset`)
}
