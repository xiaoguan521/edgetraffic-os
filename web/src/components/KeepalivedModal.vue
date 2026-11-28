<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { message } from 'antdv-next'
import { api, errMsg } from '../api/client'

interface NodeItem {
  name: string
  ip: string
  priority: number
}

const props = defineProps<{
  open: boolean
  group?: any | null
  nodes?: any[]
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
}>()

const loading = ref(false)
const result = ref<any>(null)
const activeCfgTab = ref('0')

const form = reactive({
  mode: 'edge' as 'edge' | 'console',
  vip: '',
  interface: 'eth0',
  vrid: 51,
  auth_pass: 'syha2026',
  console_url: 'https://127.0.0.1:8443/healthz',
  nodes: [
    { name: 'node-1', ip: '', priority: 100 },
    { name: 'node-2', ip: '', priority: 90 },
  ] as NodeItem[],
})

function extractHostIP(n: any, vipSet: Set<string>): string {
  const local = String(n.local_ips || '')
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s && !vipSet.has(s) && !s.includes(':'))
  if (local.length) return local[0]
  const raw = String(n.remote_addr || '').trim()
  if (!raw) return ''
  // 去掉 :port（兼容 IPv4:port 与 [IPv6]:port）
  if (raw.startsWith('[')) {
    const end = raw.indexOf(']')
    return end > 1 ? raw.slice(1, end) : raw
  }
  const idx = raw.lastIndexOf(':')
  return idx > 0 ? raw.slice(0, idx) : raw
}

function prefillFromGroup() {
  result.value = null
  const g = props.group
  const vips = String(g?.vips || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  const vipSet = new Set(vips)
  if (form.mode === 'edge') {
    form.vip = vips[0] || ''
  }

  const list = (props.nodes || []).filter((n) => n.status !== 'revoked')
  if (form.mode === 'console') {
    form.nodes = [
      { name: 'console-a', ip: '10.0.0.11', priority: 100 },
      { name: 'console-b', ip: '10.0.0.12', priority: 90 },
    ]
  } else if (list.length >= 2) {
    form.nodes = list.slice(0, 5).map((n, i) => ({
      name: n.name || `edge-${i + 1}`,
      ip: extractHostIP(n, vipSet),
      priority: Math.max(10, 100 - i * 10),
    }))
  } else if (list.length === 1) {
    form.nodes = [
      { name: list[0].name || 'edge-1', ip: extractHostIP(list[0], vipSet), priority: 100 },
      { name: 'edge-2', ip: '', priority: 90 },
    ]
  } else {
    form.nodes = [
      { name: 'edge-1', ip: '', priority: 100 },
      { name: 'edge-2', ip: '', priority: 90 },
    ]
  }
}

watch(
  () => props.open,
  (v) => {
    if (v) prefillFromGroup()
  },
)

watch(
  () => form.mode,
  () => {
    result.value = null
    if (props.open) prefillFromGroup()
  },
)

function addPeer() {
  if (form.nodes.length >= 5) return
  const idx = form.nodes.length + 1
  form.nodes.push({
    name: `${form.mode === 'console' ? 'console' : 'edge'}-${idx}`,
    ip: '',
    priority: Math.max(10, 100 - (idx - 1) * 10),
  })
}

function removePeer(i: number) {
  if (form.nodes.length <= 2) return
  form.nodes.splice(i, 1)
}

async function generate() {
  loading.value = true
  try {
    result.value = await api.post('/keepalived/render', {
      mode: form.mode,
      group_id: form.mode === 'edge' ? props.group?.id || 0 : 0,
      vip: form.vip.trim(),
      interface: form.interface.trim() || 'eth0',
      vrid: form.vrid,
      auth_pass: form.auth_pass.trim(),
      console_url: form.console_url.trim(),
      nodes: form.nodes,
    })
    activeCfgTab.value = '0'
  } catch (e) {
    message.error(errMsg(e))
  } finally {
    loading.value = false
  }
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    message.success('已复制到剪贴板')
  } catch {
    message.warning('复制失败，请手动选中复制')
  }
}
</script>

