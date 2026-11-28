<script setup lang="ts">
import { brand } from '../brand'
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import dayjs from 'dayjs'
import { Modal, message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import DiffView from '../components/DiffView.vue'

const session = useSession()
const ws = useWorkspace()
const route = useRoute()
const router = useRouter()
const groups = computed(() => ws.groups)
const gid = computed(() => ws.groupId)
const nodes = ref<any[]>([])
const group = computed(() => groups.value.find((g) => g.id === gid.value))
const profile = ref<any>(null)
const managementMode = ref<any>(null)
const view = ref<'group' | 'host'>('group')
const hosts = ref<any[]>([])
const nodeSearch = ref('')
const nodeFilter = ref<'all' | 'online' | 'offline' | 'drifted'>('all')

const filteredNodes = computed(() => {
  const q = nodeSearch.value.trim().toLowerCase()
  return nodes.value.filter((n) => {
    if (nodeFilter.value === 'online' && n.status !== 'online') return false
    if (nodeFilter.value === 'offline' && n.status === 'online') return false
    if (nodeFilter.value === 'drifted' && !n.drifted) return false
    if (!q) return true
    return n.name.toLowerCase().includes(q) || (n.remote_addr || '').toLowerCase().includes(q) || (n.applied_revision || '').toLowerCase().includes(q)
  })
})

async function load() {
  await ws.loadGroups(Number(route.query.group) || undefined)
  if (!gid.value) return
  const [n, p, h, m] = await Promise.all([
    api.get('/nodes', { group_id: gid.value }),
    api.get(`/node-groups/${gid.value}/profile`),
    api.get('/hosts'),
    api.get(`/node-groups/${gid.value}/management-mode`),
  ])
  nodes.value = n.filter((x: any) => x.status !== 'revoked')
  profile.value = p
  managementMode.value = m
  hosts.value = h
}
const instanceLabel = computed(() => {
  const p = profile.value
  if (!p) return ''
  const e = p.effective || {}
  const where = p.root ? `（${p.root}）` : ''
  const resources = p.resource_mode === 'independent' ? '独立程序' : '共享程序'
  return `${p.instance || '默认实例'} · ${p.mode === 'user' ? 'user 模式' : 'system 模式'}${where} · ${resources} · HTTP ${e.http_port} / HTTPS ${e.https_port}`
})
function dataplaneVersion(n: any) {
  return (n.syapp_version || '').replace(brand.proc + ' version: ', '')
}
watch(gid, () => {
  if (gid.value) load()
})
let t: number | undefined
onMounted(() => {
  if (route.query.group) ws.setGroup(Number(route.query.group))
  load()
  t = window.setInterval(load, 5000)
})
onUnmounted(() => clearInterval(t))

// 添加节点（分场景 Tab + 宿主机/Docker 切换 + 危险防呆 + 实时检测上线）
const enroll = ref<any>(null)
const enrollTab = ref<'new' | 'existing'>('new')
const runtimeEnv = ref<'host' | 'docker'>('host')
const dockerService = ref(brand.id)
const enrollBaselineIds = ref<number[]>([])
const enrolling = ref(false)
const switchingMode = ref(false)
const newlyJoinedNodes = computed(() => {
  if (!enroll.value) return []
  const base = new Set(enrollBaselineIds.value)
  return nodes.value.filter((n) => !base.has(n.id))
})
function dockerEnrollCmd(en: any, replaceLocal: boolean) {
  if (!en) return ''
  const svc = dockerService.value.trim() || brand.id
  const inst = en.instance ? ` -instance ${en.instance}` : ''
  const rep = replaceLocal ? ' -replace-local-config' : ''
  return `docker compose exec ${svc} ${brand.agent} enroll${inst} -server ${en.server} -token ${en.token} -ca-fingerprint ${en.ca_fingerprint}${rep}`
}
async function addNode(defaultTab: 'new' | 'existing' = 'new') {
  enrolling.value = true
  try {
    enrollBaselineIds.value = nodes.value.map((n) => n.id)
    enrollTab.value = defaultTab
    enroll.value = await api.post(`/node-groups/${gid.value}/enroll`)
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    enrolling.value = false
  }
}
async function quickSwitchMode(mode: 'observe' | 'legacy' | 'managed') {
  switchingMode.value = true
  try {
    await api.put(`/node-groups/${gid.value}/management-mode`, { mode })
    message.success(mode === 'observe' ? '已切换为“观察接入”，节点上线不会覆盖本地配置' : mode === 'legacy' ? '已切换为“兼容配置（原生 Lua）”' : '已切换为“控制台托管”，请前往发布页预览并显式发布')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    switchingMode.value = false
  }
}
function copy(text: string) {
  navigator.clipboard?.writeText(text).then(() => message.success('已复制'))
}

// 独立节点接管向导（4 步串起 观察/兼容 → 自检与注册 → 导出导入 → 切管发布）
const takeoverOpen = ref(false)
const takeoverStep = ref(0)
const takeoverGoal = ref<'observe' | 'legacy'>('observe')
const takeoverEnroll = ref<any>(null)
const takeoverBaseIds = ref<number[]>([])
const takeoverJoined = computed(() => {
  const base = new Set(takeoverBaseIds.value)
  return nodes.value.filter((n) => !base.has(n.id))
})
function openTakeover() {
  takeoverStep.value = 0
  takeoverGoal.value = managementMode.value?.mode === 'legacy' ? 'legacy' : 'observe'
  takeoverEnroll.value = null
  takeoverBaseIds.value = nodes.value.map((n) => n.id)
  takeoverOpen.value = true
}
async function takeoverApplyModeAndNext() {
  if (managementMode.value?.mode !== takeoverGoal.value) {
    await quickSwitchMode(takeoverGoal.value)
  }
  takeoverBaseIds.value = nodes.value.map((n) => n.id)
  try {
    takeoverEnroll.value = await api.post(`/node-groups/${gid.value}/enroll`)
    takeoverStep.value = 1
  } catch (e) {
    message.error(errMsg(e))
  }
}
const preflightCmd = computed(() => {
  const p = profile.value || {}
  if (runtimeEnv.value === 'docker') {
    const svc = dockerService.value.trim() || brand.id
    return `docker compose exec ${svc} ${brand.agent} preflight${p.instance ? ` -instance ${p.instance}` : ''}`
  }
  if (p.mode === 'user' && p.root) return `${p.root}/bin/${brand.agent} preflight -root ${p.root}${p.instance ? ` -instance ${p.instance}` : ''}`
  return `sudo ${brand.agent} preflight${p.instance ? ` -instance ${p.instance}` : ''}`
})
const exportCmd = computed(() => {
  const p = profile.value || {}
  const inst = p.instance ? ` -instance ${p.instance}` : ''
  const outName = `/tmp/${brand.id}-${p.instance || 'default'}-export.tar.gz`
  if (runtimeEnv.value === 'docker') {
    const svc = dockerService.value.trim() || brand.id
    return `docker compose exec ${svc} ${brand.agent} export-config${inst} --output ${outName} && docker compose cp ${svc}:${outName} ./${brand.id}-export.tar.gz`
  }
  if (p.mode === 'user' && p.root) return `${p.root}/bin/${brand.agent} export-config -root ${p.root}${inst} --output ${outName}`
  return `sudo ${brand.agent} export-config${inst} --output ${outName}`
})

// 节点组设置（含实例档案）
const editing = ref(false)
const gform = reactive({ name: '', description: '', allowed_cidrs: '', real_ip_from: '' })
const modeForm = ref('managed')
const pform = reactive({ instance: '', mode: 'system', root: '', resource_mode: 'shared', http_port: 80, https_port: 443, status_port: 8081, nofile: 0 })
const locked = computed(() => nodes.value.length > 0)
watch(() => pform.instance, (name) => {
  if (!name && !locked.value) pform.resource_mode = 'shared'
})
function openGroup() {
  Object.assign(gform, { name: group.value.name, description: group.value.description, allowed_cidrs: group.value.allowed_cidrs, real_ip_from: group.value.real_ip_from })
  const p = profile.value || {}
  const e = p.effective || {}
  Object.assign(pform, { instance: p.instance || '', mode: p.mode || 'system', root: p.root || '', resource_mode: p.resource_mode || 'shared', http_port: e.http_port, https_port: e.https_port, status_port: e.status_port, nofile: p.nofile || 0 })
  modeForm.value = managementMode.value?.mode || group.value?.management_mode || 'managed'
  editing.value = true
}
function profileChanged() {
  const p = profile.value || {}
  const e = p.effective || {}
  return (
    pform.instance !== (p.instance || '') ||
    pform.mode !== (p.mode || 'system') ||
    pform.root !== (p.root || '') ||
    pform.resource_mode !== (p.resource_mode || 'shared') ||
    pform.http_port !== e.http_port ||
    pform.https_port !== e.https_port ||
    pform.status_port !== e.status_port ||
    pform.nofile !== (p.nofile || 0)
  )
}
// 新建节点组（支持一步展开填写接管方式与实例档案）
const creating = ref(false)
const cform = reactive({ name: '', description: '', management_mode: 'managed', instance: '', mode: 'system', root: '', http_port: 80, https_port: 443, status_port: 8081 })
function openCreate() {
  Object.assign(cform, { name: '', description: '', management_mode: 'managed', instance: '', mode: 'system', root: '', http_port: 80, https_port: 443, status_port: 8081 })
  creating.value = true
}
async function createGroup() {
  try {
    const base = group.value || {}
    const g = await api.post('/node-groups', { name: cform.name.trim(), description: cform.description, allowed_cidrs: base.allowed_cidrs || '', real_ip_from: base.real_ip_from || '' })
    if (cform.instance || cform.mode === 'user' || cform.root || cform.http_port !== 80 || cform.https_port !== 443 || cform.status_port !== 8081) {
      const body: any = { instance: cform.instance, http_port: cform.http_port, https_port: cform.https_port, status_port: cform.status_port }
      if (cform.mode === 'user' || cform.root) Object.assign(body, { mode: cform.mode, root: cform.root })
      await api.put(`/node-groups/${g.id}/profile`, body)
    }
    if (cform.management_mode !== 'managed') {
      await api.put(`/node-groups/${g.id}/management-mode`, { mode: cform.management_mode })
    }
    creating.value = false
    ws.setGroup(g.id)
    view.value = 'group'
    await load()
    message.success(`已创建节点组 ${g.name}`)
  } catch (e) {
    message.error(errMsg(e))
  }
}

function confirmSwitchManaged(): Promise<boolean> {
  return new Promise((resolve) => {
    Modal.confirm({
      title: '确认切回“控制台托管”？',
      content: '切回后仍不会自动发布，但之后的显式发布会覆盖节点本地配置。请确认已完成导入核对并保留了备份。',
      okText: '确认切回',
      cancelText: '取消',
      onOk: () => resolve(true),
      onCancel: () => resolve(false),
    })
  })
}

const nameRe = /^[a-z0-9][a-z0-9-]{1,62}$/
async function saveGroup() {
  const changingMode = modeForm.value !== (managementMode.value?.mode || group.value?.management_mode || 'managed')
  if (changingMode && modeForm.value === 'managed') {
    const ok = await confirmSwitchManaged()
    if (!ok) return
  }
  try {
    await api.put(`/node-groups/${gid.value}`, { ...gform, name: gform.name.trim() })
    if (profileChanged()) {
      const body: any = { instance: pform.instance, http_port: pform.http_port, https_port: pform.https_port, status_port: pform.status_port }
      if (pform.mode === 'user' || pform.root) Object.assign(body, { mode: pform.mode, root: pform.root })
      if (pform.resource_mode === 'independent') body.resource_mode = 'independent'
      if (pform.nofile) body.nofile = pform.nofile
      await api.put(`/node-groups/${gid.value}/profile`, body)
    }
    if (changingMode) await api.put(`/node-groups/${gid.value}/management-mode`, { mode: modeForm.value })
    message.success(modeForm.value === 'managed' ? '已保存；切回托管不会自动发布，请先预览并确认后再发布' : '已保存；当前节点配置不会被控制台自动覆盖')
    editing.value = false
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

// 删除节点组
const removing = ref<any>(null)
const confirmName = ref('')
const usageItems = computed(() => {
  const u = removing.value
  if (!u) return []
  return [
    ['站点', u.sites],
    ['上游', u.upstreams],
    ['四层服务', u.streams],
    ['自定义片段', u.snippets],
    ['IP 集', u.ip_sets],
    ['静态资源包', u.static_bundles],
    ['配置导入记录', u.imports],
    ['配置版本', u.revisions],
    ['发布记录', u.deployments],
    ['已吊销的节点记录', u.revoked_nodes],
  ].filter(([, n]) => n > 0) as [string, number][]
})
const blocker = computed(() => {
  const u = removing.value
  if (!u) return ''
  if (u.last) return '这是最后一个节点组，不能删除。'
  if (u.nodes > 0) return `组内还有 ${u.nodes} 个未吊销的节点，请先在节点列表中吊销（吊销后数据面继续以当前配置运行，只是不再受控制台管理）。`
  if (u.busy) return `该节点组${u.busy}，请等待完成或取消后再删除。`
  return ''
})
async function openDelete() {
  confirmName.value = ''
  try {
    removing.value = await api.get(`/node-groups/${gid.value}/usage`)
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function deleteGroup() {
  const name = group.value?.name
  try {
    await api.del(`/node-groups/${gid.value}`, { confirm: confirmName.value })
    removing.value = null
    editing.value = false
    message.success(`已删除节点组 ${name}`)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

async function restore(n: any) {
  try {
    const d = await api.post(`/nodes/${n.id}/restore`)
    message.success(`正在把 ${n.name} 恢复为 ${d.revision_id}，可在“发布”页查看进度`, 5)
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

// 漂移详情与采纳
const drift = ref<any>(null)
const driftNode = ref<any>(null)
const adoptRes = ref<any>(null)
async function showDrift(n: any) {
  driftNode.value = n
  adoptRes.value = null
  try {
    drift.value = await api.get(`/nodes/${n.id}/drift`)
  } catch (e) {
    drift.value = { files: [], error: errMsg(e) }
  }
}
async function adopt() {
  try {
    adoptRes.value = await api.post(`/nodes/${driftNode.value.id}/drift/adopt`, {})
    message.success(`已提交 ${adoptRes.value.snippets.length} 个片段，等待另一人审批后随下一次发布生效`, 5)
    ws.loadBadges()
  } catch (e) {
    message.error(errMsg(e))
  }
}

async function revoke(n: any) {
  await api.post(`/nodes/${n.id}/revoke`)
  message.success(`已吊销 ${n.name}，其证书立即失效。如需重新接入：点“添加节点”，在该机器上执行新生成的命令`, 6)
  load()
  ws.loadBadges()
}

const columns = [
  { title: '节点', key: 'name' },
  { title: '状态', key: 'status', width: 110 },
  { title: '当前版本', key: 'rev', width: 130 },
  { title: '数据面', key: 'dataplane' },
  { title: '负载', key: 'load', width: 210 },
  { title: '最后心跳', key: 'seen', width: 120 },
  { title: '', key: 'ops', width: 80 },
]
function ago(t?: string) {
  if (!t) return '—'
  const s = dayjs().diff(dayjs(t), 'second')
  return s < 60 ? `${s} 秒前` : s < 3600 ? `${Math.floor(s / 60)} 分钟前` : dayjs(t).format('MM-DD HH:mm')
}
function mb(b: number) {
  return (b / 1024 / 1024 / 1024).toFixed(1) + ' GB'
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>节点</h1>
        <p>节点上的 {{ brand.agent }} 只向控制台发起连接，节点不需要开放任何管理端口。</p>
      </div>
      <a-space wrap>
        <a-segmented v-model:value="view" :options="[{ label: '按节点组', value: 'group' }, { label: '按主机', value: 'host' }]" />
        <a-button v-if="session.can('admin')" :disabled="!session.writable" @click="openCreate">新建节点组</a-button>
        <a-button v-if="session.can('operator')" @click="openGroup">节点组设置</a-button>
        <a-button v-if="session.can('operator')" :disabled="!session.writable" @click="openTakeover">接管独立节点</a-button>
        <a-button v-if="session.can('operator')" type="primary" :loading="enrolling" :disabled="!session.writable" @click="addNode('new')">添加节点</a-button>
      </a-space>
    </div>

    <div v-if="view === 'host'" class="panel">
      <p class="sub" style="margin-top: 0">同一台主机（按机器标识识别）上的实例共用一个 {{ brand.id }} 二进制；每个实例属于一个节点组，端口互不冲突。</p>
      <a-empty v-if="!hosts.length" description="还没有节点" />
      <div v-for="h in hosts" :key="h.host" class="hostcard">
        <div class="hhead">
          <b>{{ h.name || h.host }}</b> <span class="sub code">{{ h.host }}</span>
          <a-tag v-if="h.mixed" color="gold" style="margin-left: 8px">实例版本不一致：{{ h.versions.join('、') }}</a-tag>
        </div>
        <a-table :data-source="h.nodes" row-key="id" :pagination="false" size="small"
          :columns="[{ title: '节点', dataIndex: 'name', width: '26%' }, { title: '节点组', dataIndex: 'group_name', width: '16%' }, { title: '实例', key: 'inst', width: '14%' }, { title: 'HTTP / HTTPS', key: 'ports', width: '18%' }, { title: '运行版本', key: 'ver', width: '14%' }, { title: '状态', key: 'st' }]" table-layout="fixed">
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'inst'">{{ record.instance || '默认' }}<a-tag v-if="record.mode === 'user'" style="margin-left: 6px">user</a-tag></template>
            <template v-else-if="column.key === 'ports'"><span class="num">{{ record.http_port }} / {{ record.https_port }}</span></template>
            <template v-else-if="column.key === 'ver'">{{ record.running_version || '—' }}</template>
            <template v-else-if="column.key === 'st'"><a-badge :status="record.status === 'online' ? 'success' : 'default'" :text="record.status === 'online' ? '在线' : '离线'" /></template>
          </template>
        </a-table>
      </div>
    </div>

    <div class="panel" v-if="view === 'group'">
      <div class="gsum" v-if="group">
        <span>节点组 <b>{{ group.name }}</b></span>
        <span>组版本 <b class="num">{{ group.current_revision_id || '未发布' }}</b></span>
        <span>允许回源网段 <b class="code">{{ group.allowed_cidrs || '未设置' }}</b></span>
        <span v-if="profile">实例档案 <b>{{ instanceLabel }}</b></span>
        <span v-if="managementMode">接入方式 <a-tag :color="managementMode.mode === 'managed' ? 'green' : managementMode.mode === 'legacy' ? 'purple' : 'gold'">{{ managementMode.label }}</a-tag></span>
      </div>
      <a-alert v-if="managementMode?.mode === 'observe'" type="warning" show-icon message="观察接入：节点上线和心跳不会触发配置下发；导入内容只会进入控制台草稿。确认配置后，请在节点组设置切回“控制台托管”，再手动预览、审批和发布。" style="margin-bottom: 14px" />
      <a-alert v-else-if="managementMode?.mode === 'legacy'" type="info" show-icon message="兼容配置：原生配置包由管理员提交，经另一位管理员审批后才会下发；控制台不会自动发布站点草稿。" style="margin-bottom: 14px" />
      <a-alert v-if="managementMode?.mode === 'managed' && managementMode?.handoff_pending" type="warning" show-icon message="等待首次接管发布：节点重连不会自动对齐。核对并备份现网配置后，请显式发布。" style="margin-bottom: 14px" />
      <div class="toolbar">
        <a-input v-model:value="nodeSearch" allow-clear placeholder="按节点名、IP 或版本搜索" style="width: 240px" />
        <a-segmented
          v-model:value="nodeFilter"
          size="small"
          :options="[{ label: `全部 (${nodes.length})`, value: 'all' }, { label: '在线', value: 'online' }, { label: '离线', value: 'offline' }, { label: '配置漂移', value: 'drifted' }]"
        />
      </div>
      <a-table :columns="columns" :data-source="filteredNodes" row-key="id" :pagination="{ pageSize: 20, hideOnSinglePage: true }">
        <template #emptyText>
          <a-empty :description="nodes.length ? '没有匹配的节点' : '这个节点组还没有节点。点击“添加节点”或“接管独立节点”接入。'" />
        </template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <div class="nm">{{ record.name }}</div>
            <div class="sub code">{{ record.remote_addr }}</div>
          </template>
          <template v-else-if="column.key === 'status'">
            <a-badge :status="record.status === 'online' ? 'success' : 'default'" :text="record.status === 'online' ? '在线' : '离线'" />
            <div v-if="record.drifted" class="warn">
              配置被手动改动
              <a v-if="session.can('operator')" class="restore" @click="showDrift(record)">查看差异</a>
              <a-popconfirm v-if="session.can('releaser')" :title="`把 ${record.name} 恢复为控制台下发的 ${record.applied_revision}？节点上的改动会备份在版本目录旁，不会丢失。`" ok-text="恢复" @confirm="restore(record)">
                <a class="restore">恢复</a>
              </a-popconfirm>
            </div>
          </template>
          <template v-else-if="column.key === 'rev'">
            <span class="num">{{ record.applied_revision || '—' }}</span>
            <a-tag v-if="group?.current_revision_id && record.applied_revision !== group.current_revision_id && record.status === 'online'" color="gold" style="margin-left: 6px">落后</a-tag>
          </template>
          <template v-else-if="column.key === 'dataplane'">
            <div>{{ dataplaneVersion(record) || '—' }}<a-tooltip v-if="record.running_version && dataplaneVersion(record) && !dataplaneVersion(record).includes(record.running_version)" :title="`本实例仍在运行 ${record.running_version}：同机其他实例已升级了共用的二进制`"><a-tag color="gold" class="gm">运行 {{ record.running_version }}</a-tag></a-tooltip><a-tag v-if="record.tls_lib === 'tongsuo'" color="red" class="gm">国密</a-tag><a-tooltip v-if="record.lua_addon" :title="`${brand.lua} ${record.lua_addon}`"><a-tag color="purple" class="gm">Lua</a-tag></a-tooltip></div>
            <div class="sub">agent {{ record.agent_version }}</div>
          </template>
          <template v-else-if="column.key === 'load'">
            <template v-if="record.status === 'online'">
              <div class="num">CPU {{ Math.round(record.cpu_percent) }}%，内存 {{ mb(record.mem_used_bytes) }}</div>
              <div class="sub num">{{ record.active_connections }} 个连接</div>
            </template>
            <span v-else>—</span>
          </template>
          <template v-else-if="column.key === 'seen'">{{ ago(record.last_seen_at) }}</template>
          <template v-else-if="column.key === 'ops'">
            <a-popconfirm v-if="session.can('admin')" title="吊销后该节点证书立即失效，需重新注册。确定吊销？" ok-text="吊销" @confirm="revoke(record)">
              <a-button type="link" danger size="small">吊销</a-button>
            </a-popconfirm>
          </template>
        </template>
      </a-table>
    </div>

    <a-modal :open="!!enroll" title="添加节点" :footer="null" width="740px" @cancel="enroll = null">
      <template v-if="enroll">
        <a-alert
          v-if="newlyJoinedNodes.length"
          type="success"
          show-icon
          style="margin-bottom: 12px"
          :message="`检测到新节点已上线：${newlyJoinedNodes.map((n) => n.name).join('、')}`"
        />
        <div class="env-bar">
          <a-segmented v-model:value="runtimeEnv" size="small" :options="[{ label: '宿主机（systemd / user）', value: 'host' }, { label: 'Docker 容器', value: 'docker' }]" />
          <a-input v-if="runtimeEnv === 'docker'" v-model:value="dockerService" size="small" class="code" style="width: 150px" placeholder="Compose 服务名" />
        </div>
        <a-alert v-if="runtimeEnv === 'docker'" type="info" show-icon style="margin-bottom: 12px"
          message="Docker 容器接入提示：请确保 docker-compose.yml 已挂载 ./agent:<前缀>/agent 卷（持久化节点证书与 machine-id，重建容器不掉线），注册后容器入口会自动拉起 agent，无需执行 systemctl。" />
        <a-tabs v-model:active-key="enrollTab" :items="[{ key: 'new', label: '全新节点 / 新建实例' }, { key: 'existing', label: '已有独立实例接入' }]" />
        <template v-if="enrollTab === 'new'">
          <p v-if="runtimeEnv === 'docker'">在宿主机 Compose 目录执行下面的命令（容器需已启动且镜像含 {{ brand.agent }}）。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。</p>
          <p v-else-if="enroll.mode === 'user'">在边缘服务器上以安装用户（不需要 root，也不需要 sudo）执行下面的命令。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。
            新服务器请先解压 user 模式节点包，用 <span class="code">./install-user.sh {{ enroll.root }} --http {{ enroll.http_port }} --https {{ enroll.https_port }} --status {{ enroll.status_port }} --console {{ enroll.server }} --token &lt;令牌&gt; --ca-fingerprint &lt;指纹&gt;</span> 一步安装并注册。</p>
          <p v-else-if="enroll.instance">在边缘服务器上（已安装 {{ brand.id }} 与 {{ brand.agent }} 软件包）以 root 执行下面的命令：按节点组的实例档案创建实例 <b>{{ enroll.instance }}</b>（HTTP {{ enroll.http_port }} / HTTPS {{ enroll.https_port }}）并注册。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。</p>
          <p v-else>在边缘服务器上（已安装 {{ brand.id }} 与 {{ brand.agent }} 软件包）以 root 执行下面的命令。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。</p>
          <pre class="cmd code">{{ runtimeEnv === 'docker' ? dockerEnrollCmd(enroll, false) : enroll.command }}</pre>
          <a-button type="primary" @click="copy(runtimeEnv === 'docker' ? dockerEnrollCmd(enroll, false) : enroll.command)">复制新建命令</a-button>
        </template>
        <template v-else>
          <a-alert
            v-if="managementMode?.mode === 'managed' && group?.current_revision_id"
            type="error"
            show-icon
            style="margin-bottom: 12px"
            message="注意：当前节点组是“控制台托管”且已有线上版本"
            description="独立节点直接加入后，上线时会自动对齐组内版本，可能覆盖节点本地配置！建议先将本组切换为“观察接入”，完成导入核对后再显式切回托管。"
          >
            <template #action>
              <a-button size="small" type="primary" :loading="switchingMode" @click="quickSwitchMode('observe')">切换为观察接入</a-button>
            </template>
          </a-alert>
          <p class="sub">① 建议先执行自检，核对控制台实例档案（特别是 Docker 镜像的编译前缀目录）与是否有原生 Lua：</p>
          <pre class="cmd code">{{ preflightCmd }}</pre>
          <a-button size="small" @click="copy(preflightCmd)">复制自检命令</a-button>
          <p class="sub" style="margin-top: 14px">② 执行注册命令（检测到本地改动会自动备份当前版本目录）：</p>
          <pre class="cmd code">{{ runtimeEnv === 'docker' ? dockerEnrollCmd(enroll, true) : enroll.existing_command }}</pre>
          <a-button type="primary" @click="copy(runtimeEnv === 'docker' ? dockerEnrollCmd(enroll, true) : enroll.existing_command)">复制已有实例注册命令</a-button>
        </template>
        <a-descriptions :column="1" size="small" style="margin-top: 18px">
          <a-descriptions-item label="控制台地址"><span class="code">{{ enroll.server }}</span></a-descriptions-item>
          <a-descriptions-item label="控制台 CA 指纹"><span class="code">{{ enroll.ca_fingerprint }}</span></a-descriptions-item>
        </a-descriptions>
      </template>
    </a-modal>

    <a-drawer v-model:open="takeoverOpen" width="760" title="接管独立部署的数据面节点">
      <div class="env-bar">
        <a-segmented v-model:value="runtimeEnv" size="small" :options="[{ label: '宿主机（systemd / user）', value: 'host' }, { label: 'Docker 容器', value: 'docker' }]" />
        <a-input v-if="runtimeEnv === 'docker'" v-model:value="dockerService" size="small" class="code" style="width: 150px" placeholder="Compose 服务名" />
      </div>
      <a-alert v-if="runtimeEnv === 'docker'" type="info" show-icon style="margin-bottom: 14px"
        message="Docker 独立部署请先在 docker-compose.yml 中挂载 ./agent:<前缀>/agent 卷（保存节点证书与 machine-id），重建容器不掉线。" />
      <a-steps :current="takeoverStep" size="small" style="margin-bottom: 20px" :items="[{ title: '选择保护模式' }, { title: '自检与注册' }, { title: '导出与导入' }, { title: '切回托管发布' }]" />
      <template v-if="takeoverStep === 0">
        <a-alert type="info" show-icon style="margin-bottom: 14px" message="先在“观察接入”或“兼容配置”模式下接入节点，可确保控制台绝不覆盖节点现网流量配置。" />
        <a-form layout="vertical">
          <a-form-item label="目标接管方式">
            <a-radio-group v-model:value="takeoverGoal">
              <div style="display: grid; gap: 10px">
                <a-radio value="observe"><b>观察接入 → 平滑迁移为可视化托管</b>（推荐：先导入原配置并核对草稿，确认无误后再切回托管并显式发布）</a-radio>
                <a-radio value="legacy"><b>兼容配置（原生 Lua）</b>（原配置含复杂原生 Lua / 第三方库，保留完整配置包由双管理员审批发布）</a-radio>
              </div>
            </a-radio-group>
          </a-form-item>
        </a-form>
        <a-button type="primary" :loading="switchingMode" @click="takeoverApplyModeAndNext">应用模式并生成注册命令</a-button>
      </template>
      <template v-else-if="takeoverStep === 1">
        <a-alert
          v-if="takeoverJoined.length"
          type="success"
          show-icon
          style="margin-bottom: 12px"
          :message="`节点已成功接入：${takeoverJoined.map((n) => n.name).join('、')}`"
        />
        <p class="sub">1. 在节点执行自检，确认安装目录、端口与 Lua 情况：</p>
        <pre class="cmd code">{{ preflightCmd }}</pre>
        <a-button size="small" @click="copy(preflightCmd)">复制自检命令</a-button>
        <p class="sub" style="margin-top: 14px">2. 执行已有实例注册命令上线（不会下发配置）：</p>
        <pre v-if="takeoverEnroll" class="cmd code">{{ runtimeEnv === 'docker' ? dockerEnrollCmd(takeoverEnroll, true) : takeoverEnroll.existing_command }}</pre>
        <a-space>
          <a-button v-if="takeoverEnroll" @click="copy(runtimeEnv === 'docker' ? dockerEnrollCmd(takeoverEnroll, true) : takeoverEnroll.existing_command)">复制注册命令</a-button>
          <a-button type="primary" @click="takeoverStep = 2">下一步：导出与导入配置</a-button>
        </a-space>
      </template>
      <template v-else-if="takeoverStep === 2">
        <p>在节点执行一键扫描导出命令（自动识别 include、证书与 Lua 引用并生成规范压缩包）：</p>
        <pre class="cmd code">{{ exportCmd }}</pre>
        <a-space style="margin-bottom: 16px">
          <a-button @click="copy(exportCmd)">复制导出命令</a-button>
        </a-space>
        <p class="sub">将导出的 tar.gz（或 <span class="code">sudo {{ brand.id }} -T</span> 输出）下载到本地后，前往控制台上传：</p>
        <a-space>
          <a-button v-if="takeoverGoal === 'observe'" type="primary" @click="takeoverOpen = false; router.push({ name: 'imports', query: { group: gid } })">前往“配置导入”</a-button>
          <a-button v-else type="primary" @click="takeoverOpen = false; router.push({ name: 'releases', query: { group: gid } })">前往“发布”提交兼容包</a-button>
          <a-button v-if="takeoverGoal === 'observe'" @click="takeoverStep = 3">已导入并核对完毕，下一步</a-button>
        </a-space>
      </template>
      <template v-else>
        <a-alert type="warning" show-icon style="margin-bottom: 14px" message="切回“控制台托管”后仍不会自动覆盖节点，直到你在发布页确认接管检查单并显式发布。" />
        <a-space>
          <a-button type="primary" :loading="switchingMode" @click="quickSwitchMode('managed').then(() => { takeoverOpen = false; router.push({ name: 'releases', query: { group: gid } }) })">切回托管并前往首次发布</a-button>
          <a-button @click="takeoverOpen = false">稍后再切</a-button>
        </a-space>
      </template>
    </a-drawer>

    <a-modal v-model:open="creating" title="新建节点组" ok-text="创建" width="600px" :ok-button-props="{ disabled: !/^[a-z0-9][a-z0-9-]{1,62}$/.test(cform.name.trim()) }" @ok="createGroup">
      <p class="sub" style="margin-top: 0">一个节点组对应一套配置。若要接管已有独立数据面，建议直接把接入方式设为“观察接入”或“兼容配置”。</p>
      <a-form layout="vertical">
        <a-form-item label="名称" extra="小写字母、数字和短横线，2–63 个字符，例如 edge-blue。">
          <a-input v-model:value="cform.name" placeholder="edge-blue" />
        </a-form-item>
        <a-form-item label="说明（可选）">
          <a-input v-model:value="cform.description" />
        </a-form-item>
        <a-form-item label="配置接管方式">
          <a-radio-group v-model:value="cform.management_mode">
            <a-radio value="managed">控制台托管</a-radio>
            <a-radio value="observe">观察接入（保护现网）</a-radio>
            <a-radio value="legacy">兼容配置（原生 Lua）</a-radio>
          </a-radio-group>
        </a-form-item>
        <details class="adv">
          <summary>实例档案设置（同机多实例 / user 模式 / 自定义端口时展开）</summary>
          <div style="margin-top: 10px">
            <a-form-item label="实例名（留空为默认实例）"><a-input v-model:value="cform.instance" class="code" placeholder="默认实例" /></a-form-item>
            <a-form-item label="运行方式">
              <a-radio-group v-model:value="cform.mode">
                <a-radio value="system">system</a-radio>
                <a-radio value="user">user（非 root）</a-radio>
              </a-radio-group>
            </a-form-item>
            <a-form-item label="安装根目录（可选）"><a-input v-model:value="cform.root" class="code" :placeholder="cform.mode === 'user' ? '/home/ops/edge' : `/opt/${brand.id}`" /></a-form-item>
            <a-space wrap>
              <a-form-item label="HTTP"><a-input-number v-model:value="cform.http_port" :min="1" :max="65535" /></a-form-item>
              <a-form-item label="HTTPS"><a-input-number v-model:value="cform.https_port" :min="1" :max="65535" /></a-form-item>
              <a-form-item label="状态页"><a-input-number v-model:value="cform.status_port" :min="1" :max="65535" /></a-form-item>
            </a-space>
          </div>
        </details>
      </a-form>
    </a-modal>

    <a-modal v-model:open="editing" title="节点组设置" ok-text="保存" :ok-button-props="{ disabled: !nameRe.test(gform.name.trim()) }" @ok="saveGroup">
      <a-form layout="vertical">
        <a-form-item label="名称" extra="改名只影响显示，已注册的节点、站点与已发布的版本不受影响。">
          <a-input v-model:value="gform.name" class="code" />
        </a-form-item>
        <a-form-item label="说明"><a-input v-model:value="gform.description" /></a-form-item>
        <a-form-item label="允许回源网段" extra="上游服务器地址必须落在这些网段内（逗号分隔 CIDR）。回环与云元数据地址始终禁止。">
          <a-input v-model:value="gform.allowed_cidrs" class="code" />
        </a-form-item>
        <a-form-item label="CDN / 负载均衡回源网段" :extra="`填写后，${brand.id} 从 X-Forwarded-For 中取客户端真实 IP。`">
          <a-input v-model:value="gform.real_ip_from" class="code" placeholder="例如 10.0.0.0/8" />
        </a-form-item>
        <a-divider orientation="left" style="font-size: 13px">实例档案</a-divider>
        <p class="sub" style="margin-top: -8px">组内所有节点使用同一个档案。同一台服务器上运行多套 {{ brand.id }} 时，每套属于不同的节点组，实例名与端口不能冲突。{{ locked ? '组内已有节点：实例名、运行方式、安装目录与共享方式不能再改，端口可以改（改后需在各节点上按新端口重建实例）。' : '' }}</p>
        <a-form-item label="实例名" extra="留空为默认实例；具名实例只能用小写字母、数字和短横线。">
          <a-input v-model:value="pform.instance" class="code" :disabled="locked" placeholder="默认实例" />
        </a-form-item>
        <a-form-item label="运行方式">
          <a-radio-group v-model:value="pform.mode" :disabled="locked">
            <a-radio value="system">system（root 安装软件包）</a-radio>
            <a-radio value="user">user（普通用户安装，不需要 root）</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="数据面与模块" extra="共享：与同目录共享实例一起安装和升级。独立：本实例的 Lua 和标准版 / 国密版可单独安装、升级；仅新建具名实例可选择。">
          <a-radio-group v-model:value="pform.resource_mode" :disabled="locked">
            <a-radio value="shared">共享</a-radio>
            <a-radio value="independent" :disabled="!pform.instance">独立</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-divider orientation="left" style="font-size: 13px">配置接管方式</a-divider>
        <a-form-item label="节点配置来源" extra="观察接入不会下发配置；兼容配置需经两位管理员提交和审批后下发。切换模式后需显式发布才会自动对齐。">
          <a-radio-group v-model:value="modeForm">
            <a-radio value="managed">控制台托管</a-radio>
            <a-radio value="observe">观察接入</a-radio>
            <a-radio value="legacy">兼容配置（原生 Lua）</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="安装根目录" :extra="pform.mode === 'user' ? '节点上普通用户的安装目录（绝对路径），user 模式只能使用 1024 及以上的端口。' : `system 模式默认使用 /opt/${brand.id}；填写后，软件包会把程序与实例数据放在该绝对路径，并写入 /etc/${brand.id}/prefix。`">
          <a-input v-model:value="pform.root" class="code" :disabled="locked" :placeholder="pform.mode === 'user' ? '/home/ops/edge' : `/data/${brand.id}`" />
        </a-form-item>
        <a-space wrap>
          <a-form-item label="HTTP 端口"><a-input-number v-model:value="pform.http_port" :min="1" :max="65535" /></a-form-item>
          <a-form-item label="HTTPS 端口"><a-input-number v-model:value="pform.https_port" :min="1" :max="65535" /></a-form-item>
          <a-form-item label="状态页端口（仅本机）"><a-input-number v-model:value="pform.status_port" :min="1" :max="65535" /></a-form-item>
        </a-space>
        <p v-if="pform.http_port !== 80" class="sub">HTTP 端口不是 80：ACME 自动签发需要前置负载均衡把域名的 80 端口转发到 {{ pform.http_port }}，否则请使用上传证书。</p>
        <template v-if="session.can('admin')">
          <a-divider orientation="left" style="font-size: 13px">删除节点组</a-divider>
          <p class="sub" style="margin-top: -8px">删除节点组会同时删除组内的站点、上游、版本与发布记录；组内节点须先全部吊销。证书库与审计日志不受影响。</p>
          <a-button danger :disabled="!session.writable" @click="openDelete">删除节点组…</a-button>
        </template>
      </a-form>
    </a-modal>
    <a-modal :open="!!removing" :title="`删除节点组 ${group?.name || ''}`" ok-text="删除" :ok-button-props="{ danger: true, disabled: !!blocker || confirmName !== group?.name }" @ok="deleteGroup" @cancel="removing = null">
      <template v-if="removing">
        <a-alert v-if="blocker" type="warning" show-icon :message="blocker" />
        <template v-else>
          <p>删除后不能恢复。以下内容会随节点组一起删除：</p>
          <ul v-if="usageItems.length" class="usage">
            <li v-for="[k, n] in usageItems" :key="k">{{ k }} <b class="num">{{ n }}</b></li>
          </ul>
          <p v-else class="sub">这个节点组是空的。</p>
          <a-form layout="vertical">
            <a-form-item :label="`输入节点组名称 ${group?.name} 以确认`">
              <a-input v-model:value="confirmName" class="code" />
            </a-form-item>
          </a-form>
        </template>
      </template>
    </a-modal>
    <a-drawer :open="!!drift" :width="760" :title="`${driftNode?.name} 的手工改动`" @close="drift = null">
      <template v-if="drift">
        <a-alert v-if="drift.error" type="info" show-icon :message="drift.error" />
        <template v-else>
          <p class="sub">基于版本 {{ drift.revision_id }}，上报于 {{ drift.reported_at }}。节点只上报受管版本目录内的文件；私钥与超大文件只比较哈希。</p>
          <div v-for="f in drift.files" :key="f.path" class="dfile">
            <div><a-tag>{{ { modified: '修改', added: '新增', removed: '删除' }[f.change as string] }}</a-tag><span class="code">{{ f.path }}</span></div>
            <DiffView v-if="f.diff && f.diff.startsWith('---')" :diff="f.diff" />
            <div v-else class="sub">{{ f.diff }}</div>
          </div>
          <a-space style="margin-top: 12px">
            <a-button v-if="session.can('operator')" type="primary" :disabled="!session.writable" @click="adopt">采纳为片段</a-button>
            <a-popconfirm v-if="session.can('releaser')" :title="`把 ${driftNode?.name} 恢复为 ${driftNode?.applied_revision}？`" ok-text="恢复" @confirm="restore(driftNode); drift = null">
              <a-button>恢复为控制台版本</a-button>
            </a-popconfirm>
          </a-space>
          <p class="sub" style="margin-top: 8px">采纳：站点配置中新增的白名单内指令转为自定义片段（作者记为“节点手工修改 + 采纳人”），需另一人批准并发布；其余改动只能恢复。</p>
          <template v-if="adoptRes">
            <a-alert type="success" show-icon :message="`已提交 ${adoptRes.snippets.length} 个片段到“自定义片段”等待审批`" style="margin-top: 8px" />
            <a-alert v-for="(r, i) in adoptRes.rejected" :key="i" type="warning" :message="r" style="margin-top: 6px" />
          </template>
        </template>
      </template>
    </a-drawer>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  align-items: center;
  margin-bottom: 12px;
}
.env-bar {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  align-items: center;
  margin-bottom: 12px;
}
.adv {
  font-size: 13px;
  color: #445a5f;
}
.adv summary {
  cursor: pointer;
  color: #0e6971;
}
.hostcard {
  margin-bottom: 18px;
}
.hhead {
  margin-bottom: 6px;
}
.gm {
  margin-left: 6px;
  font-size: 11px;
  line-height: 16px;
}
.gsum {
  display: flex;
  flex-wrap: wrap;
  gap: 28px;
  margin-bottom: 14px;
  font-size: 13px;
  color: #6b7c80;
}
.gsum b {
  color: #16323a;
  font-weight: 600;
  margin-left: 6px;
}
.nm {
  font-weight: 600;
  color: #16323a;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
.warn {
  font-size: 12px;
  color: #a3332d;
}
.dfile {
  margin-bottom: 12px;
}
.usage {
  margin: 0 0 16px;
  padding-left: 20px;
  color: #16323a;
}
.usage b {
  margin-left: 6px;
}
.restore {
  margin-left: 6px;
}
.cmd {
  white-space: pre-wrap;
  word-break: break-all;
  background: #16323a;
  color: #d9ecee;
  padding: 14px 16px;
  border-radius: 6px;
  margin: 12px 0;
}
</style>
