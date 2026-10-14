import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  base: '/app/',
  build: { outDir: '../backend/cmd/mdns-gateway/web/app', emptyOutDir: true },
  server: { proxy: { '/api': 'http://localhost:8080', '/health': 'http://localhost:8080' } }
})
