<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg, http } from '../api/client'
import { useSession } from '../stores/session'

const session = useSession()
const rows = ref<any[]>([])
const loading = ref(false)
const verify = ref<any>(null)
const filter = reactive({ q: '', username: '', action: '', result: '' })

async function load() {
  loading.value = true
  try {
    const params: Record<string, any> = { limit: 500 }
    if (filter.q.trim()) params.q = filter.q.trim()
    if (filter.username.trim()) params.username = filter.username.trim()
    if (filter.action) params.action = filter.action
    if (filter.result) params.result = filter.result
    rows.value = await api.get('/audit', params)
    if (session.can('admin') && !verify.value) verify.value = await api.get('/audit/verify')
  } finally {
    loading.value = false
  }
}
function resetFilter() {
  Object.assign(filter, { q: '', username: '', action: '', result: '' })
  load()
}
onMounted(load)

async function exportCsv() {
  try {
    const r = await http.get('/audit/export', { responseType: 'blob' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(r.data)
    a.download = `audit-${dayjs().format('YYYYMMDD-HHmm')}.csv`
    a.click()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const actionName: Record<string, string> = {
  'system.setup': '初始化系统',
  'auth.login': '登录',
  'license.import': '导入授权',
  'group.create': '新建节点组',
  'group.update': '修改节点组',
  'group.copy_config': '跨组复制配置',
  'node.enroll_token': '生成注册令牌',
  'node.enroll': '节点注册',
  'node.update': '编辑/迁移节点',
  'node.restore': '恢复节点配置',
  'node.sync': '对齐节点版本',
  'node.tail_log': '查看节点实时日志',
  'node.revoke': '吊销节点',
  'site.create': '新建站点',
  'site.update': '修改站点',
  'site.delete': '删除站点',
  'upstream.create': '新建上游',
  'upstream.update': '修改上游',
  'upstream.delete': '删除上游',
  'ip_set.create': '新建 IP 集',
  'ip_set.update': '修改 IP 集',
  'ip_set.import': '导入地域 IP 集',
  'ip_set.delete': '删除 IP 集',
  'rule_template.save': '保存规则模板',
  'rule_template.delete': '删除规则模板',
  'release.publish': '发布',
  'release.rollback': '回滚',
  'release.cert_renew': '证书续期下发',
  'release.result': '发布结果',
  'config.import_analyze': '配置导入分析',
  'config.import_apply': '配置导入应用',
  'alert.silence': '维护静默设置',
  'alert.rule_save': '修改告警规则',
  'alert.channel_save': '保存告警通道',
  'alert.channel_delete': '删除告警通道',
  'audit.export': '导出审计',
  'user.create': '新建用户',
}
const resultColor: Record<string, string> = { success: 'green', applied: 'green', failure: 'red', failed: 'red', partial: 'orange', pending_nodes: 'default' }
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>审计</h1>
        <p>
          所有写操作只增不删，每条记录包含上一条的哈希。
          <template v-if="verify">
            <span v-if="verify.intact" class="ok">哈希链校验完好。</span>
            <span v-else class="bad">第 {{ verify.first_bad_id }} 条记录之后被篡改。</span>
          </template>
        </p>
      </div>
      <a-button v-if="session.can('admin')" @click="exportCsv">导出 CSV</a-button>
    </div>
    <div class="panel">
      <div class="toolbar">
        <a-input v-model:value="filter.q" allow-clear placeholder="搜索关键词（对象/详情/动作）" style="width: 240px" @press-enter="load" />
        <a-input v-model:value="filter.username" allow-clear placeholder="操作人" style="width: 140px" @press-enter="load" />
        <a-select
          v-model:value="filter.action"
          allow-clear
          placeholder="操作类型"
          style="width: 160px"
          :options="Object.entries(actionName).map(([value, label]) => ({ value, label }))"
          @change="load"
        />
        <a-select
          v-model:value="filter.result"
          allow-clear
          placeholder="结果"
          style="width: 120px"
          :options="['success', 'applied', 'failed', 'failure', 'partial'].map((v) => ({ value: v, label: v }))"
          @change="load"
        />
        <a-button type="primary" :loading="loading" @click="load">查询</a-button>
        <a-button @click="resetFilter">重置</a-button>
      </div>
      <a-table :data-source="rows" :loading="loading" row-key="id" size="middle" :scroll="{ x: 'max-content' }" :pagination="{ pageSize: 20, showSizeChanger: true, pageSizeOptions: ['20', '50', '100'] }" :columns="[{ title: '时间', key: 'ts', width: 170 }, { title: '操作人', key: 'user', width: 160 }, { title: '操作', key: 'action', width: 140 }, { title: '对象', key: 'obj', width: 160 }, { title: '结果', key: 'result', width: 100 }, { title: '详情', key: 'detail' }]">
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'ts'"><span class="num">{{ dayjs(record.ts).format('YYYY-MM-DD HH:mm:ss') }}</span></template>
          <template v-else-if="column.key === 'user'">
            <div>{{ record.username || '系统' }}</div>
            <div class="sub code">{{ record.ip }}</div>
          </template>
          <template v-else-if="column.key === 'action'">{{ actionName[record.action] || record.action }}</template>
          <template v-else-if="column.key === 'obj'"><span class="code">{{ record.object_type }} {{ record.object_id }}</span></template>
          <template v-else-if="column.key === 'result'"><a-tag :color="resultColor[record.result]">{{ record.result }}</a-tag></template>
          <template v-else-if="column.key === 'detail'"><span class="detail code">{{ record.detail }}</span></template>
        </template>
      </a-table>
    </div>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
.detail {
  color: #4a5a5e;
  word-break: break-all;
}
.ok {
  color: #1f6f43;
}
.bad {
  color: #a3332d;
  font-weight: 600;
}
</style>
