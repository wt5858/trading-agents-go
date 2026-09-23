// 业务码与 Go 侧 internal/helpers/response 里的常量一一对应。
//
// 后端的编码规则是「HTTP 状态码前缀 + 两位细分」，所以看到 404xx 就知道是一类
// 找不到。这里只列出后端已定义的那些；新增业务码要两边一起加，漏了这边的后果是
// 前端拿到一个不认识的码，走进 default 分支当未知错误处理——不会崩，但错误提示
// 会退化成后端原样返回的 message。
export const BizCode = {
  OK: 0,
  InvalidArgument: 40000,
  Unauthorized: 40100,
  Forbidden: 40300,
  NotFound: 40400,
  Conflict: 40900,
  AlreadyExists: 40901,
  QuotaExceeded: 42900,
  Internal: 50000,
  Unavailable: 50300,
} as const

export type BizCodeValue = (typeof BizCode)[keyof typeof BizCode]

// ApiError 是本层唯一往外抛的错误类型。
//
// 调用方不需要区分「HTTP 层挂了」和「业务层拒绝了」——两者都表现为一次失败的请求，
// 都有一句能给用户看的 message。真正需要分支处理的是 code，所以它是一等字段。
export class ApiError extends Error {
  readonly code: number
  readonly httpStatus: number
  /** 后端 X-Request-ID 响应头，排查时拿它去日志里搜。 */
  readonly requestId: string | null

  constructor(params: {
    code: number
    message: string
    httpStatus: number
    requestId?: string | null
  }) {
    super(params.message)
    this.name = 'ApiError'
    this.code = params.code
    this.httpStatus = params.httpStatus
    this.requestId = params.requestId ?? null
  }

  get isUnauthorized(): boolean {
    return this.code === BizCode.Unauthorized || this.httpStatus === 401
  }

  get isNotFound(): boolean {
    return this.code === BizCode.NotFound || this.httpStatus === 404
  }

  get isForbidden(): boolean {
    return this.code === BizCode.Forbidden || this.httpStatus === 403
  }

  /** 配额超限值得单独识别：提示语要引导用户去取消在跑的任务，而不是「稍后重试」。 */
  get isQuotaExceeded(): boolean {
    return this.code === BizCode.QuotaExceeded
  }
}

/** 网络层失败（断网、超时、CORS）也包成 ApiError，让调用方只处理一种错误。 */
export function networkError(cause: unknown): ApiError {
  const detail = cause instanceof Error ? cause.message : String(cause)
  return new ApiError({
    code: BizCode.Unavailable,
    message: `网络请求失败：${detail}`,
    httpStatus: 0,
  })
}
