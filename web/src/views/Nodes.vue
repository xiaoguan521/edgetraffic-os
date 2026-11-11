<script setup lang="ts">
import { brand } from '../brand'
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import DiffView from '../components/DiffView.vue'

const session = useSession()
const groups = ref<any[]>([])
const nodes = ref<any[]>([])
const gid = ref<number>(0)
const group = computed(() => groups.value.find((g) => g.id === gid.value))
const profile = ref<any>(null)
const managementMode = ref<any>(null)
const view = ref<'group' | 'host'>('group')
const hosts = ref<any[]>([])

async function load() {
  groups.value = await api.get('/node-groups')
  // 当前组被删除（或首次进入）时切到第一个组
  if (groups.value.length && !groups.value.some((g) => g.id === gid.value)) gid.value = groups.value[0].id
  const [n, p, h, m] = await Promise.all([api.get('/nodes', { group_id: gid.value }), api.get(`/node-groups/${gid.value}/profile`), api.get('/hosts'), api.get(`/node-groups/${gid.value}/management-mode`)] )
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
let t: number | undefined
onMounted(() => {
  load()
  t = window.setInterval(load, 5000)
})
onUnmounted(() => clearInterval(t))

// 添加节点
const enroll = ref<any>(null)
const enrolling = ref(false)
async function addNode() {
  enrolling.value = true
  try {
    enroll.value = await api.post(`/node-groups/${gid.value}/enroll`)
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    enrolling.value = false
  }
}
function copy(text: string) {
  navigator.clipboard?.writeText(text).then(() => message.success('已复制'))
}

// 节点组设置（含实例档案：同机多实例与 user 模式，设计 0001 第 3.4、5.2 节）
const editing = ref(false)
const gform = reactive({ name: '', description: '', allowed_cidrs: '', real_ip_from: '' })
const modeForm = ref('managed')
const pform = reactive({ instance: '', mode: 'system', root: '', resource_mode: 'shared', http_port: 80, https_port: 443, status_port: 8081, nofile: 0 })
const locked = computed(() => nodes.value.length > 0) // 组内已有节点：实例名、模式、根目录不可改
watch(() => pform.instance, (name) => { if (!name && !locked.value) pform.resource_mode = 'shared' })
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
  return pform.instance !== (p.instance || '') || pform.mode !== (p.mode || 'system') || pform.root !== (p.root || '') || pform.resource_mode !== (p.resource_mode || 'shared') ||
    pform.http_port !== e.http_port || pform.https_port !== e.https_port || pform.status_port !== e.status_port || pform.nofile !== (p.nofile || 0)
}
// 新建节点组（仅管理员）：沿用当前组的回源网段，建好后打开“节点组设置”填写实例档案
const creating = ref(false)
const cform = reactive({ name: '', description: '' })
function openCreate() {
  Object.assign(cform, { name: '', description: '' })
  creating.value = true
}
async function createGroup() {
  try {
    const base = group.value || {}
    const g = await api.post('/node-groups', { name: cform.name.trim(), description: cform.description, allowed_cidrs: base.allowed_cidrs || '', real_ip_from: base.real_ip_from || '' })
    creating.value = false
    gid.value = g.id
    view.value = 'group'
    await load()
    message.success(`已创建节点组 ${g.name}，请设置实例档案`)
    openGroup()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const nameRe = /^[a-z0-9][a-z0-9-]{1,62}$/
async function saveGroup() {
  const changingMode = modeForm.value !== (managementMode.value?.mode || group.value?.management_mode || 'managed')
  if (changingMode && modeForm.value === 'managed' && !window.confirm('切回“控制台托管”后仍不会自动发布，但之后的显式发布会覆盖节点本地配置。请确认已完成导入核对并保留了备份。')) return
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

// 删除节点组（仅管理员）：先查看占用，组内节点须已全部吊销，输入组名确认
const removing = ref<any>(null)
const confirmName = ref('')
const usageItems = computed(() => {
  const u = removing.value
  if (!u) return []
  return [
    ['站点', u.sites], ['上游', u.upstreams], ['四层服务', u.streams], ['自定义片段', u.snippets], ['IP 集', u.ip_sets],
    ['静态资源包', u.static_bundles], ['配置导入记录', u.imports], ['配置版本', u.revisions], ['发布记录', u.deployments], ['已吊销的节点记录', u.revoked_nodes],
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
    load()
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

// 漂移详情与采纳（规格书 12.7 路径 B）
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
  } catch (e) {
    message.error(errMsg(e))
  }
}

async function revoke(n: any) {
  await api.post(`/nodes/${n.id}/revoke`)
  message.success(`已吊销 ${n.name}，其证书立即失效。如需重新接入：点“添加节点”，在该机器上执行新生成的命令`, 6)
  load()
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
      <a-space>
        <a-segmented v-model:value="view" :options="[{ label: '按节点组', value: 'group' }, { label: '按主机', value: 'host' }]" />
        <a-button v-if="session.can('admin')" :disabled="!session.writable" @click="openCreate">新建节点组</a-button>
        <a-button v-if="session.can('operator')" @click="openGroup">节点组设置</a-button>
        <a-button v-if="session.can('operator')" type="primary" :loading="enrolling" :disabled="!session.writable" @click="addNode">添加节点</a-button>
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

    <a-segmented v-if="view === 'group' && groups.length > 1" v-model:value="gid" :options="groups.map((g) => ({ label: g.name, value: g.id }))" style="margin-bottom: 16px" @change="load" />

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
      <a-table :columns="columns" :data-source="nodes" row-key="id" :pagination="false">
        <template #emptyText>
          <a-empty description="这个节点组还没有节点。点击“添加节点”生成一次性注册命令。" />
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

    <a-modal :open="!!enroll" title="添加节点" :footer="null" width="720px" @cancel="enroll = null">
      <template v-if="enroll">
        <p v-if="enroll.mode === 'user'">在边缘服务器上以安装用户（不需要 root，也不需要 sudo）执行下面的命令。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。
          新服务器请先解压 user 模式节点包，用 <span class="code">./install-user.sh {{ enroll.root }} --http {{ enroll.http_port }} --https {{ enroll.https_port }} --status {{ enroll.status_port }} --console {{ enroll.server }} --token &lt;令牌&gt; --ca-fingerprint &lt;指纹&gt;</span> 一步安装并注册。</p>
        <p v-else-if="enroll.instance">在边缘服务器上（已安装 {{ brand.id }} 与 {{ brand.agent }} 软件包）以 root 执行下面的命令：按节点组的实例档案创建实例 <b>{{ enroll.instance }}</b>（HTTP {{ enroll.http_port }} / HTTPS {{ enroll.https_port }}）并注册。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。</p>
        <p v-else>在边缘服务器上（已安装 {{ brand.id }} 与 {{ brand.agent }} 软件包）以 root 执行下面的命令。令牌只能使用一次，<b>{{ dayjs(enroll.expires_at).format('HH:mm') }}</b> 前有效。</p>
        <p class="sub">新建节点或实例：</p>
        <pre class="cmd code">{{ enroll.command }}</pre>
        <a-button @click="copy(enroll.command)">复制新建命令</a-button>
        <p class="sub" style="margin-top: 16px">实例已经存在且有独立配置：</p>
        <pre class="cmd code">{{ enroll.existing_command }}</pre>
        <a-button type="primary" @click="copy(enroll.existing_command)">复制已有实例注册命令</a-button>
        <a-descriptions :column="1" size="small" style="margin-top: 20px">
          <a-descriptions-item label="控制台地址"><span class="code">{{ enroll.server }}</span></a-descriptions-item>
          <a-descriptions-item label="控制台 CA 指纹"><span class="code">{{ enroll.ca_fingerprint }}</span></a-descriptions-item>
        </a-descriptions>
        <p class="sub">节点首次连接时用 CA 指纹确认控制台身份，之后使用双向证书认证。已有实例注册时，检测到改动的当前版本目录会被备份；实际主配置、目录外的 include、证书和 Lua 依赖可用 agent 的 backup-config 命令另行备份。<template v-if="managementMode?.mode === 'managed'">节点上线后可能自动同步节点组当前版本，请先确认接入方式。</template><template v-else>当前是“{{ managementMode?.label }}”，节点上线不会自动同步配置。</template></p>
      </template>
    </a-modal>

    <a-modal v-model:open="creating" title="新建节点组" ok-text="创建" :ok-button-props="{ disabled: !/^[a-z0-9][a-z0-9-]{1,62}$/.test(cform.name.trim()) }" @ok="createGroup">
      <p class="sub" style="margin-top: 0">一个节点组对应一套配置。同一台服务器上运行多套 {{ brand.id }}（同机多实例）时，每套属于不同的节点组；创建后在“节点组设置”里填写实例名与端口。</p>
      <a-form layout="vertical">
        <a-form-item label="名称" extra="小写字母、数字和短横线，2–63 个字符，例如 edge-blue。之后可在“节点组设置”中改名。">
          <a-input v-model:value="cform.name" placeholder="edge-blue" />
        </a-form-item>
        <a-form-item label="说明（可选）">
          <a-input v-model:value="cform.description" />
        </a-form-item>
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
        <a-form-item label="安装根目录" :extra="pform.mode === 'user' ? '节点上普通用户的安装目录（绝对路径），user 模式只能使用 1024 及以上的端口。' : 'system 模式默认使用 /opt/syedge；填写后，软件包会把程序与实例数据放在该绝对路径，并写入 /etc/syedge/prefix。'">
          <a-input v-model:value="pform.root" class="code" :disabled="locked" :placeholder="pform.mode === 'user' ? '/home/ops/edge' : '/data/syedge'" />
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
