import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import './style.css'

/** 应用状态通过 Pinia 注入，页面仅调用后端 API。 */
createApp(App).use(createPinia()).mount('#app')
