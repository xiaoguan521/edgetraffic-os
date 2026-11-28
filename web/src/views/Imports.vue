<script setup lang="ts">
import { brand } from '../brand'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import dayjs from 'dayjs'
import { Modal, message } from 'antdv-next'
import { api, errMsg, http } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import SiteEditor from '../components/SiteEditor.vue'

const session = useSession()
const ws = useWorkspace()
const route = useRoute()
const router = useRouter()
const groups = computed(() => ws.groups)
const gid = computed(() => ws.groupId)
const managementMode = ref<any>(null)
const history = ref<any[]>([])
const current = ref<any>(null) // { import, report, result, certs_pending }
const uploading = ref(false)
const applying = ref(false)
const blockingOnly = ref(false)
const category = ref('all')
const review = ref<any>(null)
const reviewing = ref(false)
const reviewError = ref('')
const editorOpen = ref(false)
const editing = ref<any>(null)
const editorUpstreams = ref<any[]>([])
const focusRoute = ref('')

async function load() {
  if (!ws.groups.length) {
    await ws.loadGroups(Number(route.query.group) || undefined)
  }
  if (!gid.value) return
  managementMode.value = groups.value.find((g) => g.id === gid.value)?.management_mode === 'legacy'
    ? { mode: 'legacy', label: '兼容配置' }
    : groups.value.find((g) => g.id === gid.value)?.management_mode === 'observe'
      ? { mode: 'observe', label: '观察接入' }
      : { mode: 'managed', label: '控制台托管' }
  history.value = await api.get('/imports', { group_id: gid.value })
}
watch(gid, () => {
  current.value = null
  review.value = null
  load()
})
onMounted(async () => {
  if (route.query.group) ws.setGroup(Number(route.query.group))
  await load()
  const id = Number(route.query.import)
  if (id > 0) await open(id)
})

