<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import QRCode from 'qrcode'
import { api, type Site } from './api'
import { useGatewayStore } from './store'

/** 一个页面承载首次绑定、恢复和站点表单，状态转换由 API 返回驱动。 */
const store = useGatewayStore()
const phase = ref<'setup' | 'login' | 'recovery' | 'rebind' | 'dashboard'>('dashboard')
const username = ref('admin')
const code = ref('')
const rebindToken = ref('')
const setupId = ref('')
const qr = ref('')
const otpauthUrl = ref('')
const recoveryCodes = ref<string[]>([])
const busy = ref(false)
const error = ref('')
const selected = ref<Site | null>(null)
const editing = ref(false)
const form = ref<Site>(blank())
const customVisible = ref(false)
const generatedPreview = ref('')
/** 管理区仅切换现有站点与系统状态数据，认证流程仍由 phase 控制。 */
const dashboardView = ref<'sites' | 'system'>('sites')
/** 状态摘要与详情页使用同一组异常条件，避免正常提示掩盖发布失败。 */
const healthNeedsAttention = computed(() => {
  const status = store.status
  return !!status && (status.database !== 'OK' || status.mdns !== 'OK' || status.openrestyRuntime !== 'HEALTHY' || (!!status.openrestyApply && status.openrestyApply !== 'HEALTHY') || status.configPending)
})

function blank(): Site { return { id: 0, name: '', domain: '', targetHost: '', targetPort: 80, listenPort: 0, protocol: 'HTTP', enabled: true, configMode: 'GENERATED', customConfig: '' } }
function showError(e: unknown) {
  if (e instanceof Error && 'code' in e && e.code === 'AUTH_REQUIRED') {
    store.signOut()
    closeEditor()
    phase.value = 'dashboard'
    error.value = '登录已失效，请重新登录后编辑。'
  } else error.value = e instanceof Error && 'code' in e && e.code === 'PORT_IN_USE' ? '网关端口已被占用，请更换端口。' : e instanceof Error ? e.message : String(e)
}
async function run(action: () => Promise<void>) { busy.value = true; error.value = ''; try { await action() } catch (e) { showError(e) } finally { busy.value = false } }
async function setQR(uri: string) { otpauthUrl.value = uri; qr.value = await QRCode.toDataURL(uri, { margin: 2, width: 200 }) }
let statusTimer: ReturnType<typeof setInterval> | undefined
let previewTimer: ReturnType<typeof setTimeout> | undefined
onMounted(() => {
  run(() => store.refresh())
  statusTimer = setInterval(() => { store.refresh().catch(showError) }, 30000)
})
onUnmounted(() => { if (statusTimer) clearInterval(statusTimer); if (previewTimer) clearTimeout(previewTimer) })
watch(() => store.isAuthenticated, (authenticated) => { if (!authenticated) closeEditor() })

/** 首次设置和 CLI 重绑仍由服务器判断，仅在用户打开登录入口时显示。 */
function openLogin() { if (busy.value) return; run(async () => {
  const data = await api<{ needsSetup: boolean; needsRebind: boolean }>('/auth/setup-status')
  rebindToken.value = ''
  changePhase(data.needsSetup ? 'setup' : data.needsRebind ? 'rebind' : 'login')
}) }
function closeAuth() { if (!busy.value) { phase.value = 'dashboard'; setupId.value = ''; code.value = ''; rebindToken.value = ''; qr.value = ''; otpauthUrl.value = ''; error.value = '' } }
async function finishSignIn(token: string) {
  await store.signIn(token)
  phase.value = 'dashboard'
  setupId.value = ''; code.value = ''; rebindToken.value = ''; qr.value = ''; otpauthUrl.value = ''
  await store.refresh()
}

