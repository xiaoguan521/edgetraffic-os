import { createApp } from 'vue'
import { createPinia } from 'pinia'
import Antdv from 'antdv-next'
import App from './App.vue'
import router from './router'
import { loadBrand } from './brand'
import './styles/base.css'

// 先取品牌名再挂载：界面上的产品名、数据面名与命令名都来自它。取不到时仍然挂载（名字为空，不影响功能）
loadBrand()
  .catch((e) => console.error(e))
  .finally(() => createApp(App).use(createPinia()).use(router).use(Antdv).mount('#app'))
