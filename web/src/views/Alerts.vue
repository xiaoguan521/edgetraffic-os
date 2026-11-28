<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'

const session = useSession()
const data = ref<any>(null)
const typeName: Record<string, string> = { webhook: 'Webhook', wecom: '企业微信机器人', dingtalk: '钉钉机器人', email: '邮件' }
const unit: Record<string, string> = {
  node_offline: '分钟',
  error_rate: '%',
  cert_expiry: '天',
  drift: '',
  deployment_rolled_back: '',
  backup_stale: '小时',
  disk_usage: '%',
  host_version_mixed: '',
}
const hint: Record<string, string> = {
  node_offline: '节点超过该时长没有心跳',
  error_rate: '近 5 分钟某站点 5xx 占比达到该值（请求不少于 50 次）',
  cert_expiry: '证书剩余天数少于该值',
  drift: '节点上的配置被手工改动',
  deployment_rolled_back: '近期发布失败或灰度发布触发自动回滚',
  backup_stale: '距离控制台上一次成功加密备份超过该时长',
  disk_usage: '控制台数据目录磁盘空间或 inode 使用率达到该阈值',
  host_version_mixed: '同一台主机上的多个实例运行着不同的数据面版本',
}

async function load() {
  data.value = await api.get('/alerts')
}
onMounted(load)

// 维护窗口（全局静默）
const silenceOpen = ref(false)
const silenceForm = reactive({ minutes: 60, reason: '' })
const savingSilence = ref(false)
function openSilence() {
  silenceForm.reason = data.value?.silence_reason || ''
  silenceOpen.value = true
}
async function saveSilence(minutes?: number) {
  savingSilence.value = true
  const mins = minutes !== undefined ? minutes : silenceForm.minutes
  try {
    await api.put('/alerts/silence', { minutes: mins, reason: mins > 0 ? silenceForm.reason.trim() : '' })
    silenceOpen.value = false
    message.success(mins > 0 ? `已开启维护静默 ${mins} 分钟` : '已取消维护静默，恢复告警推送')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    savingSilence.value = false
  }
}

// Tab 与事件筛选（默认先看告警事件，值班优先处理进行中告警）
const activeTab = ref<'events' | 'rules' | 'channels'>('events')
const firingCount = computed(() => (data.value?.events || []).filter((e: any) => e.status === 'firing').length)
const eventStatus = ref<'all' | 'firing' | 'resolved'>('all')
const eventKind = ref('')
const filteredEvents = computed(() =>
  (data.value?.events || []).filter((e: any) => {
    if (eventStatus.value !== 'all' && e.status !== eventStatus.value) return false
    if (eventKind.value && e.kind !== eventKind.value) return false
    return true
  }),
)

