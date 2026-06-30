import { tanstackStart } from '@tanstack/react-start/plugin/vite'
import { defineConfig } from 'vite'
import viteReact from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { nitro } from 'nitro/vite'

export default defineConfig({
  server: {
    // 3000/3001 are the Usermaven app/admin dev servers on this host; the
    // console gets its own fixed port so nginx can proxy it stably.
    port: 3100,
    // Bind IPv4 loopback so nginx hits the primary upstream (127.0.0.1:3100)
    // directly; the [::1] backup in nginx still covers an occasional rebind.
    host: '127.0.0.1',
    // Vite's dev server rejects Host headers it doesn't recognise; allow the
    // domain nginx fronts this with.
    allowedHosts: ['agent-runtime-ui.azhar.dev.usrmvn.com'],
  },
  resolve: {
    tsconfigPaths: true,
  },
  plugins: [
    tailwindcss(),
    tanstackStart({
      srcDirectory: 'src',
    }),
    viteReact(),
    nitro(),
  ],
})
