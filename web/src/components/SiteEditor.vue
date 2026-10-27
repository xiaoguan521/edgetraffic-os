<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'

const props = defineProps<{ open: boolean; site: any | null; groupId: number; upstreams: any[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()

interface Route {
  path: string
  match: 'prefix' | 'exact'
  mode: 'static' | 'proxy' | 'redirect' | 'deny'
  static?: { index_html: string }
  proxy?: { upstream: string }
  redirect?: { to: string; code: number }
}

const blank = () => ({
  name: '',
  domains: [] as string[],
  https: false,
  cert_pem: '',
  key_pem: '',
  force_https: false,
  http2: false,
  enabled: true,
  version: 0,
  routes: [{ path: '/', match: 'prefix', mode: 'static', static: { index_html: '<h1>Hello from EdgeTraffic OS</h1>\n' } }] as Route[],
})
const f = reactive(blank())
const saving = ref(false)
const error = ref('')
const isNew = computed(() => !props.site)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    error.value = ''
    Object.assign(f, blank())
    if (props.site) {
      const s = props.site.spec
      Object.assign(f, {
        name: props.site.name,
        domains: [...s.domains],
        https: !!s.https,
        cert_pem: s.https?.cert_pem || '',
        key_pem: '',
        force_https: s.force_https,
        http2: s.http2,
        enabled: props.site.enabled,
        version: props.site.version,
        routes: JSON.parse(JSON.stringify(s.routes)),
      })
    }
  },
)

function addRoute() {
  f.routes.push({ path: '/new/', match: 'prefix', mode: 'deny' })
}
function setMode(r: Route, m: Route['mode']) {
  r.mode = m
  if (m === 'static' && !r.static) r.static = { index_html: '' }
  if (m === 'proxy' && !r.proxy) r.proxy = { upstream: props.upstreams[0]?.name || '' }
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
    routes: f.routes.map((r) => {
      const o: any = { path: r.path, match: r.match, mode: r.mode }
      if (r.mode === 'static') o.static = r.static
      if (r.mode === 'proxy') o.proxy = r.proxy
      if (r.mode === 'redirect') o.redirect = { to: r.redirect!.to, code: Number(r.redirect!.code) }
      return o
    }),
  }
  if (f.https) spec.https = { cert_pem: f.cert_pem, key_pem: f.key_pem }
  try {
    if (isNew.value) await api.post('/sites', { group_id: props.groupId, enabled: f.enabled, spec })
    else await api.put(`/sites/${props.site.id}`, { version: f.version, enabled: f.enabled, spec })
    message.success('已保存。到“发布”页查看变更并发布到节点。')
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
  <a-drawer :open="open" :title="isNew ? '新建站点' : `编辑站点 ${site?.name}`" width="760" @close="emit('close')">
    <a-form layout="vertical">
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

      <a-form-item>
        <a-checkbox v-model:checked="f.https">启用 HTTPS</a-checkbox>
      </a-form-item>
      <template v-if="f.https">
        <div class="row2">
          <a-form-item label="证书（PEM）">
            <a-textarea v-model:value="f.cert_pem" :rows="4" class="code" placeholder="-----BEGIN CERTIFICATE-----" />
          </a-form-item>
          <a-form-item label="私钥（PEM）" :extra="isNew ? '' : '留空表示沿用已保存的私钥；私钥不会回显'">
            <a-textarea v-model:value="f.key_pem" :rows="4" class="code" placeholder="-----BEGIN PRIVATE KEY-----" />
          </a-form-item>
        </div>
        <a-space :size="24" style="margin-bottom: 16px">
          <a-checkbox v-model:checked="f.force_https">HTTP 自动跳转 HTTPS</a-checkbox>
          <a-checkbox v-model:checked="f.http2">启用 HTTP/2</a-checkbox>
        </a-space>
      </template>

      <div class="routes-head">
        <h3>路由</h3>
        <a-button size="small" @click="addRoute">添加路由</a-button>
      </div>
      <div v-for="(r, i) in f.routes" :key="i" class="route">
        <div class="rline">
          <a-select v-model:value="r.match" style="width: 104px" :options="[{ label: '前缀匹配', value: 'prefix' }, { label: '精确匹配', value: 'exact' }]" />
          <a-input v-model:value="r.path" class="code" style="flex: 1" />
          <a-select :value="r.mode" style="width: 120px" :options="modes" @change="(v: any) => setMode(r, v)" />
          <a-button type="text" danger :disabled="f.routes.length === 1" @click="f.routes.splice(i, 1)">删除</a-button>
        </div>
        <a-textarea v-if="r.mode === 'static'" v-model:value="r.static!.index_html" :rows="3" class="code" placeholder="页面 HTML，随配置版本一起发布" />
        <a-select v-else-if="r.mode === 'proxy'" v-model:value="r.proxy!.upstream" style="width: 100%" placeholder="选择上游服务" :options="upstreams.map((u) => ({ label: `${u.name}（${u.spec.servers.map((s: any) => s.addr).join('，')}）`, value: u.name }))" />
        <div v-else-if="r.mode === 'redirect'" class="rline">
          <a-input v-model:value="r.redirect!.to" class="code" style="flex: 1" placeholder="https://new.example.com/" />
          <a-select v-model:value="r.redirect!.code" style="width: 100px" :options="[301, 302, 307, 308].map((c) => ({ label: String(c), value: c }))" />
        </div>
        <div v-else class="deny">返回 403</div>
      </div>

      <a-alert v-if="error" type="error" :message="error" show-icon style="margin-top: 16px" />
    </a-form>
    <template #footer>
      <a-space style="float: right">
        <a-button @click="emit('close')">取消</a-button>
        <a-button type="primary" :loading="saving" @click="save">保存</a-button>
      </a-space>
    </template>
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
.deny {
  font-size: 12px;
  color: #8a9a9d;
}
</style>
