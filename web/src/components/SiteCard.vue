<script setup lang="ts">
import { computed } from 'vue'
import type { Site } from '../api'

/** 卡片只负责展示和访问；管理动作交给父页面执行鉴权与请求。 */
const props = defineProps<{ site: Site; conflict?: boolean; editable: boolean; busy: boolean }>()
const emit = defineEmits<{ edit: [site: Site]; remove: [site: Site] }>()

/** 访问网关域名而非上游地址；SSH 必须携带本站点的监听端口。 */
const destination = computed(() => props.site.protocol === 'SSH'
  ? `ssh://${props.site.domain}:${props.site.listenPort}`
  : `http://${props.site.domain}/`)
/** WebSocket 的 HTTP 入口支持浏览器访问和后续协议升级。 */
const protocolName = computed(() => props.site.protocol === 'WEBSOCKET' ? 'WebSocket' : props.site.protocol)
/** 停用状态优先展示；冲突提示不阻止用户检查实际访问结果。 */
const state = computed(() => !props.site.enabled ? '已停用' : props.conflict ? 'mDNS 冲突' : '已启用')
/** 原生链接提供回车、复制链接和浏览器的新标签页操作。 */
const actionLabel = computed(() => props.site.protocol === 'SSH' ? '连接 SSH' : '访问站点')
/** IPv6 上游使用方括号，避免与端口分隔符混淆。 */
const upstream = computed(() => `${props.site.targetHost.includes(':') ? `[${props.site.targetHost}]` : props.site.targetHost}:${props.site.targetPort}`)
</script>

<template>
  <article class="site-card" :class="{ 'is-off': !site.enabled, 'has-conflict': site.enabled && conflict }">
    <!-- 卡片主体和管理按钮互为兄弟，编辑、删除不会触发域名跳转。 -->
    <component :is="site.enabled ? 'a' : 'div'" class="site-entry"
      :href="site.enabled ? destination : undefined"
      :target="site.enabled && site.protocol !== 'SSH' ? '_blank' : undefined"
      :rel="site.enabled ? 'noopener noreferrer' : undefined"
      :aria-label="site.enabled ? `${actionLabel}：${destination}${site.protocol === 'SSH' ? '（需要 SSH 客户端）' : '（在新标签页打开）'}` : undefined">
      <div class="site-top">
        <span class="site-icon" aria-hidden="true">
          <svg v-if="site.protocol === 'SSH'" viewBox="0 0 24 24"><path d="m5 7 5 5-5 5m8 0h6" /></svg>
          <svg v-else-if="site.protocol === 'WEBSOCKET'" viewBox="0 0 24 24"><path d="M4 8h16m-4-4 4 4-4 4M20 16H4m4-4-4 4 4 4" /></svg>
          <svg v-else viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18" /></svg>
        </span>
        <span class="site-name" :title="site.name">{{ site.name }}</span>
        <span class="badge" :class="{ off: !site.enabled, conflict: site.enabled && conflict }">{{ state }}</span>
      </div>
      <h2>{{ site.domain }}<span v-if="site.protocol === 'SSH'" class="site-port">:{{ site.listenPort }}</span></h2>
      <p class="site-target"><span>转发至</span><code>{{ upstream }}</code></p>
      <div class="site-access">
        <span class="site-protocol">{{ protocolName }}</span>
        <span v-if="site.enabled" class="site-open">{{ actionLabel }}<span aria-hidden="true">↗</span></span>
        <span v-else class="site-unavailable">启用后可访问</span>
      </div>
    </component>
    <!-- 访客只显示配置类型，已登录用户沿用现有管理权限与忙碌状态。 -->
    <div class="site-footer">
      <span class="site-meta">{{ site.configMode === 'CUSTOM' ? '自定义配置' : '自动配置' }}</span>
      <div v-if="editable" class="site-actions">
        <button type="button" :disabled="busy" :aria-label="`编辑 ${site.domain}`" @click="emit('edit', site)">编辑</button>
        <button type="button" class="danger" :disabled="busy" :aria-label="`删除 ${site.domain}`" @click="emit('remove', site)">删除</button>
      </div>
    </div>
  </article>
</template>

