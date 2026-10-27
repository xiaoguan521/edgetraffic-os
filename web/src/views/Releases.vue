<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'
import ReleaseLanes from '../components/ReleaseLanes.vue'
import DiffView from '../components/DiffView.vue'
import { deployStatus, kindName } from '../components/status'

const route = useRoute()
const session = useSession()
const groups = ref<any[]>([])
const gid = ref(0)
const group = computed(() => groups.value.find((g) => g.id === gid.value))
const preview = ref<any>(null)
const previewErr = ref('')
const note = ref('')
const publishing = ref(false)
const deployments = ref<any[]>([])
const revisions = ref<any[]>([])
const active = ref<any>(null)
const openFiles = ref<string[]>([])

async function loadGroups() {
  groups.value = await api.get('/node-groups')
  if (!gid.value && groups.value.length) gid.value = groups.value[0].id
}
async function loadPreview() {
  previewErr.value = ''
  try {
    preview.value = await api.get(`/node-groups/${gid.value}/preview`)
    openFiles.value = preview.value.files.filter((f: any) => f.path.startsWith('conf.d/')).map((f: any) => f.path)
  } catch (e) {
    preview.value = null
    previewErr.value = errMsg(e)
  }
}
async function loadHistory() {
  ;[deployments.value, revisions.value] = await Promise.all([api.get('/deployments', { group_id: gid.value }), api.get('/revisions', { group_id: gid.value })])
  if (!active.value && deployments.value.length) {
    const want = Number(route.query.d)
    active.value = deployments.value.find((d) => d.id === want) || deployments.value[0]
  }
}
async function loadAll() {
  await loadGroups()
  await Promise.all([loadPreview(), loadHistory()])
}

let poll: number | undefined
function watchDeployment(id: number) {
  clearInterval(poll)
  const tick = async () => {
    active.value = await api.get(`/deployments/${id}`)
    if (active.value.status !== 'APPLYING') {
      clearInterval(poll)
      await loadGroups()
      await Promise.all([loadPreview(), loadHistory()])
      const s = active.value.status
      if (s === 'APPLIED') message.success(`${active.value.revision_id} 已在全部在线节点生效`)
      else if (s === 'FAILED' || s === 'PARTIAL') message.warning(`${active.value.revision_id} ${deployStatus[s].text}，详见各节点结果`)
    }
  }
  tick()
  poll = window.setInterval(tick, 700)
}

async function publish() {
  publishing.value = true
  try {
    const d = await api.post(`/node-groups/${gid.value}/publish`, { note: note.value })
    note.value = ''
    active.value = d
    watchDeployment(d.id)
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    publishing.value = false
  }
}

async function rollback(rev: string) {
  try {
    const d = await api.post(`/node-groups/${gid.value}/rollback`, { revision_id: rev })
    active.value = d
    watchDeployment(d.id)
  } catch (e) {
    message.error(errMsg(e))
  }
}

function show(d: any) {
  active.value = d
  if (d.status === 'APPLYING') watchDeployment(d.id)
}

onMounted(async () => {
  await loadAll()
  const running = deployments.value.find((d) => d.status === 'APPLYING')
  if (running) watchDeployment(running.id)
})
onUnmounted(() => clearInterval(poll))
watch(gid, () => {
  active.value = null
  loadPreview()
  loadHistory()
})