function startSetup() { run(async () => { const data = await api<{ setupId: string; otpauthUrl: string }>('/auth/setup', 'POST', { username: username.value }); setupId.value = data.setupId; await setQR(data.otpauthUrl) }) }
function verifySetup() { run(async () => { const data = await api<{ token: string; recoveryCodes: string[] }>('/auth/setup/verify', 'POST', { setupId: setupId.value, code: code.value }); recoveryCodes.value = data.recoveryCodes; await finishSignIn(data.token) }) }
function login() { run(async () => { const data = await api<{ token: string }>('/auth/login', 'POST', { username: username.value, code: code.value }); await finishSignIn(data.token) }) }
/** 恢复码验证成功后直接开始绑定；若下一请求失败，仍保留一次性令牌供重试。 */
function recover() { run(async () => {
  const data = await api<{ rebindToken: string }>('/auth/recovery', 'POST', { username: username.value, code: code.value })
  rebindToken.value = data.rebindToken
  changePhase('rebind')
  const binding = await api<{ setupId: string; otpauthUrl: string }>('/auth/2fa/setup', 'POST', { username: username.value, rebindToken: rebindToken.value })
  setupId.value = binding.setupId
  await setQR(binding.otpauthUrl)
}) }
function startRebind() { run(async () => { const data = await api<{ setupId: string; otpauthUrl: string }>('/auth/2fa/setup', 'POST', { username: username.value, rebindToken: rebindToken.value }); setupId.value = data.setupId; await setQR(data.otpauthUrl) }) }
function verifyRebind() { run(async () => { const data = await api<{ token: string; recoveryCodes: string[] }>('/auth/2fa/verify', 'POST', { setupId: setupId.value, code: code.value }); recoveryCodes.value = data.recoveryCodes; await finishSignIn(data.token) }) }
/** 所有认证阶段都由表单 submit 分发，回车与主按钮执行相同的动作。 */
function submitAuth() {
  if (busy.value) return
  if (phase.value === 'setup' || phase.value === 'rebind') {
    if (!setupId.value) {
      if (phase.value === 'setup') startSetup()
      else startRebind()
    } else if (/^\d{6}$/.test(code.value)) {
      if (phase.value === 'setup') verifySetup()
      else verifyRebind()
    }
  } else if (phase.value === 'login' && /^\d{6}$/.test(code.value)) {
    login()
  } else if (phase.value === 'recovery' && code.value) {
    recover()
  }
}
function changePhase(next: 'setup' | 'login' | 'recovery' | 'rebind') {
  phase.value = next
  setupId.value = ''
  code.value = ''
  qr.value = ''
  otpauthUrl.value = ''
  error.value = ''
  if (next !== 'rebind') rebindToken.value = ''
}
/** 公开列表不含自定义配置，编辑时通过鉴权详情接口读取完整站点。 */
function edit(site?: Site) {
  if (busy.value || !store.isAuthenticated) return
  run(async () => {
    const detail = site ? await api<Site>(`/sites/${site.id}`, 'GET', undefined, store.token) : null
    selected.value = detail
    form.value = detail ? { ...detail } : blank()
    customVisible.value = detail?.configMode === 'CUSTOM'
    generatedPreview.value = ''
    editing.value = true
  })
}
async function fetchPreview() {
  if (!store.isAuthenticated || !form.value.domain || !form.value.targetHost) return
  const data = await api<{ config: string }>('/sites/config/preview', 'POST', form.value, store.token)
  generatedPreview.value = data.config
}
function toggleAdvanced() {
  customVisible.value = !customVisible.value
  if (customVisible.value) run(fetchPreview)
}
watch(() => [form.value.domain, form.value.targetHost, form.value.targetPort, form.value.listenPort, form.value.protocol], () => {
  if (!customVisible.value || form.value.configMode !== 'GENERATED') return
  if (previewTimer) clearTimeout(previewTimer)
  previewTimer = setTimeout(() => { fetchPreview().catch(showError) }, 350)
})
watch(() => form.value.configMode, (mode) => { if (mode === 'CUSTOM' && !form.value.customConfig) form.value.customConfig = generatedPreview.value })
/** 只响应用户主动切换协议；打开另一站点时保留数据库中的原端口。 */
function onProtocolChange() {
  if (form.value.protocol === 'SSH') {
    form.value.configMode = 'GENERATED'
    customVisible.value = false
    if (!form.value.listenPort) form.value.listenPort = 2222
    if (form.value.targetPort === 80) form.value.targetPort = 22
  } else {
    form.value.listenPort = 0
    if (form.value.targetPort === 22) form.value.targetPort = 80
  }
}
function closeEditor() { editing.value = false; selected.value = null; form.value = blank() }
function requestCloseEditor() { if (!busy.value) closeEditor() }
function save() { if (busy.value || !store.isAuthenticated) return; run(async () => { const site = form.value; await api<Site>(site.id ? `/sites/${site.id}` : '/sites', site.id ? 'PUT' : 'POST', site, store.token); closeEditor(); await store.refresh() }) }
function remove(site: Site) { if (busy.value || !store.isAuthenticated || !confirm(`删除 ${site.domain}？`)) return; run(async () => { await api(`/sites/${site.id}`, 'DELETE', undefined, store.token); await store.refresh() }) }
function resetConfig() { if (!confirm('恢复默认配置后，当前自定义配置将不再使用。')) return; form.value.configMode = 'GENERATED'; form.value.customConfig = ''; customVisible.value = false }
function logout() { if (busy.value) return; run(async () => { try { await api('/auth/logout', 'POST', {}, store.token) } finally { store.signOut(); closeEditor(); recoveryCodes.value = []; phase.value = 'dashboard' } }) }
</script>

