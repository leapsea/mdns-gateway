import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, type Account, type Site, type Status } from './api'

/** Session 只保存在前端内存；刷新页面回到访客浏览，编辑前需重新登录。 */
export const useGatewayStore = defineStore('gateway', () => {
  const token = ref('')
  const account = ref<Account | null>(null)
  const isAuthenticated = computed(() => !!token.value && !!account.value)
  const sites = ref<Site[]>([])
  const status = ref<Status | null>(null)
  const mdnsConflicts = ref<Record<string, boolean>>({})
  async function refresh() {
    // 访客与管理员使用同一份公开数据；并行刷新后一次性更新页面。
    const [list, health, mdns] = await Promise.all([
      api<Site[]>('/sites'),
      api<Status>('/system/status'),
      api<{ status: string; conflicts: Record<string, boolean> }>('/system/mdns'),
    ])
    sites.value = list || []
    status.value = health
    mdnsConflicts.value = mdns.conflicts
    const currentToken = token.value
    if (currentToken) {
      try {
        const currentAccount = await api<Account>('/auth/me', 'GET', undefined, currentToken)
        if (token.value === currentToken) account.value = currentAccount
      } catch (e) {
        // Session 过期后退回只读浏览，保留已加载的站点与状态。
        if (e instanceof Error && 'code' in e && e.code === 'AUTH_REQUIRED') {
          if (token.value === currentToken) signOut()
        } else throw e
      }
    }
  }
  async function signIn(sessionToken: string) {
    const currentAccount = await api<Account>('/auth/me', 'GET', undefined, sessionToken)
    account.value = currentAccount
    token.value = sessionToken
  }
  function signOut() { token.value = ''; account.value = null }
  return { token, account, isAuthenticated, sites, status, mdnsConflicts, refresh, signIn, signOut }
})
