import { api, type Page } from './client'
import type { FinancialView, KlineView, NewsView, QuoteView, StockView } from '../types/api'

export interface ListStocksParams {
  keyword?: string
  market?: string
  page?: number
  pageSize?: number
}

export function listStocks(
  params: ListStocksParams,
  signal?: AbortSignal,
): Promise<Page<StockView>> {
  return api.get<Page<StockView>>('/stocks', { query: { ...params }, signal })
}

export function getStock(code: string, signal?: AbortSignal): Promise<StockView> {
  return api.get<StockView>(`/stocks/${encodeURIComponent(code)}`, { signal })
}

export function getQuote(code: string, signal?: AbortSignal): Promise<QuoteView> {
  return api.get<QuoteView>(`/stocks/${encodeURIComponent(code)}/quote`, { signal })
}

export interface KlineParams {
  market?: string
  period?: 'daily' | 'weekly' | 'monthly'
  start?: string
  end?: string
  limit?: number
}

export function getKlines(
  code: string,
  params: KlineParams = {},
  signal?: AbortSignal,
): Promise<KlineView[]> {
  return api.get<KlineView[]>(`/stocks/${encodeURIComponent(code)}/klines`, {
    query: { ...params },
    signal,
  })
}

export function getNews(code: string, signal?: AbortSignal): Promise<NewsView[]> {
  return api.get<NewsView[]>(`/stocks/${encodeURIComponent(code)}/news`, { signal })
}

export function getFinancials(code: string, signal?: AbortSignal): Promise<FinancialView[]> {
  return api.get<FinancialView[]>(`/stocks/${encodeURIComponent(code)}/financials`, { signal })
}
