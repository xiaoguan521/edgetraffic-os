import { defineStore } from 'pinia'
import { api } from '../api/client'

export interface Badges {
  pending_releases: number
  pending_snippets: number
  firing_alerts: number
  drifted_nodes: number
  offline_nodes: number
}

const STORAGE_KEY = 'syedge_active_group_id'
const SIDER_KEY = 'syedge_sider_collapsed'

export const useWorkspace = defineStore('workspace', {
  state: () => ({
    groups: [] as any[],
    groupId: Number(localStorage.getItem(STORAGE_KEY) || 0),
    siderCollapsed: localStorage.getItem(SIDER_KEY) === '1',
    hasDraftChanges: false,
    draftChangedFiles: 0,
    badges: {
      pending_releases: 0,
      pending_snippets: 0,
      firing_alerts: 0,
      drifted_nodes: 0,
      offline_nodes: 0,
    } as Badges,
  }),
  getters: {
    currentGroup: (s) => s.groups.find((g) => g.id === s.groupId) || null,
  },
  actions: {
    toggleSider() {
      this.siderCollapsed = !this.siderCollapsed
      try {
        localStorage.setItem(SIDER_KEY, this.siderCollapsed ? '1' : '0')
      } catch {
        // ignore storage errors
      }
    },
    setGroup(id: number) {
      if (!id) return
      const changed = this.groupId !== id
      this.groupId = id
      try {
        localStorage.setItem(STORAGE_KEY, String(id))
      } catch {
        // ignore storage errors
      }
      if (changed) {
        this.hasDraftChanges = false
        this.draftChangedFiles = 0
        this.refreshDraftState()
      }
    },
    markDraftDirty() {
      this.hasDraftChanges = true
      this.refreshDraftState()
    },
    async refreshDraftState() {
      if (!this.groupId) return
      const cur = this.groups.find((g) => g.id === this.groupId)
      if (cur?.management_mode === 'legacy') {
        this.hasDraftChanges = false
        this.draftChangedFiles = 0
        return
      }
      try {
        const p = await api.get(`/node-groups/${this.groupId}/preview`)
        this.hasDraftChanges = !!p?.changed
        this.draftChangedFiles = Array.isArray(p?.files) ? p.files.length : 0
      } catch {
        // ignore preview errors (e.g. incomplete draft)
      }
    },
    async loadGroups(preferredId?: number) {
      const list = await api.get<any[]>('/node-groups')
      this.groups = list
      if (!list.length) return list
      if (preferredId && list.some((g) => g.id === preferredId)) {
        this.groupId = preferredId
        try {
          localStorage.setItem(STORAGE_KEY, String(preferredId))
        } catch {
          // ignore
        }
      } else if (!list.some((g) => g.id === this.groupId)) {
        this.groupId = list[0].id
        try {
          localStorage.setItem(STORAGE_KEY, String(list[0].id))
        } catch {
          // ignore
        }
      }
      this.refreshDraftState()
      return list
    },
    async loadBadges() {
      try {
        this.badges = await api.get<Badges>('/badges')
      } catch {
        // ignore transient polling errors
      }
    },
  },
})
