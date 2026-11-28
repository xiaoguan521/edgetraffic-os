<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'

const session = useSession()
const ws = useWorkspace()
const gid = computed(() => ws.groupId)
const rows = ref<any[]>([])
const licensed = computed(() => !!session.license?.features?.includes('stream'))

async function load() {
  if (!ws.groups.length) await ws.loadGroups()
  if (!gid.value) return
  rows.value = await api.get('/streams', { group_id: gid.value })
}
watch(gid, load)
onMounted(load)

const open = ref(false)
const editId = ref(0)
const form = reactive({
  name: '',
  listen: 3306,
  protocol: 'tcp',
  algorithm: '',
  servers: [{ addr: '', weight: 0 }] as { addr: string; weight: number }[],
  sni_routes: [] as { domain: string; servers: string }[],
  proxy_protocol: false,
  connect_timeout: 5,
  idle_timeout: 600,
})
const toList = (s: string) =>
  s
    .split(/[\s,]+/)
    .filter(Boolean)
    .map((a) => {
      const [addr, w] = a.split('*')
      return { addr, weight: Number(w || 0) }
    })
const fromList = (l: any[]) => (l || []).map((x) => (x.weight ? `${x.addr}*${x.weight}` : x.addr)).join(', ')

function edit(r?: any) {
  editId.value = r?.id || 0
  const sp = r?.spec || {}
  Object.assign(form, {
    name: sp.name || '',
    listen: sp.listen || 3306,
    protocol: sp.protocol || 'tcp',
    algorithm: sp.algorithm || '',
    servers: sp.servers?.length ? sp.servers.map((s: any) => ({ addr: s.addr, weight: s.weight || 0 })) : [{ addr: '', weight: 0 }],
    sni_routes: (sp.sni_routes || []).map((x: any) => ({ domain: x.domain, servers: fromList(x.servers) })),
    proxy_protocol: !!sp.proxy_protocol,
    connect_timeout: sp.connect_timeout || 5,
    idle_timeout: sp.idle_timeout || 600,
  })
  open.value = true
}
async function save() {
  const spec = {
    name: form.name,
    listen: form.listen,
    protocol: form.protocol,
    algorithm: form.algorithm,
    servers: form.servers.filter((s) => s.addr.trim()).map((s) => ({ addr: s.addr.trim(), weight: Number(s.weight) || 0 })),
    proxy_protocol: form.proxy_protocol,
    connect_timeout: form.connect_timeout,
    idle_timeout: form.idle_timeout,
    sni_routes: form.sni_routes.filter((x) => x.domain.trim() && x.servers.trim()).map((x) => ({ domain: x.domain.trim(), servers: toList(x.servers) })),
  }
  try {
    if (editId.value) await api.put(`/streams/${editId.value}`, { spec })
    else await api.post('/streams', { group_id: gid.value, spec })
    open.value = false
    ws.markDraftDirty()
    message.success('已保存，发布后生效')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function toggle(r: any) {
  try {
    await api.put(`/streams/${r.id}`, { spec: r.spec, enabled: !r.enabled })
    ws.markDraftDirty()
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function del(r: any) {
  await api.del(`/streams/${r.id}`)
  ws.markDraftDirty()
  await load()
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>四层代理</h1>
        <p>TCP / UDP 端口转发，可按 TLS SNI 分流（不解密）。保存后进入待发布变更，发布后生效；节点启用了防火墙时需放行对应端口。</p>
      </div>
      <a-space>
        <a-button v-if="session.can('operator')" type="primary" :disabled="!licensed || !session.writable" @click="edit()">新建</a-button>
      </a-space>
    </div>
    <a-alert v-if="!licensed" type="info" show-icon style="margin-bottom: 16px" message="当前授权不包含四层代理（专业版及以上）。" />
    <section class="panel">
      <a-table :data-source="rows" row-key="id" :scroll="{ x: 'max-content' }" :pagination="{ pageSize: 20, hideOnSinglePage: true }" size="middle"
        :columns="[{ title: '名称', key: 'name' }, { title: '监听', key: 'listen' }, { title: '后端', key: 'servers' }, { title: '状态', key: 'st', width: 90 }, { title: '', key: 'op', width: 200 }]">
        <template #emptyText><a-empty description="还没有四层代理服务" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">{{ record.name }}</template>
          <template v-else-if="column.key === 'listen'"><span class="code">{{ record.spec.protocol }}/{{ record.spec.listen }}</span><a-tag v-if="record.spec.sni_routes?.length" style="margin-left: 6px">SNI 分流 {{ record.spec.sni_routes.length }}</a-tag></template>
          <template v-else-if="column.key === 'servers'"><span class="code">{{ record.spec.servers.map((s: any) => s.addr).join('、') }}</span></template>
          <template v-else-if="column.key === 'st'"><a-tag :color="record.enabled ? 'green' : 'default'">{{ record.enabled ? '启用' : '停用' }}</a-tag></template>
          <template v-else-if="column.key === 'op' && session.can('operator')">
            <a-space :size="4">
              <a-button size="small" :disabled="!licensed" @click="edit(record)">编辑</a-button>
              <a-button size="small" :disabled="!licensed && !record.enabled" @click="toggle(record)">{{ record.enabled ? '停用' : '启用' }}</a-button>
              <a-popconfirm title="删除该服务？发布后生效" @confirm="del(record)"><a-button size="small" danger>删除</a-button></a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </section>
    <a-modal v-model:open="open" :title="editId ? '编辑四层代理' : '新建四层代理'" :width="640" @ok="save">
      <a-form layout="vertical">
        <a-form-item label="名称"><a-input v-model:value="form.name" :disabled="!!editId" placeholder="mysql" /></a-form-item>
        <a-space wrap>
          <a-form-item label="协议"><a-select v-model:value="form.protocol" style="width: 100px" :options="[{ value: 'tcp', label: 'TCP' }, { value: 'udp', label: 'UDP' }]" /></a-form-item>
          <a-form-item label="监听端口"><a-input-number v-model:value="form.listen" :min="1" :max="65535" /></a-form-item>
          <a-form-item label="负载算法"><a-select v-model:value="form.algorithm" style="width: 170px" :options="[{ value: '', label: '轮询' }, { value: 'least_conn', label: '最少连接' }, { value: 'hash', label: '按来源 IP 一致性哈希' }]" /></a-form-item>
        </a-space>
        <a-form-item label="后端服务器">
          <div v-for="(s, i) in form.servers" :key="i" class="srow">
            <a-input v-model:value="s.addr" class="code" placeholder="10.0.1.21:3306" style="flex: 1" />
            <a-input-number v-model:value="s.weight" :min="0" :max="100" placeholder="权重" style="width: 90px" />
            <a-button type="text" danger :disabled="form.servers.length === 1" @click="form.servers.splice(i, 1)">删除</a-button>
          </div>
          <a-button size="small" @click="form.servers.push({ addr: '', weight: 0 })">添加后端</a-button>
        </a-form-item>
        <a-form-item v-if="form.protocol === 'tcp'" label="按 SNI 分流（可选）">
          <div v-for="(x, i) in form.sni_routes" :key="i" class="srow">
            <a-input v-model:value="x.domain" class="code" placeholder="a.example.com" style="width: 190px" />
            <a-input v-model:value="x.servers" class="code" placeholder="10.0.1.23:443, 10.0.1.24:443*2" style="flex: 1" />
            <a-button type="text" danger @click="form.sni_routes.splice(i, 1)">删除</a-button>
          </div>
          <a-button size="small" @click="form.sni_routes.push({ domain: '', servers: '' })">添加 SNI 分流</a-button>
        </a-form-item>
        <a-space>
          <a-form-item label="连接超时（秒）"><a-input-number v-model:value="form.connect_timeout" :min="1" :max="60" /></a-form-item>
          <a-form-item label="空闲超时（秒）"><a-input-number v-model:value="form.idle_timeout" :min="1" :max="86400" /></a-form-item>
        </a-space>
        <a-checkbox v-model:checked="form.proxy_protocol">向后端发送 PROXY protocol（后端需支持）</a-checkbox>
      </a-form>
    </a-modal>
  </div>
</template>

<style scoped>
.srow {
  display: flex;
  gap: 8px;
  margin-bottom: 8px;
  align-items: center;
}
</style>
