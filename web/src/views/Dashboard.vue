<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import dayjs from 'dayjs'
import { api } from '../api/client'
import TrafficChart from '../components/TrafficChart.vue'
import { deployStatus, kindName } from '../components/status'
import { useWorkspace } from '../stores/workspace'

const router = useRouter()
const ws = useWorkspace()
const data = ref<any>(null)
const nodes = ref<any[]>([])
const groups = ref<any[]>([])
const nodeFilter = ref<'all' | 'abnormal' | 'online' | 'offline'>('all')
const nodeView = ref<'card' | 'table'>('card')
const showAllNodes = ref(false)
const rangeMinutes = ref(30)
const metricGroupId = ref(0)
const series = ref<any[]>([])

async function loadSeries() {
  const r = await api.get('/metrics', { minutes: rangeMinutes.value, ...(metricGroupId.value ? { group_id: metricGroupId.value } : {}) })
  series.value = r.series || []
}

async function load() {
  const [d, n, g] = await Promise.all([api.get('/dashboard'), api.get('/nodes'), api.get('/node-groups')])
  data.value = d
  nodes.value = n.filter((x: any) => x.status !== 'revoked')
  groups.value = g
  if (rangeMinutes.value === 30 && !metricGroupId.value) {
    series.value = d.series || []
  } else {
    await loadSeries()
  }
}

watch([rangeMinutes, metricGroupId], loadSeries)

let t: number | undefined
onMounted(() => {
  load()
  ws.loadBadges()
  t = window.setInterval(load, 5000)
})
onUnmounted(() => clearInterval(t))

function groupName(id: number) {
  return groups.value.find((g) => g.id === id)?.name || id
}
function currentRev(groupId: number) {
  return groups.value.find((g) => g.id === groupId)?.current_revision_id || ''
}
function isAbnormal(n: any) {
  return n.drifted || n.status !== 'online' || (currentRev(n.group_id) && n.applied_revision !== currentRev(n.group_id))
}
const abnormalCount = computed(() => nodes.value.filter(isAbnormal).length)
const pendingApprovals = computed(() => (ws.badges.pending_releases || 0) + (ws.badges.pending_snippets || 0))
const filteredNodes = computed(() => {
  let list = nodes.value
  if (nodeFilter.value === 'abnormal') list = list.filter(isAbnormal)
  else if (nodeFilter.value === 'online') list = list.filter((n) => n.status === 'online')
  else if (nodeFilter.value === 'offline') list = list.filter((n) => n.status !== 'online')
  return [...list].sort((a, b) => Number(isAbnormal(b)) - Number(isAbnormal(a)))
})
const visibleNodes = computed(() => (showAllNodes.value ? filteredNodes.value : filteredNodes.value.slice(0, 12)))

function goNode(n: any) {
  ws.setGroup(n.group_id)
  router.push({ name: 'nodes', query: { group: n.group_id } })
}

