import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发期前端跑在 5173，后端在 8080。
//
// 走 proxy 而不是直连 8080 + CORS：后端的 CORS 中间件会回显 Origin 并带上
// Access-Control-Allow-Credentials，本地调通了不代表线上配置对；而生产形态是
// 前端产物由 Go 二进制自己托管（同源，根本没有跨域）。用 proxy 能让开发期的
// 请求形态和生产保持一致——都是同源的 /api/v1/...，少一类「本地好的线上挂」。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    // 产物要被 Go 的 go:embed 收进二进制，路径写死在 internal/server 那边，
    // 改这里记得同步改那边的 embed 指令。
    outDir: 'dist',
    sourcemap: false,
  },
})
