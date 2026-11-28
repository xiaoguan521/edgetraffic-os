<script setup lang="ts">
import { brand } from '../brand'
import { computed, h, onMounted, onUnmounted } from 'vue'
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
  RocketOutlined,
  SafetyCertificateOutlined,
} from '@antdv-next/icons'
import { editionName, licenseStateName, useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'

const route = useRoute()
const router = useRouter()
const session = useSession()
const ws = useWorkspace()

function labelWithBadge(text: string, count: number, tone: 'warn' | 'bad' = 'warn') {
  if (!count) return text
  return h('span', { class: 'nav-lbl' }, [
    h('span', text),
    h('span', { class: `nav-badge ${tone}` }, count > 99 ? '99+' : String(count)),
  ])
}

const items = computed(() => {
  const b = ws.badges
  const nodeIssues = (b.drifted_nodes || 0) + (b.offline_nodes || 0)
  return [
    { key: 'dashboard', icon: () => h(DashboardOutlined), label: '总览' },
    {
      type: 'group',
      label: '流量配置',
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
      label: '发布与运维',
      children: [
        { key: 'releases', icon: () => h(RocketOutlined), label: labelWithBadge('发布', b.pending_releases) },
        { key: 'nodes', icon: () => h(ClusterOutlined), label: labelWithBadge('节点', nodeIssues, b.drifted_nodes ? 'bad' : 'warn') },
        { key: 'dataplane', icon: () => h(CloudUploadOutlined), label: '数据面版本' },
      ],
    },
    {
      type: 'group',
      label: '安全与治理',
      children: [
        { key: 'alerts', icon: () => h(AlertOutlined), label: labelWithBadge('告警', b.firing_alerts, 'bad') },
        { key: 'audit', icon: () => h(AuditOutlined), label: '审计' },
        { key: 'system', icon: () => h(SafetyCertificateOutlined), label: '授权与系统' },
      ],
    },
  ]
})

const roleName: Record<string, string> = { admin: '管理员', operator: '运维', releaser: '发布员', viewer: '只读' }
const modeTag: Record<string, { text: string; color: string }> = {
  managed: { text: '托管', color: 'green' },
  observe: { text: '观察', color: 'gold' },
  legacy: { text: '兼容', color: 'purple' },
}

const selected = computed(() => [String(route.name || 'dashboard')])
const showGroupPicker = computed(() => ws.groups.length > 0 && ['nodes', 'sites', 'streams', 'ip-sets', 'imports', 'snippets', 'releases', 'dataplane'].includes(String(route.name || '')))

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

let licTimer: number | undefined
let badgeTimer: number | undefined
onMounted(() => {
  ws.loadGroups(Number(route.query.group) || undefined).catch(() => {})
  ws.loadBadges()
  licTimer = window.setInterval(() => session.loadLicense().catch(() => {}), 60000)
  badgeTimer = window.setInterval(() => ws.loadBadges(), 30000)
})
onUnmounted(() => {
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
    <a-layout-sider :width="224" class="sider">
      <div class="brand">
        <div class="mark" aria-hidden="true"><span /><span /><span /></div>
        <div>
          <div class="name">{{ brand.product }}</div>
          <div class="sub">边缘流量操作系统</div>
        </div>
      </div>
      <div class="menu-wrap">
        <a-menu theme="dark" mode="inline" :selected-keys="selected" :items="items" @click="(e: any) => router.push({ name: e.key })" />
      </div>
      <div class="runtime">数据面 {{ brand.proc }}</div>
    </a-layout-sider>
    <a-layout>
      <a-layout-header class="topbar">
        <div class="left">
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
          <router-link v-if="lic" :to="{ name: 'system' }" class="lic" :class="licTone">
            <i class="dot" />{{ licText }}
          </router-link>
          <a-dropdown :menu="{ items: [{ key: 'password', icon: () => h(KeyOutlined), label: '修改口令' }, { key: 'logout', icon: () => h(LogoutOutlined), label: '退出登录' }], onClick: (e: any) => (e.key === 'password' ? router.push({ name: 'password' }) : logout()) }">
            <a-button type="text">{{ session.username }}（{{ roleName[session.role] || session.role }}）</a-button>
          </a-dropdown>
        </div>
      </a-layout-header>
      <a-layout-content>
        <router-view />
      </a-layout-content>
    </a-layout>
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
}
.brand {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 20px 18px 14px;
  color: #fff;
}
.mark {
  display: grid;
  gap: 3px;
  width: 26px;
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
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px 0 28px;
  border-bottom: 1px solid #e1e7e6;
  height: 56px;
  line-height: 56px;
}
.left {
  display: flex;
  align-items: center;
  gap: 18px;
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
  padding-left: 16px;
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
.right {
  display: flex;
  align-items: center;
  gap: 16px;
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