function ago(t?: string) {
  if (!t) return '从未'
  const s = dayjs().diff(dayjs(t), 'second')
  return s < 60 ? `${s} 秒前` : s < 3600 ? `${Math.floor(s / 60)} 分钟前` : dayjs(t).format('MM-DD HH:mm')
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>全网概览</h1>
        <p>实时掌握边缘节点健康、核心流量与最近发布动态。</p>
      </div>
      <a-space>
        <router-link to="/releases"><a-button>去发布</a-button></router-link>
        <router-link to="/nodes"><a-button type="primary">添加节点</a-button></router-link>
      </a-space>
    </div>

    <section v-if="data && data.nodes === 0 && data.sites === 0" class="panel onboard">
      <h2>3 步快速上手</h2>
      <div class="steps-grid">
        <div class="step-card" @click="router.push('/nodes')">
          <span class="step-no">1</span>
          <div>
            <strong>接入或接管边缘节点</strong>
            <p>全新部署执行注册命令，已有独立数据面点击“接管独立节点”安全迁入。</p>
          </div>
        </div>
        <div class="step-card" @click="router.push('/sites')">
          <span class="step-no">2</span>
          <div>
            <strong>配置上游服务与站点</strong>
            <p>定义回源后端、域名、HTTPS 证书、路由转发与访问规则。</p>
          </div>
        </div>
        <div class="step-card" @click="router.push('/releases')">
          <span class="step-no">3</span>
          <div>
            <strong>预览差异并首次发布</strong>
            <p>核对生成的配置 Diff，一键热重载下发，失败自动回滚不中断流量。</p>
          </div>
        </div>
      </div>
    </section>

    <a-alert v-if="data && (data.certs_expired || data.certs_expiring)" :type="data.certs_expired ? 'error' : 'warning'" show-icon style="margin-bottom: 16px">
      <template #message>
        <span v-if="data.certs_expired"><b class="num">{{ data.certs_expired }}</b> 张证书已过期</span>
        <span v-if="data.certs_expired && data.certs_expiring">，</span>
        <span v-if="data.certs_expiring"><b class="num">{{ data.certs_expiring }}</b> 张证书 30 天内到期</span>
        <router-link to="/certificates" style="margin-left: 12px">去续期</router-link>
      </template>
    </a-alert>

    <section v-if="data" class="kpis">
      <div class="kpi" :class="abnormalCount ? 'warn' : 'ok'" @click="router.push('/nodes')">
        <div class="klbl">边缘节点</div>
        <div class="kval num">{{ data.online }} <span class="kden">/ {{ data.nodes }} 在线</span></div>
        <div class="ksub">{{ abnormalCount ? `${abnormalCount} 台异常（离线 / 漂移 / 落后）` : `共 ${data.groups} 个节点组，全部健康` }}</div>
      </div>
      <div class="kpi" :class="data.certs_expired ? 'bad' : data.certs_expiring ? 'warn' : 'ok'" @click="router.push('/sites')">
        <div class="klbl">站点与证书</div>
        <div class="kval num">{{ data.sites }} <span class="kden">个站点</span></div>
        <div class="ksub">
          <template v-if="data.certs_expired">{{ data.certs_expired }} 张证书已过期</template>
          <template v-else-if="data.certs_expiring">{{ data.certs_expiring }} 张证书即将到期</template>
          <template v-else>证书状态正常</template>
        </div>
      </div>
      <div class="kpi" :class="data.error_rate_15m >= 5 ? 'bad' : data.error_rate_15m >= 1 ? 'warn' : 'ok'">
        <div class="klbl">近 15 分钟流量</div>
        <div class="kval num">{{ data.requests_15m.toLocaleString() }} <span class="kden">次请求</span></div>
        <div class="ksub">5xx 占比 <b class="num">{{ data.error_rate_15m.toFixed(2) }}%</b></div>
      </div>
      <div class="kpi" :class="ws.badges.firing_alerts ? 'bad' : pendingApprovals ? 'warn' : 'ok'" @click="router.push(ws.badges.firing_alerts ? '/alerts' : '/releases')">
        <div class="klbl">待办与告警</div>
        <div class="kval num">{{ ws.badges.firing_alerts }} <span class="kden">告警中</span></div>
        <div class="ksub">{{ pendingApprovals ? `${pendingApprovals} 项发布 / 片段待审批` : '无待处理审批' }}</div>
      </div>
    </section>

    <div class="grid" v-if="data">
      <section class="panel">
        <div class="chart-head">
          <h2>流量监控</h2>
          <a-space size="small" wrap>
            <a-select
              v-model:value="metricGroupId"
              size="small"
              style="width: 140px"
              :options="[{ value: 0, label: '全网节点组' }, ...groups.map((g) => ({ value: g.id, label: g.name }))]"
            />
            <a-segmented
              v-model:value="rangeMinutes"
              size="small"
              :options="[
                { label: '30分', value: 30 },
                { label: '1小时', value: 60 },
                { label: '6小时', value: 360 },
                { label: '24小时', value: 1440 },
                { label: '7天', value: 10080 },
              ]"
            />
          </a-space>
        </div>
        <TrafficChart :series="series" />
      </section>
      <section class="panel">
        <div class="chart-head">
          <h2>最近发布</h2>
          <router-link to="/releases" style="font-size: 12.5px">查看全部</router-link>
        </div>
        <a-empty v-if="!data.deployments.length" description="还没有发布记录" />
        <ul class="deps">
          <li v-for="d in data.deployments" :key="d.id">
            <router-link :to="{ name: 'releases', query: { d: d.id, group: d.group_id } }" class="num">{{ d.revision_id }}</router-link>
            <span class="kind">{{ kindName[d.kind] || d.kind }}</span>
            <a-tag :color="deployStatus[d.status]?.color">{{ deployStatus[d.status]?.text || d.status }}</a-tag>
            <span class="note">{{ d.note }}</span>
            <time>{{ ago(d.started_at) }}</time>
          </li>
        </ul>
      </section>
    </div>

    <section class="panel" v-if="data" style="margin-top: 16px">
      <div class="chart-head">
        <h2>节点健康状态</h2>
        <a-space size="small" wrap>
          <a-segmented
            v-if="nodes.length > 0"
            v-model:value="nodeFilter"
            size="small"
            :options="[
              { label: `全部 (${nodes.length})`, value: 'all' },
              { label: `异常 (${abnormalCount})`, value: 'abnormal' },
              { label: '在线', value: 'online' },
              { label: '离线', value: 'offline' },
            ]"
          />
          <a-segmented
            v-if="nodes.length > 0"
            v-model:value="nodeView"
            size="small"
            :options="[{ label: '卡片', value: 'card' }, { label: '列表', value: 'table' }]"
          />
        </a-space>
      </div>
      <a-empty v-if="!filteredNodes.length" :description="nodes.length ? '当前筛选条件下没有节点' : '还没有节点。点击右上角“添加节点”接入边缘服务器。'" />
      <template v-else-if="nodeView === 'card'">
        <div class="fleet">
          <article
            v-for="n in visibleNodes"
            :key="n.id"
            class="node clickable"
            :class="[n.status, { drift: n.drifted, behind: n.status === 'online' && currentRev(n.group_id) && n.applied_revision !== currentRev(n.group_id) }]"
            @click="goNode(n)"
          >
            <header>
              <span class="light" />
              <strong>{{ n.name }}</strong>
              <span class="grp">{{ groupName(n.group_id) }}</span>
            </header>
            <div class="rev num">{{ n.applied_revision || '未发布' }}</div>
            <div class="meta">
              <template v-if="n.status === 'online'">
                <span class="num">{{ n.active_connections }}</span> 连接 · CPU <span class="num">{{ Math.round(n.cpu_percent) }}%</span>
              </template>
              <template v-else>离线，最后心跳 {{ ago(n.last_seen_at) }}</template>
            </div>
            <div v-if="n.drifted" class="flag">配置被手动改动</div>
            <div v-else-if="n.status === 'online' && currentRev(n.group_id) && n.applied_revision !== currentRev(n.group_id)" class="flag">落后于组版本 {{ currentRev(n.group_id) }}</div>
          </article>
        </div>
        <div v-if="filteredNodes.length > 12" style="margin-top: 8px; text-align: center">
          <a-button type="link" size="small" @click="showAllNodes = !showAllNodes">
            {{ showAllNodes ? '收起节点列表' : `展开全部 ${filteredNodes.length} 个节点` }}
          </a-button>
        </div>
      </template>
      <a-table
        v-else
        :data-source="filteredNodes"
        row-key="id"
        size="small"
        :pagination="{ pageSize: 15, hideOnSinglePage: true }"
        :scroll="{ x: 'max-content' }"
        :columns="[{ title: '节点', key: 'name' }, { title: '节点组', key: 'grp' }, { title: '状态', key: 'st' }, { title: '当前版本', key: 'rev' }, { title: '连接 / CPU', key: 'load' }, { title: '最后心跳', key: 'seen' }]"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'"><a @click="goNode(record)">{{ record.name }}</a></template>
          <template v-else-if="column.key === 'grp'">{{ groupName(record.group_id) }}</template>
          <template v-else-if="column.key === 'st'">
            <a-badge :status="record.status === 'online' ? 'success' : 'default'" :text="record.status === 'online' ? '在线' : '离线'" />
            <a-tag v-if="record.drifted" color="red" style="margin-left: 6px">漂移</a-tag>
          </template>
          <template v-else-if="column.key === 'rev'">
            <span class="num">{{ record.applied_revision || '—' }}</span>
            <a-tag v-if="record.status === 'online' && currentRev(record.group_id) && record.applied_revision !== currentRev(record.group_id)" color="gold" style="margin-left: 6px">落后</a-tag>
          </template>
          <template v-else-if="column.key === 'load'">
            <span v-if="record.status === 'online'" class="num">{{ record.active_connections }} 连接 · {{ Math.round(record.cpu_percent) }}%</span>
            <span v-else>—</span>
          </template>
          <template v-else-if="column.key === 'seen'">{{ ago(record.last_seen_at) }}</template>
        </template>
      </a-table>
    </section>
  </div>
