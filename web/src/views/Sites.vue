<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import dayjs from 'dayjs'
import { Modal, message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import SiteEditor from '../components/SiteEditor.vue'

const session = useSession()
const ws = useWorkspace()
const route = useRoute()
const tab = ref('sites')
const sites = ref<any[]>([])
const ups = ref<any[]>([])
const search = ref('')
const statusFilter = ref<'all' | 'enabled' | 'disabled'>('all')
const togglingSiteId = ref(0)
const selectedSiteIds = ref<number[]>([])
const expandedRoutes = ref<Record<number, boolean>>({})
function confirmDeleteSite(s: any) {
  Modal.confirm({
    title: `删除站点 ${s.name}？`,
    content: '发布后将从本节点组的所有节点移除。',
    okText: '删除',
    okButtonProps: { danger: true },
    cancelText: '取消',
    onOk: () => removeSite(s),
  })
}
function onSiteMenu(record: any, key: string) {
  if (key === 'dup') openDuplicate(record)
  else if (key === 'copy') openCopySites([record.id])
  else if (key === 'del') confirmDeleteSite(record)
}

// 跨组复制站点（自动携带站点引用的上游、组内 IP 集与静态包）
const copySitesOpen = ref(false)
const copySiteIds = ref<number[]>([])
const copyTargetGroups = ref<number[]>([])
const copyOverwrite = ref(false)
const copyingSites = ref(false)
function openCopySites(ids: number[]) {
  copySiteIds.value = ids
  copyTargetGroups.value = ws.groups.filter((g) => g.id !== gid.value).slice(0, 1).map((g) => g.id)
  copyOverwrite.value = false
  copySitesOpen.value = true
}
async function runCopySites() {
  copyingSites.value = true
  try {
    let createdTotal = 0
    let updatedTotal = 0
    const allWarnings: string[] = []
    for (const targetId of copyTargetGroups.value) {
      const r = await api.post(`/node-groups/${targetId}/copy-from`, {
        source_group_id: gid.value,
        site_ids: copySiteIds.value,
        overwrite: copyOverwrite.value,
      })
      createdTotal += r.sites.created
      updatedTotal += r.sites.updated
      const gname = ws.groups.find((g) => g.id === targetId)?.name || targetId
      for (const w of r.warnings || []) allWarnings.push(`[${gname}] ${w}`)
    }
    copySitesOpen.value = false
    selectedSiteIds.value = []
    message.success(`已复制到 ${copyTargetGroups.value.length} 个节点组（新增 ${createdTotal}、更新 ${updatedTotal}），连同依赖上游与静态包一并复制`, 6)
    allWarnings.forEach((w) => message.warning(w, 6))
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    copyingSites.value = false
  }
}

const gid = computed(() => ws.groupId)

async function load() {
  if (!ws.groups.length) await ws.loadGroups(Number(route.query.group) || undefined)
  if (!gid.value) return
  ;[sites.value, ups.value] = await Promise.all([api.get('/sites', { group_id: gid.value }), api.get('/upstreams', { group_id: gid.value })])
}
watch(gid, () => {
  selectedSiteIds.value = []
  load()
})
function syncRouteQuery() {
  if (route.query.tab === 'ups') tab.value = 'ups'
  else if (route.query.tab === 'sites') tab.value = 'sites'
  if (route.query.group) ws.setGroup(Number(route.query.group))
  if (typeof route.query.q === 'string') search.value = route.query.q
}
watch(() => route.query, syncRouteQuery)
onMounted(async () => {
  syncRouteQuery()
  await load()
})

// 行内一键启停站点
async function toggleSite(s: any) {
  togglingSiteId.value = s.id
  try {
    await api.put(`/sites/${s.id}`, { version: s.version, enabled: !s.enabled, spec: s.spec })
    ws.markDraftDirty()
    message.success(`站点 ${s.name} 已${s.enabled ? '停用' : '启用'}（草稿，发布后生效）`)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    togglingSiteId.value = 0
  }
}

// 批量启停已选站点
async function batchToggleSites(enable: boolean) {
  const targets = sites.value.filter((s) => selectedSiteIds.value.includes(s.id) && s.enabled !== enable)
  let ok = 0
  for (const s of targets) {
    try {
      await api.put(`/sites/${s.id}`, { version: s.version, enabled: enable, spec: s.spec })
      ok++
    } catch (e) {
      message.warning(`${s.name}：${errMsg(e)}`)
    }
  }
  if (ok) {
    ws.markDraftDirty()
    message.success(`已批量${enable ? '启用' : '停用'} ${ok} 个站点（发布后生效）`)
  }
  selectedSiteIds.value = []
  await load()
}

// 同组克隆站点
const dupOpen = ref(false)
const dupSaving = ref(false)
const dupSource = ref<any>(null)
const dupForm = reactive({ name: '', domains: [] as string[] })
function openDuplicate(s: any) {
  dupSource.value = s
  dupForm.name = `${s.name}-copy`
  dupForm.domains = (s.spec?.domains || []).map((d: string) => `copy-${d}`)
  dupOpen.value = true
}
async function saveDuplicate() {
  if (!dupSource.value) return
  dupSaving.value = true
  try {
    const spec = JSON.parse(JSON.stringify(dupSource.value.spec))
    spec.name = dupForm.name.trim()
    spec.domains = dupForm.domains.map((d) => d.trim().toLowerCase()).filter(Boolean)
    await api.post('/sites', { group_id: gid.value, enabled: dupSource.value.enabled, spec })
    dupOpen.value = false
    ws.markDraftDirty()
    message.success(`已克隆站点 ${spec.name}`)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    dupSaving.value = false
  }
}

// 上游单后端一键摘流 / 恢复
async function toggleServerDown(u: any, idx: number) {
  const spec = JSON.parse(JSON.stringify(u.spec))
  spec.servers[idx].down = !spec.servers[idx].down
  try {
    await api.put(`/upstreams/${u.id}`, { group_id: gid.value, spec })
    ws.markDraftDirty()
    message.success(`${spec.servers[idx].addr} 已${spec.servers[idx].down ? '标记摘流' : '恢复流量'}（发布后生效）`)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const filteredSites = computed(() => {
  const q = search.value.trim().toLowerCase()
  return sites.value.filter((s) => {
    if (statusFilter.value === 'enabled' && !s.enabled) return false
    if (statusFilter.value === 'disabled' && s.enabled) return false
    if (!q) return true
    return (
      s.name.toLowerCase().includes(q) ||
      (s.spec?.domains || []).some((d: string) => d.toLowerCase().includes(q)) ||
      (s.spec?.routes || []).some((r: any) => r.proxy?.upstream?.toLowerCase().includes(q))
    )
  })
})
const filteredUps = computed(() => {
  const q = search.value.trim().toLowerCase()
  if (!q) return ups.value
  return ups.value.filter((u) => u.name.toLowerCase().includes(q) || (u.spec?.servers || []).some((s: any) => s.addr.toLowerCase().includes(q)))
})

const editorOpen = ref(false)
const editing = ref<any>(null)
function edit(s: any | null) {
  editing.value = s
  editorOpen.value = true
}
function onSiteSaved() {
  editorOpen.value = false
  ws.markDraftDirty()
  load()
}
function onUpstreamCreated() {
  ws.markDraftDirty()
  load()
}
async function removeSite(s: any) {
  try {
    await api.del(`/sites/${s.id}`)
    ws.markDraftDirty()
    message.success(`已删除 ${s.name}，发布后从节点移除`)
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const modeText: Record<string, string> = { static: '静态', proxy: '代理', redirect: '跳转', deny: '拒绝' }
const siteCols = [
  { title: '站点', key: 'name' },
  { title: '路由', key: 'routes' },
  { title: 'HTTPS', key: 'https', width: 110 },
  { title: '状态', key: 'enabled', width: 96 },
  { title: '最后修改', key: 'updated', width: 150 },
  { title: '', key: 'ops', width: 120 },
]

// 上游
interface UpServerRow {
  addr: string
  weight: number
  max_fails: number
  fail_timeout: number
  backup: boolean
  down: boolean
}
const upOpen = ref(false)
const upForm = reactive({
  id: 0,
  name: '',
  algorithm: 'least_conn',
  keepalive: 32,
  servers: [{ addr: '', weight: 0, max_fails: 0, fail_timeout: 0, backup: false, down: false }] as UpServerRow[],
})
const upErr = ref('')
function editUp(u: any | null) {
  upErr.value = ''
  if (u) {
    Object.assign(upForm, {
      id: u.id,
      name: u.name,
      algorithm: u.spec.algorithm || 'round_robin',
      keepalive: u.spec.keepalive || 32,
      servers: (u.spec.servers || []).map((s: any) => ({
        addr: s.addr || '',
        weight: s.weight || 0,
        max_fails: s.max_fails || 0,
        fail_timeout: s.fail_timeout || 0,
        backup: !!s.backup,
        down: !!s.down,
      })),
    })
  } else {
    Object.assign(upForm, {
      id: 0,
      name: '',
      algorithm: 'least_conn',
      keepalive: 32,
      servers: [{ addr: '', weight: 0, max_fails: 0, fail_timeout: 0, backup: false, down: false }],
    })
  }
  upOpen.value = true
}
watch(
  () => upForm.algorithm,
  (algo) => {
    if (algo === 'ip_hash') upForm.servers.forEach((s) => (s.backup = false))
  },
)
async function saveUp() {
  upErr.value = ''
  const spec: any = {
    name: upForm.name,
    algorithm: upForm.algorithm,
    ...(upForm.keepalive && upForm.keepalive !== 32 ? { keepalive: Number(upForm.keepalive) } : {}),
    servers: upForm.servers
      .filter((s) => s.addr.trim())
      .map((s) => ({
        addr: s.addr.trim(),
        weight: Number(s.weight) || 0,
        ...(s.max_fails ? { max_fails: Number(s.max_fails) } : {}),
        ...(s.fail_timeout ? { fail_timeout: Number(s.fail_timeout) } : {}),
        ...(s.backup && upForm.algorithm !== 'ip_hash' ? { backup: true } : {}),
        ...(s.down ? { down: true } : {}),
      })),
  }
  try {
    if (upForm.id) await api.put(`/upstreams/${upForm.id}`, { group_id: gid.value, spec })
    else await api.post('/upstreams', { group_id: gid.value, spec })
    upOpen.value = false
    ws.markDraftDirty()
    message.success('已保存')
    load()
  } catch (e) {
    upErr.value = errMsg(e)
  }
}
async function removeUp(u: any) {
  try {
    await api.del(`/upstreams/${u.id}`)
    ws.markDraftDirty()
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
const algoText: Record<string, string> = { round_robin: '轮询', least_conn: '最少连接', ip_hash: '源 IP 哈希' }
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>站点与上游</h1>
        <p>这里的修改先保存为草稿，到“发布”页确认差异后再下发到节点。</p>
        <router-link v-if="route.query.import" :to="{ path: '/imports', query: { group: gid, import: route.query.import } }">返回导入报告</router-link>
      </div>
      <a-space v-if="session.can('operator')">
        <a-button v-if="tab === 'ups'" type="primary" :disabled="!session.writable" @click="editUp(null)">新建上游</a-button>
        <a-button v-else type="primary" :disabled="!session.writable" @click="edit(null)">新建站点</a-button>
      </a-space>
    </div>

    <a-tabs v-model:active-key="tab" :items="[{ key: 'sites', label: `站点（${sites.length}）` }, { key: 'ups', label: `上游服务（${ups.length}）` }]" />

    <div v-show="tab === 'sites'" class="panel">
      <div class="toolbar">
        <a-input v-model:value="search" allow-clear placeholder="按站点名、域名或上游搜索" style="width: 260px" />
        <a-segmented
          v-model:value="statusFilter"
          size="small"
          :options="[{ label: '全部', value: 'all' }, { label: '启用', value: 'enabled' }, { label: '停用', value: 'disabled' }]"
        />
        <template v-if="session.can('operator') && selectedSiteIds.length">
          <a-button size="small" :disabled="!session.writable" @click="batchToggleSites(true)">批量启用</a-button>
          <a-button size="small" :disabled="!session.writable" @click="batchToggleSites(false)">批量停用</a-button>
          <a-button
            v-if="ws.groups.length > 1"
            size="small"
            :disabled="!session.writable"
            @click="openCopySites(selectedSiteIds)"
          >
            复制已选 {{ selectedSiteIds.length }} 个站点到其他组
          </a-button>
        </template>
      </div>
      <a-table
        :columns="siteCols"
        :data-source="filteredSites"
        row-key="id"
        :scroll="{ x: 'max-content' }"
        :row-selection="session.can('operator') ? { selectedRowKeys: selectedSiteIds, onChange: (keys: any[]) => (selectedSiteIds = keys.map(Number)) } : undefined"
        :pagination="{ pageSize: 20, hideOnSinglePage: true }"
      >
        <template #emptyText><a-empty :description="sites.length ? '没有匹配的站点' : '还没有站点。新建一个站点，填写域名和路由即可。'" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <div class="nm">{{ record.name }}</div>
            <div class="sub">{{ record.spec.domains.join('，') }}</div>
          </template>
          <template v-else-if="column.key === 'routes'">
            <div v-for="r in expandedRoutes[record.id] ? record.spec.routes : record.spec.routes.slice(0, 3)" :key="r.match + r.path" class="rt">
              <span class="code">{{ r.match === 'exact' ? '= ' : '' }}{{ r.path }}</span>
              <span class="mode" :class="r.mode">{{ modeText[r.mode] }}</span>
              <span v-if="r.mode === 'proxy'" class="sub">{{ r.proxy.https ? 'https://' : '' }}{{ r.proxy.upstream }}{{ r.proxy.strip_prefix ? '（剥离前缀）' : '' }}</span>
              <span v-if="r.mode === 'redirect'" class="sub">{{ r.redirect.code }} {{ r.redirect.to }}</span>
            </div>
            <a v-if="record.spec.routes.length > 3" class="sub" style="color: #0e7c86" @click="expandedRoutes[record.id] = !expandedRoutes[record.id]">
              {{ expandedRoutes[record.id] ? '收起路由' : `+ 展开其余 ${record.spec.routes.length - 3} 条路由` }}
            </a>
          </template>
          <template v-else-if="column.key === 'https'">
            <span v-if="record.spec.https">{{ record.spec.force_https ? '强制' : '启用' }}{{ record.spec.http2 ? '，HTTP/2' : '' }}</span>
            <span v-else class="sub">未启用</span>
          </template>
          <template v-else-if="column.key === 'enabled'">
            <a-switch
              v-if="session.can('operator')"
              size="small"
              :checked="record.enabled"
              :loading="togglingSiteId === record.id"
              :disabled="!session.writable || (togglingSiteId !== 0 && togglingSiteId !== record.id)"
              checked-children="启用"
              un-checked-children="停用"
              @change="toggleSite(record)"
            />
            <a-badge v-else :status="record.enabled ? 'success' : 'default'" :text="record.enabled ? '启用' : '停用'" />
          </template>
          <template v-else-if="column.key === 'updated'">
            <div>{{ dayjs(record.updated_at).format('MM-DD HH:mm') }}</div>
            <div class="sub">{{ record.updated_by }}，第 {{ record.version }} 版</div>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-space v-if="session.can('operator')" :size="0">
              <a-button type="link" size="small" @click="edit(record)">编辑</a-button>
              <a-dropdown
                :menu="{
                  items: [
                    { key: 'dup', label: '同组克隆', disabled: !session.writable },
                    ...(ws.groups.length > 1 ? [{ key: 'copy', label: '复制到其他组', disabled: !session.writable }] : []),
                    { type: 'divider' },
                    { key: 'del', label: '删除站点', danger: true, disabled: !session.writable },
                  ],
                  onClick: (e: any) => onSiteMenu(record, String(e.key)),
                }"
              >
                <a-button type="link" size="small">更多 ▾</a-button>
              </a-dropdown>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <div v-show="tab === 'ups'" class="panel">
      <div class="toolbar">
        <a-input v-model:value="search" allow-clear placeholder="按上游名或后端地址搜索" style="width: 260px" />
      </div>
      <a-table :data-source="filteredUps" row-key="id" :scroll="{ x: 'max-content' }" :pagination="{ pageSize: 20, hideOnSinglePage: true }" :columns="[{ title: '名称', key: 'name' }, { title: '负载均衡', key: 'algo', width: 140 }, { title: '服务器', key: 'servers' }, { title: '', key: 'ops', width: 120 }]">
        <template #emptyText><a-empty :description="ups.length ? '没有匹配的上游' : '还没有上游服务。站点的“反向代理”路由需要先定义上游。'" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'"><span class="nm">{{ record.name }}</span></template>
          <template v-else-if="column.key === 'algo'">{{ algoText[record.spec.algorithm] || '轮询' }}</template>
          <template v-else-if="column.key === 'servers'">
            <span v-for="(s, idx) in record.spec.servers" :key="s.addr + idx" class="srv code">
              <span :style="s.down ? 'text-decoration: line-through; color: #9aa8aa' : ''">{{ s.addr }}<template v-if="s.weight"> ×{{ s.weight }}</template></span>
              <a-tag v-if="s.backup" size="small" style="margin-left: 4px">备</a-tag>
              <a-popconfirm
                v-if="session.can('operator') && session.writable"
                :title="s.down ? `恢复 ${s.addr} 的流量？` : `临时摘流 ${s.addr}（发布后不转发流量到此后端）？`"
                :ok-text="s.down ? '恢复' : '摘流'"
                @confirm="toggleServerDown(record, idx)"
              >
                <a-tag :color="s.down ? 'red' : 'default'" style="margin-left: 4px; cursor: pointer">{{ s.down ? '已摘流·点此恢复' : '摘流' }}</a-tag>
              </a-popconfirm>
              <a-tag v-else-if="s.down" color="red" size="small" style="margin-left: 4px">摘流</a-tag>
            </span>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-space v-if="session.can('operator')">
              <a-button type="link" size="small" @click="editUp(record)">编辑</a-button>
              <a-popconfirm title="删除上游？被站点引用时无法删除。" ok-text="删除" @confirm="removeUp(record)">
                <a-button type="link" size="small" danger>删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <SiteEditor :open="editorOpen" :site="editing" :group-id="gid" :upstreams="ups" @close="editorOpen = false" @saved="onSiteSaved" @upstream-created="onUpstreamCreated" />

    <a-modal v-model:open="dupOpen" :title="`克隆站点 ${dupSource?.name || ''}`" ok-text="创建副本" :confirm-loading="dupSaving" :ok-button-props="{ disabled: !dupForm.name.trim() || !dupForm.domains.length }" @ok="saveDuplicate">
      <a-form layout="vertical">
        <a-form-item label="新站点标识" extra="小写字母、数字、连字符"><a-input v-model:value="dupForm.name" class="code" /></a-form-item>
        <a-form-item label="新域名" extra="同节点组内不能与原站点域名冲突；回车添加"><a-select v-model:value="dupForm.domains" mode="tags" :open="false" /></a-form-item>
      </a-form>
    </a-modal>

    <a-modal v-model:open="copySitesOpen" title="复制站点到其他节点组" ok-text="开始复制" :confirm-loading="copyingSites" :ok-button-props="{ disabled: !copyTargetGroups.length }" @ok="runCopySites">
      <p class="sub" style="margin-top: 0">将选中的 {{ copySiteIds.length }} 个站点及其引用的上游服务、组内 IP 集与静态资源包一并复制到目标节点组草稿。</p>
      <a-form layout="vertical">
        <a-form-item label="目标节点组">
          <a-checkbox-group v-model:value="copyTargetGroups" :options="ws.groups.filter((g) => g.id !== gid).map((g) => ({ value: g.id, label: g.name }))" />
        </a-form-item>
        <a-form-item>
          <a-checkbox v-model:checked="copyOverwrite">覆盖目标组中同名站点与上游（不勾选则跳过已有项）</a-checkbox>
        </a-form-item>
      </a-form>
    </a-modal>

    <a-modal v-model:open="upOpen" :title="upForm.id ? `编辑上游 ${upForm.name}` : '新建上游'" width="680px" ok-text="保存" @ok="saveUp">
      <a-form layout="vertical">
        <a-form-item label="名称"><a-input v-model:value="upForm.name" :disabled="!!upForm.id" placeholder="order-api" /></a-form-item>
        <div class="up-meta">
          <a-form-item label="负载均衡">
            <a-radio-group v-model:value="upForm.algorithm" :options="[{ label: '最少连接', value: 'least_conn' }, { label: '轮询', value: 'round_robin' }, { label: '源 IP 哈希', value: 'ip_hash' }]" />
          </a-form-item>
          <a-form-item label="长连接池（keepalive）">
            <a-input-number v-model:value="upForm.keepalive" :min="1" :max="1024" style="width: 110px" />
          </a-form-item>
        </div>
        <a-form-item label="服务器" extra="IP:端口，必须在节点组的允许回源网段内；可按需设置权重、失败阈值、备机或临时摘流">
          <div v-for="(s, i) in upForm.servers" :key="i" class="srvrow">
            <a-input v-model:value="s.addr" class="code" placeholder="10.0.1.11:8080" style="flex: 1; min-width: 150px" />
            <a-input-number v-model:value="s.weight" :min="0" :max="100" placeholder="权重" style="width: 78px" />
            <a-input-number v-model:value="s.max_fails" :min="0" :max="100" placeholder="失败次" style="width: 82px" />
            <a-input-number v-model:value="s.fail_timeout" :min="0" :max="600" placeholder="熔断(s)" style="width: 86px" />
            <a-checkbox v-model:checked="s.backup" :disabled="upForm.algorithm === 'ip_hash'">备机</a-checkbox>
            <a-checkbox v-model:checked="s.down">摘流</a-checkbox>
            <a-button type="text" danger :disabled="upForm.servers.length === 1" @click="upForm.servers.splice(i, 1)">删</a-button>
          </div>
          <a-button size="small" @click="upForm.servers.push({ addr: '', weight: 0, max_fails: 0, fail_timeout: 0, backup: false, down: false })">添加服务器</a-button>
        </a-form-item>
        <a-alert v-if="upErr" type="error" :message="upErr" show-icon />
      </a-form>
    </a-modal>
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
.nm {
  font-weight: 600;
  color: #16323a;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
.rt {
  display: flex;
  gap: 8px;
  align-items: center;
  line-height: 1.9;
}
.mode {
  font-size: 12px;
  padding: 0 6px;
  border-radius: 4px;
  background: #eef2f1;
  color: #3b4a4e;
}
.mode.proxy {
  background: #e3f1f2;
  color: #0b5f66;
}
.mode.deny {
  background: #f8e3e1;
  color: #a3332d;
}
.srv {
  display: inline-block;
  margin-right: 10px;
}
.up-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  align-items: flex-start;
}
.srvrow {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  margin-bottom: 8px;
}
</style>
