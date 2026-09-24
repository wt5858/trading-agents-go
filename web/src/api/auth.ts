import {api, request} from './client'
import {clearTokens, setTokens} from './tokens'
import type {MeView, TokenResponse} from '../types/api'

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
    // skipRefresh：登出请求不该因为「续期失败」而根本发不出去。
    //
    // 走普通 request() 的话，它会先 ensureFreshAccessToken()，那一步失败就直接抛，
    // 后端那次会话吊销压根没发生——用户以为自己登出了，服务端会话还活着。
    // 手上这个访问令牌哪怕已经过期，发出去也无非是被后端拒绝，没有更坏的结果。
    await request('/auth/logout', { method: 'POST', skipRefresh: true })
  } finally {
    // 无论后端那次吊销成功与否，本地都要清干净。
    // 后端失败时保留令牌的话，用户会看到「已登出」却仍然能操作，这是更坏的状态。
    clearTokens()
  }
}

export function fetchMe(): Promise<MeView> {
  return api.get<MeView>('/auth/me')
}