function pick() {
  const el = document.createElement('input')
  el.type = 'file'
  el.multiple = true
  el.accept = '.txt,.conf,.inc,.types,.gz,.tgz,.crt,.pem,.key'
  el.onchange = async () => {
    const files = Array.from(el.files || [])
    if (!files.length) return
    const fd = new FormData()
    fd.append('group_id', String(gid.value))
    for (const f of files) fd.append('file', f)
    uploading.value = true
    try {
      const r = (await http.post('/imports', fd)).data
      current.value = { ...r, certs_pending: Object.values(r.report.sites || {}).some((s: any) => s.cert_found) }
      review.value = null
      category.value = 'all'
      await load()
    } catch (e) {
      message.error(errMsg(e))
    } finally {
      uploading.value = false
    }
  }
  el.click()
}
async function open(id: number) {
  try {
    const data = await api.get(`/imports/${id}`)
    if (data.import.group_id !== gid.value) {
      message.error('导入记录不属于当前节点组')
      return
    }
    current.value = data
    review.value = null
    category.value = 'all'
    if (data.import.applied_at) await checkReview()
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function checkReview() {
  if (!current.value?.import.applied_at) return
  reviewing.value = true
  reviewError.value = ''
  try {
    review.value = await api.get(`/imports/${current.value.import.id}/review`)
  } catch (e) {
    review.value = null
    reviewError.value = errMsg(e)
  } finally {
    reviewing.value = false
  }
}
async function apply() {
  applying.value = true
  try {
    const r = await api.post(`/imports/${current.value.import.id}/apply`, {})
    message.success(`已导入 ${r.sites.length} 个站点（草稿），${r.snippets.length} 个片段待审批`)
    await open(current.value.import.id)
    await load()
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    applying.value = false
  }
}
async function download() {
  const r = await http.get(`/imports/${current.value.import.id}/report`, { responseType: 'blob' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(r.data)
  a.download = `import-report-${current.value.import.id}.json`
  a.click()
  URL.revokeObjectURL(a.href)
}
const rep = computed(() => current.value?.report)
const returnQuery = computed(() => ({ group: gid.value, import: current.value?.import.id }))
const issueKinds: Record<string, { title: string; help: string }> = {
  certificate: { title: '证书与 HTTPS', help: '先到证书库上传证书及私钥，再在对应站点草稿中选择证书，核对域名和 HTTPS 设置。' },
  static: { title: '静态资源', help: '在站点路由中上传静态资源包；原配置中的 root、alias、index 和 try_files 需要逐项核对，节点本地文件不会自动读取。' },
  upstream: { title: '上游与代理', help: '在上游服务中填写允许回源网段内的地址，再在站点路由选择上游；请核对原请求路径和代理头。' },
  websocket: { title: 'WebSocket 与代理头', help: '在代理路由中可启用标准 WebSocket 升级。自定义 Host、动态 Connection、超时等仍需按原配置核对。' },
  lua: { title: 'Lua 与脚本', help: 'Lua 脚本需相应授权、节点 Lua 扩展包和另一人审批；原有脚本及依赖不保证兼容受限运行环境，需人工迁移测试。' },
  include: { title: '缺失文件', help: '同时选择主配置和被 include 的文件重新分析，或上传包含相对目录结构的 tar.gz；文件不会从节点自动读取。' },
  tuning: { title: '参数与安全头', help: '部分参数由平台统一管理；可先查看自定义片段允许的指令，无法表达的设置需评估业务影响。' },
  other: { title: '其他待核对', help: '根据文件、行号和所属 location 对照原配置，评估是否需要在站点、上游或自定义片段中手动重建。' },
}
function issueKind(i: any) {
  const d = String(i.directive || '').toLowerCase()
  const reason = String(i.reason || '').toLowerCase()
  if (d.includes('lua') || d.startsWith('content_by_') || d.startsWith('access_by_')) return 'lua'
  if (d.startsWith('ssl_') || reason.includes('证书') || reason.includes('https 站点缺少')) return 'certificate'
  if (d === 'include') return 'include'
  if (['root', 'alias', 'index', 'try_files', 'autoindex'].includes(d)) return 'static'
  if (d === 'proxy_set_header' || d === 'proxy_http_version') return 'websocket'
  if (d === 'proxy_pass' || d === 'upstream' || reason.includes('上游') || reason.includes('回源')) return 'upstream'
  if (/^(add_header|server_tokens|sendfile|client_|proxy_|gzip|access_log|error_log|ssl_)/.test(d)) return 'tuning'
  return 'other'
}
const issueCounts = computed(() => {
  const count: Record<string, number> = {}
  const issues = [...(rep.value?.global || []), ...(rep.value?.sites || []).flatMap((s: any) => s.issues || [])]
  for (const issue of issues) count[issueKind(issue)] = (count[issueKind(issue)] || 0) + 1
  return count
})
function filtered(issues: any[], siteIssues = true) {
  return (issues || []).filter((i) => (category.value === 'all' || issueKind(i) === category.value) && (!siteIssues || !blockingOnly.value || i.blocking))
}
function reviewedSite(s: any) {
  return review.value?.sites.find((x: any) => x.source_name === s.name)
}
async function editSite(s: any, issue?: any) {
  const item = reviewedSite(s)
  if (!item?.site) return
  try {
    editorUpstreams.value = await api.get('/upstreams', { group_id: gid.value })
    editing.value = item.site
    focusRoute.value = String(issue?.location || '').match(/^location (?:= )?(\/[^\s]+)$/)?.[1] || ''
    editorOpen.value = true
  } catch (e) {
    message.error(errMsg(e))
  }
}
async function savedSite() {
  editorOpen.value = false
  await checkReview()
}
function routeText(r: any) {
  if (!r) return '—'
  if (r.mode === 'proxy') return `代理 → ${r.proxy?.upstream || '未选择'}${r.proxy?.websocket ? ' · WebSocket' : ''}`
  if (r.mode === 'static') return r.static?.bundle_id ? `静态资源包 #${r.static.bundle_id}` : '静态页面'
  if (r.mode === 'redirect') return `跳转 ${r.redirect?.code} ${r.redirect?.to}`
  return r.mode === 'deny' ? '拒绝访问' : String(r.mode || '—')
}
function compareRoutes(s: any) {
  const currentSite = reviewedSite(s)?.site
  if (!currentSite) return []
  const original = s.spec.routes || []
  const draft = currentSite.spec.routes || []
  const key = (r: any) => `${r.match}:${r.path}`
  const old = new Map<string, any>(original.map((r: any) => [key(r), r]))
  const now = new Map<string, any>(draft.map((r: any) => [key(r), r]))
  return [...new Set([...old.keys(), ...now.keys()])].map((k) => {
    const a = old.get(k), b = now.get(k)
    return { key: k, path: `${(a || b).match === 'exact' ? '= ' : ''}${(a || b).path}`, original: routeText(a), draft: routeText(b), status: !a ? '新增' : !b ? '移除' : routeText(a) !== routeText(b) ? '变更' : '相同' }
  })
}
function handoffToManaged() {
  Modal.confirm({
    title: '切回“控制台托管”并前往首次发布？',
    content: '切回后仍不会自动覆盖节点，进入发布页后请再次核对检查单与配置差异，然后显式点击发布。',
    okText: '确认切回并前往发布',
    cancelText: '取消',
    onOk: async () => {
      try {
        await api.put(`/node-groups/${gid.value}/management-mode`, { mode: 'managed' })
        await ws.loadGroups(gid.value)
        message.success('已切回控制台托管，请核对后显式发布')
        router.push({ path: '/releases', query: returnQuery.value })
      } catch (e) {
        message.error(errMsg(e))
      }
    },
  })
}
const statusTag: Record<string, { t: string; c: string }> = {
  full: { t: '完全转换', c: 'green' },
  partial: { t: '部分转换', c: 'gold' },
  failed: { t: '无法转换', c: 'red' },
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>配置导入</h1>
        <p>选择一份 <span class="code">{{ brand.id }} -T</span> 输出、配置目录 tar.gz，或同时选择主配置与它引用的多个文件。支持的内容会转为草稿；请先核对报告中需要人工处理的路由、脚本、静态文件和证书，再预览、审批和发布。</p>
      </div>
      <a-space>
        <a-button v-if="session.can('operator')" type="primary" :loading="uploading" :disabled="!session.writable" @click="pick">上传配置</a-button>
      </a-space>
    </div>
    <a-alert v-if="managementMode?.mode === 'observe'" type="warning" show-icon style="margin-bottom: 16px"
      message="当前节点组是观察接入" description="上传、分析和应用只会生成控制台草稿，不会覆盖节点本地配置。完成核对后，请到“节点”页切回控制台托管，再显式预览、审批和发布。" />
    <a-alert v-else-if="managementMode?.mode === 'legacy'" type="info" show-icon style="margin-bottom: 16px"
      message="当前节点组是兼容配置" description="原生 Lua 配置和第三方依赖继续由节点本地维护。导入器只分析上传的主配置、include、脚本和证书材料，不会从节点读取文件，也不会自动迁移任意 Lua 运行时。" />

    <template v-if="rep">
      <section class="panel">
        <div class="phead">
          <h2>导入报告 #{{ current.import.id }} <span class="sub">{{ current.import.file_name }}</span></h2>
          <a-space>
            <a-button @click="download">下载报告</a-button>
            <a-button v-if="!current.import.applied_at && session.can('operator')" type="primary" :loading="applying" :disabled="!session.writable" @click="apply">应用为草稿</a-button>
            <a-tag v-else-if="current.import.applied_at" color="green">{{ current.import.applied_by }} 已于 {{ dayjs(current.import.applied_at).format('MM-DD HH:mm') }} 应用</a-tag>
          </a-space>
        </div>
        <div class="stats">
          <div><b class="num">{{ rep.summary.servers }}</b><span>server 块</span></div>
          <div><b class="num">{{ rep.summary.full }}</b><span>完全转换</span></div>
          <div><b class="num">{{ rep.summary.partial }}</b><span>部分转换</span></div>
          <div><b class="num">{{ rep.summary.failed }}</b><span>无法转换</span></div>
          <div><b class="num">{{ rep.upstreams.length }}</b><span>上游组</span></div>
          <div><b class="num">{{ rep.summary.issues }}</b><span>未转换条目</span></div>
        </div>
        <a-alert v-if="rep.summary.issues" type="warning" show-icon style="margin-top: 12px"
          message="请逐项核对未转换内容；包含问题的站点可能处于停用状态，应用为草稿并不代表迁移完成。" />
        <a-alert v-if="rep.lua?.requires_legacy" type="warning" show-icon style="margin-top: 12px"
          :message="`检测到 ${rep.lua.references?.length || 0} 项原生 Lua 配置：不能直接转为受限托管脚本`"
          :description="`报告已记录 ${rep.lua.assets?.length || 0} 个文件。未精确匹配的引用和 ${rep.lua.dependencies?.length || 0} 项脚本依赖线索需要人工确认节点路径及运行环境。${rep.lua.missing?.length ? `上传内容中未找到：${rep.lua.missing.join('、')}。` : ''}`" />
        <div v-if="rep.lua?.references?.some((r: any) => r.resolution === 'unverified')" class="sub" style="margin-top: 8px">
          待确认引用：{{ rep.lua.references.filter((r: any) => r.resolution === 'unverified').map((r: any) => r.target + (r.candidate ? `（可能是 ${r.candidate}）` : '')).filter(Boolean).join('、') }}
        </div>
        <div v-if="rep.summary.issues" class="issue-groups">
          <div class="sub">按问题类型筛选（数字为导入时的报告条目，不会因编辑草稿自动消失）：</div>
          <a-space wrap>
            <a-button size="small" :type="category === 'all' ? 'primary' : 'default'" @click="category = 'all'">全部</a-button>
            <a-button v-for="(info, key) in issueKinds" v-show="issueCounts[key]" :key="key" size="small" :type="category === key ? 'primary' : 'default'" @click="category = String(key)">{{ info.title }} {{ issueCounts[key] || 0 }}</a-button>
          </a-space>
          <a-alert v-if="category !== 'all'" type="info" show-icon :message="issueKinds[category]?.help" style="margin-top: 8px" />
        </div>
        <a-checkbox v-if="rep.sites.some((s: any) => s.issues.length)" v-model:checked="blockingOnly" style="margin-top: 12px">只看需人工处理的站点问题</a-checkbox>
        <a-alert v-if="!current.import.applied_at && rep.sites.some((s: any) => s.cert_found)" type="info" show-icon style="margin-top: 12px"
          message="上传内容中的证书与私钥只在内存中保留 30 分钟等待确认；应用时加密存入证书库，不会出现在报告里。" />
      </section>

      <section v-if="current.import.applied_at" class="panel">
        <div class="phead">
          <h2>处理进度与重新校验</h2>
          <a-button :loading="reviewing" @click="checkReview">重新校验草稿</a-button>
        </div>
        <a-alert v-if="reviewError" type="error" :message="reviewError" show-icon />
        <a-alert v-if="review?.state === 'applying'" type="warning" show-icon message="导入尚未完整记录" description="应用过程中出错或被中断，下面是最后记录的进度。请到站点页检查已创建的草稿；该导入不能再次应用，需要时删除草稿后重新上传。" />
        <template v-if="review">
          <p>草稿模型通过 {{ review.sites.filter((s: any) => s.valid).length }} / {{ review.sites.length }} 个；片段待审批 {{ review.snippets.filter((s: any) => s.status === 'pending').length }} 个，已审批 {{ review.snippets.filter((s: any) => s.status === 'approved').length }} 个，已删除或不可用 {{ review.snippets.filter((s: any) => s.status === 'missing' || s.status === 'unavailable').length }} 个。检查时间：{{ dayjs(review.checked_at).format('MM-DD HH:mm:ss') }}</p>
          <a-alert type="info" show-icon message="这里检查的是当前草稿模型和片段状态，原报告中的未转换内容仍需人工核对；节点发布时还会执行实际配置校验。停用的站点不会出现在发布预览中。" />
          <ul>
            <li v-for="s in review.sites" :key="s.id || s.name">
              {{ s.name }}：
              <a-tag v-if="s.identity_unverified" color="orange">旧记录未保存站点标识，请到站点页核对</a-tag>
              <a-tag v-else-if="s.missing" color="red">站点草稿已删除</a-tag>
              <template v-else><a-tag :color="s.valid ? 'green' : 'red'">{{ s.valid ? '模型通过' : '模型未通过' }}</a-tag><a-tag :color="s.site?.enabled ? 'green' : 'default'">{{ s.site?.enabled ? '草稿已启用' : '草稿停用' }}</a-tag>{{ s.validation_error }}</template>
            </li>
          </ul>
        </template>
        <a-space wrap>
          <router-link :to="{ path: '/certificates', query: returnQuery }">上传或查看证书</router-link>
          <router-link :to="{ path: '/sites', query: { ...returnQuery, tab: 'ups' } }">处理上游服务</router-link>
          <router-link :to="{ path: '/sites', query: returnQuery }">查看站点</router-link>
          <router-link :to="{ path: '/snippets', query: returnQuery }">处理片段和 Lua</router-link>
          <router-link :to="{ path: '/releases', query: returnQuery }">预览草稿与线上版本</router-link>
          <a-button v-if="managementMode?.mode === 'observe' && session.can('operator')" type="primary" size="small" :disabled="!session.writable" @click="handoffToManaged">已完成核对：切回托管并前往发布</a-button>
        </a-space>
      </section>

      <section class="panel" v-for="s in rep.sites" :key="s.name">
        <div class="phead">
          <h2>{{ s.name }} <a-tag :color="statusTag[s.status].c">{{ statusTag[s.status].t }}</a-tag></h2>
          <a-space><span class="sub code">{{ s.file }}:{{ s.line }}</span><a-button v-if="reviewedSite(s)?.site && session.can('operator')" size="small" :disabled="!session.writable" @click="editSite(s)">编辑站点草稿</a-button></a-space>
        </div>
        <a-alert v-if="current.import.applied_at && s.status === 'failed'" type="warning" show-icon message="该站点无法自动创建草稿，请参考原配置在站点页手工创建并核对。" style="margin-bottom: 10px" />
        <div class="sub">{{ (s.spec.domains || []).join('、') }}<template v-if="s.spec.https"> · HTTPS{{ s.spec.force_https ? '（强制跳转）' : '' }}{{ s.cert_found ? ' · 证书已找到' : '' }}</template></div>
        <details>
          <summary>导入时识别的路由（{{ (s.spec.routes || []).length }}）</summary>
          <ul class="routes">
            <li v-for="r in s.spec.routes" :key="r.match + r.path"><span class="code">{{ r.match === 'exact' ? '= ' : '' }}{{ r.path }}</span> → {{ routeText(r) }}<a-tag v-if="s.location_snippets?.[r.path]" style="margin-left: 6px">片段</a-tag></li>
          </ul>
        </details>
        <details v-if="reviewedSite(s)?.site" class="comparison">
          <summary>已识别路由与当前草稿对照（{{ compareRoutes(s).filter((r: any) => r.status !== '相同').length }} 项摘要差异）</summary>
          <p class="sub">只对比路径、匹配类型、路由模式、上游及 WebSocket 开关等摘要，不包含静态文件内容和配置全文；未转换的指令及实际业务行为仍需对照原配置。发布页可对比当前启用草稿与线上版本。</p>
          <a-table :data-source="compareRoutes(s)" :pagination="{ pageSize: 10 }" row-key="key" size="small" :columns="[{ title: '路由', dataIndex: 'path' }, { title: '导入时识别', dataIndex: 'original' }, { title: '当前草稿', dataIndex: 'draft' }, { title: '状态', dataIndex: 'status', width: 80 }]" />
        </details>
        <template v-if="s.server_snippet">
          <div class="sub">转为 server 片段（应用后需另一人审批）：</div>
          <pre class="snip">{{ s.server_snippet }}</pre>
        </template>
        <div v-if="s.issues.length" class="sub">{{ s.issues.length }} 项说明，其中 {{ s.issues.filter((i: any) => i.blocking).length }} 项需人工处理</div>
        <a-table v-if="s.issues.length" :data-source="filtered(s.issues)" :pagination="{ pageSize: 20 }" size="small" :row-key="(r: any) => r.file + r.line + r.directive + r.reason"
          :columns="[{ title: '位置', key: 'loc', width: 260 }, { title: '指令', dataIndex: 'directive', width: 160 }, { title: '原因', key: 'reason' }, { title: '处理', key: 'action', width: 140 }]">
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'loc'"><span class="code">{{ record.file }}:{{ record.line }}</span><div v-if="record.location" class="sub code">{{ record.location }}</div></template>
            <template v-else-if="column.key === 'reason'"><a-tag v-if="record.blocking" color="red">需人工处理</a-tag>{{ record.reason }}</template>
            <template v-else-if="column.key === 'action'"><a v-if="reviewedSite(s)?.site" @click="editSite(s, record)">编辑站点草稿</a><span v-else class="sub">{{ current.import.applied_at ? '到站点页处理' : '应用后处理' }}</span></template>
          </template>
        </a-table>
      </section>

      <section class="panel" v-if="rep.global.length || rep.http_snippet">
        <h2>全局与其他内容</h2>
        <template v-if="rep.http_snippet">
          <div class="sub">http 层白名单内的指令转为片段（应用后需审批）：</div>
          <pre class="snip">{{ rep.http_snippet }}</pre>
        </template>
        <a-table :data-source="filtered(rep.global, false)" :pagination="{ pageSize: 20 }" size="small" :row-key="(r: any) => r.file + r.line + r.directive + r.reason"
          :columns="[{ title: '位置', key: 'loc', width: 260 }, { title: '指令', dataIndex: 'directive', width: 160 }, { title: '说明', dataIndex: 'reason' }]">
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'loc'"><span class="code">{{ record.file }}:{{ record.line }}</span></template>
          </template>
        </a-table>
      </section>

      <section class="panel" v-if="current.result">
        <h2>应用时的结果</h2>
        <p>新建上游：{{ current.result.upstreams.join('、') || '无' }}；导入证书：{{ current.result.certificates.join('、') || '无' }}；提交片段 {{ current.result.snippets.length }} 个（到“自定义片段”审批）。</p>
        <ul>
          <li v-for="s in current.result.sites" :key="s.name">{{ s.name }}：<a-tag :color="s.enabled ? 'green' : 'default'">{{ s.enabled ? '已启用（草稿）' : '已停用' }}</a-tag>{{ s.note }}</li>
        </ul>
        <a-alert v-for="(e, i) in current.result.errors" :key="i" type="warning" :message="e" style="margin-top: 6px" />
      </section>
    </template>

    <section class="panel">
      <h2>导入记录</h2>
      <a-table :data-source="history" row-key="id" :pagination="false" size="middle"
        :columns="[{ title: '#', dataIndex: 'id', width: 60 }, { title: '文件', dataIndex: 'file_name' }, { title: '结果', key: 'sum' }, { title: '上传', key: 'by', width: 170 }, { title: '状态', key: 'st', width: 110 }, { title: '', key: 'op', width: 80 }]">
        <template #emptyText><a-empty description="还没有导入过配置" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'sum'">{{ record.sites }} 个站点：完全 {{ record.full }} / 部分 {{ record.partial }} / 无法 {{ record.failed }}</template>
          <template v-else-if="column.key === 'by'">{{ record.created_by }} · {{ dayjs(record.created_at).format('MM-DD HH:mm') }}</template>
          <template v-else-if="column.key === 'st'"><a-tag :color="record.applied_at ? 'green' : 'default'">{{ record.applied_at ? '已应用' : '未应用' }}</a-tag></template>
          <template v-else-if="column.key === 'op'"><a @click="open(record.id)">查看</a></template>
        </template>
      </a-table>
    </section>
    <SiteEditor :open="editorOpen" :site="editing" :group-id="gid" :upstreams="editorUpstreams" :focus-route="focusRoute" migration @close="editorOpen = false" @saved="savedSite" />
  </div>
</template>

<style scoped>
.issue-groups { margin-top: 12px; display: grid; gap: 8px; }
.comparison { margin: 10px 0; }
summary { cursor: pointer; color: #0e6971; margin: 8px 0; }
.phead {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}
.phead h2 {
  margin: 0;
}
.stats {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 10px;
}
.stats div {
  display: flex;
  flex-direction: column;
}
.stats b {
  font-size: 20px;
  color: #16323a;
}
.stats span,
.sub {
  font-size: 12px;
  color: #6b7c80;
}
.routes {
  margin: 8px 0;
  padding-left: 18px;
}
.snip {
  margin: 4px 0 8px;
  padding: 8px 10px;
  background: #f6f8f8;
  border: 1px solid #e1e7e6;
  border-radius: 6px;
  font-family: 'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, monospace;
  font-size: 12px;
  white-space: pre-wrap;
}
</style>