const ruleEdit = reactive<Record<string, any>>({})
function ruleOf(k: string) {
  if (!ruleEdit[k]) {
    const r = data.value.rules.find((x: any) => x.kind === k)
    ruleEdit[k] = { enabled: r.enabled, threshold: r.threshold, channel_ids: (r.channel_ids || '').split(',').filter(Boolean).map(Number) }
  }
  return ruleEdit[k]
}
async function saveRule(k: string) {
  try {
    await api.put(`/alert-rules/${k}`, ruleOf(k))
    message.success('已保存')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const open = ref(false)
const editId = ref(0)
const form = reactive({ name: '', type: 'webhook', url: '', secret: '', smtp_user: '', mail_from: '', mail_to: '', enabled: true })
function edit(ch?: any) {
  editId.value = ch?.id || 0
  Object.assign(form, { name: '', type: 'webhook', url: '', secret: '', smtp_user: '', mail_from: '', mail_to: '', enabled: true }, ch ? { ...ch, secret: '' } : {})
  open.value = true
}
async function saveChannel() {
  try {
    if (editId.value) await api.put(`/alert-channels/${editId.value}`, form)
    else await api.post('/alert-channels', form)
    open.value = false
    message.success('已保存')
    await load()
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function test(ch: any) {
  try {
    await api.post(`/alert-channels/${ch.id}/test`, {})
    message.success('测试消息已发送')
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function del(ch: any) {
  await api.del(`/alert-channels/${ch.id}`)
  await load()
}
const channelOpts = computed(() => (data.value?.channels || []).map((c: any) => ({ value: c.id, label: c.name })))
const typeOpts = computed(() =>
  Object.entries(typeName).map(([value, label]) => ({ value, label, disabled: (value === 'wecom' || value === 'dingtalk') && !data.value?.im_licensed })),
)
</script>

<template>
  <div class="page" v-if="data">
    <div class="page-head">
      <div>
        <h1>告警</h1>
        <p>控制台每分钟评估一次规则，告警触发和恢复时各推送一次；同一对象不会重复推送。</p>
      </div>
      <a-space v-if="session.can('admin')">
        <a-button v-if="data.silence_until" :loading="savingSilence" @click="saveSilence(0)">取消维护静默</a-button>
        <a-button :disabled="!session.writable" @click="openSilence">{{ data.silence_until ? '调整维护窗口' : '开启维护静默' }}</a-button>
      </a-space>
    </div>

    <a-alert
      v-if="data.silence_until"
      type="warning"
      show-icon
      style="margin-bottom: 16px"
      :message="`当前处于告警维护静默窗口（至 ${dayjs(data.silence_until).format('MM-DD HH:mm')}）`"
      :description="`${data.silence_reason ? `原因：${data.silence_reason}。` : ''}静默期间告警事件仍会记录到下方列表，但暂停向外部通道推送通知。`"
    />

    <section class="panel">
      <a-tabs v-model:active-key="activeTab" class="alert-tabs">
        <a-tab-pane key="events" :tab="firingCount ? `告警事件（${firingCount} 告警中）` : `告警事件 (${(data.events || []).length})`">
          <div class="phead" style="margin-bottom: 12px">
            <a-space wrap size="small">
              <a-segmented
                v-model:value="eventStatus"
                size="small"
                :options="[
                  { label: `全部 (${(data.events || []).length})`, value: 'all' },
                  { label: `告警中 (${firingCount})`, value: 'firing' },
                  { label: '已恢复', value: 'resolved' },
                ]"
              />
              <a-select
                v-model:value="eventKind"
                allow-clear
                size="small"
                placeholder="按规则筛选"
                style="width: 160px"
                :options="Object.entries(data.kind_names || {}).map(([value, label]) => ({ value, label: String(label) }))"
              />
            </a-space>
          </div>
          <a-table :data-source="filteredEvents" row-key="id" size="middle" :scroll="{ x: 'max-content' }" :pagination="{ pageSize: 20, hideOnSinglePage: true }"
            :columns="[{ title: '时间', key: 'ts', width: 150 }, { title: '规则', key: 'kind', width: 130 }, { title: '内容', dataIndex: 'message' }, { title: '状态', key: 'st', width: 160 }, { title: '通知', dataIndex: 'notify' }]">
            <template #emptyText><a-empty description="没有告警记录" /></template>
            <template #bodyCell="{ column, record }">
              <template v-if="column.key === 'ts'"><span class="num">{{ dayjs(record.fired_at).format('MM-DD HH:mm:ss') }}</span></template>
              <template v-else-if="column.key === 'kind'">{{ data.kind_names[record.kind] || record.kind }}</template>
              <template v-else-if="column.key === 'st'">
                <a-tag v-if="record.status === 'firing'" color="red">告警中</a-tag>
                <span v-else class="sub">已恢复 {{ dayjs(record.resolved_at).format('MM-DD HH:mm') }}</span>
              </template>
            </template>
          </a-table>
        </a-tab-pane>

        <a-tab-pane key="rules" :tab="`告警规则 (${(data.rules || []).length})`">
          <div class="rules">
            <div v-for="r in data.rules" :key="r.kind" class="rule">
              <div class="rh">
                <a-switch :checked="ruleOf(r.kind).enabled" :disabled="!session.can('admin')" @change="(v: any) => (ruleOf(r.kind).enabled = v)" />
                <b>{{ data.kind_names[r.kind] || r.kind }}</b>
              </div>
              <div class="sub">{{ hint[r.kind] || r.kind }}</div>
              <a-space wrap>
                <a-input-number v-if="unit[r.kind]" v-model:value="ruleOf(r.kind).threshold" :min="0" :addon-after="unit[r.kind]" style="width: 150px" />
                <a-select v-model:value="ruleOf(r.kind).channel_ids" mode="multiple" placeholder="推送通道" style="min-width: 220px" :options="channelOpts" />
                <a-button v-if="session.can('admin')" size="small" :disabled="!session.writable" @click="saveRule(r.kind)">保存</a-button>
              </a-space>
            </div>
          </div>
        </a-tab-pane>

        <a-tab-pane key="channels" :tab="`通知通道 (${(data.channels || []).length})`">
          <div class="phead" style="margin-bottom: 10px">
            <span class="sub">
              <template v-if="!data.im_licensed">当前授权不包含企业微信 / 钉钉告警（专业版及以上），可使用邮件与 Webhook。</template>
              <template v-else>支持 Webhook、企业微信、钉钉机器人与 SMTP 邮件通道。</template>
            </span>
            <a-button v-if="session.can('admin')" type="primary" size="small" :disabled="!session.writable" @click="edit()">新建通道</a-button>
          </div>
          <a-table :data-source="data.channels" row-key="id" :pagination="false" size="middle" :scroll="{ x: 'max-content' }"
            :columns="[{ title: '名称', dataIndex: 'name' }, { title: '类型', key: 'type', width: 140 }, { title: '地址', key: 'url' }, { title: '状态', key: 'st', width: 90 }, { title: '', key: 'op', width: 200 }]">
            <template #emptyText><a-empty description="还没有告警通道" /></template>
            <template #bodyCell="{ column, record }">
              <template v-if="column.key === 'type'">{{ typeName[record.type] }}</template>
              <template v-else-if="column.key === 'url'"><span class="code url">{{ record.url }}</span><a-tag v-if="record.has_secret" style="margin-left: 6px">已配置密钥</a-tag></template>
              <template v-else-if="column.key === 'st'"><a-tag :color="record.enabled ? 'green' : 'default'">{{ record.enabled ? '启用' : '停用' }}</a-tag></template>
              <template v-else-if="column.key === 'op' && session.can('admin')">
                <a-space :size="4">
                  <a-button size="small" @click="test(record)">发送测试</a-button>
                  <a-button size="small" @click="edit(record)">编辑</a-button>
                  <a-popconfirm title="删除该通道？" @confirm="del(record)"><a-button size="small" danger>删除</a-button></a-popconfirm>
                </a-space>
              </template>
            </template>
          </a-table>
        </a-tab-pane>
      </a-tabs>
    </section>

    <a-modal v-model:open="silenceOpen" title="告警维护静默窗口" ok-text="开启静默" :confirm-loading="savingSilence" @ok="saveSilence()">
      <p class="sub" style="margin-top: 0">例行发布或升级期间可临时暂停外部告警推送，告警记录仍正常落库。</p>
      <a-form layout="vertical">
        <a-form-item label="静默时长（分钟）">
          <a-space wrap>
            <a-input-number v-model:value="silenceForm.minutes" :min="5" :max="10080" />
            <a-button size="small" @click="silenceForm.minutes = 30">30 分钟</a-button>
            <a-button size="small" @click="silenceForm.minutes = 60">1 小时</a-button>
            <a-button size="small" @click="silenceForm.minutes = 120">2 小时</a-button>
            <a-button size="small" @click="silenceForm.minutes = 1440">24 小时</a-button>
          </a-space>
        </a-form-item>
        <a-form-item label="原因（可选）"><a-input v-model:value="silenceForm.reason" :maxlength="120" placeholder="例如：夜间数据面热升级" /></a-form-item>
      </a-form>
    </a-modal>

    <a-modal v-model:open="open" :title="editId ? '编辑通道' : '新建通道'" @ok="saveChannel">
      <a-form layout="vertical">
        <a-form-item label="名称"><a-input v-model:value="form.name" /></a-form-item>
        <a-form-item label="类型"><a-select v-model:value="form.type" :options="typeOpts" /></a-form-item>
        <template v-if="form.type === 'email'">
          <a-form-item label="邮件服务器（smtp://主机:587 使用 STARTTLS，smtps://主机:465）"><a-input v-model:value="form.url" placeholder="smtp://smtp.example.com:587" /></a-form-item>
          <a-form-item label="用户名"><a-input v-model:value="form.smtp_user" autocomplete="off" /></a-form-item>
          <a-form-item :label="editId ? '口令（留空不改）' : '口令'"><a-input-password v-model:value="form.secret" autocomplete="new-password" /></a-form-item>
          <a-form-item label="发件人"><a-input v-model:value="form.mail_from" /></a-form-item>
          <a-form-item label="收件人（逗号分隔）"><a-input v-model:value="form.mail_to" /></a-form-item>
        </template>
        <template v-else>
          <a-form-item label="地址"><a-input v-model:value="form.url" :placeholder="form.type === 'wecom' ? 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…' : form.type === 'dingtalk' ? 'https://oapi.dingtalk.com/robot/send?access_token=…' : 'https://'" /></a-form-item>
          <a-form-item v-if="form.type === 'dingtalk'" :label="editId ? '加签密钥（留空不改）' : '加签密钥（可选）'"><a-input-password v-model:value="form.secret" autocomplete="new-password" /></a-form-item>
        </template>
        <a-checkbox v-model:checked="form.enabled">启用</a-checkbox>
      </a-form>
    </a-modal>
  </div>
</template>

<style scoped>
.rules {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 12px;
}
.rule {
  border: 1px solid #e1e7e6;
  border-radius: 8px;
  padding: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.rh {
  display: flex;
  gap: 8px;
  align-items: center;
}
.phead {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.phead h2 {
  margin: 0;
}
.sub {
  font-size: 12px;
  color: #6b7c80;
}
.url {
  word-break: break-all;
}
</style>
