<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'
import { editionName, licenseStateName, useSession } from '../stores/session'
import LicenseImport from '../components/LicenseImport.vue'

const session = useSession()
const lic = computed(() => session.license)
const users = ref<any[]>([])
const version = ref<any>({})

const approval = ref<{ enabled: boolean; licensed: boolean }>({ enabled: false, licensed: false })
onMounted(async () => {
  await session.loadLicense()
  approval.value = await api.get('/release-approval')
  if (session.can('admin')) users.value = await api.get('/users')
})
const tokens = ref<any[]>([])
const tkOpen = ref(false)
const tkForm = reactive({ name: '', role: 'operator', expire_days: 90 })
const newToken = ref('')
async function loadTokens() {
  tokens.value = await api.get('/api-tokens').catch(() => [])
}
onMounted(loadTokens)
async function createToken() {
  try {
    const r = await api.post('/api-tokens', tkForm)
    newToken.value = r.token
    tkOpen.value = false
    await loadTokens()
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function revokeToken(id: number) {
  try {
    await api.del(`/api-tokens/${id}`)
    await loadTokens()
  } catch (e) {
    message.error(errMsg(e))
  }
}
const apiLicensed = computed(() => !!lic.value?.features?.includes('api_token'))

async function setApproval(on: boolean) {
  try {
    approval.value = await api.put('/release-approval', { enabled: on })
    message.success(on ? '已开启发布审批' : '已关闭发布审批')
  } catch (e) {
    message.error(errMsg(e))
  }
}

const featureName: Record<string, string> = {
  core: '站点、证书、发布、回滚、监控',
  api_token: '开放 API',
  batch_release: '批量发布',
  release_approval: '发布审批',
  sso: 'LDAP / OIDC 单点登录',
  audit_export: '审计导出',
  alert_im: '企业微信 / 钉钉告警',
  stream: '四层代理',
  ha: '高可用',
  canary: '灰度发布',
  gm_tls: '国密 TLS',
}
const allFeatures = Object.keys(featureName)
const tone = computed(() => (lic.value?.writable ? (lic.value.state === 'VALID' ? 'ok' : 'warn') : 'bad'))

const uOpen = ref(false)
const uForm = reactive({ username: '', password: '', role: 'operator' })
async function addUser() {
  try {
    await api.post('/users', uForm)
    message.success('已创建')
    uOpen.value = false
    users.value = await api.get('/users')
  } catch (e) {
    message.error(errMsg(e))
  }
}
const roleName: Record<string, string> = { admin: '管理员', operator: '运维', releaser: '发布员', viewer: '只读' }
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>授权与系统</h1>
        <p>授权过期只限制控制台的新增变更，从不停止或干扰节点上的 syapp 流量。</p>
      </div>
    </div>

    <div class="cols">
      <section class="panel lic" :class="tone" v-if="lic">
        <div class="state">
          <span class="badge">{{ licenseStateName[lic.state] }}</span>
          <span v-if="lic.edition" class="ed">{{ editionName[lic.edition] }}</span>
        </div>
        <h2 v-if="lic.customer">{{ lic.customer }}</h2>
        <p v-if="lic.reason" class="reason">{{ lic.reason }}</p>
        <dl v-if="lic.license_id">
          <dt>授权编号</dt>
          <dd class="code">{{ lic.license_id }}</dd>
          <dt>到期日</dt>
          <dd>{{ dayjs(lic.expires_at).format('YYYY-MM-DD') }}<span class="sub">（{{ lic.days_left >= 0 ? `剩余 ${lic.days_left} 天` : `已过期 ${-lic.days_left} 天` }}）</span></dd>
          <dt>维保到期</dt>
          <dd>{{ dayjs(lic.maintenance_until).format('YYYY-MM-DD') }}</dd>
          <dt>节点</dt>
          <dd>
            <a-progress :percent="lic.nodes_limit ? Math.round((lic.nodes_used / lic.nodes_limit) * 100) : 0" :show-info="false" size="small" />
            <span class="num">{{ lic.nodes_used }} / {{ lic.nodes_limit || '不限' }}</span>
          </dd>
          <dt>站点</dt>
          <dd>
            <a-progress :percent="lic.sites_limit ? Math.round((lic.sites_used / lic.sites_limit) * 100) : 0" :show-info="false" size="small" />
            <span class="num">{{ lic.sites_used }} / {{ lic.sites_limit || '不限' }}</span>
          </dd>
          <dt>安装 ID</dt>
          <dd class="code">{{ lic.installation_id }}</dd>
          <dt>吊销列表</dt>
          <dd v-if="lic.crl">序号 {{ lic.crl.serial }}<span class="sub">（{{ dayjs(lic.crl.issued_at).format('YYYY-MM-DD') }}，{{ lic.crl.revoked }} 项）</span></dd>
          <dd v-else class="sub">无</dd>
        </dl>
        <template v-else>
          <dl>
            <dt>安装 ID</dt>
            <dd class="code">{{ lic.installation_id }}</dd>
          </dl>
        </template>
        <h3>功能</h3>
        <ul class="feats">
          <li v-for="f in allFeatures" :key="f" :class="{ on: lic.features?.includes(f) }">{{ featureName[f] }}</li>
        </ul>
      </section>

      <section class="panel" v-if="session.can('admin')">
        <h2>{{ lic?.license_id ? '更新授权' : '导入授权' }}</h2>
        <LicenseImport />
      </section>
    </div>

    <section class="panel">
      <div class="uhead">
        <h2>开放 API 令牌</h2>
        <a-button size="small" :disabled="!apiLicensed || !session.writable" @click="tkOpen = true">新建令牌</a-button>
      </div>
      <p class="muted">
        用 <span class="code">Authorization: Bearer &lt;令牌&gt;</span> 调用 <span class="code">/api/v1</span>，接口清单见 <a href="/api/v1/openapi.json" target="_blank">openapi.json</a>。令牌权限不高于创建者，创建者被降级时随之降级。
        <template v-if="!apiLicensed">当前授权不包含“开放 API”。</template>
      </p>
      <a-alert v-if="newToken" type="success" show-icon closable style="margin: 8px 0" @close="newToken = ''">
        <template #message>令牌只显示这一次，请立即保存：<span class="code">{{ newToken }}</span></template>
      </a-alert>
      <a-table v-if="tokens.length" :data-source="tokens" row-key="id" :pagination="false" size="small"
        :columns="[{ title: '名称', dataIndex: 'name' }, { title: '前缀', key: 'prefix' }, { title: '用户 / 角色', key: 'who' }, { title: '到期', key: 'exp' }, { title: '最近使用', key: 'used' }, { title: '', key: 'op', width: 80 }]">
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'prefix'"><span class="code">{{ record.prefix }}…</span></template>
          <template v-else-if="column.key === 'who'">{{ record.username }} · {{ roleName[record.role] }}</template>
          <template v-else-if="column.key === 'exp'">{{ record.expires_at ? dayjs(record.expires_at).format('YYYY-MM-DD') : '不过期' }}</template>
          <template v-else-if="column.key === 'used'">{{ record.last_used_at ? dayjs(record.last_used_at).format('MM-DD HH:mm') + ' ' + record.last_used_ip : '从未' }}</template>
          <template v-else-if="column.key === 'op'">
            <a-tag v-if="record.revoked">已撤销</a-tag>
            <a-popconfirm v-else title="撤销后立即失效，确定？" @confirm="revokeToken(record.id)"><a class="danger">撤销</a></a-popconfirm>
          </template>
        </template>
      </a-table>
      <a-modal v-model:open="tkOpen" title="新建 API 令牌" @ok="createToken">
        <a-form layout="vertical">
          <a-form-item label="名称"><a-input v-model:value="tkForm.name" placeholder="例如 ci-pipeline" /></a-form-item>
          <a-form-item label="角色（不高于自己）">
            <a-select v-model:value="tkForm.role" :options="Object.entries(roleName).map(([value, label]) => ({ value, label }))" />
          </a-form-item>
          <a-form-item label="有效天数（0 表示不过期）"><a-input-number v-model:value="tkForm.expire_days" :min="0" :max="3650" /></a-form-item>
        </a-form>
      </a-modal>
    </section>

    <section class="panel" v-if="session.can('admin')">
      <div class="uhead">
        <h2>发布审批</h2>
        <a-switch :checked="approval.enabled" :disabled="!approval.enabled && (!approval.licensed || !session.writable)" @change="(v: any) => setApproval(!!v)" />
      </div>
      <p class="muted">
        开启后，每次发布都要由另一位发布员看过差异并批准才会下发（提交人不能批准自己的申请）。回滚、漂移恢复与证书续期不经审批，保证应急与授权过期时可用。
        <template v-if="!approval.licensed">当前授权不包含“发布审批”（专业版及以上）。</template>
      </p>
    </section>

    <section class="panel" v-if="session.can('admin')">
      <div class="uhead">
        <h2>用户</h2>
        <a-button size="small" :disabled="!session.writable" @click="uOpen = true">新建用户</a-button>
      </div>
      <a-table :data-source="users" row-key="id" :pagination="false" size="middle" :columns="[{ title: '用户名', dataIndex: 'username' }, { title: '角色', key: 'role' }, { title: '最近登录', key: 'login' }]">
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'role'">{{ roleName[record.role] || record.role }}</template>
          <template v-else-if="column.key === 'login'">{{ record.last_login_at ? dayjs(record.last_login_at).format('YYYY-MM-DD HH:mm') : '从未' }}</template>
        </template>
      </a-table>
    </section>

    <section class="panel about">
      <h2>关于</h2>
      <p>EdgeTraffic OS，数据面 syapp。</p>
      <p class="sub">The syapp data plane is based on NGINX Open Source (BSD-2-Clause). 完整的第三方许可声明见安装目录 /opt/syapp/share/doc/NOTICE。</p>
    </section>

    <a-modal v-model:open="uOpen" title="新建用户" ok-text="创建" @ok="addUser">
      <a-form layout="vertical">
        <a-form-item label="用户名"><a-input v-model:value="uForm.username" /></a-form-item>
        <a-form-item label="初始密码"><a-input-password v-model:value="uForm.password" /></a-form-item>
        <a-form-item label="角色">
          <a-radio-group v-model:value="uForm.role" :options="[{ label: '运维', value: 'operator' }, { label: '发布员', value: 'releaser' }, { label: '只读', value: 'viewer' }, { label: '管理员', value: 'admin' }]" />
        </a-form-item>
      </a-form>
    </a-modal>
  </div>
</template>

<style scoped>
.cols {
  display: grid;
  grid-template-columns: 1.2fr 1fr;
  gap: 16px;
  margin-bottom: 16px;
  align-items: start;
}
.cols .panel + .panel {
  margin-top: 0;
}
.lic {
  border-top: 4px solid #2f9e5b;
}
.lic.warn {
  border-top-color: #d9a21b;
}
.lic.bad {
  border-top-color: #c8423b;
}
.state {
  display: flex;
  gap: 10px;
  align-items: center;
  margin-bottom: 8px;
}
.badge {
  font-size: 13px;
  font-weight: 600;
  padding: 2px 10px;
  border-radius: 10px;
  background: #e6f3ec;
  color: #1f6f43;
}
.warn .badge {
  background: #fbf1d8;
  color: #8a6510;
}
.bad .badge {
  background: #f8e3e1;
  color: #a3332d;
}
.ed {
  color: #6b7c80;
}
.lic h2 {
  font-size: 18px;
}
.reason {
  color: #a3332d;
  margin: -6px 0 12px;
}
dl {
  display: grid;
  grid-template-columns: 88px 1fr;
  row-gap: 10px;
  margin: 0 0 16px;
  font-size: 13px;
}
dt {
  color: #6b7c80;
}
dd {
  margin: 0;
  color: #1f2a2e;
  display: flex;
  align-items: center;
  gap: 10px;
}
dd :deep(.ant-progress) {
  width: 160px;
  margin: 0;
}
.sub {
  font-size: 12px;
  color: #8a9a9d;
}
h3 {
  font-size: 13px;
  color: #16323a;
  margin: 0 0 8px;
}
.feats {
  list-style: none;
  padding: 0;
  margin: 0;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 6px 16px;
  font-size: 13px;
}
.feats li {
  color: #a7b3b5;
  padding-left: 18px;
  position: relative;
}
.feats li::before {
  content: '';
  position: absolute;
  left: 0;
  top: 7px;
  width: 8px;
  height: 8px;
  border-radius: 2px;
  background: #d6dedd;
}
.feats li.on {
  color: #1f2a2e;
}
.feats li.on::before {
  background: #0e7c86;
}
.uhead {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.about p {
  margin: 0 0 4px;
}
@media (max-width: 980px) {
  .cols {
    grid-template-columns: 1fr;
  }
}
.muted {
  margin: 8px 0 0;
  font-size: 13px;
  color: #6b7c80;
}
.danger {
  color: #a3332d;
}
</style>
