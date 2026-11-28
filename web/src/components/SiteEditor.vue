<script setup lang="ts">
import { brand } from '../brand'
import { computed, reactive, ref, watch } from 'vue'
import { Modal, message } from 'antdv-next'
import { api, errMsg, http } from '../api/client'
import RulesEditor, { type Rule as AccessRule } from './RulesEditor.vue'

const props = defineProps<{ open: boolean; site: any | null; groupId: number; upstreams: any[]; focusRoute?: string; migration?: boolean }>()
const emit = defineEmits<{ close: []; saved: []; upstreamCreated: [name: string] }>()

interface Route {
  path: string
  match: 'prefix' | 'exact'
  mode: 'static' | 'proxy' | 'redirect' | 'deny'
  static?: { index_html: string; bundle_id?: number; spa?: boolean }
  proxy?: {
    upstream: string
    websocket?: boolean
    https?: boolean
    strip_prefix?: boolean
    connect_timeout?: number
    read_timeout?: number
    send_timeout?: number
    buffering?: boolean
  }
  redirect?: { to: string; code: number }
}

const blank = () => ({
  name: '',
  domains: [] as string[],
  https: false,
  cert_id: undefined as number | undefined,
  gm_cert_id: undefined as number | undefined,
  force_https: false,
  http2: false,
  client_max_body_size: '',
  gzip: false,
  enabled: true,
  version: 0,
  routes: [{ path: '/', match: 'prefix', mode: 'static', static: { index_html: `<h1>Hello from ${brand.product}</h1>\n`, spa: false } }] as Route[],
  rules: [] as AccessRule[],
})
const f = reactive(blank())
const saving = ref(false)
const error = ref('')
const isNew = computed(() => !props.site)
const certs = ref<any[]>([])
const bundles = ref<any[]>([])
const ipSets = ref<any[]>([])
const localUpstreams = ref<any[]>([])
const routeSearch = ref('')
const editorTab = ref<'basic' | 'routes' | 'rules'>('basic')

// 快速新建上游
const quickUpOpen = ref(false)
const quickUpForm = reactive({ name: '', algorithm: 'least_conn', addr: '' })
const quickUpSaving = ref(false)
const quickUpTarget = ref<Route | null>(null)
function openQuickUp(r: Route) {
  quickUpTarget.value = r
  Object.assign(quickUpForm, { name: '', algorithm: 'least_conn', addr: '' })
  quickUpOpen.value = true
}
async function saveQuickUp() {
  quickUpSaving.value = true
  try {
    const spec = { name: quickUpForm.name.trim(), algorithm: quickUpForm.algorithm, servers: [{ addr: quickUpForm.addr.trim(), weight: 0 }] }
    await api.post('/upstreams', { group_id: props.groupId, spec })
    localUpstreams.value = await api.get('/upstreams', { group_id: props.groupId })
    if (quickUpTarget.value?.proxy) quickUpTarget.value.proxy.upstream = spec.name
    quickUpOpen.value = false
    message.success(`已新建上游 ${spec.name}`)
    emit('upstreamCreated', spec.name)
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    quickUpSaving.value = false
  }
}

async function loadBundles() {
  bundles.value = await api.get('/static-bundles', { group_id: props.groupId }).catch(() => [])
}
function confirmNoIndex(): Promise<boolean> {
  return new Promise((resolve) => {
    Modal.confirm({
      title: '资源包根目录缺少 index.html',
      content: '如果这是网站，请检查是否多打包了一层目录；如果这是纯资源包（下载文件、图片、脚本等），点击“继续上传”，访问该路由根路径将返回 403。',
      okText: '继续上传',
      cancelText: '取消',
      onOk: () => resolve(true),
      onCancel: () => resolve(false),
    })
  })
}
function uploadBundle(r: Route) {
  const el = document.createElement('input')
  el.type = 'file'
  el.accept = '.zip,.gz,.tgz'
  el.onchange = async () => {
    const file = el.files?.[0]
    if (!file) return
    const send = (allowNoIndex: boolean) => {
      const fd = new FormData()
      fd.append('group_id', String(props.groupId))
      fd.append('name', file.name)
      if (allowNoIndex) fd.append('allow_no_index', '1')
      fd.append('file', file)
      return http.post('/static-bundles', fd)
    }
    try {
      let b
      try {
        b = (await send(false)).data
      } catch (e: any) {
        if (e?.response?.data?.code !== 'STATIC_NO_INDEX') throw e
        const ok = await confirmNoIndex()
        if (!ok) return
        b = (await send(true)).data
      }
      await loadBundles()
      r.static!.bundle_id = b.id
      message.success(`已上传 ${b.files} 个文件，保存站点并发布后生效`)
    } catch (e) {
      message.error(errMsg(e))
    }
  }
  el.click()
}

