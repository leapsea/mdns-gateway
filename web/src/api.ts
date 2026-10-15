export interface Site { id: number; name: string; domain: string; targetHost: string; targetPort: number; listenPort: number; protocol: 'HTTP' | 'WEBSOCKET' | 'SSH'; enabled: boolean; configMode: 'GENERATED' | 'CUSTOM'; customConfig?: string }
export interface Status { version: string; database: string; mdns: string; openrestyActive: string; openrestyRuntime: string; openrestyApply: string; desiredVersion: number; activeRelease: string; configPending: boolean }
/** 账户信息只由已鉴权的 /auth/me 返回，公开浏览不加载它。 */
export interface Account { session: { userId: number; username: string; createdAt: string; lastActivity: string; expiresAt: string }; totpEnabled: boolean }

/** 所有请求统一解包；业务判断只使用 error.code。 */
export async function api<T>(path: string, method = 'GET', body?: unknown, token?: string): Promise<T> {
  const response = await fetch(`/api/v1${path}`, { method, headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: body === undefined ? undefined : JSON.stringify(body) })
  const result = await response.json() as { data: T; error: { code: string; message: string } | null }
  if (!response.ok || result.error) throw Object.assign(new Error(result.error?.message || '请求失败'), { code: result.error?.code || 'INTERNAL_ERROR' })
  return result.data
}