</template>

<style scoped>
.onboard {
  margin-bottom: 16px;
  border-color: #b7dcdf;
  background: linear-gradient(180deg, #f5fbfb 0%, #ffffff 100%);
}
.steps-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 12px;
}
.step-card {
  display: flex;
  gap: 12px;
  align-items: flex-start;
  padding: 12px 14px;
  border: 1px solid #d9eaec;
  border-radius: 8px;
  background: #fff;
  cursor: pointer;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.step-card:hover {
  border-color: #0e7c86;
  box-shadow: 0 2px 8px rgba(14, 124, 134, 0.08);
}
.step-no {
  width: 24px;
  height: 24px;
  line-height: 24px;
  border-radius: 50%;
  background: #0e7c86;
  color: #fff;
  text-align: center;
  font-size: 12px;
  font-weight: 600;
  flex-shrink: 0;
}
.step-card p {
  margin: 4px 0 0;
  font-size: 12.5px;
  color: #6b7c80;
}
.kpis {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 14px;
  margin-bottom: 16px;
}
.kpi {
  background: #fff;
  border: 1px solid #e1e7e6;
  border-top: 3px solid #0e7c86;
  border-radius: 8px;
  padding: 14px 16px;
  cursor: pointer;
  transition: box-shadow 0.15s ease;
}
.kpi:hover {
  box-shadow: 0 3px 10px rgba(22, 50, 58, 0.06);
}
.kpi.ok {
  border-top-color: #2f9e5b;
}
.kpi.warn {
  border-top-color: #d9a21b;
}
.kpi.bad {
  border-top-color: #c8423b;
}
.klbl {
  font-size: 12.5px;
  color: #6b7c80;
}
.kval {
  font-size: 24px;
  font-weight: 650;
  color: #16323a;
  margin: 6px 0 4px;
  line-height: 1.2;
}
.kden {
  font-size: 13px;
  font-weight: 400;
  color: #6b7c80;
}
.ksub {
  font-size: 12px;
  color: #6b7c80;
}
.fleet {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 12px;
}
.node {
  position: relative;
  background: #fbfcfc;
  border: 1px solid #e1e7e6;
  border-left: 4px solid #2f9e5b;
  border-radius: 6px;
  padding: 11px 14px;
  transition: box-shadow 0.15s ease, transform 0.15s ease;
}
.node.clickable {
  cursor: pointer;
}
.node.clickable:hover {
  background: #fff;
  box-shadow: 0 3px 10px rgba(22, 50, 58, 0.08);
  transform: translateY(-1px);
}
.node.offline {
  border-left-color: #b8c3c2;
  background: #fafbfb;
}
.node.behind {
  border-left-color: #d9a21b;
}
.node.drift {
  border-left-color: #c8423b;
}
.node header {
  display: flex;
  align-items: center;
  gap: 8px;
}
.light {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #2f9e5b;
  box-shadow: 0 0 0 3px #e6f3ec;
}
.offline .light {
  background: #b8c3c2;
  box-shadow: none;
}
.grp {
  margin-left: auto;
  font-size: 12px;
  color: #6b7c80;
}
.rev {
  margin: 8px 0 4px;
  font-size: 19px;
  font-weight: 600;
  color: #16323a;
}
.offline .rev {
  color: #8a9a9d;
}
.meta {
  font-size: 12px;
  color: #6b7c80;
}
.flag {
  margin-top: 6px;
  font-size: 12px;
  color: #a3332d;
}
.behind .flag {
  color: #8a6510;
}
.chart-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 10px;
}
.chart-head h2 {
  margin: 0;
}
.grid {
  display: grid;
  grid-template-columns: 1.6fr 1fr;
  gap: 16px;
  align-items: start;
}
.grid .panel + .panel {
  margin-top: 0;
}
.deps {
  list-style: none;
  margin: 0;
  padding: 0;
}
.deps li {
  display: grid;
  grid-template-columns: 76px 64px auto 1fr auto;
  align-items: center;
  gap: 8px;
  padding: 9px 0;
  border-bottom: 1px solid #eef2f1;
  font-size: 13px;
}
.deps li:last-child {
  border-bottom: 0;
}
.kind {
  color: #6b7c80;
}
.note {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: #3b4a4e;
}
time {
  color: #8a9a9d;
  font-size: 12px;
}
@media (max-width: 1080px) {
  .kpis {
    grid-template-columns: repeat(2, 1fr);
  }
  .steps-grid {
    grid-template-columns: 1fr;
  }
}
@media (max-width: 980px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
</style>
