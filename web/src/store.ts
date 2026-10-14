import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, type Site, type Status } from './api'

/** Session 只保存在前端内存，刷新页面也需要重新登录。 */
export const useGatewayStore = defineStore('gateway', () => {
  const token = ref('')
  const sites = ref<Site[]>([])
  const status = ref<Status | null>(null)
  const mdnsConflicts = ref<Record<string, boolean>>({})
  async function refresh() {
    sites.value = await api<Site[]>('/sites', 'GET', undefined, token.value) || []
    status.value = await api<Status>('/system/status', 'GET', undefined, token.value)
    const mdns = await api<{ status: string; conflicts: Record<string, boolean> }>('/system/mdns', 'GET', undefined, token.value)
    mdnsConflicts.value = mdns.conflicts
  }
  function signOut() { token.value = ''; sites.value = []; status.value = null; mdnsConflicts.value = {} }
  return { token, sites, status, mdnsConflicts, refresh, signOut }
})