const changeText: Record<string, string> = { added: '新增', removed: '删除', modified: '修改' }
// 只有曾经成功生效过的版本才提供回滚入口
function everApplied(rev: string) {
  return deployments.value.some((d) => d.revision_id === rev && (d.status === 'APPLIED' || d.status === 'PARTIAL'))
}
function lastDeploy(rev: string) {
  return deployments.value.find((d) => d.revision_id === rev)
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>发布</h1>
        <p>每次发布生成一个不可变版本。节点逐台校验、切换、加载并探活，任何一步失败都会自动回滚，流量不中断。</p>
      </div>
      <a-segmented v-if="groups.length > 1" v-model:value="gid" :options="groups.map((g) => ({ label: g.name, value: g.id }))" />
    </div>

    <section class="panel">
      <div class="phead">
        <h2>待发布变更</h2>
        <span class="cur">线上版本 <b class="num">{{ group?.current_revision_id || '无' }}</b>，{{ group?.online ?? 0 }} / {{ group?.nodes ?? 0 }} 个节点在线</span>
      </div>
      <a-alert v-if="previewErr" type="error" :message="previewErr" show-icon />
      <template v-else-if="preview">
        <a-empty v-if="!preview.changed" :description="`草稿与线上版本一致，没有需要发布的变更`" />
        <template v-else>
          <a-collapse v-model:active-key="openFiles" :bordered="false" class="files">
            <a-collapse-panel v-for="f in preview.files" :key="f.path">
              <template #header>
                <span class="fchg" :class="f.change">{{ changeText[f.change] }}</span>
                <span class="code">{{ f.path }}</span>
              </template>
              <DiffView v-if="f.diff && f.diff.startsWith('---')" :diff="f.diff" />
              <div v-else class="nodiff">{{ f.diff || '内容较长，未展开' }}</div>
            </a-collapse-panel>
          </a-collapse>
          <div class="pub" v-if="session.can('releaser')">
            <a-input v-model:value="note" placeholder="发布说明，例如：首页改版" :maxlength="120" />
            <a-button type="primary" size="large" :loading="publishing" :disabled="!session.writable" @click="publish">发布到 {{ group?.name }}</a-button>
          </div>
          <div v-if="!session.writable" class="ro">授权已失效，暂不能发布新版本；回滚不受影响。</div>
        </template>
      </template>
    </section>

    <section class="panel" v-if="active">
      <div class="phead">
        <h2>
          {{ kindName[active.kind] || active.kind }} <span class="num">{{ active.revision_id }}</span>
        </h2>
        <span class="cur">
          <a-tag :color="deployStatus[active.status]?.color">{{ deployStatus[active.status]?.text || active.status }}</a-tag>
          {{ active.created_by }}，{{ dayjs(active.started_at).format('MM-DD HH:mm:ss') }}<template v-if="active.note">，{{ active.note }}</template>
        </span>
      </div>
      <a-empty v-if="!active.nodes?.length" description="这个节点组还没有节点，版本已生成，节点接入后自动同步" />
      <ReleaseLanes v-else :nodes="active.nodes" :revision="active.revision_id" />
    </section>

    <section class="panel">
      <h2>版本历史</h2>
      <a-table :data-source="revisions" row-key="id" :pagination="false" size="middle" :columns="[{ title: '版本', key: 'id', width: 110 }, { title: '说明', key: 'note' }, { title: '最近一次下发', key: 'dep', width: 260 }, { title: '创建', key: 'created', width: 170 }, { title: '', key: 'ops', width: 130 }]">
        <template #emptyText><a-empty description="还没有版本" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'id'">
            <span class="num rev">{{ record.id }}</span>
            <a-tag v-if="record.id === group?.current_revision_id" color="cyan" style="margin-left: 6px">线上</a-tag>
          </template>
          <template v-else-if="column.key === 'note'">{{ record.note || '—' }}</template>
          <template v-else-if="column.key === 'dep'">
            <template v-if="lastDeploy(record.id)">
              <a @click="show(lastDeploy(record.id))">
                <span class="kd">{{ kindName[lastDeploy(record.id).kind] }}</span><a-tag :color="deployStatus[lastDeploy(record.id).status]?.color">{{ deployStatus[lastDeploy(record.id).status]?.text }}</a-tag>
              </a>
            </template>
          </template>
          <template v-else-if="column.key === 'created'">
            <div>{{ dayjs(record.created_at).format('MM-DD HH:mm') }}</div>
            <div class="sub">{{ record.created_by }}</div>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-popconfirm v-if="session.can('releaser') && record.id !== group?.current_revision_id && everApplied(record.id)" :title="`把 ${group?.name} 的全部节点回滚到 ${record.id}？`" ok-text="回滚" @confirm="rollback(record.id)">
              <a-button size="small">回滚到此版本</a-button>
            </a-popconfirm>
          </template>
        </template>
      </a-table>
    </section>
  </div>
</template>

<style scoped>
.phead {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 12px;
}
.phead h2 {
  margin: 0;
}
.cur {
  font-size: 13px;
  color: #6b7c80;
}
.cur b {
  color: #16323a;
}
.files :deep(.ant-collapse-item) {
  border-bottom: 1px solid #eef2f1;
}
.fchg {
  display: inline-block;
  width: 40px;
  font-size: 12px;
  font-weight: 600;
}
.fchg.added {
  color: #1f6f43;
}
.fchg.modified {
  color: #8a6510;
}
.fchg.removed {
  color: #a3332d;
}
.nodiff {
  font-size: 12px;
  color: #8a9a9d;
}
.pub {
  display: flex;
  gap: 12px;
  margin-top: 16px;
}
.ro {
  margin-top: 10px;
  font-size: 13px;
  color: #a3332d;
}
.rev {
  font-weight: 600;
  color: #16323a;
}
.kd {
  margin-right: 8px;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
</style>
