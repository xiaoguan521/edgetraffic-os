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

export const useWorkspace = defineStore('workspace', {
  state: () => ({
    groups: [] as any[],
    groupId: Number(localStorage.getItem(STORAGE_KEY) || 0),
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
    setGroup(id: number) {
      if (!id) return
      this.groupId = id
      try {
        localStorage.setItem(STORAGE_KEY, String(id))
      } catch {
        // ignore storage errors
      }
    },
    async loadGroups(preferredId?: number) {
      const list = await api.get<any[]>('/node-groups')
      this.groups = list
      if (!list.length) return list
      if (preferredId && list.some((g) => g.id === preferredId)) {
        this.setGroup(preferredId)
      } else if (!list.some((g) => g.id === this.groupId)) {
        this.setGroup(list[0].id)
      }
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
