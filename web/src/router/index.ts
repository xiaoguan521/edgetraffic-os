import { createRouter, createWebHistory } from 'vue-router'

// 路由与规格书 7.1 的 M1–M12 一一对应；守卫在 P5 实现（登录态、角色、License 门禁）。
export default createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/dashboard' },
    { path: '/login', component: () => import('../views/m01-login/index.vue'), meta: { title: '登录与初始化', role: 'viewer' } },
    { path: '/dashboard', component: () => import('../views/m02-dashboard/index.vue'), meta: { title: '总览', role: 'viewer' } },
    { path: '/nodes', component: () => import('../views/m03-nodes/index.vue'), meta: { title: '节点管理', role: 'operator' } },
    { path: '/sites', component: () => import('../views/m04-sites/index.vue'), meta: { title: '站点', role: 'developer' } },
    { path: '/upstreams', component: () => import('../views/m05-upstreams/index.vue'), meta: { title: '上游服务', role: 'developer' } },
    { path: '/certs', component: () => import('../views/m06-certs/index.vue'), meta: { title: '证书', role: 'operator' } },
    { path: '/releases', component: () => import('../views/m07-releases/index.vue'), meta: { title: '发布中心', role: 'releaser' } },
    { path: '/security', component: () => import('../views/m08-security/index.vue'), meta: { title: '访问控制', role: 'operator' } },
    { path: '/monitor', component: () => import('../views/m09-monitor/index.vue'), meta: { title: '监控与日志', role: 'viewer' } },
    { path: '/audit', component: () => import('../views/m10-audit/index.vue'), meta: { title: '审计', role: 'auditor' } },
    { path: '/users', component: () => import('../views/m11-users/index.vue'), meta: { title: '用户与权限', role: 'admin' } },
    { path: '/system', component: () => import('../views/m12-system/index.vue'), meta: { title: '授权与系统', role: 'admin' } },
  ],
})
