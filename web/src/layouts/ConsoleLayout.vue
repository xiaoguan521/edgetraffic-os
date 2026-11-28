<script setup lang="ts">
import { brand } from '../brand'
import { computed, h, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  AlertOutlined,
  ApiOutlined,
  AuditOutlined,
  ClusterOutlined,
  CloudUploadOutlined,
  CodeOutlined,
  DashboardOutlined,
  EnvironmentOutlined,
  GlobalOutlined,
  ImportOutlined,
  KeyOutlined,
  LockOutlined,
  LogoutOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  RocketOutlined,
  SafetyCertificateOutlined,
  SearchOutlined,
} from '@antdv-next/icons'
import { api } from '../api/client'
import { editionName, licenseStateName, useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'

const route = useRoute()
const router = useRouter()
const session = useSession()
const ws = useWorkspace()

function labelWithBadge(text: string, count: number, tone: 'warn' | 'bad' = 'warn') {
  if (!count || ws.siderCollapsed) return text
  return h('span', { class: 'nav-lbl' }, [
    h('span', text),
    h('span', { class: `nav-badge ${tone}` }, count > 99 ? '99+' : String(count)),
  ])
}

const items = computed(() => {
  const b = ws.badges
  const nodeIssues = (b.drifted_nodes || 0) + (b.offline_nodes || 0)
  const nav = [
    { key: 'dashboard', icon: () => h(DashboardOutlined), label: '总览' },
    {
      type: 'group',
      label: ws.siderCollapsed ? '' : '流量配置',
      children: [
        { key: 'sites', icon: () => h(GlobalOutlined), label: '站点与上游' },
        { key: 'streams', icon: () => h(ApiOutlined), label: '四层代理' },
        { key: 'ip-sets', icon: () => h(EnvironmentOutlined), label: 'IP 集与地域库' },
        { key: 'certificates', icon: () => h(LockOutlined), label: '证书' },
        { key: 'snippets', icon: () => h(CodeOutlined), label: labelWithBadge('自定义片段', b.pending_snippets) },
        { key: 'imports', icon: () => h(ImportOutlined), label: '配置导入' },
      ],
    },
    {
      type: 'group',
      label: ws.siderCollapsed ? '' : '发布与运维',
      children: [
        { key: 'releases', icon: () => h(RocketOutlined), label: labelWithBadge('发布', b.pending_releases) },
        { key: 'nodes', icon: () => h(ClusterOutlined), label: labelWithBadge('节点', nodeIssues, b.drifted_nodes ? 'bad' : 'warn') },
        { key: 'dataplane', icon: () => h(CloudUploadOutlined), label: '数据面版本' },
      ],
    },
    {
      type: 'group',
      label: ws.siderCollapsed ? '' : '安全与治理',
      children: [
        { key: 'alerts', icon: () => h(AlertOutlined), label: labelWithBadge('告警', b.firing_alerts, 'bad') },
        { key: 'audit', icon: () => h(AuditOutlined), label: '审计' },
        { key: 'system', icon: () => h(SafetyCertificateOutlined), label: '授权与系统' },
      ],
    },
  ]
  return nav
})

const roleName: Record<string, string> = { admin: '管理员', operator: '运维', releaser: '发布员', viewer: '只读' }
const modeTag: Record<string, { text: string; color: string }> = {
  managed: { text: '托管', color: 'green' },
  observe: { text: '观察', color: 'gold' },
  legacy: { text: '兼容', color: 'purple' },
}

const selected = computed(() => [String(route.name || 'dashboard')])
const showGroupPicker = computed(() => ws.groups.length > 0 && ['nodes', 'sites', 'streams', 'ip-sets', 'imports', 'snippets', 'releases', 'dataplane'].includes(String(route.name || '')))
const showDraftBanner = computed(
  () => ws.hasDraftChanges && ws.currentGroup?.management_mode === 'managed' && ['sites', 'streams', 'ip-sets', 'snippets', 'imports'].includes(String(route.name || '')),
)

const lic = computed(() => session.license)
const licTone = computed(() => {
  switch (lic.value?.state) {
    case 'VALID':
      return 'ok'
    case 'EXPIRING':
    case 'GRACE':
      return 'warn'
    default:
      return 'bad'
  }
})
const licText = computed(() => {
  const l = lic.value
  if (!l) return ''
  const name = licenseStateName[l.state] || l.state
  if (l.state === 'VALID' || l.state === 'EXPIRING' || l.state === 'GRACE') {
    const ed = editionName[l.edition || ''] || l.edition
    return l.state === 'VALID' ? `${ed}，剩余 ${Math.max(l.days_left, 0)} 天` : `${ed}${name}，剩余 ${Math.max(l.days_left, 0)} 天`
  }
  return `授权${name}，控制台只读`
})

// 全局跨组搜索（⌘K / Ctrl+K）
const searchOpen = ref(false)
const searchQ = ref('')
const searchHits = ref<any[]>([])
const searching = ref(false)
const hitKindName: Record<string, { t: string; c: string }> = {
  node: { t: '节点', c: 'green' },
  site: { t: '站点', c: 'cyan' },
  upstream: { t: '上游', c: 'blue' },
  cert: { t: '证书', c: 'purple' },
  stream: { t: '四层', c: 'orange' },
}
let searchTimer: number | undefined
watch(searchQ, (q) => {
  clearTimeout(searchTimer)
  if (!q.trim()) {
    searchHits.value = []
    return
  }
  searchTimer = window.setTimeout(async () => {
    searching.value = true
    try {
      const r = await api.get('/search', { q: q.trim() })
      searchHits.value = r.hits || []
    } catch {
      searchHits.value = []
    } finally {
      searching.value = false
    }
  }, 180)
})
function openGlobalSearch() {
  searchOpen.value = true
}
function jumpHit(hit: any) {
  searchOpen.value = false
  if (hit.group_id) ws.setGroup(hit.group_id)
  if (hit.kind === 'upstream') {
    router.push({ path: '/sites', query: { tab: 'ups', group: String(hit.group_id), q: hit.name } })
  } else if (hit.kind === 'site') {
    router.push({ path: '/sites', query: { group: String(hit.group_id), q: hit.name } })
  } else if (hit.kind === 'node') {
    router.push({ path: '/nodes', query: { group: String(hit.group_id), q: hit.name } })
  } else if (hit.kind === 'cert') {
    router.push({ path: '/certificates', query: { q: hit.name } })
  } else {
    router.push({ path: hit.path, query: hit.group_id ? { group: String(hit.group_id) } : undefined })
  }
}
function onKeydown(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    searchOpen.value = !searchOpen.value
  }
}