<template>
  <main class="shell">
    <header class="topbar">
      <div class="brand"><span class="mark">m</span><strong>mDNS Gateway</strong></div>
      <nav class="main-nav" aria-label="管理菜单">
        <button type="button" :class="{ active: dashboardView === 'sites' }" :aria-current="dashboardView === 'sites' ? 'page' : undefined" @click="dashboardView = 'sites'">站点</button>
        <button type="button" :class="{ active: dashboardView === 'system' }" :aria-current="dashboardView === 'system' ? 'page' : undefined" @click="dashboardView = 'system'">运行状态</button>
      </nav>
      <div class="account-area">
        <details v-if="store.isAuthenticated" class="account-menu">
          <summary aria-label="账户信息"><span class="account-avatar" aria-hidden="true">{{ store.account?.session.username.slice(0, 1).toUpperCase() }}</span><span>{{ store.account?.session.username }}</span><span aria-hidden="true">⌄</span></summary>
          <div class="account-popover"><span class="account-caption">当前账户</span><strong>{{ store.account?.session.username }}</strong><p>{{ store.account?.totpEnabled ? '验证器已绑定' : '验证器未绑定' }}</p><button type="button" class="subtle" :disabled="busy" @click="logout">退出登录</button></div>
        </details>
        <button v-else type="button" class="primary login-button" :disabled="busy" @click="openLogin">登录</button>
      </div>
    </header>
    <div v-if="phase !== 'dashboard'" class="overlay" @click.self="closeAuth" @keydown.esc.prevent="closeAuth">
      <form class="panel auth-panel auth-dialog" role="dialog" aria-modal="true" aria-labelledby="auth-title" @submit.prevent="submitAuth">
        <div class="auth-heading"><div class="eyebrow">安全访问</div><button type="button" class="icon-button" aria-label="关闭登录" :disabled="busy" @click="closeAuth">×</button></div>
        <h1 id="auth-title">{{ phase === 'setup' ? '首次设置' : phase === 'login' ? '登录后台' : phase === 'recovery' ? '恢复账户' : '重新绑定验证器' }}</h1>
        <p class="muted">{{ phase === 'setup' ? '创建管理员并绑定 6 位动态验证码。' : phase === 'login' ? '输入账户名和验证器中的 6 位验证码。' : phase === 'recovery' ? '使用一次性恢复码重置验证器。' : setupId ? '扫描二维码后，输入新验证器中的验证码。' : '验证器已重置，输入账户名开始重新绑定。' }}</p>
        <label>账户名<input v-model.trim="username" autocomplete="username" required :disabled="busy || !!setupId"></label>
        <template v-if="phase === 'setup' || phase === 'rebind'">
          <button v-if="!setupId" type="submit" class="primary" :disabled="busy">{{ busy ? '请稍候…' : '生成绑定二维码' }}</button>
          <template v-else>
            <img class="qr" :src="qr" alt="验证器绑定二维码">
            <p class="hint">如无法扫描，可复制绑定链接：<code>{{ otpauthUrl }}</code></p>
            <label>6 位验证码<input v-model.trim="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required :disabled="busy"></label>
            <button type="submit" class="primary" :disabled="busy || code.length !== 6">{{ busy ? '验证中…' : '完成绑定' }}</button>
          </template>
        </template>
        <template v-else>
          <label>{{ phase === 'recovery' ? '恢复码' : '6 位验证码' }}<input v-model.trim="code" :inputmode="phase === 'login' ? 'numeric' : 'text'" :autocomplete="phase === 'login' ? 'one-time-code' : 'off'" :pattern="phase === 'login' ? '[0-9]{6}' : undefined" :maxlength="phase === 'login' ? 6 : 64" required :disabled="busy"></label>
          <button type="submit" class="primary" :disabled="busy || (phase === 'login' ? code.length !== 6 : !code)">{{ busy ? '请稍候…' : phase === 'login' ? '登录' : '继续' }}</button>
        </template>
        <div class="switch">
          <button v-if="phase === 'login'" type="button" class="link" :disabled="busy" @click="changePhase('recovery')">丢失验证器？</button>
          <button v-if="phase === 'login'" type="button" class="link" :disabled="busy" @click="changePhase('rebind')">已通过 CLI 重置？</button>
          <button v-if="phase === 'recovery' || phase === 'rebind'" type="button" class="link" :disabled="busy" @click="changePhase('login')">返回登录</button>
        </div>
        <button type="button" class="link auth-back" :disabled="busy" @click="closeAuth">返回浏览</button>
      </form>
    </div>
    <div class="dashboard">
      <section class="workspace-head">
        <div><div class="eyebrow">管理控制台</div><h1>{{ dashboardView === 'sites' ? '站点' : '运行状态' }}</h1></div>
        <button v-if="dashboardView === 'sites' && store.isAuthenticated" type="button" class="primary add" :disabled="busy" @click="edit()">＋ 添加站点</button>
        <button v-if="dashboardView === 'system'" type="button" class="refresh-button" :disabled="busy" @click="run(() => store.refresh())">{{ busy ? '刷新中…' : '刷新状态' }}</button>
      </section>
      <template v-if="dashboardView === 'sites'">
        <div class="list-toolbar">
          <span class="list-count">{{ store.status ? `${store.sites.length} 个站点` : '加载中…' }}<span v-if="!store.isAuthenticated" class="readonly-label">只读浏览</span></span>
          <button v-if="store.status" type="button" class="health-summary" @click="dashboardView = 'system'" aria-label="查看运行状态">
            <span :class="['health-dot', { warn: healthNeedsAttention }]"></span>
            {{ healthNeedsAttention ? '有状态需要关注' : '服务运行正常' }}
            <span aria-hidden="true">↗</span>
          </button>
        </div>
        <div v-if="!store.status && busy" class="empty" role="status">正在加载站点…</div>
        <div v-else-if="!store.sites.length" class="empty">还没有站点。{{ store.isAuthenticated ? '点击“添加站点”开始连接内网服务。' : '登录后可以添加站点。' }}</div>
        <div class="grid">
          <article v-for="site in store.sites" :key="site.id" class="site-card" :class="{ 'is-off': !site.enabled, 'has-conflict': store.mdnsConflicts[site.domain] }">
            <div class="site-top"><div class="site-icon" aria-hidden="true">↗</div><span class="badge" :class="{ off: !site.enabled, conflict: site.enabled && store.mdnsConflicts[site.domain] }">{{ !site.enabled ? '已停用' : store.mdnsConflicts[site.domain] ? 'mDNS 冲突' : '运行中' }}</span></div>
            <div class="site-content"><span class="site-name">{{ site.name }}</span><h2>{{ site.domain }}</h2><p class="site-target"><span>{{ site.protocol === 'SSH' ? `网关端口 ${site.listenPort} →` : '目标地址' }}</span><code>{{ site.targetHost }}:{{ site.targetPort }}</code></p></div>
            <div class="site-footer"><span class="site-meta">{{ site.protocol === 'WEBSOCKET' ? 'WebSocket' : site.protocol }} · {{ site.configMode === 'CUSTOM' ? '自定义配置' : '自动配置' }}</span><div v-if="store.isAuthenticated" class="site-actions"><button type="button" :disabled="busy" @click="edit(site)">编辑</button><button type="button" class="danger" :disabled="busy" @click="remove(site)">删除</button></div></div>
          </article>
        </div>
      </template>
      <section v-else class="system-grid" aria-label="系统运行状态">
        <article class="system-card"><span class="system-label">数据库</span><strong><i class="dot" :class="{ warn: store.status?.database !== 'OK' }"></i>{{ store.status?.database || '读取中' }}</strong><p>站点与认证数据</p></article>
        <article class="system-card"><span class="system-label">mDNS</span><strong><i class="dot" :class="{ warn: store.status?.mdns !== 'OK' }"></i>{{ store.status?.mdns || '读取中' }}</strong><p>本地网络广播</p></article>
        <article class="system-card"><span class="system-label">OpenResty</span><strong><i class="dot" :class="{ warn: store.status?.openrestyRuntime !== 'HEALTHY' }"></i>{{ store.status?.openrestyRuntime || '读取中' }}</strong><p>代理运行状态</p></article>
        <article class="system-card config-card"><span class="system-label">配置发布</span><strong>{{ store.status?.configPending ? '待应用' : store.status?.openrestyApply || '读取中' }}</strong><dl><div><dt>当前 Release</dt><dd>{{ store.status?.activeRelease || '—' }}</dd></div><div><dt>目标版本</dt><dd>{{ store.status?.desiredVersion ?? '—' }}</dd></div><div><dt>应用版本</dt><dd>{{ store.status?.openrestyActive || '—' }}</dd></div></dl></article>
        <article class="system-card version-card"><span class="system-label">网关版本</span><strong>{{ store.status?.version || '—' }}</strong></article>
      </section>
      <div v-if="editing && store.isAuthenticated" class="overlay" @click.self="requestCloseEditor">
        <form class="dialog" @submit.prevent="save">
          <div class="dialog-head">
            <h2>{{ form.id ? '编辑站点' : '添加站点' }}</h2>
            <button type="button" class="icon-button" :disabled="busy" @click="requestCloseEditor">×</button>
          </div>
          <label>名称<input v-model.trim="form.name" placeholder="例如：媒体库" required :disabled="busy"></label>
          <label>域名<input v-model.trim="form.domain" placeholder="media.local" required :disabled="busy"></label>
          <div class="row">
            <label>目标主机<input v-model.trim="form.targetHost" placeholder="192.168.1.10" required :disabled="busy"></label>
            <label class="port">端口<input v-model.number="form.targetPort" type="number" min="1" max="65535" required :disabled="busy"></label>
          </div>
          <div class="row">
            <label>协议<select v-model="form.protocol" :disabled="busy" @change="onProtocolChange"><option value="HTTP">HTTP</option><option value="WEBSOCKET">WebSocket</option><option value="SSH">SSH</option></select></label>
            <label v-if="form.protocol === 'SSH'" class="port">网关端口<input v-model.number="form.listenPort" type="number" min="1" max="65535" required :disabled="busy"></label>
          </div>
          <p v-if="form.protocol === 'SSH'" class="field-hint">SSH 通过网关独立端口转发，连接方式：ssh -p {{ form.listenPort || '端口' }} 用户@{{ form.domain || '域名.local' }}</p>
          <p v-if="form.protocol === 'SSH' && selected?.configMode === 'CUSTOM'" class="field-hint warning">保存后，原站点的自定义 HTTP 配置将被移除。</p>
          <label class="check"><input v-model="form.enabled" type="checkbox" :disabled="busy"> 启用站点</label>
          <button v-if="form.protocol !== 'SSH'" type="button" class="link advanced" :disabled="busy" @click="toggleAdvanced">高级配置 {{ customVisible ? '⌃' : '⌄' }}</button>
          <div v-if="customVisible && form.protocol !== 'SSH'" class="advanced-box">
            <label class="check"><input v-model="form.configMode" type="radio" name="configMode" value="GENERATED" :disabled="busy"> 自动生成</label>
            <label class="check"><input v-model="form.configMode" type="radio" name="configMode" value="CUSTOM" :disabled="busy"> 自定义</label>
            <textarea v-if="form.configMode === 'GENERATED'" :value="generatedPreview" rows="10" spellcheck="false" readonly></textarea>
            <template v-else>
              <textarea v-model="form.customConfig" rows="10" spellcheck="false" placeholder="server { ... }" required :disabled="busy"></textarea>
              <button v-if="form.id" type="button" class="link" :disabled="busy" @click="resetConfig">恢复默认配置</button>
            </template>
          </div>
          <div class="dialog-actions">
            <button type="button" :disabled="busy" @click="requestCloseEditor">取消</button>
            <button type="submit" class="primary" :disabled="busy || !form.name || !form.domain || !form.targetHost">{{ busy ? '保存中…' : '保存' }}</button>
          </div>
        </form>
      </div>
    </div>
    <p v-if="error" class="toast" role="alert">{{ error }}</p>
    <div v-if="recoveryCodes.length" class="overlay"><section class="dialog"><h2>保存恢复码</h2><p>这些恢复码只显示一次。请离线保存。</p><pre>{{ recoveryCodes.join('\n') }}</pre><button type="button" class="primary" @click="recoveryCodes = []">我已保存</button></section></div>
  </main>
</template>
