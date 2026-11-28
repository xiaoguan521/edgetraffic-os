<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import SiteEditor from '../components/SiteEditor.vue'

const session = useSession()
const ws = useWorkspace()
const route = useRoute()
const router = useRouter()
const tab = ref('sites')
const sites = ref<any[]>([])
const ups = ref<any[]>([])
const search = ref('')
const statusFilter = ref<'all' | 'enabled' | 'disabled'>('all')
const dirtyNotice = ref(false)

const gid = computed(() => ws.groupId)

async function load() {
  if (!ws.groups.length) await ws.loadGroups(Number(route.query.group) || undefined)
  if (!gid.value) return
  ;[sites.value, ups.value] = await Promise.all([api.get('/sites', { group_id: gid.value }), api.get('/upstreams', { group_id: gid.value })])
}
watch(gid, () => {
  dirtyNotice.value = false
  load()
})
onMounted(async () => {
  if (route.query.tab === 'ups') tab.value = 'ups'
  if (route.query.group) ws.setGroup(Number(route.query.group))
  await load()
})

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
  dirtyNotice.value = true
  load()
}
async function removeSite(s: any) {
  try {
    await api.del(`/sites/${s.id}`)
    dirtyNotice.value = true
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
  { title: '状态', key: 'enabled', width: 80 },
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
    dirtyNotice.value = true
    message.success('已保存')
    load()
  } catch (e) {
    upErr.value = errMsg(e)
  }
}
async function removeUp(u: any) {
  try {
    await api.del(`/upstreams/${u.id}`)
    dirtyNotice.value = true
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
      <a-space>
        <a-button v-if="dirtyNotice && session.can('releaser')" @click="router.push({ name: 'releases' })">去发布变更</a-button>
        <template v-if="session.can('operator')">
          <a-button v-if="tab === 'ups'" type="primary" :disabled="!session.writable" @click="editUp(null)">新建上游</a-button>
          <a-button v-else type="primary" :disabled="!session.writable" @click="edit(null)">新建站点</a-button>
        </template>
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
      </div>
      <a-table :columns="siteCols" :data-source="filteredSites" row-key="id" :pagination="{ pageSize: 20, hideOnSinglePage: true }">
        <template #emptyText><a-empty :description="sites.length ? '没有匹配的站点' : '还没有站点。新建一个站点，填写域名和路由即可。'" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <div class="nm">{{ record.name }}</div>
            <div class="sub">{{ record.spec.domains.join('，') }}</div>
          </template>
          <template v-else-if="column.key === 'routes'">
            <div v-for="r in record.spec.routes" :key="r.match + r.path" class="rt">
              <span class="code">{{ r.match === 'exact' ? '= ' : '' }}{{ r.path }}</span>
              <span class="mode" :class="r.mode">{{ modeText[r.mode] }}</span>
              <span v-if="r.mode === 'proxy'" class="sub">{{ r.proxy.https ? 'https://' : '' }}{{ r.proxy.upstream }}{{ r.proxy.strip_prefix ? '（剥离前缀）' : '' }}</span>
              <span v-if="r.mode === 'redirect'" class="sub">{{ r.redirect.code }} {{ r.redirect.to }}</span>
            </div>
          </template>
          <template v-else-if="column.key === 'https'">
            <span v-if="record.spec.https">{{ record.spec.force_https ? '强制' : '启用' }}{{ record.spec.http2 ? '，HTTP/2' : '' }}</span>
            <span v-else class="sub">未启用</span>
          </template>
          <template v-else-if="column.key === 'enabled'">
            <a-badge :status="record.enabled ? 'success' : 'default'" :text="record.enabled ? '启用' : '停用'" />
          </template>
          <template v-else-if="column.key === 'updated'">
            <div>{{ dayjs(record.updated_at).format('MM-DD HH:mm') }}</div>
            <div class="sub">{{ record.updated_by }}，第 {{ record.version }} 版</div>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-space v-if="session.can('operator')">
              <a-button type="link" size="small" @click="edit(record)">编辑</a-button>
              <a-popconfirm title="删除站点？发布后从所有节点移除。" ok-text="删除" @confirm="removeSite(record)">
                <a-button type="link" size="small" danger>删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <div v-show="tab === 'ups'" class="panel">
      <div class="toolbar">
        <a-input v-model:value="search" allow-clear placeholder="按上游名或后端地址搜索" style="width: 260px" />
      </div>
      <a-table :data-source="filteredUps" row-key="id" :pagination="{ pageSize: 20, hideOnSinglePage: true }" :columns="[{ title: '名称', key: 'name' }, { title: '负载均衡', key: 'algo', width: 140 }, { title: '服务器', key: 'servers' }, { title: '', key: 'ops', width: 120 }]">
        <template #emptyText><a-empty :description="ups.length ? '没有匹配的上游' : '还没有上游服务。站点的“反向代理”路由需要先定义上游。'" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'"><span class="nm">{{ record.name }}</span></template>
          <template v-else-if="column.key === 'algo'">{{ algoText[record.spec.algorithm] || '轮询' }}</template>
          <template v-else-if="column.key === 'servers'">
            <span v-for="s in record.spec.servers" :key="s.addr" class="srv code">
              {{ s.addr }}<template v-if="s.weight"> ×{{ s.weight }}</template>
              <a-tag v-if="s.backup" size="small" style="margin-left: 4px">备</a-tag>
              <a-tag v-if="s.down" color="red" size="small" style="margin-left: 4px">摘流</a-tag>
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

    <SiteEditor :open="editorOpen" :site="editing" :group-id="gid" :upstreams="ups" @close="editorOpen = false" @saved="onSiteSaved" @upstream-created="load" />

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
