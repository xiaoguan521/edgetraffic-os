<script setup lang="ts">
// IP 集与地域库（支持跨组全局共享与节点组私有）+ 跨组公共访问规则模板
import { computed, onMounted, reactive, ref, watch } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg, http } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import RulesEditor, { type Rule as AccessRule } from '../components/RulesEditor.vue'

const session = useSession()
const ws = useWorkspace()
const gid = computed(() => ws.groupId)
const tab = ref<'ipsets' | 'templates'>('ipsets')
const rows = ref<any[]>([])
const templates = ref<any[]>([])
const upstreams = ref<any[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    if (!ws.groups.length) await ws.loadGroups()
    if (!gid.value) return
    ;[rows.value, templates.value, upstreams.value] = await Promise.all([
      api.get('/ip-sets', { group_id: gid.value }),
      api.get('/rule-templates').catch(() => []),
      api.get('/upstreams', { group_id: gid.value }).catch(() => []),
    ])
  } finally {
    loading.value = false
  }
}
watch(gid, load)
onMounted(load)

// 手工维护 IP 集
const open = ref(false)
const editing = ref<any>(null)
const form = reactive({ scope: 'global' as 'global' | 'group', name: '', label: '', text: '' })
const saving = ref(false)
const error = ref('')
async function openEdit(r?: any) {
  error.value = ''
  editing.value = r || null
  Object.assign(form, {
    scope: r ? (r.global || r.group_id === 0 ? 'global' : 'group') : 'global',
    name: r?.name || '',
    label: r?.label || '',
    text: '',
  })
  if (r) {
    const full = await api.get(`/ip-sets/${r.id}`)
    form.text = (full.cidrs || []).join('\n')
  }
  open.value = true
}
async function save() {
  error.value = ''
  saving.value = true
  const cidrs = form.text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
  const targetGid = form.scope === 'global' ? 0 : gid.value
  try {
    if (editing.value) await api.put(`/ip-sets/${editing.value.id}`, { label: form.label, cidrs })
    else await api.post('/ip-sets', { group_id: targetGid, name: form.name, label: form.label, cidrs })
    message.success(targetGid === 0 ? '已保存全局 IP 集。所有引用它的节点组下次发布（或批量发布）后生效。' : '已保存。被规则引用时，下次发布生效。')
    open.value = false
    ws.markDraftDirty()
    load()
  } catch (e) {
    error.value = errMsg(e)
  } finally {
    saving.value = false
  }
}

// 从 RIR 分配统计文件导入地域库
const impOpen = ref(false)
const imp = reactive({ scope: 'global' as 'global' | 'group', name: 'cn', label: '', country: 'CN', file: null as File | null })
const importing = ref(false)
function pick() {
  const el = document.createElement('input')
  el.type = 'file'
  el.onchange = () => (imp.file = el.files?.[0] || null)
  el.click()
}
async function doImport() {
  if (!imp.file) return
  error.value = ''
  importing.value = true
  const fd = new FormData()
  fd.append('group_id', String(imp.scope === 'global' ? 0 : gid.value))
  fd.append('name', imp.name)
  fd.append('label', imp.label)
  fd.append('country', imp.country)
  fd.append('file', imp.file)
  try {
    const r = (await http.post('/ip-sets/import', fd)).data
    message.success(`已导入 ${r.count} 条网段（${imp.scope === 'global' ? '全局共享' : '仅当前节点组'}）`)
    impOpen.value = false
    ws.markDraftDirty()
    load()
  } catch (e) {
    error.value = errMsg(e)
  } finally {
    importing.value = false
  }
}

