<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { VAceEditor } from 'vue3-ace-editor'
import 'ace-builds/src-noconflict/mode-lua'
import 'ace-builds/src-noconflict/mode-sh'
import 'ace-builds/src-noconflict/theme-chrome'
import { api, errMsg, http } from '../api/client'
import { useSession } from '../stores/session'
import { useWorkspace } from '../stores/workspace'
import DiffView from '../components/DiffView.vue'

const session = useSession()
const ws = useWorkspace()
const route = useRoute()
const gid = computed(() => ws.groupId)
const sites = ref<any[]>([])
const rows = ref<any[]>([])
const loading = ref(false)

const scopeName: Record<string, string> = { http: 'http 层（节点组）', server: 'server 层（站点）', location: 'location 层（路由）', lua: 'Lua 脚本（路由）' }
const luaPhases = [
  { value: 'rewrite', label: 'rewrite（改写请求）' },
  { value: 'access', label: 'access（访问控制）' },
  { value: 'content', label: 'content（生成响应）' },
  { value: 'header_filter', label: 'header_filter（改响应头）' },
  { value: 'body_filter', label: 'body_filter（改响应体）' },
]
const luaLicensed = computed(() => !!session.license?.features?.includes('lua'))
const canLua = computed(() => session.can('admin') && luaLicensed.value)

async function load() {
  loading.value = true
  try {
    if (!ws.groups.length) await ws.loadGroups(Number(route.query.group) || undefined)
    if (!gid.value) return
    ;[rows.value, sites.value] = await Promise.all([api.get('/snippets', { group_id: gid.value }), api.get('/sites', { group_id: gid.value })])
  } finally {
    loading.value = false
  }
}
watch(gid, load)
onMounted(async () => {
  if (route.query.group) ws.setGroup(Number(route.query.group))
  await load()
})

function siteName(id: number) {
  return sites.value.find((s) => s.id === id)?.name || `#${id}`
}
function where(r: any) {
  if (r.scope === 'http') return `http · ${r.name}`
  if (r.scope === 'server') return `${siteName(r.site_id)} · server`
  if (r.scope === 'lua') return `${siteName(r.site_id)} · ${r.route_path} · ${r.name}`
  return `${siteName(r.site_id)} · location ${r.route_path}`
}
const mayAct = (r: any) => r.scope !== 'lua' || session.can('admin')

// LCS 行级 Diff，供 DiffView 准确展示插入/删除/修改
function simpleDiff(oldText: string, newText: string) {
  const a = (oldText || '').split('\n')
  const b = (newText || '').split('\n')
  const m = a.length
  const n = b.length
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0))
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }
  const out = ['--- 生效版本', '+++ 待审批版本', `@@ -1,${m} +1,${n} @@`]
  let i = 0
  let j = 0
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      out.push(' ' + a[i++])
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push('-' + a[i++])
    } else {
      out.push('+' + b[j++])
    }
  }
  while (i < m) out.push('-' + a[i++])
  while (j < n) out.push('+' + b[j++])
  return out.join('\n')
}

// ---------- 编辑器 ----------
const open = ref(false)
const form = reactive({ scope: 'server', site_id: undefined as number | undefined, route_path: '', name: '', content: '', note: '' })
const issues = ref<any[]>([])
const checkErr = ref('')
const normalized = ref('')
const saving = ref(false)
const routes = computed(() => sites.value.find((s) => s.id === form.site_id)?.spec?.routes || [])
const routeMode = computed(() => routes.value.find((r: any) => r.path === form.route_path)?.mode || '')
const phaseOptions = computed(() =>
  luaPhases.map((p) => ({ ...p, disabled: (routeMode.value === 'redirect' || routeMode.value === 'deny') && ['rewrite', 'access', 'content'].includes(p.value) })),
)
watch(
  () => form.scope,
  (sc, old) => {
    if (sc === 'lua' && old !== 'lua' && !luaPhases.some((p) => p.value === form.name)) form.name = 'content'
    if (sc !== 'lua' && old === 'lua') form.name = ''
  },
)

function edit(r?: any) {
  Object.assign(form, { scope: 'server', site_id: undefined, route_path: '', name: '', content: '', note: '' })
  if (r) Object.assign(form, { scope: r.scope, site_id: r.site_id || undefined, route_path: r.route_path, name: r.name, content: r.pending || r.content })
  issues.value = []
  checkErr.value = ''
  normalized.value = ''
  open.value = true
}
let timer: number | undefined
watch(
  () => ({ ...form }),
  () => {
    clearTimeout(timer)
    timer = window.setTimeout(check, 400)
  },
)
async function check() {
  if (!open.value || !form.content.trim()) return
  try {
    const r = await api.post('/snippets/validate', { ...form, group_id: gid.value })
    issues.value = r.issues
    normalized.value = r.ok ? r.normalized : ''
    checkErr.value = ''
  } catch (e) {
    checkErr.value = errMsg(e)
    issues.value = []
    normalized.value = ''
  }
}
async function save() {
  saving.value = true
  try {
    await http.post('/snippets', { ...form, group_id: gid.value })
    message.success('已提交，等待另一人审批')
    open.value = false
    await load()
    ws.loadBadges()
  } catch (e: any) {
    if (e.response?.data?.issues) issues.value = e.response.data.issues
    message.error(errMsg(e))
  } finally {
    saving.value = false
  }
}