// 证书是否覆盖全部域名（只做提示，最终以服务端校验为准）
function covers(pattern: string, host: string) {
  if (pattern === host) return true
  if (pattern.startsWith('*.')) {
    const rest = host.split('.').slice(1).join('.')
    return rest === pattern.slice(2) && host.split('.').length > 2
  }
  return false
}
function warnFor(id: number | undefined) {
  const c = certs.value.find((x) => x.id === id)
  if (!c) return ''
  const miss = f.domains.map((d) => d.trim().toLowerCase()).filter((d) => d && !c.domains.some((p: string) => covers(p, d)))
  if (miss.length) return `该证书不包含域名 ${miss.join('、')}`
  if (c.status === 'expired') return '该证书已过期，请先在证书页续期'
  if (c.status === 'expiring') return `该证书 ${c.days_left} 天后到期`
  return ''
}
const certWarn = computed(() => warnFor(f.cert_id))
const gmWarn = computed(() => warnFor(f.gm_cert_id))
const certOpt = (c: any) => ({ value: c.id, label: `${c.name}（${c.domains.join('、')}）` })
const stdCerts = computed(() => certs.value.filter((c) => c.kind !== 'sm2').map(certOpt))
const gmCerts = computed(() => certs.value.filter((c) => c.kind === 'sm2').map(certOpt))
const allUpstreams = computed(() => (localUpstreams.value.length ? localUpstreams.value : props.upstreams))

// 勾选 HTTPS 时，如果尚未选证书，自动匹配覆盖当前域名的有效证书
watch(
  () => f.https,
  (on) => {
    if (!on || f.cert_id || !f.domains.length) return
    const ds = f.domains.map((d) => d.trim().toLowerCase()).filter(Boolean)
    const hit = certs.value.find((c) => c.kind !== 'sm2' && c.status !== 'expired' && ds.every((d) => (c.domains || []).some((p: string) => covers(p, d))))
    if (hit) f.cert_id = hit.id
  },
)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    error.value = ''
    localUpstreams.value = [...props.upstreams]
    routeSearch.value = props.focusRoute || ''
    editorTab.value = props.focusRoute ? 'routes' : 'basic'
    api.get('/certificates').then((c) => (certs.value = c)).catch(() => {})
    loadBundles()
    api.get('/ip-sets', { group_id: props.groupId }).then((x) => (ipSets.value = x)).catch(() => {})
    Object.assign(f, blank())
    if (props.site) {
      const s = props.site.spec
      Object.assign(f, {
        name: props.site.name,
        domains: [...s.domains],
        https: !!s.https,
        cert_id: s.https?.cert_id || undefined,
        gm_cert_id: s.https?.gm_cert_id || undefined,
        force_https: s.force_https,
        http2: s.http2,
        client_max_body_size: s.client_max_body_size || '',
        gzip: !!s.gzip,
        enabled: props.site.enabled,
        version: props.site.version,
        routes: JSON.parse(JSON.stringify(Array.isArray(s.routes) ? s.routes : [])).map((r: Route) => {
          if (r.mode === 'proxy' && r.proxy && r.proxy.buffering === undefined) r.proxy.buffering = true
          return r
        }),
        rules: JSON.parse(JSON.stringify(s.rules || [])).map((r: AccessRule) => ({ ...r, conditions: (r.conditions || []).map((c) => ({ ...c, negate: !!c.negate })) })),
      })
    }
  },
)

function addRoute() {
  routeSearch.value = ''
  f.routes.push({ path: '/new/', match: 'prefix', mode: 'deny' })
}
function setMode(r: Route, m: Route['mode']) {
  r.mode = m
  if (m === 'static' && !r.static) r.static = { index_html: '', spa: false }
  if (m === 'proxy' && !r.proxy) r.proxy = { upstream: allUpstreams.value[0]?.name || '', buffering: true }
  if (m === 'redirect' && !r.redirect) r.redirect = { to: 'https://', code: 301 }
}

