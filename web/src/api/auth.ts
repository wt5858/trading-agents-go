import { api, request } from './client'
import { clearTokens, setTokens } from './tokens'
import type { MeView, TokenResponse } from '../types/api'

export async function login(username: string, password: string): Promise<TokenResponse> {
  // anonymous：登录时手上还没有令牌，带 Authorization 头是没意义的；
  // 更要紧的是别让它走 401 重试那条路——登录失败本来就该是 401。
  const data = await request<TokenResponse>('/auth/login', {
    method: 'POST',
    body: { username, password },
    anonymous: true,
  })
  setTokens({
    accessToken: data.accessToken!,
    refreshToken: data.refreshToken!,
    expiresIn: data.expiresIn!,
  })
  return data
}

export async function logout(): Promise<void> {
  try {
    await api.post('/auth/logout')
  } finally {
    // 无论后端那次吊销成功与否，本地都要清干净。
    // 后端失败时保留令牌的话，用户会看到「已登出」却仍然能操作，这是更坏的状态。
    clearTokens()
  }
}

export function fetchMe(): Promise<MeView> {
  return api.get<MeView>('/auth/me')
}