<template>
  <a-drawer
    :open="open"
    width="min(860px, 94vw)"
    title="Keepalived 高可用配置生成器（单播 VRRP）"
    @close="emit('update:open', false)"
  >
    <a-alert
      type="info"
      show-icon
      style="margin-bottom: 14px"
      message="采用 unicast 单播 VRRP 模式（兼容物理机、虚拟机与禁止组播的云内网）。"
      description="边缘节点组登记入口 VIP 后，Agent 会只读感知本机网卡 VIP 归属：控制台自动标注主备角色、触发 VIP 漂移/失联/脑裂告警，并在灰度发布与分批热升级时优先调度备机（先备后主）。"
    />

    <a-form layout="vertical">
      <a-form-item label="高可用场景">
        <a-segmented
          v-model:value="form.mode"
          :options="[
            { label: `边缘节点入口 HA（${group?.name || '当前节点组'}）`, value: 'edge' },
            { label: '控制台主备 HA（/healthz）', value: 'console' },
          ]"
        />
      </a-form-item>

      <div class="grid3">
        <a-form-item label="入口 VIP（可选带掩码）" extra="IPv4 默认 /24，IPv6 默认 /64">
          <a-input v-model:value="form.vip" class="code" placeholder="10.0.0.100 或 10.0.0.100/24" />
        </a-form-item>
        <a-form-item label="绑定网卡">
          <a-input v-model:value="form.interface" class="code" placeholder="eth0" />
        </a-form-item>
        <a-form-item label="VRID（1–255）" extra="同网段多个 VIP 需不同">
          <a-input-number v-model:value="form.vrid" :min="1" :max="255" style="width: 100%" />
        </a-form-item>
      </div>

      <div class="grid2">
        <a-form-item label="VRRP 认证口令（1–8 字符）">
          <a-input v-model:value="form.auth_pass" class="code" :maxlength="8" />
        </a-form-item>
        <a-form-item v-if="form.mode === 'console'" label="控制台健康检查地址">
          <a-input v-model:value="form.console_url" class="code" placeholder="https://127.0.0.1:8443/healthz" />
        </a-form-item>
      </div>

      <a-form-item label="参与高可用的主机（2–5 台，单播互联）">
        <div v-for="(n, idx) in form.nodes" :key="idx" class="peer-row">
          <a-input v-model:value="n.name" placeholder="节点名" style="width: 160px" />
          <a-input v-model:value="n.ip" class="code" placeholder="主机通信 IP，如 10.0.0.11" style="flex: 1" />
          <a-input-number v-model:value="n.priority" :min="1" :max="254" addon-before="优先级" style="width: 150px" />
          <a-button v-if="form.nodes.length > 2" size="small" danger @click="removePeer(idx)">删除</a-button>
        </div>
        <a-space style="margin-top: 8px">
          <a-button v-if="form.nodes.length < 5" size="small" @click="addPeer">+ 添加主机</a-button>
          <a-button type="primary" :loading="loading" @click="generate">生成配置与探活脚本</a-button>
        </a-space>
      </a-form-item>
    </a-form>

    <template v-if="result">
      <a-divider orientation="left" style="font-size: 13px">部署步骤</a-divider>
      <pre class="hint-box">{{ result.install_hint }}</pre>

      <div class="sec-head">
        <b>1. 健康检查脚本（<span class="code">{{ result.check_script_path }}</span>）</b>
        <a-button size="small" @click="copy(result.check_script)">复制脚本</a-button>
      </div>
      <pre class="code-box">{{ result.check_script }}</pre>

      <div class="sec-head" style="margin-top: 14px">
        <b>2. 各节点 <span class="code">/etc/keepalived/keepalived.conf</span></b>
      </div>
      <a-tabs v-model:active-key="activeCfgTab" size="small">
        <a-tab-pane v-for="(cfg, idx) in result.configs" :key="String(idx)" :tab="`${cfg.name} (${cfg.ip} · 优先级 ${cfg.priority})`">
          <div style="margin-bottom: 8px">
            <a-button size="small" type="primary" @click="copy(cfg.config)">复制 {{ cfg.name }} 的 keepalived.conf</a-button>
          </div>
          <pre class="code-box">{{ cfg.config }}</pre>
        </a-tab-pane>
      </a-tabs>
    </template>
  </a-drawer>
</template>

<style scoped>
.grid3 { display: grid; grid-template-columns: 1.5fr 1fr 1fr; gap: 12px; }
.grid2 { display: grid; grid-template-columns: 1fr 1.5fr; gap: 12px; }
.peer-row { display: flex; gap: 8px; align-items: center; margin-bottom: 8px; }
.sec-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; }
.hint-box {
  white-space: pre-wrap;
  background: #f4f7f7;
  border: 1px solid #d9e3e4;
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 12px;
  line-height: 1.7;
  color: #16323a;
}
.code-box {
  white-space: pre;
  overflow: auto;
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 12px 14px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  font-size: 12px;
  line-height: 1.55;
  max-height: 360px;
}
</style>