async function save() {
  error.value = ''
  saving.value = true
  const spec: any = {
    name: f.name,
    domains: f.domains.map((d) => d.trim().toLowerCase()).filter(Boolean),
    force_https: f.https && f.force_https,
    http2: f.https && f.http2,
    ...(f.client_max_body_size.trim() ? { client_max_body_size: f.client_max_body_size.trim() } : {}),
    ...(f.gzip ? { gzip: true } : {}),
    routes: f.routes.map((r) => {
      const o: any = { path: r.path, match: r.match, mode: r.mode }
      if (r.mode === 'static') {
        o.static = {
          index_html: r.static?.index_html || '',
          ...(r.match === 'prefix' && r.static?.bundle_id ? { bundle_id: r.static.bundle_id } : {}),
          ...(r.match === 'prefix' && r.static?.spa ? { spa: true } : {}),
        }
      }
      if (r.mode === 'proxy') {
        const p: any = { upstream: r.proxy!.upstream }
        if (r.proxy?.websocket) p.websocket = true
        if (r.proxy?.https) p.https = true
        if (r.proxy?.strip_prefix && r.match === 'prefix' && r.path !== '/' && r.path.endsWith('/')) p.strip_prefix = true
        if (r.proxy?.connect_timeout) p.connect_timeout = Number(r.proxy.connect_timeout)
        if (r.proxy?.read_timeout) p.read_timeout = Number(r.proxy.read_timeout)
        if (r.proxy?.send_timeout) p.send_timeout = Number(r.proxy.send_timeout)
        if (r.proxy?.buffering === false) p.buffering = false
        o.proxy = p
      }
      if (r.mode === 'redirect') o.redirect = { to: r.redirect!.to, code: Number(r.redirect!.code) }
      return o
    }),
  }
  if (f.https) spec.https = { cert_id: f.cert_id, ...(f.gm_cert_id ? { gm_cert_id: f.gm_cert_id } : {}) }
  if (f.rules.length) {
    spec.rules = f.rules.map((r) => {
      const a = JSON.parse(JSON.stringify(r.action))
      if (a.jwt) {
        delete a.jwt.has_secret
        if (!a.jwt.secret) delete a.jwt.secret
      }
      return { name: r.name, conditions: r.conditions, action: a }
    })
  }
  try {
    if (isNew.value) await api.post('/sites', { group_id: props.groupId, enabled: f.enabled, spec })
    else await api.put(`/sites/${props.site.id}`, { version: f.version, enabled: f.enabled, spec })
    message.success('已保存草稿，可在顶部草稿提醒条点击“预览差异并发布”。')
    emit('saved')
  } catch (e) {
    error.value = errMsg(e)
  } finally {
    saving.value = false
  }
}

const modes = [
  { label: '静态页面', value: 'static' },
  { label: '反向代理', value: 'proxy' },
  { label: '跳转', value: 'redirect' },
  { label: '拒绝访问', value: 'deny' },
]
</script>