let licTimer: number | undefined
let badgeTimer: number | undefined
onMounted(() => {
  ws.loadGroups(Number(route.query.group) || undefined).catch(() => {})
  ws.loadBadges()
  window.addEventListener('keydown', onKeydown)
  licTimer = window.setInterval(() => session.loadLicense().catch(() => {}), 60000)
  badgeTimer = window.setInterval(() => ws.loadBadges(), 30000)
})
onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
  clearInterval(licTimer)
  clearInterval(badgeTimer)
})

function onGroupChange(v: any) {
  const id = Number(v)
  ws.setGroup(id)
  if (route.query.group) {
    router.replace({ query: { ...route.query, group: String(id) } })
  }
}

async function logout() {
  await session.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <a-layout class="shell">
    <a-layout-sider :width="224" :collapsed-width="64" :collapsed="ws.siderCollapsed" :trigger="null" collapsible class="sider">
      <div class="brand" :class="{ mini: ws.siderCollapsed }">
        <div class="mark" aria-hidden="true"><span /><span /><span /></div>
        <div v-if="!ws.siderCollapsed">
          <div class="name">{{ brand.product }}</div>
          <div class="sub">边缘流量操作系统</div>
        </div>
      </div>
      <div class="menu-wrap">
        <a-menu theme="dark" mode="inline" :inline-collapsed="ws.siderCollapsed" :selected-keys="selected" :items="items" @click="(e: any) => router.push({ name: e.key })" />
      </div>
      <div v-if="!ws.siderCollapsed" class="runtime">数据面 {{ brand.proc }}</div>
    </a-layout-sider>
    <a-layout>
      <a-layout-header class="topbar">
        <div class="left">
          <a-button type="text" size="small" class="fold-btn" :title="ws.siderCollapsed ? '展开侧栏' : '收起侧栏'" @click="ws.toggleSider()">
            <MenuUnfoldOutlined v-if="ws.siderCollapsed" />
            <MenuFoldOutlined v-else />
          </a-button>
          <div class="title">{{ route.meta.title }}</div>
          <div v-if="showGroupPicker" class="grp-picker">
            <span class="grp-lbl">节点组</span>
            <a-select
              :value="ws.groupId"
              size="small"
              show-search
              option-filter-prop="label"
              style="min-width: 170px"
              :options="ws.groups.map((g) => ({ value: g.id, label: g.name }))"
              @change="onGroupChange"
            />
            <a-tag v-if="ws.currentGroup?.management_mode" :color="modeTag[ws.currentGroup.management_mode]?.color" class="mtag">
              {{ modeTag[ws.currentGroup.management_mode]?.text || ws.currentGroup.management_mode }}
            </a-tag>
          </div>
        </div>
        <div class="right">
          <a-button size="small" class="gsearch" @click="openGlobalSearch">
            <SearchOutlined /> <span class="gsearch-txt">搜索域名 / IP / 节点</span> <kbd>⌘K</kbd>
          </a-button>
          <router-link v-if="lic" :to="{ name: 'system' }" class="lic" :class="licTone">
            <i class="dot" />{{ licText }}
          </router-link>
          <a-dropdown :menu="{ items: [{ key: 'password', icon: () => h(KeyOutlined), label: '修改口令' }, { key: 'logout', icon: () => h(LogoutOutlined), label: '退出登录' }], onClick: (e: any) => (e.key === 'password' ? router.push({ name: 'password' }) : logout()) }">
            <a-button type="text">{{ session.username }}（{{ roleName[session.role] || session.role }}）</a-button>
          </a-dropdown>
        </div>
      </a-layout-header>
      <div v-if="showDraftBanner" class="draft-bar">
        <div class="draft-inner">
          <span class="draft-dot" />
          <span>节点组 <b>{{ ws.currentGroup?.name }}</b> 存在未发布草稿变更<template v-if="ws.draftChangedFiles">（涉及 {{ ws.draftChangedFiles }} 个配置文件）</template>，当前线上流量未受影响。</span>
          <a-button type="primary" size="small" @click="router.push({ name: 'releases' })">预览差异并发布</a-button>
        </div>
      </div>
      <a-layout-content>
        <router-view />
      </a-layout-content>
    </a-layout>
    <a-modal v-model:open="searchOpen" title="全网跨组快速搜索" :footer="null" width="620px">
      <a-input v-model:value="searchQ" allow-clear autofocus placeholder="输入域名、站点名、上游后端 IP、节点名 / IP 或证书名…" size="large" />
      <a-spin :spinning="searching">
        <div v-if="searchHits.length" class="hits">
          <div v-for="hit in searchHits" :key="`${hit.kind}-${hit.id}`" class="hit" @click="jumpHit(hit)">
            <a-tag :color="hitKindName[hit.kind]?.c">{{ hitKindName[hit.kind]?.t || hit.kind }}</a-tag>
            <strong class="code">{{ hit.name }}</strong>
            <span v-if="hit.group_name" class="hgrp">{{ hit.group_name }}</span>
            <span class="hdet code">{{ hit.detail }}</span>
          </div>
        </div>
        <a-empty v-else-if="searchQ.trim()" description="未找到匹配项" style="margin: 24px 0" />
        <p v-else class="hhint">支持一次检索全部节点组的节点、站点、域名、上游后端 IP、四层服务与证书，点击直接跳转。</p>
      </a-spin>
    </a-modal>
  </a-layout>
</template>

<style scoped>
.shell {
  min-height: 100vh;
}
.sider {
  position: sticky;
  top: 0;
  height: 100vh;
}
.sider :deep(.ant-layout-sider-children) {
  display: flex;
  flex-direction: column;
}
.menu-wrap {
  flex: 1;
  overflow-y: auto;
  overflow-x: hidden;
}
.brand {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 18px 18px 14px;
  color: #fff;
}
.brand.mini {
  justify-content: center;
  padding: 18px 0 14px;
}
.mark {
  display: grid;
  gap: 3px;
  width: 26px;
  flex-shrink: 0;
}
.mark span {
  display: block;
  height: 4px;
  border-radius: 2px;
  background: #0e7c86;
}
.mark span:nth-child(2) {
  width: 70%;
  background: #2f9e5b;
}
.mark span:nth-child(3) {
  width: 40%;
  background: #d9a21b;
}
.name {
  font-weight: 650;
  font-size: 15px;
  letter-spacing: 0.02em;
}
.sub {
  font-size: 12px;
  color: #8fa7ab;
  margin-top: 2px;
}
.runtime {
  margin-top: auto;
  padding: 12px 20px;
  font-size: 12px;
  color: #7f979b;
  border-top: 1px solid rgba(255, 255, 255, 0.06);
}
.topbar {
  position: sticky;
  top: 0;
  z-index: 20;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px 0 16px;
  border-bottom: 1px solid #e1e7e6;
  box-shadow: 0 1px 4px rgba(22, 50, 58, 0.04);
  height: 54px;
  line-height: 54px;
}
.left {
  display: flex;
  align-items: center;
  gap: 12px;
}
.fold-btn {
  color: #445a5f;
  font-size: 15px;
}
.title {
  font-size: 15px;
  color: #16323a;
  font-weight: 600;
}
.grp-picker {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding-left: 14px;
  border-left: 1px solid #e1e7e6;
  line-height: normal;
}
.grp-lbl {
  font-size: 12px;
  color: #6b7c80;
}
.mtag {
  margin: 0;
}
.draft-bar {
  background: #fff9e6;
  border-bottom: 1px solid #ecd98a;
  padding: 8px 28px;
}
.draft-inner {
  max-width: 1464px;
  margin: 0 auto;
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  color: #6e500a;
}
.draft-inner > :last-child {
  margin-left: auto;
}
.draft-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #d9a21b;
  flex-shrink: 0;
}
.right {
  display: flex;
  align-items: center;
  gap: 12px;
}
.gsearch {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: #556a6e;
  background: #f5f8f8;
  border-color: #dce4e3;
}
.gsearch kbd {
  font-size: 11px;
  padding: 0 4px;
  border-radius: 3px;
  background: #e5eceb;
  color: #445a5f;
}
@media (max-width: 1100px) {
  .gsearch-txt {
    display: none;
  }
}
.hits {
  margin-top: 12px;
  max-height: 360px;
  overflow-y: auto;
  border: 1px solid #e6eceb;
  border-radius: 6px;
}
.hit {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  border-bottom: 1px solid #eef2f1;
  cursor: pointer;
}
.hit:last-child {
  border-bottom: 0;
}
.hit:hover {
  background: #f3f8f8;
}
.hgrp {
  font-size: 12px;
  color: #0b5f66;
  background: #e3f1f2;
  padding: 0 6px;
  border-radius: 4px;
}
.hdet {
  margin-left: auto;
  font-size: 12px;
  color: #6b7c80;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 260px;
}
.hhint {
  margin: 16px 0 4px;
  font-size: 12.5px;
  color: #8a9a9d;
}
.lic {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 28px;
  line-height: 28px;
  padding: 0 12px;
  border-radius: 14px;
  font-size: 13px;
  text-decoration: none;
}
.lic .dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: currentColor;
}
.lic.ok {
  color: #1f6f43;
  background: #e6f3ec;
}
.lic.warn {
  color: #8a6510;
  background: #fbf1d8;
}
.lic.bad {
  color: #a3332d;
  background: #f8e3e1;
}
:deep(.nav-lbl) {
  display: inline-flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  gap: 8px;
}
:deep(.nav-badge) {
  display: inline-block;
  min-width: 18px;
  height: 18px;
  line-height: 18px;
  padding: 0 5px;
  border-radius: 9px;
  font-size: 11px;
  text-align: center;
  font-weight: 600;
}
:deep(.nav-badge.warn) {
  background: #d9a21b;
  color: #16262a;
}
:deep(.nav-badge.bad) {
  background: #c8423b;
  color: #fff;
}
</style>
