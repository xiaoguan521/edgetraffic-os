import { reactive } from 'vue'

// 构建时的品牌名（设计 0001 第 4.4 节），启动时从 /api/v1/brand 读取一次。
// 前端只构建一次，供任何品牌使用：这里不写死任何品牌名，否则定制品牌的控制台会残留默认名字。
export interface Brand {
  id: string // 命令名、路径、包名，例如数据面命令
  proc: string // 进程标题与对外显示名
  product: string // 产品名（简称）
  fullname: string // 产品中文全称
  agent: string // agent 程序名
  lua: string // Lua 附加包名
  instance: string // 同机多实例管理命令
}

export const brand = reactive<Brand>({ id: '', proc: '', product: '', fullname: '', agent: '', lua: '', instance: '' })

export async function loadBrand(): Promise<void> {
  // 用 fetch 而不是 api 客户端：客户端依赖路由，路由又要用品牌名设置标题
  const r = await fetch('/api/v1/brand', { headers: { 'X-Requested-With': 'etos' } })
  if (!r.ok) throw new Error(`brand: HTTP ${r.status}`)
  Object.assign(brand, (await r.json()) as Brand)
}