<template>
  <a-drawer :open="open" :title="isNew ? '新建站点' : `编辑站点 ${site?.name}`" width="780" @close="emit('close')">
    <a-tabs
      v-model:active-key="editorTab"
      style="margin-top: -8px"
      :items="[
        { key: 'basic', label: '基础与 HTTPS' },
        { key: 'routes', label: `路由转发（${f.routes.length}）` },
        { key: 'rules', label: `访问规则（${f.rules.length}）` },
      ]"
    />
    <a-alert v-if="error" type="error" :message="error" show-icon style="margin-bottom: 14px" />
    <a-form layout="vertical">
      <div v-show="editorTab === 'basic'">
        <div class="row2">
          <a-form-item label="站点标识" extra="小写字母、数字、连字符，用于配置文件名，创建后不可修改">
            <a-input v-model:value="f.name" :disabled="!isNew" placeholder="shop" />
          </a-form-item>
          <a-form-item label="状态">
            <a-switch v-model:checked="f.enabled" checked-children="启用" un-checked-children="停用" />
          </a-form-item>
        </div>
        <a-form-item label="域名" extra="回车添加；支持 *.example.com，不允许单独的 *">
          <a-select v-model:value="f.domains" mode="tags" :open="false" placeholder="shop.example.com" />
        </a-form-item>

        <div class="row2">
          <a-form-item label="请求体大小上限（可选）" extra="留空沿用全局 1m；如 10m、100m、0（不限）">
            <a-input v-model:value="f.client_max_body_size" class="code" placeholder="1m" />
          </a-form-item>
          <a-form-item label="传输优化">
            <a-checkbox v-model:checked="f.gzip">启用站点级 Gzip 压缩</a-checkbox>
          </a-form-item>
        </div>

        <a-form-item>
          <a-checkbox v-model:checked="f.https">启用 HTTPS</a-checkbox>
        </a-form-item>
        <template v-if="f.https">
          <a-form-item label="证书" :extra="certWarn ? '' : '证书与私钥在“证书”页统一上传和续期；勾选 HTTPS 时会自动匹配覆盖域名的有效证书'">
            <a-select v-model:value="f.cert_id" placeholder="从证书库选择" :options="stdCerts">
              <template #notFoundContent>
                <span>证书库为空，<router-link to="/certificates">先去上传证书</router-link></span>
              </template>
            </a-select>
            <div v-if="certWarn" class="cert-warn" :class="{ soft: certWarn.includes('天后到期') }">{{ certWarn }}</div>
          </a-form-item>
          <a-form-item v-if="gmCerts.length || f.gm_cert_id" label="国密证书（可选）" :extra="gmWarn ? '' : '国密 TLCP 双证书：国密浏览器用 SM2 证书，其他客户端仍用上面的标准证书。只能发布到国密版 ' + brand.id + ' 节点。'">
            <a-select v-model:value="f.gm_cert_id" allow-clear placeholder="不启用国密" :options="gmCerts" />
            <div v-if="gmWarn" class="cert-warn" :class="{ soft: gmWarn.includes('天后到期') }">{{ gmWarn }}</div>
          </a-form-item>
          <a-space :size="24" style="margin-bottom: 16px">
            <a-checkbox v-model:checked="f.force_https">HTTP 自动跳转 HTTPS</a-checkbox>
            <a-checkbox v-model:checked="f.http2">启用 HTTP/2</a-checkbox>
          </a-space>
        </template>
      </div>

      <div v-show="editorTab === 'routes'">
        <div class="routes-head">
          <h3>路由转发</h3>
          <a-button size="small" @click="addRoute">添加路由</a-button>
        </div>
        <a-input v-if="f.routes.length > 5 || routeSearch" v-model:value="routeSearch" allow-clear placeholder="按路径查找路由" style="margin-bottom: 10px" />
        <a-alert v-if="migration && routeSearch && !f.routes.some((r) => r.path.toLowerCase().includes(routeSearch.toLowerCase()))" type="info" show-icon message="当前草稿没有匹配的路由，请核对原配置后添加；正则与特殊优先级不能直接转换为普通前缀路由。" style="margin-bottom: 10px" />
        <template v-for="(r, i) in f.routes" :key="i">
          <div v-if="!routeSearch || r.path.toLowerCase().includes(routeSearch.toLowerCase())" class="route">
            <div class="rline">
              <a-select v-model:value="r.match" style="width: 104px" :options="[{ label: '前缀匹配', value: 'prefix' }, { label: '精确匹配', value: 'exact' }]" />
              <a-input v-model:value="r.path" class="code" style="flex: 1" />
              <a-select :value="r.mode" style="width: 120px" :options="modes" @change="(v: any) => setMode(r, v)" />
              <a-button type="text" danger :disabled="f.routes.length === 1" @click="f.routes.splice(i, 1)">删除</a-button>
            </div>
            <template v-if="r.mode === 'static'">
              <div v-if="migration" class="sub">请上传实际静态资源包，并核对原配置的 root、alias 和 try_files 行为；不会自动读取节点上的目录。</div>
              <div class="rline" v-if="r.match === 'prefix'">
                <a-select v-model:value="r.static!.bundle_id" allow-clear style="flex: 1" placeholder="静态资源包（不选则使用下方内联页面）"
                  :options="bundles.map((b) => ({ value: b.id, label: `${b.name} · ${b.files} 个文件 · ${(b.size / 1048576).toFixed(1)} MiB · ${b.sha256.slice(0, 8)}` }))" />
                <a-button @click="uploadBundle(r)">上传 zip / tar.gz</a-button>
              </div>
              <a-checkbox v-if="r.match === 'prefix'" v-model:checked="r.static!.spa">单页应用模式（SPA：未命中文件时回退到 index.html）</a-checkbox>
              <a-textarea v-if="!r.static!.bundle_id" v-model:value="r.static!.index_html" :rows="3" class="code" placeholder="页面 HTML，随配置版本一起发布" />
            </template>
            <template v-else-if="r.mode === 'proxy'">
              <div class="rline">
                <a-select v-model:value="r.proxy!.upstream" style="flex: 1" placeholder="选择上游服务" :options="allUpstreams.map((u) => ({ label: `${u.name}（${u.spec.servers.map((s: any) => s.addr).join('，')}）`, value: u.name }))" />
                <a-button size="small" @click="openQuickUp(r)">+ 新建上游</a-button>
              </div>
              <a-space wrap :size="14">
                <a-checkbox v-model:checked="r.proxy!.websocket">WebSocket 升级</a-checkbox>
                <a-checkbox v-model:checked="r.proxy!.https">HTTPS 回源</a-checkbox>
                <a-checkbox v-if="r.match === 'prefix' && r.path !== '/' && r.path.endsWith('/')" v-model:checked="r.proxy!.strip_prefix">剥离路径前缀</a-checkbox>
                <a-checkbox :checked="r.proxy!.buffering === false" @change="(e: any) => (r.proxy!.buffering = !e.target.checked)">关闭响应缓冲（SSE / 流式）</a-checkbox>
              </a-space>
              <details class="adv">
                <summary>代理超时设置（可选，留空沿用全局 3s / 30s / 30s）</summary>
                <a-space wrap style="margin-top: 6px">
                  <a-input-number v-model:value="r.proxy!.connect_timeout" :min="0" :max="60" placeholder="连接(s)" style="width: 120px" />
                  <a-input-number v-model:value="r.proxy!.read_timeout" :min="0" :max="3600" placeholder="读取(s)" style="width: 120px" />
                  <a-input-number v-model:value="r.proxy!.send_timeout" :min="0" :max="3600" placeholder="发送(s)" style="width: 120px" />
                </a-space>
              </details>
              <div v-if="migration" class="sub">请核对原配置的 Host、升级头和超时；勾选只启用标准连接升级，不保证与自定义头或 Lua 逻辑一致。</div>
            </template>
            <div v-else-if="r.mode === 'redirect'" class="rline">
              <a-input v-model:value="r.redirect!.to" class="code" style="flex: 1" placeholder="https://new.example.com/" />
              <a-select v-model:value="r.redirect!.code" style="width: 100px" :options="[301, 302, 307, 308].map((c) => ({ label: String(c), value: c }))" />
            </div>
            <div v-else class="deny">返回 403</div>
          </div>
        </template>
      </div>

      <div v-show="editorTab === 'rules'">
        <RulesEditor v-model="f.rules" :upstreams="allUpstreams" :ip-sets="ipSets" />
      </div>
    </a-form>
    <template #footer>
      <a-space style="float: right">
        <a-button @click="emit('close')">取消</a-button>
        <a-button type="primary" :loading="saving" @click="save">保存</a-button>
      </a-space>
    </template>

    <a-modal v-model:open="quickUpOpen" title="快速新建上游" ok-text="创建并选用" :confirm-loading="quickUpSaving" @ok="saveQuickUp">
      <a-form layout="vertical">
        <a-form-item label="名称"><a-input v-model:value="quickUpForm.name" placeholder="order-api" /></a-form-item>
        <a-form-item label="算法">
          <a-radio-group v-model:value="quickUpForm.algorithm" :options="[{ label: '最少连接', value: 'least_conn' }, { label: '轮询', value: 'round_robin' }, { label: '源 IP 哈希', value: 'ip_hash' }]" />
        </a-form-item>
        <a-form-item label="后端地址（IP:端口）" extra="必须在节点组允许回源网段内；创建后可在“上游服务”页继续添加更多后端">
          <a-input v-model:value="quickUpForm.addr" class="code" placeholder="10.0.1.11:8080" />
        </a-form-item>
      </a-form>
    </a-modal>
  </a-drawer>
</template>

<style scoped>
.row2 {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}
.routes-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 8px 0 10px;
}
.routes-head h3 {
  margin: 0;
  font-size: 14px;
  color: #16323a;
}
.route {
  border: 1px solid #e1e7e6;
  border-left: 3px solid #0e7c86;
  border-radius: 6px;
  padding: 10px 12px;
  margin-bottom: 10px;
  display: grid;
  gap: 8px;
}
.rline {
  display: flex;
  gap: 8px;
  align-items: center;
}
.adv {
  font-size: 12px;
  color: #6b7c80;
}
.adv summary {
  cursor: pointer;
}
.deny {
  font-size: 12px;
  color: #8a9a9d;
}
.cert-warn {
  margin-top: 6px;
  font-size: 12.5px;
  color: #a3332d;
}
.cert-warn.soft {
  color: #a86a12;
}
</style>