async function act(r: any, action: 'approve' | 'reject' | 'withdraw' | 'delete', ok: string) {
  try {
    await api.post(`/snippets/${r.id}/${action}`, {})
    message.success(ok)
    await load()
    ws.loadBadges()
  } catch (e) {
    message.error(errMsg(e))
  }
}
const pendingCount = computed(() => rows.value.filter((r) => r.pending_by).length)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>自定义片段</h1>
        <router-link v-if="route.query.import" :to="{ path: '/imports', query: { group: gid, import: route.query.import } }">返回导入报告</router-link>
        <p>可视化表单覆盖不到的需求，用受限的配置片段补充：只允许白名单内的指令，由渲染器规范化输出；每次新增、修改、删除都要另一人批准，批准后随下一次发布生效。</p>
      </div>
      <a-space>
        <a-button v-if="session.can('operator')" type="primary" :disabled="!session.writable" @click="edit()">新建片段</a-button>
      </a-space>
    </div>
    <a-alert v-if="pendingCount" type="warning" show-icon style="margin-bottom: 16px" :message="`${pendingCount} 个片段变更等待审批`" />
    <section class="panel">
      <a-table :data-source="rows" row-key="id" :loading="loading" size="middle" :pagination="{ pageSize: 20, hideOnSinglePage: true }"
        :columns="[{ title: '位置', key: 'where' }, { title: '状态', key: 'st', width: 130 }, { title: '内容', key: 'content' }, { title: '', key: 'ops', width: 230 }]">
        <template #emptyText><a-empty description="还没有自定义片段" /></template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'where'">
            <div class="code">{{ where(record) }}</div>
            <div class="sub">{{ scopeName[record.scope] }}<template v-if="record.origin && record.origin !== 'manual'"> · 来自{{ record.origin === 'import' ? '配置导入' : '漂移采纳' }}</template></div>
          </template>
          <template v-else-if="column.key === 'st'">
            <a-tag v-if="record.pending_by" color="gold">{{ record.pending_delete ? '删除待审批' : record.content ? '修改待审批' : '新建待审批' }}</a-tag>
            <a-tag v-else color="green">生效中</a-tag>
          </template>
          <template v-else-if="column.key === 'content'">
            <template v-if="record.pending_by && !record.pending_delete && record.content">
              <div class="sub">待审批的变更对比（{{ record.pending_by }}，{{ dayjs(record.pending_at).format('MM-DD HH:mm') }}<template v-if="record.pending_note">：{{ record.pending_note }}</template>）</div>
              <DiffView :diff="simpleDiff(record.content, record.pending)" />
            </template>
            <template v-else>
              <pre v-if="record.content" class="snip">{{ record.content }}</pre>
              <template v-if="record.pending_by && !record.pending_delete">
                <div class="sub">待审批的新内容（{{ record.pending_by }}，{{ dayjs(record.pending_at).format('MM-DD HH:mm') }}<template v-if="record.pending_note">：{{ record.pending_note }}</template>）</div>
                <pre class="snip new">{{ record.pending }}</pre>
              </template>
              <div v-else-if="record.pending_by" class="sub">{{ record.pending_by }} 申请删除</div>
            </template>
            <div v-if="record.approved_by && record.content" class="sub">批准：{{ record.approved_by }} · sha256 {{ record.content_sha256.slice(0, 12) }}</div>
          </template>
          <template v-else-if="column.key === 'ops' && session.can('operator') && mayAct(record)">
            <a-space wrap :size="4">
              <template v-if="record.pending_by && record.pending_by !== session.username">
                <a-button size="small" type="primary" :disabled="!session.writable" @click="act(record, 'approve', '已批准，下一次发布生效')">批准</a-button>
                <a-button size="small" danger @click="act(record, 'reject', '已驳回')">驳回</a-button>
              </template>
              <a-button v-if="record.pending_by === session.username" size="small" @click="act(record, 'withdraw', '已撤回')">撤回</a-button>
              <template v-if="!record.pending_by">
                <a-button size="small" :disabled="!session.writable" @click="edit(record)">修改</a-button>
                <a-popconfirm title="申请删除该片段？批准后生效" @confirm="act(record, 'delete', '已提交删除申请')">
                  <a-button size="small" danger :disabled="!session.writable">删除</a-button>
                </a-popconfirm>
              </template>
            </a-space>
          </template>
        </template>
      </a-table>
    </section>

    <a-modal v-model:open="open" title="自定义片段" :width="780" ok-text="提交审批" :confirm-loading="saving" :ok-button-props="{ disabled: !form.content.trim() || issues.length > 0 || !!checkErr }" @ok="save">
      <a-form layout="vertical">
        <a-form-item label="插入点">
          <a-radio-group v-model:value="form.scope">
            <a-radio-button value="http">http（节点组）</a-radio-button>
            <a-radio-button value="server">server（站点）</a-radio-button>
            <a-radio-button value="location">location（路由）</a-radio-button>
            <a-radio-button v-if="canLua || form.scope === 'lua'" value="lua">Lua 脚本（路由）</a-radio-button>
          </a-radio-group>
        </a-form-item>
        <a-alert v-if="form.scope === 'lua'" type="warning" show-icon style="margin-bottom: 16px"
          message="Lua 脚本在边缘节点上处理每个命中的请求，只允许可信的管理员编写"
          description="提交后需另一位管理员批准，批准后随下一次发布生效。脚本运行在受限环境中：可以使用 ngx（不含 ngx.socket）、string、table、math、os.time / os.date 与 require('etos.json')；不能读写文件、执行命令、加载代码或模块，不能写全局变量（请用 local）。语法错误会让节点拒绝加载新配置（继续以当前配置服务）并自动回滚，错误显示在发布记录中；运行错误记入 error.log 并返回 500。组内节点须装有 Lua 附加包。" />
        <a-form-item v-if="form.scope === 'http'" label="片段名（小写字母、数字、-）"><a-input v-model:value="form.name" placeholder="例如 gzip-tuning" /></a-form-item>
        <a-space v-else>
          <a-form-item label="站点">
            <a-select v-model:value="form.site_id" style="width: 220px" :options="sites.map((s) => ({ value: s.id, label: s.name }))" @change="form.route_path = ''" />
          </a-form-item>
          <a-form-item v-if="form.scope === 'location' || form.scope === 'lua'" label="路由">
            <a-select v-model:value="form.route_path" style="width: 220px" :options="routes.map((r: any) => ({ value: r.path, label: `${r.path}（${r.mode}）` }))" />
          </a-form-item>
          <a-form-item v-if="form.scope === 'lua'" label="执行阶段">
            <a-select v-model:value="form.name" style="width: 220px" :options="phaseOptions" />
          </a-form-item>
        </a-space>
        <a-form-item :label="form.scope === 'lua' ? 'Lua 代码（带行号）' : '内容（带行号）'">
          <div class="ace-box">
            <VAceEditor
              v-model:value="form.content"
              :lang="form.scope === 'lua' ? 'lua' : 'sh'"
              theme="chrome"
              :style="{ height: form.scope === 'lua' ? '260px' : '190px' }"
              :options="{ fontSize: 13, tabSize: 2, useSoftTabs: true, showPrintMargin: false }"
            />
          </div>
        </a-form-item>
        <a-alert v-if="checkErr" type="error" :message="checkErr" show-icon style="margin-bottom: 8px" />
        <a-alert v-for="(i, k) in issues" :key="k" type="error" show-icon style="margin-bottom: 6px" :message="`第 ${i.line} 行 ${i.directive}：${i.message}`" />
        <template v-if="normalized">
          <div class="sub">{{ form.scope === 'lua' ? '格式检查通过（语法在节点加载时验证，出错自动回滚），将写入版本目录：' : '校验通过，渲染器将输出：' }}</div>
          <pre class="snip">{{ normalized }}</pre>
        </template>
        <a-form-item label="说明（写入审计）"><a-input v-model:value="form.note" :maxlength="200" /></a-form-item>
      </a-form>
      <details class="sub">
        <summary>允许与禁止的指令</summary>
        <p>允许：proxy_* 调优（超时、缓冲、头等）、rewrite、return、try_files、gzip*、expires、sub_filter*、limit_req / limit_conn、allow / deny、error_page、client_max_body_size；server 层另可用 add_header（不能覆盖安全头）、location（proxy_pass 只能指向已登记上游）、if（块内只允许 return / rewrite）；http 层另可用 limit_req_zone、limit_conn_zone、map。</p>
        <p>禁止：include、load_module、配置片段中的 Lua / Perl / njs 指令（Lua 请使用“Lua 脚本”插入点，仅管理员）、ssi、autoindex、指向站点目录外的 root / alias、未登记的回源地址、覆盖 Server 与安全头、Transfer-Encoding、listen / server_name / 证书 / 日志。</p>
      </details>
    </a-modal>
  </div>
</template>

<style scoped>
.ace-box {
  border: 1px solid #d9e2e1;
  border-radius: 6px;
  overflow: hidden;
}
.snip {
  margin: 4px 0;
  padding: 8px 10px;
  background: #f6f8f8;
  border: 1px solid #e1e7e6;
  border-radius: 6px;
  font-family: 'JetBrains Mono', 'SFMono-Regular', Menlo, Consolas, monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.snip.new {
  background: #fffbea;
  border-color: #ecd98a;
}
.sub {
  font-size: 12px;
  color: #6b7c80;
}
</style>