<style scoped>
/* 同行卡片随最长内容等高，管理栏始终贴合底部。 */
.site-card { display: flex; flex-direction: column; min-width: 0; overflow: hidden; background: #fff; border: 1px solid #dde7ed; border-radius: 14px; box-shadow: 0 4px 16px #1f404f06; transition: border-color .16s ease, box-shadow .16s ease; }
.site-card:has(a:hover), .site-card:focus-within { border-color: #8ebfc9; box-shadow: 0 8px 24px #1f404f12; }
.site-card.is-off { background: #fafcfd; }
.site-card.has-conflict { border-color: #e8cf9e; }
.site-entry { display: flex; flex-direction: column; flex: 1; padding: 20px; color: inherit; text-decoration: none; }
/* 使用链接本身呈现交互反馈，避免停用站点出现可点击的错觉。 */
a.site-entry { transition: background .16s ease; }
a.site-entry:hover { background: #f7fbfc; }
a.site-entry:active { background: #edf6f8; }
a.site-entry:focus-visible { outline: 3px solid #8bc8df; outline-offset: -3px; border-radius: 13px 13px 0 0; }
.site-top { display: flex; align-items: center; gap: 10px; min-width: 0; }
.site-icon { display: grid; place-items: center; flex: none; width: 32px; height: 32px; border: 1px solid #d9ecef; border-radius: 10px; background: #edf7f8; color: #08778b; }
.site-icon svg { width: 18px; height: 18px; fill: none; stroke: currentColor; stroke-width: 1.6; stroke-linecap: round; stroke-linejoin: round; }
.is-off .site-icon { color: #83959f; border-color: #e2e9ed; background: #f0f4f6; }
/* 名称省略但保留完整提示，状态标签不被长名称挤出卡片。 */
.site-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 14px; font-weight: 600; color: #536d7a; }
.badge { display: inline-flex; align-items: center; gap: 6px; flex: none; color: #147b5d; background: #eaf6ef; border-radius: 20px; padding: 4px 8px; font-size: 12px; font-weight: 600; }
.badge::before { content: ''; width: 5px; height: 5px; border-radius: 50%; background: currentColor; }
.badge.off { color: #768895; background: #edf1f3; }
.badge.conflict { color: #a3671c; background: #fff4df; }
h2 { margin: 20px 0 8px; overflow-wrap: anywhere; font-size: 22px; line-height: 1.3; letter-spacing: -.5px; }
.site-port { color: #738996; font-size: 16px; font-weight: 500; letter-spacing: 0; }
.site-target { display: flex; align-items: baseline; gap: 8px; margin: 0; min-width: 0; color: #81929c; font-size: 12px; line-height: 1.5; }
.site-target > span { flex: none; }
.site-target code { min-width: 0; overflow-wrap: anywhere; color: #536d7a; font-size: 13px; }
/* 长域名可自然换行，访问入口在同一行卡片中保持对齐。 */
.site-access { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-top: auto; padding-top: 20px; }
.site-protocol { padding: 3px 7px; border: 1px solid #e0e9ed; border-radius: 5px; color: #647d8b; font-size: 12px; line-height: 1.4; }
.site-open { display: inline-flex; align-items: center; gap: 8px; color: #08778b; font-size: 13px; font-weight: 600; }
.site-open > span { transition: transform .16s ease; }
a:hover .site-open > span { transform: translate(2px, -2px); }
.site-unavailable { color: #81929c; font-size: 12px; }
.site-footer { display: flex; flex: none; justify-content: space-between; align-items: center; gap: 8px; min-height: 44px; padding: 6px 16px 6px 20px; border-top: 1px solid #edf1f3; background: #fcfdfe; }
.site-meta { color: #81929c; font-size: 12px; }
.site-actions { display: flex; gap: 4px; }
.site-actions button { border: 0; background: transparent; padding: 6px 8px; font-size: 12px; color: #08778b; }
.site-actions button:hover { background: #eaf4f6; }
.site-actions .danger { color: #b74141; }
.site-actions .danger:hover { background: #fff0ef; }
/* 跟随系统的减少动态效果设置。 */
@media (prefers-reduced-motion: reduce) { .site-card, a.site-entry, .site-open > span { transition: none; } }
</style>
