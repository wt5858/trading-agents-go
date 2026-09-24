import type {components} from './api.generated'

// 生成的类型键名带包前缀（'analysis.TaskView'），页面里写那一长串既难读又难改，
// 所以在这里收一层别名。本文件手写，api.generated.ts 才是生成物。
type S = components['schemas']

// 身份
export type UserView = S['identity.UserView']
export type MeView = S['identity.MeView']
export type TokenResponse = S['identity.TokenResponse']
export type LoginRequest = S['identity.LoginRequest']

// 股票
export type StockView = S['stock.StockView']
export type QuoteView = S['stock.QuoteView']
export type KlineView = S['stock.KlineView']
export type NewsView = S['stock.NewsView']
export type FinancialView = S['stock.FinancialView']

// 分析
export type TaskView = S['analysis.TaskView']
export type ProgressView = S['analysis.ProgressView']
export type ResultView = S['analysis.ResultView']
export type DecisionChainView = S['analysis.DecisionChainView']
export type ChainLinkView = S['analysis.ChainLinkView']
export type SubmitRequest = S['analysis.SubmitRequest']
export type Step = S['value_objects.Step']

// 报告
export type ReportView = S['report.ReportView']
export type ReportSummaryView = S['report.ReportSummaryView']
export type SectionView = S['report.SectionView']
