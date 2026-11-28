<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import dayjs from 'dayjs'
import { message } from 'antdv-next'
import { brand } from '../brand'

interface AccessEntry {
  ts?: string
  rid?: string
  site?: string
  ip?: string
  method?: string
  uri?: string
  status?: number
  bytes?: number
  rt?: number
  urt?: string
  ua?: string
}

interface LogRow {
  seq: number
  raw: string
  parsed?: AccessEntry
  level?: 'error' | 'warn' | 'info' | 'notice'
}

const props = defineProps<{
  node: any | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const route = useRoute()
const router = useRouter()
const MAX_LINES = 2000
const fileKind = ref<'access' | 'error'>('access')
const statusFilter = ref<'all' | '5xx' | '4xx5xx' | 'slow'>('all')
const keyword = ref('')
const autoScroll = ref(true)
const paused = ref(false)
const state = ref<'idle' | 'connecting' | 'streaming' | 'ended' | 'error'>('idle')
const errText = ref('')
const rows = ref<LogRow[]>([])
const pendingCount = ref(0)
const droppedCount = ref(0)
const viewportRef = ref<HTMLElement | null>(null)
const wrapLines = ref(true)
const streamSeconds = ref(180)
const remainingSec = ref(0)

let seqCounter = 0
let abortCtrl: AbortController | null = null
let countdownTimer: number | undefined
let pendingRows: LogRow[] = []

function parseErrorLevel(line: string): LogRow['level'] {
  const lower = line.toLowerCase()
  if (lower.includes('[emerg]') || lower.includes('[alert]') || lower.includes('[crit]') || lower.includes('[error]')) return 'error'
  if (lower.includes('[warn]')) return 'warn'
  if (lower.includes('[notice]') || lower.includes(`[${brand.agent.toLowerCase()}]`)) return 'notice'
  return 'info'
}

function buildRow(raw: string): LogRow {
  seqCounter += 1
  if (fileKind.value === 'access' && raw.startsWith('{')) {
    try {
      const obj = JSON.parse(raw) as AccessEntry
      if (obj && typeof obj === 'object' && ('status' in obj || 'uri' in obj || 'ts' in obj)) {
        return { seq: seqCounter, raw, parsed: obj }
      }
    } catch {
      // 非 JSON 行原样展示
    }
  }
  return { seq: seqCounter, raw, level: parseErrorLevel(raw) }
}

function appendLines(lines: string[]) {
  if (!lines.length) return
  const next = lines.map(buildRow)
  if (paused.value) {
    pendingRows.push(...next)
    if (pendingRows.length > MAX_LINES) {
      droppedCount.value += pendingRows.length - MAX_LINES
      pendingRows = pendingRows.slice(-MAX_LINES)
    }
    pendingCount.value = pendingRows.length
    return
  }
  pushRows(next)
}

function pushRows(next: LogRow[]) {
  if (!next.length) return
  const merged = rows.value.concat(next)
  if (merged.length > MAX_LINES) {
    droppedCount.value += merged.length - MAX_LINES
    rows.value = merged.slice(-MAX_LINES)
  } else {
    rows.value = merged
  }
  if (autoScroll.value) {
    nextTick(() => {
      const el = viewportRef.value
      if (el) el.scrollTop = el.scrollHeight
    })
  }
}

function togglePause() {
  paused.value = !paused.value
  if (!paused.value && pendingRows.length) {
    const flushed = pendingRows
    pendingRows = []
    pendingCount.value = 0
    pushRows(flushed)
  }
}

function clearLogs() {
  rows.value = []
  pendingRows = []
  pendingCount.value = 0
  droppedCount.value = 0
}

function stopStream(nextState: 'idle' | 'ended' = 'ended') {
  clearInterval(countdownTimer)
  countdownTimer = undefined
  remainingSec.value = 0
  if (abortCtrl) {
    abortCtrl.abort()
    abortCtrl = null
  }
  if (state.value === 'connecting' || state.value === 'streaming') {
    state.value = nextState
  }
}

function startCountdown(secs: number) {
  clearInterval(countdownTimer)
  remainingSec.value = secs
  countdownTimer = window.setInterval(() => {
    if (remainingSec.value > 0) remainingSec.value -= 1
    else {
      clearInterval(countdownTimer)
      countdownTimer = undefined
    }
  }, 1000)
}

async function startStream() {
  if (!props.node) return
  stopStream('idle')
  errText.value = ''
  clearLogs()
  paused.value = false
  state.value = 'connecting'

  const ctrl = new AbortController()
  abortCtrl = ctrl

  try {
    const url = `/api/v1/nodes/${props.node.id}/logs/stream?file=${fileKind.value}&seconds=${streamSeconds.value}`
    const resp = await fetch(url, {
      method: 'GET',
      credentials: 'same-origin',
      headers: { 'X-Requested-With': 'etos', Accept: 'text/event-stream' },
      signal: ctrl.signal,
    })
    if (!resp.ok) {
      if (resp.status === 401) {
        stopStream('idle')
        router.push({ name: 'login', query: { next: route.fullPath } })
        return
      }
      let msg = `拉取日志失败（HTTP ${resp.status}）`
      try {
        const body = await resp.json()
        if (resp.status === 403 && body?.code === 'PASSWORD_CHANGE_REQUIRED') {
          stopStream('idle')
          router.push({ name: 'password' })
          return
        }
        if (body?.message) msg = body.message
      } catch {
        // ignore non-json error body
      }
      throw new Error(msg)
    }
    if (!resp.body) {
      throw new Error('浏览器不支持流式响应读取')
    }
    state.value = 'streaming'
    startCountdown(streamSeconds.value)

    const reader = resp.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''

    const handleFrame = (frame: string) => {
      let evName = 'message'
      const dataLines: string[] = []
      for (const rawLine of frame.split('\n')) {
        const line = rawLine.replace(/\r$/, '')
        if (!line || line.startsWith(':')) continue
        if (line.startsWith('event:')) evName = line.slice(6).trim()
        else if (line.startsWith('data:')) dataLines.push(line.slice(5).trimStart())
      }
      if (!dataLines.length) return
      const dataStr = dataLines.join('\n')
      try {
        const payload = JSON.parse(dataStr)
        if (evName === 'ready' && payload?.seconds) {
          startCountdown(Number(payload.seconds) || streamSeconds.value)
        } else if (evName === 'chunk' && Array.isArray(payload?.lines)) {
          appendLines(payload.lines)
        } else if (evName === 'end') {
          state.value = 'ended'
          clearInterval(countdownTimer)
          remainingSec.value = 0
        }
      } catch {
        // ignore malformed frame
      }
    }

    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      let idx: number
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        handleFrame(frame)
      }
    }
    if (abortCtrl === ctrl && state.value === 'streaming') {
      state.value = 'ended'
      clearInterval(countdownTimer)
      remainingSec.value = 0
    }
  } catch (e: any) {
    if (e?.name === 'AbortError') return
    state.value = 'error'
    errText.value = e?.message || String(e)
    clearInterval(countdownTimer)
    remainingSec.value = 0
  } finally {
    if (abortCtrl === ctrl) abortCtrl = null
  }
}

