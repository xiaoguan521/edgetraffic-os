<script setup lang="ts">
import { brand } from '../brand'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg, http } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'

const session = useSession()
const ws = useWorkspace()
const groups = computed(() => ws.groups)
const gid = computed(() => ws.groupId)
const nodes = ref<any[]>([])
const packages = ref<any[]>([])
const upgrades = ref<any[]>([])
const hosts = ref<any[]>([])
const ver = ref<string>()
const variant = ref('standard')
const starting = ref(false)

async function load() {
  if (!ws.groups.length) await ws.loadGroups()
  if (!gid.value) return
  const [n, p, u, h] = await Promise.all([api.get('/nodes'), session.can('operator') ? api.get('/packages') : Promise.resolve([]), api.get('/upgrades', { group_id: gid.value }), api.get('/hosts')])
  nodes.value = n.filter((x: any) => x.group_id === gid.value && x.status !== 'revoked')
  packages.value = p
  upgrades.value = u
  hosts.value = h
}
let timer: number | undefined
onMounted(async () => {
  await load()
  timer = window.setInterval(() => {
    if (upgrades.value.some((u) => u.status === 'running')) load()
  }, 1500)
})
onUnmounted(() => clearInterval(timer))
watch(gid, load)

const agentVer = ref<string>()
const agentVersions = computed(() => [...new Set(packages.value.filter((p) => p.name === brand.agent).map((p) => p.version))])
async function startAgent() {
  try {
    await api.post(`/node-groups/${gid.value}/agent-upgrade`, { version: agentVer.value })
    message.success('已开始分批升级 agent（只重启 agent 进程，不影响流量）')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
const versions = computed(() => [...new Set(packages.value.filter((p) => p.name === brand.id && p.variant === variant.value).map((p) => p.version))])
const variantName: Record<string, string> = { standard: '标准版', gmssl: '国密版', lua: 'Lua 附加包', agent: 'agent' }
const nodeStatus: Record<string, { t: string; c: string }> = {
  pending: { t: '等待', c: 'default' },
  upgrading: { t: '升级中', c: 'processing' },
  upgraded: { t: '已升级', c: 'green' },
  rolled_back: { t: '已自动回退', c: 'orange' },
  failed: { t: '失败', c: 'red' },
  skipped: { t: '已停止', c: 'default' },
  offline: { t: '离线', c: 'default' },
}

function pickFiles() {
  const el = document.createElement('input')
  el.type = 'file'
  el.multiple = true
  el.accept = '.gz,.tgz,.sig'
  el.onchange = async () => {
    const fs = Array.from(el.files || [])
    const pkg = fs.find((f) => !f.name.endsWith('.sig'))
    const sig = fs.find((f) => f.name.endsWith('.sig'))
    if (!pkg || !sig) {
      message.warning('请同时选择升级包（.tar.gz）和对应的签名文件（.sig）')
      return
    }
    const fd = new FormData()
    fd.append('package', pkg)
    fd.append('signature', sig)
    try {
      const r = (await http.post('/packages', fd)).data
      message.success(`已导入 ${r.name} ${r.version}（${r.arch}，${variantName[r.variant]}），签名校验通过`)
      await load()
    } catch (e) {
      message.error(errMsg(e))
    }
  }
  el.click()
}
async function start() {
  starting.value = true
  try {
    await api.post(`/node-groups/${gid.value}/upgrade`, { version: ver.value, variant: variant.value })
    message.success('已开始分批升级：先 1 台，再 10%，再全量')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    starting.value = false
  }
}
const running = computed(() => upgrades.value.some((u) => u.status === 'running'))

// 同机多实例（设计 0001 第 5.6 节）：数据面二进制按主机共用，同机其他节点组的实例随主机一起升级
const siblings = computed(() => {
  const mine = new Set(nodes.value.map((n) => n.id))
  const out: any[] = []
  for (const h of hosts.value) {
    if (!h.nodes.some((n: any) => mine.has(n.id))) continue
    for (const n of h.nodes) if (!mine.has(n.id)) out.push({ ...n, host: h.name || h.host })
  }
  return out
})
const mixedHosts = computed(() => hosts.value.filter((h) => h.mixed && h.nodes.some((n: any) => nodes.value.some((m) => m.id === n.id))))
function dataplaneVersion(n: any) {
  return (n.syapp_version || '').replace(brand.proc + ' version: ', '').replace(brand.id + '/', '')
}
const upStatus: Record<string, { t: string; c: string }> = {
  done: { t: '完成', c: 'green' },
  running: { t: '进行中', c: 'processing' },
  failed: { t: '已停止', c: 'red' },
  partial: { t: '主机部分升级', c: 'orange' },
}
const hostStatus: Record<string, { t: string; c: string }> = {
  done: { t: '已升级', c: 'green' },
  upgrading: { t: '升级中', c: 'processing' },
  partial: { t: '部分升级', c: 'orange' },
  failed: { t: '保持原版本', c: 'red' },
  skipped: { t: '未开始', c: 'default' },
  pending: { t: '等待', c: 'default' },
}
async function retry(u: any) {
  try {
    await api.post(`/upgrades/${u.id}/retry`)
    message.success('已开始重试未完成的实例')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
const rollbackVer = ref<Record<number, string>>({})
function rollbackVersions(u: any) {
  return [...new Set(packages.value.filter((p) => p.name === brand.id && p.variant === u.variant && p.version !== u.version).map((p) => p.version))]
}
async function rollbackHosts(u: any) {
  try {
    await api.post(`/upgrades/${u.id}/rollback-hosts`, { version: rollbackVer.value[u.id] })
    message.success('已开始把部分升级主机上已升级的实例切回所选版本（热升级，不中断连接）')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>数据面版本</h1>
        <p>导入厂商签名的 {{ brand.id }} 升级包，按节点组分批热升级（二进制热替换，连接不中断）。同一主机上的实例共用一个二进制，按主机分批、主机内依次升级。任一节点探活失败会自动回到旧版本并停止后续主机；回退即用上一版本再升级一次。</p>
      </div>
    </div>

    <section class="panel">
      <h2>节点版本</h2>
      <a-table :data-source="nodes" row-key="id" :pagination="false" size="small"
        :columns="[{ title: '节点', dataIndex: 'name' }, { title: '架构', dataIndex: 'arch' }, { title: brand.proc, key: 'v' }, { title: '实际运行', key: 'run' }, { title: 'agent', dataIndex: 'agent_version' }, { title: '状态', key: 'st' }]">
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'v'">{{ dataplaneVersion(record) || '—' }}</template>
          <template v-else-if="column.key === 'run'">
            {{ record.running_version || '—' }}
            <a-tooltip v-if="record.running_version && dataplaneVersion(record) && !dataplaneVersion(record).includes(record.running_version)" title="同机其他实例已升级了共用的二进制，本实例仍在运行旧版本：请完成或回退该主机的升级">
              <a-tag color="gold" style="margin-left: 6px">未切换</a-tag>
            </a-tooltip>
          </template>
          <template v-else-if="column.key === 'st'"><a-badge :status="record.status === 'online' ? 'success' : 'default'" :text="record.status === 'online' ? '在线' : '离线'" /></template>
        </template>
      </a-table>
      <a-alert v-if="siblings.length" type="info" show-icon style="margin-top: 12px"
        :message="`同机还有 ${siblings.length} 个其他节点组的实例，会随主机一起升级`"
        :description="`同一台主机上的实例共用一个 ${brand.id} 二进制，升级以主机为单位：本组实例先升级，同机实例随后依次升级。涉及：${siblings.map((n) => `${n.name}（${n.group_name}，主机 ${n.host}）`).join('、')}`" />
      <a-alert v-for="h in mixedHosts" :key="h.host" type="warning" show-icon style="margin-top: 8px"
        :message="`主机 ${h.name || h.host} 上的实例运行着不同版本：${h.versions.join('、')}`" description="通常是上一次升级只完成了一部分：在下方升级记录中重试剩余实例，或整体回退。" />
      <div class="up" v-if="session.can('operator')">
        <a-select v-model:value="variant" style="width: 120px" :options="Object.entries(variantName).map(([value, label]) => ({ value, label }))" />
        <a-select v-model:value="ver" style="width: 200px" placeholder="目标版本" :options="versions.map((v) => ({ value: v, label: v }))" />
        <a-popconfirm :title="`把 ${groups.find((g) => g.id === gid)?.name} 的 ${nodes.length} 台节点升级到 ${ver}？`" @confirm="start">
          <a-button type="primary" :loading="starting" :disabled="!ver || running || !session.writable">开始分批升级</a-button>
        </a-popconfirm>
        <span class="gap" />
        <a-select v-model:value="agentVer" style="width: 200px" placeholder="agent 目标版本" :options="agentVersions.map((v) => ({ value: v, label: 'agent ' + v }))" />
        <a-popconfirm :title="`把 ${nodes.length} 台节点的 agent 升级到 ${agentVer}？${brand.id} 不受影响`" @confirm="startAgent">
          <a-button :disabled="!agentVer || running || !session.writable">升级 agent</a-button>
        </a-popconfirm>
      </div>
    </section>

    <section class="panel" v-for="u in upgrades.slice(0, 5)" :key="u.id">
      <div class="phead">
        <h2>{{ u.variant === 'agent' ? 'agent 升级' : '升级' }} #{{ u.id }} → {{ u.version }} <span class="sub">{{ variantName[u.variant] }}</span></h2>
        <span class="sub">
          <a-tag :color="upStatus[u.status]?.c">{{ upStatus[u.status]?.t }}</a-tag>
          {{ u.created_by }} · {{ dayjs(u.created_at).format('MM-DD HH:mm') }}
        </span>
      </div>
      <div v-if="u.variant !== 'agent' && (u.hosts || []).length > 1" class="hosts">
        <span v-for="h in u.hosts" :key="h.host" class="host">
          主机 <b>{{ h.name || h.host }}</b> <a-tag :color="hostStatus[h.status]?.c">{{ hostStatus[h.status]?.t }}</a-tag>
        </span>
      </div>
      <a-table :data-source="u.nodes" row-key="node_id" :pagination="false" size="small"
        :columns="[{ title: '批次', dataIndex: 'batch', width: 70 }, { title: '主机', key: 'host', width: 160 }, { title: '节点', key: 'name' }, { title: '结果', key: 'st', width: 130 }, { title: '说明', dataIndex: 'message' }]">
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'host'"><span class="sub">{{ record.host_name || record.host || '—' }}</span></template>
          <template v-else-if="column.key === 'name'">{{ record.name }}<a-tag v-if="record.sibling" style="margin-left: 6px">同机实例</a-tag></template>
          <template v-else-if="column.key === 'st'"><a-tag :color="nodeStatus[record.status]?.c">{{ nodeStatus[record.status]?.t }}</a-tag></template>
        </template>
      </a-table>
      <div class="up" v-if="u.variant !== 'agent' && (u.status === 'partial' || u.status === 'failed') && session.can('operator')">
        <a-popconfirm title="对未完成的实例（失败、回退、未开始、离线）以同一版本重新执行？已升级的实例不受影响。" @confirm="retry(u)">
          <a-button :disabled="running || !session.writable">重试未完成的实例</a-button>
        </a-popconfirm>
        <template v-if="u.status === 'partial'">
          <span class="gap" />
          <a-select v-model:value="rollbackVer[u.id]" style="width: 200px" placeholder="回退到版本" :options="rollbackVersions(u).map((v) => ({ value: v, label: v }))" />
          <a-popconfirm :title="`把部分升级主机上已升级的实例热切回 ${rollbackVer[u.id]}？连接不中断，任何实例都不会停止。`" @confirm="rollbackHosts(u)">
            <a-button :disabled="!rollbackVer[u.id] || running || !session.writable">主机整体回退</a-button>
          </a-popconfirm>
        </template>
      </div>
    </section>

    <section class="panel" v-if="session.can('operator')">
      <div class="phead">
        <h2>版本库</h2>
        <a-button v-if="session.can('admin')" size="small" :disabled="!session.writable" @click="pickFiles">导入升级包</a-button>
      </div>
      <a-table :data-source="packages" row-key="id" :pagination="false" size="small"
        :columns="[{ title: '包', key: 'n' }, { title: '架构', dataIndex: 'arch' }, { title: '变体', key: 'var' }, { title: 'SHA-256', key: 'sha' }, { title: '签名密钥', dataIndex: 'kid' }, { title: '导入', key: 'by' }]">
        <template #emptyText><a-empty :description="`版本库为空：导入厂商发布的 ${brand.id}-<版本>-<架构>.tar.gz 与 .sig`" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'n'">{{ record.name }} {{ record.version }}</template>
          <template v-else-if="column.key === 'var'">{{ variantName[record.variant] }}</template>
          <template v-else-if="column.key === 'sha'"><span class="code">{{ record.sha256.slice(0, 16) }}…</span></template>
          <template v-else-if="column.key === 'by'">{{ record.uploaded_by }} · {{ dayjs(record.created_at).format('MM-DD HH:mm') }}</template>
        </template>
      </a-table>
    </section>
  </div>
</template>

<style scoped>
.gap {
  width: 16px;
}
.up {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 14px;
}
.hosts {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin-bottom: 10px;
  font-size: 13px;
}
.phead {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 8px;
}
.phead h2 {
  margin: 0;
}
.sub {
  font-size: 12px;
  color: #6b7c80;
}
</style>