async function remove(r: any) {
  try {
    await api.del(`/ip-sets/${r.id}`)
    message.success(`已删除 ${r.name}`)
    ws.markDraftDirty()
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

// 公共规则模板
const tplOpen = ref(false)
const tplEditing = ref(false)
const tplSaving = ref(false)
const tplErr = ref('')
const tplForm = reactive({ name: '', description: '', rules: [] as AccessRule[] })
function openTpl(t?: any) {
  tplErr.value = ''
  tplEditing.value = !!t
  if (t) {
    Object.assign(tplForm, {
      name: t.name,
      description: t.description || '',
      rules: JSON.parse(JSON.stringify(t.rules || [])).map((r: AccessRule) => ({ ...r, conditions: (r.conditions || []).map((c) => ({ ...c, negate: !!c.negate })) })),
    })
  } else {
    Object.assign(tplForm, {
      name: '',
      description: '',
      rules: [{ name: 'rule-1', conditions: [{ kind: 'path', op: 'prefix', value: '/admin/', negate: false }], action: { type: 'deny', status: 403 } }],
    })
  }
  tplOpen.value = true
}
async function saveTpl() {
  tplErr.value = ''
  tplSaving.value = true
  try {
    await api.post('/rule-templates', {
      name: tplForm.name.trim(),
      description: tplForm.description.trim(),
      rules: tplForm.rules,
    })
    message.success('公共规则模板已保存，可在任意节点组的站点编辑页一键导入')
    tplOpen.value = false
    await load()
  } catch (e) {
    tplErr.value = errMsg(e)
  } finally {
    tplSaving.value = false
  }
}
async function removeTpl(t: any) {
  try {
    await api.del(`/rule-templates/${encodeURIComponent(t.name)}`)
    message.success(`已删除模板 ${t.name}`)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const columns = [
  { title: 'IP 集', key: 'name' },
  { title: '作用域', key: 'scope', width: 110 },
  { title: '网段', key: 'count', width: 100 },
  { title: '来源', key: 'source', width: 140 },
  { title: '被规则引用', key: 'users' },
  { title: '', key: 'ops', width: 150 },
]
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>IP 集与地域库</h1>
        <p>支持创建跨节点组全局共享的 IP 集、地域库与公共访问规则模板（节点组内同名 IP 集可覆盖全局值）。</p>
      </div>
      <a-space v-if="session.can('operator')">
        <template v-if="tab === 'ipsets'">
          <a-button :disabled="!session.writable" @click="openEdit()">新建 IP 集</a-button>
          <a-button type="primary" :disabled="!session.writable" @click="(error = ''), (impOpen = true)">导入地域库</a-button>
        </template>
        <a-button v-else type="primary" :disabled="!session.writable" @click="openTpl()">新建规则模板</a-button>
      </a-space>
    </div>

    <a-tabs v-model:active-key="tab" :items="[{ key: 'ipsets', label: `IP 集与地域库（${rows.length}）` }, { key: 'templates', label: `公共规则模板（${templates.length}）` }]" />

    <div v-show="tab === 'ipsets'" class="panel">
      <a-table :columns="columns" :data-source="rows" row-key="id" :pagination="{ pageSize: 20, hideOnSinglePage: true }" :loading="loading">
        <template #emptyText><a-empty description="还没有 IP 集" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <div class="nm code">{{ record.name }}</div>
            <div class="sub">{{ record.label }} · {{ record.updated_by }} {{ dayjs(record.updated_at).format('MM-DD HH:mm') }}</div>
          </template>
          <template v-else-if="column.key === 'scope'">
            <a-tag v-if="record.global || record.group_id === 0" color="cyan">全局共享</a-tag>
            <a-tag v-else>当前组</a-tag>
          </template>
          <template v-else-if="column.key === 'count'"><span class="num">{{ record.count.toLocaleString() }}</span></template>
          <template v-else-if="column.key === 'source'">
            <a-tag v-if="record.source.startsWith('rir:')" color="blue">地域 {{ record.source.slice(4) }}</a-tag>
            <span v-else class="sub">手工维护</span>
          </template>
          <template v-else-if="column.key === 'users'">
            <span v-if="!record.users.length" class="sub">未使用</span>
            <span v-else class="code users">{{ record.users.join('、') }}</span>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-space v-if="session.can('operator')" :size="0">
              <a-button type="link" size="small" :disabled="!session.writable" @click="openEdit(record)">编辑</a-button>
              <a-popconfirm :title="`删除 ${record.name}？`" ok-text="删除" @confirm="remove(record)">
                <a-button type="link" danger size="small" :disabled="!session.writable || record.users.length > 0">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <div v-show="tab === 'templates'" class="panel">
      <a-table
        :data-source="templates"
        row-key="name"
        :pagination="{ pageSize: 20, hideOnSinglePage: true }"
        :loading="loading"
        :columns="[{ title: '模板名称', key: 'name' }, { title: '说明', dataIndex: 'description' }, { title: '规则数', key: 'cnt', width: 90 }, { title: '更新', key: 'upd', width: 170 }, { title: '', key: 'ops', width: 140 }]"
      >
        <template #emptyText><a-empty description="还没有公共规则模板。创建后可在任意节点组的站点编辑页一键导入。" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'"><span class="nm code">{{ record.name }}</span></template>
          <template v-else-if="column.key === 'cnt'"><span class="num">{{ (record.rules || []).length }}</span></template>
          <template v-else-if="column.key === 'upd'">
            <div class="sub">{{ record.updated_by }} · {{ dayjs(record.updated_at).format('MM-DD HH:mm') }}</div>
          </template>
          <template v-else-if="column.key === 'ops' && session.can('operator')">
            <a-space :size="0">
              <a-button type="link" size="small" :disabled="!session.writable" @click="openTpl(record)">编辑</a-button>
              <a-popconfirm :title="`删除模板 ${record.name}？不影响已导入到站点的规则`" ok-text="删除" @confirm="removeTpl(record)">
                <a-button type="link" danger size="small" :disabled="!session.writable">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <a-modal v-model:open="open" :title="editing ? `编辑 ${editing.name}` : '新建 IP 集'" ok-text="保存" :confirm-loading="saving" width="600px" @ok="save">
      <a-form layout="vertical">
        <a-form-item v-if="!editing" label="作用域" extra="全局共享对所有节点组可见，一次维护多组复用；节点组内可建同名 IP 集覆盖全局值">
          <a-radio-group v-model:value="form.scope">
            <a-radio value="global">全局共享（所有节点组）</a-radio>
            <a-radio value="group">仅当前节点组</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item v-if="!editing" label="名称" extra="小写字母、数字、连字符；规则中按名称引用"><a-input v-model:value="form.name" class="code" placeholder="office" /></a-form-item>
        <a-form-item label="说明"><a-input v-model:value="form.label" placeholder="办公网出口" /></a-form-item>
        <a-form-item label="网段" extra="每行一个，支持 IPv4 / IPv6 地址或 CIDR"><a-textarea v-model:value="form.text" :rows="8" class="code" placeholder="10.20.0.0/16&#10;203.0.113.7" /></a-form-item>
      </a-form>
      <a-alert v-if="error" type="error" :message="error" show-icon />
    </a-modal>

    <a-modal v-model:open="impOpen" title="导入地域库" ok-text="导入" :confirm-loading="importing" :ok-button-props="{ disabled: !imp.file }" width="560px" @ok="doImport">
      <a-alert type="info" show-icon style="margin-bottom: 16px"
        message="从 APNIC / ARIN / RIPE NCC / LACNIC / AFRINIC 下载 delegated-*-extended-latest 文件（中国大陆、港澳台在 APNIC），按国家 / 地区代码挑出已分配的 IPv4 / IPv6 网段。同名 IP 集会被整体替换。" />
      <a-form layout="vertical">
        <a-form-item label="作用域">
          <a-radio-group v-model:value="imp.scope">
            <a-radio value="global">全局共享（推荐，一次导入全网复用）</a-radio>
            <a-radio value="group">仅当前节点组</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="国家 / 地区代码" extra="ISO 3166 两位代码，例如 CN、HK、SG"><a-input v-model:value="imp.country" class="code" style="width: 120px" /></a-form-item>
        <a-form-item label="保存为 IP 集"><a-input v-model:value="imp.name" class="code" placeholder="cn" /></a-form-item>
        <a-form-item label="说明（可选）"><a-input v-model:value="imp.label" placeholder="中国大陆" /></a-form-item>
        <a-form-item label="分配统计文件"><a-button @click="pick">{{ imp.file ? imp.file.name : '选择文件' }}</a-button></a-form-item>
      </a-form>
      <a-alert v-if="error" type="error" :message="error" show-icon />
    </a-modal>

    <a-drawer v-model:open="tplOpen" :title="tplEditing ? `编辑规则模板 ${tplForm.name}` : '新建公共规则模板'" width="760">
      <a-form layout="vertical">
        <a-form-item label="模板名称" extra="小写字母、数字、连字符，例如 admin-ip-guard">
          <a-input v-model:value="tplForm.name" :disabled="tplEditing" class="code" placeholder="admin-ip-guard" />
        </a-form-item>
        <a-form-item label="说明（可选）">
          <a-input v-model:value="tplForm.description" placeholder="限制 /admin 仅允许办公网访问并启用基础限速" />
        </a-form-item>
        <RulesEditor v-model="tplForm.rules" :upstreams="upstreams" :ip-sets="rows" template-mode />
        <a-alert v-if="tplErr" type="error" :message="tplErr" show-icon style="margin-top: 12px" />
      </a-form>
      <template #footer>
        <a-space style="float: right">
          <a-button @click="tplOpen = false">取消</a-button>
          <a-button type="primary" :loading="tplSaving" @click="saveTpl">保存模板</a-button>
        </a-space>
      </template>
    </a-drawer>
  </div>
</template>

<style scoped>
.nm {
  font-weight: 600;
  color: #16323a;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
.users {
  font-size: 12.5px;
}
</style>
