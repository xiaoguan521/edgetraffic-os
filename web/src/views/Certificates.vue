<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { useSession } from '../stores/session'

const session = useSession()
const certs = ref<any[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    certs.value = await api.get('/certificates')
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 上传 / 续期共用一个表单：renewing 为空表示上传新证书
const open = ref(false)
const renewing = ref<any>(null)
const saving = ref(false)
const error = ref('')
const form = reactive({ name: '', cert_pem: '', key_pem: '' })

function openUpload() {
  renewing.value = null
  Object.assign(form, { name: '', cert_pem: '', key_pem: '' })
  error.value = ''
  open.value = true
}
function openRenew(c: any) {
  renewing.value = c
  Object.assign(form, { name: c.name, cert_pem: '', key_pem: '' })
  error.value = ''
  open.value = true
}

// 从本地文件读取 PEM，避免手工复制粘贴出错
function readFile(field: 'cert_pem' | 'key_pem') {
  const input = document.createElement('input')
  input.type = 'file'
  input.accept = '.pem,.crt,.cer,.key,.txt'
  input.onchange = async () => {
    const file = input.files?.[0]
    if (file) form[field] = await file.text()
  }
  input.click()
}

function describeDeploys(list: any[]) {
  if (!list?.length) return '没有站点在使用，无需下发。'
  const started = list.filter((d) => d.status === 'started').length
  const busy = list.filter((d) => d.status === 'busy').length
  const failed = list.filter((d) => d.status === 'failed')
  const parts = []
  if (started) parts.push(`已向 ${started} 个节点组下发新证书`)
  if (busy) parts.push(`${busy} 个节点组正在发布，请稍后点“重新下发”`)
  if (failed.length) parts.push(`${failed.length} 个节点组下发失败：${failed[0].message}`)
  if (!parts.length) parts.push('线上版本中没有需要替换的证书')
  return parts.join('；')
}

async function submit() {
  error.value = ''
  saving.value = true
  try {
    if (renewing.value) {
      const r = await api.put(`/certificates/${renewing.value.id}`, { cert_pem: form.cert_pem, key_pem: form.key_pem })
      message.success(`证书已更新。${describeDeploys(r.deployments)}`, 6)
    } else {
      await api.post('/certificates', form)
      message.success('证书已上传，可在站点的 HTTPS 设置中选择')
    }
    open.value = false
    load()
  } catch (e) {
    error.value = errMsg(e)
  } finally {
    saving.value = false
  }
}

async function redeploy(c: any) {
  try {
    const r = await api.post(`/certificates/${c.id}/deploy`)
    message.success(describeDeploys(r.deployments), 6)
  } catch (e) {
    message.error(errMsg(e))
  }
}

async function remove(c: any) {
  try {
    await api.del(`/certificates/${c.id}`)
    message.success(`已删除证书 ${c.name}`)
    load()
  } catch (e) {
    message.error(errMsg(e))
  }
}

const statusTag: Record<string, { text: string; color: string }> = {
  valid: { text: '有效', color: 'green' },
  expiring: { text: '即将到期', color: 'gold' },
  expired: { text: '已过期', color: 'red' },
}

const columns = [
  { title: '证书', key: 'name' },
  { title: '域名', key: 'domains' },
  { title: '到期', key: 'expiry', width: 170 },
  { title: '使用站点', key: 'sites', width: 160 },
  { title: '', key: 'ops', width: 200 },
]
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>证书</h1>
        <p>站点从这里选择证书。私钥加密保存，任何页面都不会回显。证书续期在授权过期后仍可使用，续期后自动下发到引用它的节点组。</p>
      </div>
      <a-button v-if="session.can('operator')" type="primary" :disabled="!session.writable" @click="openUpload">上传证书</a-button>
    </div>

    <div class="panel">
      <a-table :columns="columns" :data-source="certs" row-key="id" :pagination="false" :loading="loading">
        <template #emptyText>
          <a-empty description="还没有证书。上传证书与私钥（PEM 格式）后，就能在站点中启用 HTTPS。" />
        </template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <div class="nm">{{ record.name }}</div>
            <div class="sub">签发者 {{ record.issuer || '—' }}<span v-if="record.source === 'migrated'"> · 由旧版本站点迁移</span></div>
          </template>
          <template v-else-if="column.key === 'domains'">
            <span v-for="d in record.domains" :key="d" class="dm code">{{ d }}</span>
          </template>
          <template v-else-if="column.key === 'expiry'">
            <a-tag :color="statusTag[record.status].color">{{ statusTag[record.status].text }}</a-tag>
            <div class="sub num">{{ dayjs(record.not_after).format('YYYY-MM-DD') }}<template v-if="record.status !== 'expired'">，剩 {{ record.days_left }} 天</template></div>
          </template>
          <template v-else-if="column.key === 'sites'">
            <span v-if="!record.sites" class="sub">未使用</span>
            <span v-else>{{ record.site_list.join('、') }}</span>
          </template>
          <template v-else-if="column.key === 'ops'">
            <a-space v-if="session.can('operator')" :size="0">
              <a-button type="link" size="small" @click="openRenew(record)">续期</a-button>
              <a-button v-if="record.sites" type="link" size="small" @click="redeploy(record)">重新下发</a-button>
              <a-popconfirm v-if="!record.sites" :title="`删除证书 ${record.name}？`" ok-text="删除" @confirm="remove(record)">
                <a-button type="link" danger size="small" :disabled="!session.writable">删除</a-button>
              </a-popconfirm>
            </a-space>
          </template>
        </template>
      </a-table>
    </div>

    <a-modal v-model:open="open" :title="renewing ? `续期证书 ${renewing.name}` : '上传证书'" :ok-text="renewing ? '续期并下发' : '上传'" :confirm-loading="saving" width="680px" @ok="submit">
      <a-alert v-if="renewing" type="info" show-icon style="margin-bottom: 16px" :message="`新证书必须仍包含 ${renewing.site_list.length ? '站点使用的全部域名' : '原有域名'}。续期后会以线上版本为基础只替换证书文件，生成新版本下发，不会带上未发布的草稿改动。`" />
      <a-form layout="vertical">
        <a-form-item v-if="!renewing" label="名称" extra="小写字母、数字、连字符，例如 shop-example-com">
          <a-input v-model:value="form.name" placeholder="shop-example-com" />
        </a-form-item>
        <a-form-item label="证书（PEM，可包含中间证书）">
          <template #extra><a @click="readFile('cert_pem')">从文件读取</a></template>
          <a-textarea v-model:value="form.cert_pem" :rows="5" class="code" placeholder="-----BEGIN CERTIFICATE-----" />
        </a-form-item>
        <a-form-item label="私钥（PEM，不能带密码）">
          <template #extra><a @click="readFile('key_pem')">从文件读取</a></template>
          <a-textarea v-model:value="form.key_pem" :rows="5" class="code" placeholder="-----BEGIN PRIVATE KEY-----" />
        </a-form-item>
      </a-form>
      <a-alert v-if="error" type="error" :message="error" show-icon />
    </a-modal>
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
.dm {
  display: inline-block;
  margin: 0 8px 4px 0;
  font-size: 12.5px;
}
</style>