const filteredRows = computed(() => {
  const q = keyword.value.trim().toLowerCase()
  return rows.value.filter((r) => {
    if (fileKind.value === 'access' && statusFilter.value !== 'all') {
      const st = r.parsed?.status ?? 0
      const rt = Number(r.parsed?.rt ?? 0)
      if (statusFilter.value === '5xx' && st < 500) return false
      if (statusFilter.value === '4xx5xx' && st < 400) return false
      if (statusFilter.value === 'slow' && rt < 1) return false
    }
    if (q && !r.raw.toLowerCase().includes(q)) return false
    return true
  })
})

function statusTone(st?: number) {
  if (!st) return 'st-other'
  if (st >= 500) return 'st-5xx'
  if (st >= 400) return 'st-4xx'
  if (st >= 300) return 'st-3xx'
  return 'st-2xx'
}

function shortTime(ts?: string) {
  if (!ts) return '—'
  const d = dayjs(ts)
  return d.isValid() ? d.format('HH:mm:ss') : ts
}

function formatMs(rt?: number) {
  if (rt === undefined || rt === null || Number.isNaN(Number(rt))) return '—'
  const ms = Number(rt) * 1000
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)}s` : `${Math.round(ms)}ms`
}

function downloadLogs() {
  const list = filteredRows.value
  if (!list.length) {
    message.info('当前没有可导出的日志行')
    return
  }
  const blob = new Blob([list.map((r) => r.raw).join('\n') + '\n'], { type: 'text/plain;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `${props.node?.name || 'node'}-${fileKind.value}-${dayjs().format('YYYYMMDD-HHmmss')}.log`
  a.click()
  URL.revokeObjectURL(a.href)
}

watch(
  () => props.node?.id,
  (id) => {
    if (id) {
      startStream()
    } else {
      stopStream('idle')
      clearLogs()
    }
  },
)

watch(fileKind, () => {
  statusFilter.value = 'all'
  if (props.node) startStream()
})

onUnmounted(() => stopStream('idle'))
</script>

<template>
  <a-drawer
    :open="!!node"
    :width="'min(1080px, 94vw)'"
    placement="right"
    :destroy-on-close="false"
    @close="emit('close')"
  >
    <template #title>
      <div class="log-title">
        <span>实时日志 · {{ node?.name }}</span>
        <span v-if="node?.remote_addr" class="sub code">{{ node.remote_addr }}</span>
        <a-tag v-if="state === 'streaming'" color="green">实时连接中<template v-if="remainingSec > 0">（{{ remainingSec }}s）</template></a-tag>
        <a-tag v-else-if="state === 'connecting'" color="processing">连接节点中…</a-tag>
        <a-tag v-else-if="state === 'ended'" color="default">已停止</a-tag>
        <a-tag v-else-if="state === 'error'" color="red">连接失败</a-tag>
        <a-tag v-if="paused" color="gold">已暂停刷新<template v-if="pendingCount">（待显示 {{ pendingCount }} 行）</template></a-tag>
      </div>
    </template>

    <div class="log-shell">
      <div class="log-toolbar">
        <a-space wrap size="small">
          <a-segmented
            v-model:value="fileKind"
            size="small"
            :options="[
              { label: '访问日志 (access.json)', value: 'access' },
              { label: '错误日志 (error.log)', value: 'error' },
            ]"
          />
          <a-segmented
            v-if="fileKind === 'access'"
            v-model:value="statusFilter"
            size="small"
            :options="[
              { label: '全部', value: 'all' },
              { label: '仅 5xx', value: '5xx' },
              { label: '4xx / 5xx', value: '4xx5xx' },
              { label: '慢请求 ≥1s', value: 'slow' },
            ]"
          />
          <a-input
            v-model:value="keyword"
            allow-clear
            size="small"
            placeholder="过滤 URI / 站点 / IP / 报错关键词"
            style="width: 230px"
          />
        </a-space>
        <a-space wrap size="small">
          <a-button size="small" @click="togglePause">{{ paused ? '继续刷新' : '暂停刷新' }}</a-button>
          <a-button v-if="state === 'streaming' || state === 'connecting'" size="small" danger @click="stopStream('ended')">停止</a-button>
          <a-button v-else size="small" type="primary" @click="startStream">重新连接</a-button>
          <a-button size="small" @click="clearLogs">清屏</a-button>
          <a-button size="small" @click="downloadLogs">导出 .log</a-button>
        </a-space>
      </div>

      <a-alert
        v-if="state === 'error'"
        type="error"
        show-icon
        style="margin-bottom: 10px"
        :message="errText || '无法连接节点日志流'"
        description="请确认节点处于在线状态，且该节点的 Agent 已升级到支持实时日志的版本。"
      />

      <div ref="viewportRef" class="log-viewport" :class="{ nowrap: !wrapLines }">
        <div v-if="!filteredRows.length" class="log-empty">
          <template v-if="state === 'connecting'">正在向节点请求近期日志并建立实时流…</template>
          <template v-else-if="rows.length">当前筛选条件下没有匹配的日志（已缓冲 {{ rows.length }} 行）</template>
          <template v-else>暂无日志输出，等待新请求或日志写入…</template>
        </div>
        <div v-for="r in filteredRows" :key="r.seq" class="log-row">
          <template v-if="r.parsed">
            <span class="c-ts">{{ shortTime(r.parsed.ts) }}</span>
            <span class="c-st" :class="statusTone(r.parsed.status)">{{ r.parsed.status ?? '-' }}</span>
            <span class="c-site">{{ r.parsed.site || '-' }}</span>
            <span class="c-method">{{ r.parsed.method || '-' }}</span>
            <span class="c-uri">{{ r.parsed.uri || '/' }}</span>
            <span class="c-rt" :class="{ slow: Number(r.parsed.rt || 0) >= 1 }">{{ formatMs(r.parsed.rt) }}</span>
            <span v-if="r.parsed.urt && r.parsed.urt !== '-'" class="c-urt">up={{ r.parsed.urt }}s</span>
            <span class="c-ip">{{ r.parsed.ip || '' }}</span>
          </template>
          <template v-else>
            <span class="c-raw" :class="`lvl-${r.level || 'info'}`">{{ r.raw }}</span>
          </template>
        </div>
      </div>

      <div class="log-foot">
        <span class="sub">
          显示 {{ filteredRows.length }} / 缓冲 {{ rows.length }} 行（上限 {{ MAX_LINES }} 行<template v-if="droppedCount">，已滚动淘汰 {{ droppedCount }} 行</template>）· 单次流最长 {{ streamSeconds }} 秒，超时后点“重新连接”继续
        </span>
        <a-space size="middle">
          <a-checkbox v-model:checked="autoScroll">自动滚到底部</a-checkbox>
          <a-checkbox v-model:checked="wrapLines">自动换行</a-checkbox>
        </a-space>
      </div>
    </div>
  </a-drawer>
</template>

<style scoped>
.log-title { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.log-shell { display: flex; flex-direction: column; height: calc(100vh - 110px); }
.log-toolbar { display: flex; justify-content: space-between; align-items: center; gap: 10px; flex-wrap: wrap; margin-bottom: 10px; }
.log-viewport {
  flex: 1;
  min-height: 300px;
  overflow: auto;
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 8px;
  padding: 10px 12px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
}
.log-viewport.nowrap .log-row { white-space: pre; }
.log-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  padding: 2px 4px;
  border-radius: 4px;
  word-break: break-all;
}
.log-row:hover { background: rgba(148, 163, 184, 0.12); }
.log-empty { color: #94a3b8; text-align: center; padding: 48px 16px; }
.c-ts { color: #94a3b8; flex-shrink: 0; }
.c-st { font-weight: 600; padding: 0 5px; border-radius: 4px; flex-shrink: 0; }
.st-2xx { background: rgba(34, 197, 94, 0.18); color: #86efac; }
.st-3xx { background: rgba(56, 189, 248, 0.18); color: #7dd3fc; }
.st-4xx { background: rgba(245, 158, 11, 0.22); color: #fcd34d; }
.st-5xx { background: rgba(239, 68, 68, 0.28); color: #fca5a5; }
.st-other { background: rgba(148, 163, 184, 0.2); color: #cbd5e1; }
.c-site { color: #a5b4fc; flex-shrink: 0; }
.c-method { color: #38bdf8; font-weight: 600; flex-shrink: 0; }
.c-uri { color: #f8fafc; }
.c-rt { color: #94a3b8; margin-left: auto; flex-shrink: 0; }
.c-rt.slow { color: #fbbf24; font-weight: 600; }
.c-urt { color: #64748b; flex-shrink: 0; }
.c-ip { color: #64748b; flex-shrink: 0; }
.c-raw { white-space: pre-wrap; }
.lvl-error { color: #fca5a5; }
.lvl-warn { color: #fcd34d; }
.lvl-notice { color: #7dd3fc; }
.lvl-info { color: #e2e8f0; }
.log-foot { display: flex; justify-content: space-between; align-items: center; gap: 10px; flex-wrap: wrap; margin-top: 10px; }
</style>
